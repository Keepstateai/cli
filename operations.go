// operations.go: the client half of KS-004. Every request has a deadline;
// every mutation carries an idempotency key that is written to disk BEFORE
// the request leaves, is retried only with that same key and body, and is
// never replaced by a fresh key after a timeout; an outcome the network
// swallowed is recovered by reading the operation back, never by guessing.
//
// The control plane's operation layer is feature-detected: a control plane
// that does not know GET /api/operations answers 404, and the client says
// so instead of inventing a state.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// C04 defaults. KS_HTTP_TIMEOUT_MS scales them for tests that must observe a
// timeout in seconds rather than minutes; it is configuration, not a hook,
// and it can only make the client MORE impatient.
const (
	connectTimeout  = 10 * time.Second
	responseTimeout = 30 * time.Second
	waitTimeout     = 120 * time.Second
	pollInterval    = 2 * time.Second
)

func scaledTimeouts() (connect, response time.Duration) {
	connect, response = connectTimeout, responseTimeout
	if v := os.Getenv("KS_HTTP_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 && time.Duration(ms)*time.Millisecond < response {
			d := time.Duration(ms) * time.Millisecond
			return d, d
		}
	}
	return
}

// ordinaryClient bounds connect, TLS handshake and the whole response.
func ordinaryClient() *http.Client {
	connect, response := scaledTimeouts()
	return &http.Client{
		Timeout: response,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: connect}).DialContext,
			TLSHandshakeTimeout:   connect,
			ResponseHeaderTimeout: response,
			Proxy:                 http.ProxyFromEnvironment,
		},
	}
}

// streamClient bounds the dial and the headers but not the body: an
// artifact download is as long as the artifact.
func streamClient() *http.Client {
	connect, response := scaledTimeouts()
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: connect}).DialContext,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: response,
		Proxy:                 http.ProxyFromEnvironment,
	}}
}

// ---------------------------------------------------------------------
// the local operation ledger
// ---------------------------------------------------------------------

