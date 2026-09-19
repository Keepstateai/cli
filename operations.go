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
// read, which is the only case in which the outcome is unknown.
type transportErr struct{ err error }

func (e transportErr) Error() string { return e.err.Error() }

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
		return nil, nil, transportErr{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, transportErr{err}
	}
	return resp, raw, nil
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
	var resp *http.Response
	var body2 []byte
	var lastErr error
	for attempt := 0; attempt < mutationAttempts; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(os.Stderr, "retrying the same operation (%s), attempt %d of %d\n", key, attempt+1, mutationAttempts)
			time.Sleep(retryAfter(resp, attempt-1))
		}
		r, b, err := doBounded(cr, method, path, headers, raw)
		if err != nil {
			lastErr = err
			resp = nil
			continue
		}
		resp, body2 = r, b
		if r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusServiceUnavailable {
			lastErr = hostedError(method, path, r, b)
			continue
		}
		break
	}
	if resp == nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		// the outcome is unknown: read it back rather than resend
		if o, err := fetchOperation(cr, key); err == nil {
			return settleOperation(cr, o, key, out)
		}
		fmt.Fprintf(os.Stderr, "The request did not complete: %v\n", lastErr)
		fmt.Fprintf(os.Stderr, "The operation may exist on the control plane. Check it: ks operation show %s\n", key)
		fmt.Fprintln(os.Stderr, "No new work was started.")
		os.Exit(4)
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
		fmt.Fprintf(os.Stderr, "the control plane already had this operation (%s); showing its original result\n", key)
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
	bound := waitTimeout
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
	fmt.Fprintf(os.Stderr, "operation %s accepted; waiting up to %s\n", id, bound.Round(time.Second))
	for {
		select {
		case <-sig:
			fmt.Fprintf(os.Stderr, "\ninterrupted locally; the operation continues on the control plane. Read it later: ks operation show %s\n", id)
			os.Exit(130)
		case <-time.After(interval):
		}
		o, err := fetchOperation(cr, id)
		if err != nil {
			if errors.Is(err, errNoOperations) {
				return err
			}
			fmt.Fprintf(os.Stderr, "reading the operation: %v (still waiting)\n", err)
		} else if o.State == "succeeded" || o.State == "failed" {
			return settleOperation(cr, o, key, out)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "operation %s is still running after %s; it continues on the control plane.\n", id, bound.Round(time.Second))
			fmt.Fprintf(os.Stderr, "Read it later: ks operation show %s\n", id)
			fmt.Fprintln(os.Stderr, "No new work was started.")
			os.Exit(4)
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
	printOperation(o, inv.Bool("json"))
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
	printOperation(o, inv.Bool("json"))
}

func printOperation(o *remoteOp, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(o)
		fmt.Println(string(b))
		return
	}
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
