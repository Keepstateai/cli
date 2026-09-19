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
	"math/big"
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
	connectTimeout   = 10 * time.Second
	responseTimeout  = 30 * time.Second
	waitTimeout      = 120 * time.Second
	pollInterval     = 2 * time.Second
	mutationAttempts = 3
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
	Key       string `json:"key"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	BodySHA   string `json:"body_sha256"`
	CTL       string `json:"ctl"`
	CreatedAt string `json:"created_at"`
	Outcome   string `json:"outcome,omitempty"` // filled in when known: "sent", "replayed", "unknown", "failed"
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

// replaySupported asks the control plane, live and just before the first
// attempt, whether it enforces the Idempotency-Key it is about to be sent.
// Replaying the same key is only safe where the receiver deduplicates it:
// on a control plane that ignores the header, a replay is a second session
// (review R01, 2026-09-20). A cached answer, a client version or the fact
// that the header is being sent prove nothing here; only the live registry
// does, and a registry that cannot be read means no.
func replaySupported(cr hostedCreds) (bool, string) {
	set, err := fetchCapabilities(cr)
	if err != nil {
		if errors.Is(err, errNoOperations) {
			return false, "this control plane publishes no capability registry, so it cannot confirm that it deduplicates operation keys"
		}
		return false, "the capability registry could not be read (" + sanitize(err.Error()) + "), so replay support is unconfirmed"
	}
	row := set.find("operations.idempotent")
	switch {
	case row == nil:
		return false, "this control plane does not list operations.idempotent, so it does not deduplicate operation keys"
	case row.Availability == "available":
		return true, ""
	default:
		return false, "this control plane reports operations.idempotent as " + row.Availability
	}
}

// retryAfter reads Retry-After in seconds, bounded, with 20% jitter.
func retryAfter(resp *http.Response, attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt)) * time.Second // 1, 2, 4
	if resp != nil {
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 15 {
				base = time.Duration(n) * time.Second
			}
		}
	}
	if v := os.Getenv("KS_HTTP_TIMEOUT_MS"); v != "" {
		base = base / 10 // tests: the same shape, ten times faster
	}
	j, _ := rand.Int(rand.Reader, big.NewInt(int64(base/5)+1))
	return base + time.Duration(j.Int64())
}

// hostedMutate sends one mutation with a persisted idempotency key. On a
// transport failure it retries the SAME key and body, at most three attempts
// in all, then reads the operation back; it never synthesises new work. A
// 202 receipt is followed by polling the operation until it finishes or the
// wait bound passes, in which case the state is reported as continuing.
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
	// Replay support is decided LAZILY, only when a first attempt fails
	// uncertainly, so the happy path costs no extra request. Then it is the
	// LIVE registry that decides, never a cached answer or the fact that the
	// header is being sent: replaying a key a server ignores is a second
	// session (review R01). A registry that cannot be read means no.
	var resp *http.Response
	var body2 []byte
	var lastErr error
	var replay, replayChecked bool
	var why string
	for attempt := 0; attempt < mutationAttempts; attempt++ {
		if attempt > 0 {
			progress("retrying the same operation (%s), attempt %d of %d", key, attempt+1, mutationAttempts)
			time.Sleep(retryAfter(resp, attempt-1))
		}
		r, b, err := doBounded(cr, method, path, headers, raw)
		if err == nil && !(r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusServiceUnavailable) && !uncertain(nil, r) {
			resp, body2 = r, b
			break // a clean answer (2xx or a definite non-2xx): stop
		}
		if err == nil && (r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusServiceUnavailable) {
			// the server answered and refused before doing any work: safe to
			// retry whatever its deduplication contract, up to the bound
			resp, body2, lastErr = r, b, hostedError(method, path, r, b)
			continue
		}
		// uncertain: a transport failure on a mutation, or a 502/504 from a
		// control plane that could not hear back from its fleet. The work may
		// have started; retry ONLY where the receiver deduplicates the key.
		lastErr = err
		if err == nil {
			lastErr = hostedError(method, path, r, b)
		}
		resp = nil
		if !replayChecked {
			replay, why = replaySupported(cr)
			replayChecked = true
		}
		if !replay {
			break // one attempt: report the unknown outcome, never resend
		}
	}
	if resp == nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if resp != nil && !uncertain(lastErr, resp) {
			return lastErr // answered and refused: the refusal, not an uncertainty
		}
		// the outcome is unknown: read it back where that is possible, say so
		// plainly where it is not; a fresh submission is never the answer
		if replay {
			if o, err := fetchOperation(cr, key); err == nil {
				return settleOperation(cr, o, key, out)
			}
			fail(&cliError{Code: exitTemporary, Kind: "outcome_unknown", Message: "the request did not complete: " + sanitize(fmt.Sprint(lastErr)) + ". The operation may exist on the control plane",
				WorkStarted: workUnknown, OperationID: key, NextAction: "ks operation show " + key})
		}
		reason := why
		if reason == "" {
			reason = "this control plane does not confirm that it deduplicates operation keys"
		}
		fail(&cliError{Code: exitTemporary, Kind: "outcome_unknown", Message: "the request did not complete: " + sanitize(fmt.Sprint(lastErr)) + ". It was sent once and not retried, because " + reason + "; the work may or may not have started",
			WorkStarted: workUnknown, OperationID: key, NextAction: "check the console's session list before running this again; operation " + key + " is recorded locally in " + operationsPath()})
	}
	if resp.StatusCode == http.StatusAccepted {
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
	}
	if resp.StatusCode/100 != 2 {
		return hostedError(method, path, resp, body2)
	}
	if resp.Header.Get("KS-Operation-Replayed") == "true" {
		progress("the control plane already had this operation (%s); showing its original result", key)
	}
	if out != nil && len(body2) > 0 {
		return json.Unmarshal(body2, out)
	}
	return nil
}

type remoteOp struct {
	ID         string          `json:"operation_id"`
	State      string          `json:"state"`
	HTTPStatus int             `json:"http_status"`
	Response   json.RawMessage `json:"response"`
	Error      string          `json:"error"`
	AcceptedAt string          `json:"accepted_at"`
	FinishedAt string          `json:"finished_at"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
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
	default:
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
		} else if o.State == "succeeded" || o.State == "failed" {
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
	if o.Error != "" {
		fmt.Printf("error: %s\n", o.Error)
	}
	if len(o.Response) > 0 {
		fmt.Printf("result: %s\n", strings.TrimSpace(string(o.Response)))
	}
}