type localOp struct {
	Key        string `json:"key"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	BodySHA    string `json:"body_sha256"`
	CTL        string `json:"ctl"`
	CreatedAt  string `json:"created_at"`
	Outcome    string `json:"outcome,omitempty"`       // filled in when known: "sent", "replayed", "unknown", "failed"
	Submission string `json:"submission_id,omitempty"` // the queue-level id a submitted instruction carries
}

func operationsPath() string { return filepath.Join(configDir(), "operations.jsonl") }

func newIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "ksop_" + hex.EncodeToString(b)
}

// recordOperation appends the intent before the request is sent, so a
// crash between the write and the reply still leaves the key to ask about.
func recordOperation(op localOp) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(operationsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(op)
	_, err = f.Write(append(b, '\n'))
	return err
}

// recordedOperations reads the journal back. A line this client cannot
// read is skipped rather than failing the command that consults it: the
// journal is a record to read, never a lock to hold.
func recordedOperations() []localOp {
	b, err := os.ReadFile(operationsPath())
	if err != nil {
		return nil
	}
	var out []localOp
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var o localOp
		if json.Unmarshal([]byte(line), &o) == nil && o.Key != "" {
			out = append(out, o)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// bounded requests
// ---------------------------------------------------------------------

// transportErr reports whether the request failed before a response was
// read, which is the only case in which the outcome is unknown. mutation
// says whether the request could have started remote work.
type transportErr struct {
	err      error
	mutation bool
}

func (e transportErr) Error() string { return e.err.Error() }

// uncertain: the reply was lost after the request may have been acted on.
// A transport failure on a mutation, or a 502/504 from a control plane that
// could not hear back from its fleet, are the two shapes.
func uncertain(err error, resp *http.Response) bool {
	var te transportErr
	if errors.As(err, &te) {
		return te.mutation
	}
	return resp != nil && (resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout)
}

func doBounded(cr hostedCreds, method, path string, headers map[string]string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, strings.TrimRight(cr.CTL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cr.Token)
	req.Header.Set(protocolHeader, clientProtocolText)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ordinaryClient().Do(req)
	if err != nil {
		return nil, nil, transportErr{err: err, mutation: method != "GET"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, transportErr{err: err, mutation: method != "GET"}
	}
	return resp, raw, nil
}

// hostedMutate sends one mutation with a persisted idempotency key, ONCE.
//
// The follow-up review of 2026-09-20 (R01-U, R01-S, R01-O) settled the
// rule this implements. Holding the same key is necessary but not
// sufficient for a safe resend: the receiver must have enforced that key
// for the ORIGINAL request, and nothing the client learns afterwards (a
// capability registry that now says "available", a 503 that looked like a
// refusal) proves that about a request already sent. A mixed rollout can
// serve the first request from a node that ignored the key and the resend
// from one that honours it, and a front door can report 503 after the
// origin did the work. So:
//
//   - one submission on the successful path, no extra request;
//   - an uncertain outcome (nothing came back, a 5xx, a 429, an
//     unparseable success) is RECOVERED BY READ where the control plane
//     keeps operation records, and reported as unknown with the key and a
//     practical next step where it does not, or where no record exists;
//   - nothing is resent automatically. Automatic resubmission returns only
//     under a protocol guarantee that covers the first submission and the
//     retained records (KS-013/KS-020), see replayGuaranteed.
//
// A 202 receipt is followed by polling the record, which is a read.
func hostedMutate(cr hostedCreds, method, path string, body any, out any) error {
	var raw []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		raw = b
	default:
		raw, _ = json.Marshal(b)
	}
	key := newIdempotencyKey()
	sum := sha256Hex(raw)
	if err := recordOperation(localOp{Key: key, Method: method, Path: path, BodySHA: sum, CTL: cr.CTL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return fmt.Errorf("could not record the operation locally before sending it (%v); nothing was sent", err)
	}
	headers := map[string]string{"Idempotency-Key": key, "Prefer": "respond-async"}
	resp, body2, err := doBounded(cr, method, path, headers, raw)
	switch {
	case err != nil:
		return recoverUncertain(cr, key, out, "the reply was lost: "+sanitize(err.Error()))
	case resp.StatusCode == http.StatusAccepted:
		var receipt struct {
			Data struct {
				OperationID string `json:"operation_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body2, &receipt)
		id := receipt.Data.OperationID
		if id == "" {
			id = key
		}
		return waitOperation(cr, id, key, out)
	case resp.StatusCode/100 == 2:
		if resp.Header.Get("KS-Operation-Replayed") == "true" {
			progress("the control plane already had this operation (%s); showing its original result", key)
		}
		if out != nil && len(body2) > 0 {
			if uerr := json.Unmarshal(body2, out); uerr != nil {
				// a success the client cannot read: the work happened, the
				// result did not arrive intact; never resend for a better copy
				return recoverUncertain(cr, key, out, "the control plane answered "+resp.Status+" with a result this client could not read")
			}
		}
		return nil
	case definiteRefusal(resp, body2):
		return hostedError(method, path, resp, body2) // answered before any dispatch: no work
	default:
		// 5xx, 429, 408: the answer says nothing certain about whether the
		// request was applied on the way through
		he := hostedError(method, path, resp, body2)
		return recoverUncertain(cr, key, out, sanitize(he.Error()))
	}
}

// definiteRefusal: a 4xx the origin returns before dispatching anything.
// 429 and 408 are excluded because a front door can send them after the
// origin has done the work; only a KS-typed refusal (error.type ks_…) is
// taken as a pre-admission contract for those.
func definiteRefusal(resp *http.Response, raw []byte) bool {
	if resp.StatusCode/100 != 4 {
		return false
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		var e struct {
			Error struct{ Type string } `json:"error"`
		}
		return json.Unmarshal(raw, &e) == nil && strings.HasPrefix(e.Error.Type, "ks_")
	}
	return true
}

// replayGuaranteed is the one place an automatic resend could ever be
// authorised: only when a protocol negotiated BEFORE the first submission
// guarantees that the route enforced the key for that submission and that
// the records survive rollout and rollback (KS-013/KS-020). No such
// protocol exists on /api yet, so the answer is no.
func replayGuaranteed(cr hostedCreds) bool { return false }

// recoverUncertain establishes the outcome of a submission whose reply was
// not usable. Where the control plane keeps operation records, the key is
// read back and a found record settles the result exactly as the original
// reply would have. A missing record is NOT proof that nothing happened: a
// node that ignored the key may have done the work without recording it.
// So every other path reports unknown, keeps the key, and never resends.
func recoverUncertain(cr hostedCreds, key string, out any, what string) error {
	if replayGuaranteed(cr) {
		// reserved for the negotiated protocol; unreachable today
		return nil
	}
	next := "check the console's session list before running this again; operation " + key + " is recorded locally in " + operationsPath()
	o, err := fetchOperation(cr, key)
	var te transportErr
	switch {
	case err == nil:
		progress("the control plane holds a record for operation %s; using it", key)
		return settleOperation(cr, o, key, out)
	case errors.Is(err, errNoOperations):
		what += ". This control plane keeps no operation records, so the outcome cannot be read back here"
	case errors.As(err, &te):
		what += ". The operation record could not be read either (" + sanitize(te.err.Error()) + ")"
		next = "ks operation show " + key + " when the control plane answers again, and check the console's session list before running this again"
	default:
		what += ". No record of this operation was found on the control plane, which does not prove nothing happened"
		next = "ks operation show " + key + " once the control plane has settled, and check the console's session list before running this again"
	}
	fail(&cliError{Code: exitTemporary, Kind: "outcome_unknown", Message: "the request did not complete: " + what + "; it was sent once and not resent, and the work may or may not have started",
		WorkStarted: workUnknown, OperationID: key, NextAction: next})
	return nil
}

type remoteOp struct {
	ID             string          `json:"operation_id"`
	State          string          `json:"state"`
	HTTPStatus     int             `json:"http_status"`
	Response       json.RawMessage `json:"response"`
	Error          string          `json:"error"`
	AcceptedAt     string          `json:"accepted_at"`
	FinishedAt     string          `json:"finished_at"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	ResourceIDs    []string        `json:"resource_ids"`
	Reconciliation string          `json:"reconciliation_state"`
}

var errNoOperations = errors.New("this control plane does not serve operation records (no GET /api/operations); the outcome cannot be read back here")

func fetchOperation(cr hostedCreds, id string) (*remoteOp, error) {
	resp, raw, err := doBounded(cr, "GET", "/api/operations/"+id, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		var e struct {
			Error struct{ Type string } `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Type == "ks_not_found" {
			return nil, fmt.Errorf("no operation %s on the control plane", id)
		}
		return nil, errNoOperations
	}
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("GET", "/api/operations/"+id, resp, raw)
	}
	var env struct {
		Data remoteOp `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	// a record is a record only with an id and a state; anything else is an
	// answer this client cannot read, never a running operation to poll
	if env.Data.ID == "" || env.Data.State == "" {
		return nil, fmt.Errorf("the operation record could not be read (unexpected shape)")
	}
	return &env.Data, nil
}

// settleOperation turns a finished operation into the caller's result or
// error, exactly as the original response would have.
func settleOperation(cr hostedCreds, o *remoteOp, key string, out any) error {
	switch o.State {
	case "succeeded":
		if out != nil && len(o.Response) > 0 {
			return json.Unmarshal(o.Response, out)
		}
		return nil
	case "failed":
		if o.Error != "" {
			return fmt.Errorf("%s (operation %s)", o.Error, o.ID)
		}
		return hostedError(o.Method, o.Path, &http.Response{StatusCode: o.HTTPStatus, Status: strconv.Itoa(o.HTTPStatus)}, o.Response)
	case "reconciliation_required":
		// C05: completion could not be proven; only reconciliation resolves
		// it. Terminal for this command: unknown, the record named, no wait,
		// no resend.
		fail(&cliError{Code: exitTemporary, Kind: "reconciliation_required", Message: fmt.Sprintf("operation %s needs reconciliation: %s", o.ID, sanitize(o.Error)),
			WorkStarted: workUnknown, OperationID: o.ID, NextAction: "ks operation show " + o.ID + " after reconciliation; do not resubmit"})
		return nil
	default:
		if o.ID == "" {
			return fmt.Errorf("the operation record names no id; nothing to wait for")
		}
		return waitOperation(cr, o.ID, key, out)
	}
}

// waitOperation polls a running operation until it finishes or the wait
// bound passes. Ctrl-C stops the local waiting only: the operation keeps
// running, and the message says how to read it later.
func waitOperation(cr hostedCreds, id, key string, out any) error {
	bound := waitBound
	if v := os.Getenv("KS_WAIT_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			bound = time.Duration(ms) * time.Millisecond
		}
	}
	interval := pollInterval
	if bound < interval*4 {
		interval = bound / 4
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	deadline := time.Now().Add(bound)
	progress("operation %s accepted; waiting up to %s", id, bound.Round(time.Second))
	for {
		select {
		case <-sig:
			fail(&cliError{Code: exitInterrupt, Kind: "interrupted", Message: "interrupted locally; the operation continues on the control plane", WorkStarted: workYes, OperationID: id, NextAction: "ks operation show " + id})
		case <-time.After(interval):
		}
		o, err := fetchOperation(cr, id)
		if err != nil {
			if errors.Is(err, errNoOperations) {
				// the route vanished under an accepted operation (a rollback):
				// the work is on the control plane; nothing here may resubmit it
				fail(&cliError{Code: exitTemporary, Kind: "outcome_unknown", Message: fmt.Sprintf("operation %s was accepted, and this control plane no longer serves operation records, so its outcome cannot be read back here", id),
					WorkStarted: workUnknown, OperationID: id, NextAction: "do not resubmit; check the console, or ks operation show " + id + " once the control plane serves records again"})
			}
			progress("reading the operation: %v (still waiting)", err)
		} else if o.State == "succeeded" || o.State == "failed" || o.State == "reconciliation_required" {
			return settleOperation(cr, o, key, out)
		}
		if time.Now().After(deadline) {
			fail(&cliError{Code: exitTemporary, Kind: "still_running", Message: fmt.Sprintf("operation %s is still running after %s; it continues on the control plane", id, bound.Round(time.Second)),
				WorkStarted: workYes, OperationID: id, NextAction: "ks operation show " + id})
		}
	}
}

func sha256Hex(b []byte) string {
	h := sha256Sum(b)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------
// ks operation show | wait
// ---------------------------------------------------------------------

func hostedOperationShow(cr hostedCreds, inv *Invocation) {
	o, err := fetchOperation(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	printOperation(o)
}

func hostedOperationWait(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	o, err := fetchOperation(cr, id)
	if err != nil {
		die(err)
	}
	if o.State != "succeeded" && o.State != "failed" {
		var raw json.RawMessage
		if err := waitOperation(cr, id, id, &raw); err != nil {
			die(err)
		}
		if o, err = fetchOperation(cr, id); err != nil {
			die(err)
		}
	}
	printOperation(o)
}

func printOperation(o *remoteOp) {
	emit(o, func() { printOperationText(o) })
}

func printOperationText(o *remoteOp) {
	fmt.Printf("operation %s  state %s\n", o.ID, o.State)
	fmt.Printf("request: %s %s\n", o.Method, o.Path)
	fmt.Printf("accepted: %s\n", o.AcceptedAt)
	if o.FinishedAt != "" {
		fmt.Printf("finished: %s (HTTP %d)\n", o.FinishedAt, o.HTTPStatus)
	}
	if o.Reconciliation != "" {
		fmt.Printf("reconciliation: %s\n", o.Reconciliation)
	}
	if len(o.ResourceIDs) > 0 {
		fmt.Printf("resources: %s\n", strings.Join(o.ResourceIDs, ", "))
	}
	if o.Error != "" {
		fmt.Printf("error: %s\n", o.Error)
	}
	if len(o.Response) > 0 {
		fmt.Printf("result: %s\n", strings.TrimSpace(string(o.Response)))
	}
}
