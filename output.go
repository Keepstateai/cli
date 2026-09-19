// output.go: one shape for what the client says (KS-007). Humans get lines;
// scripts get --json: a document with schema_version, request_id and either
// data or error, on stdout and nothing else there, with progress and
// warnings on stderr. Every failure names what failed, whether remote work
// started, the one safe next command, and the operation or request id when
// there is one, and exits by the table in C03. A missing figure is null,
// never zero; a secret that a server message might carry never reaches
// the terminal.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// exit codes, C03
const (
	exitOK        = 0
	exitFailed    = 1 // a known operation failure
	exitUsage     = 2 // syntax, missing selection, unsupported local input, untrusted configuration
	exitAuth      = 3 // authentication or authorization
	exitTemporary = 4 // transport, unavailable, timeout; remote work may exist
	exitConflict  = 5 // revision or state conflict, quota, admission
	exitIntegrity = 6 // integrity or security-policy rejection
	exitInterrupt = 130
)

// globalFlags are accepted by every command, after its own options.
var globalFlags = []Flag{
	{Name: "json", Kind: flagBool, Summary: "stable JSON on stdout: schema_version, request_id, data or error; progress stays on stderr"},
	{Name: "plain", Kind: flagBool, Summary: "one fact per line, no alignment or colour (the human output is already plain; this pins it)"},
	{Name: "no-color", Kind: flagBool, Summary: "no colour codes (this release prints none; accepted so scripts can pass it)"},
	{Name: "quiet", Kind: flagBool, Summary: "no progress lines on stderr; results and errors still print"},
	{Name: "no-input", Kind: flagBool, Summary: "never prompt or open a browser; a decision that needs you fails with exit 2 and the flag to pass"},
	{Name: "yes", Kind: flagBool, Summary: "skip a displayed non-destructive confirmation when every input is already exact; never a deletion plan, grant, budget, approval or trust"},
	{Name: "wait-timeout", Kind: flagString, Value: "DURATION", Summary: "how long to wait for an accepted operation before reporting it as continuing; default 120s"},
}

type outputSettings struct {
	json, plain, quiet, noInput, yes bool
	requestID                        string
}

var out outputSettings

// applyGlobals reads the global flags of a parsed invocation once.
func applyGlobals(inv *Invocation) error {
	out = outputSettings{json: inv.Bool("json"), plain: inv.Bool("plain"), quiet: inv.Bool("quiet"), noInput: inv.Bool("no-input"), yes: inv.Bool("yes"), requestID: "req_" + newIdempotencyKey()[5:17]}
	if inv.Set("wait-timeout") {
		d, err := time.ParseDuration(inv.Str("wait-timeout"))
		if err != nil || d <= 0 {
			return fmt.Errorf("--wait-timeout %q is not a duration like 120s or 5m", inv.Str("wait-timeout"))
		}
		waitBound = d
	}
	return nil
}

var waitBound = waitTimeout

// progress writes a progress or warning line to stderr, never to stdout.
func progress(format string, a ...any) {
	if out.quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// emit prints a read's result or a mutation's receipt. In --json mode the
// envelope goes to stdout; in human mode the caller's text does.
func emit(data any, human func()) {
	if out.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"schema_version": 2, "request_id": out.requestID, "data": data})
		return
	}
	human()
}

// emitLine prints one event object per line in --json mode (a follow or
// log stream), or the human line otherwise.
func emitLine(data any, human string) {
	if out.json {
		b, _ := json.Marshal(data)
		fmt.Println(string(b))
		return
	}
	fmt.Println(human)
}

// cliError is the one error shape.
type cliError struct {
	Code        int    `json:"-"`
	Kind        string `json:"code"`
	Message     string `json:"message"`
	WorkStarted string `json:"work_started"` // yes | no | unknown; unknown is a real answer, never rounded to no
	NextAction  string `json:"next_action,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	HTTPStatus  int    `json:"http_status,omitempty"`
}

const (
	workNo      = "no"
	workYes     = "yes"
	workUnknown = "unknown"
)

func (e *cliError) Error() string { return e.Message }

// hostedErr carries the control plane's status and typed reason so the
// exit code follows the table rather than the message. Mutation marks a
// request that could have started remote work.
type hostedErr struct {
	Status   int
	Type     string
	Message  string
	Mutation bool
}

func (e *hostedErr) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Type)
	}
	return e.Message
}

// classify maps any error to the exit table and the error shape.
func classify(err error) *cliError {
	var ce *cliError
	if errors.As(err, &ce) {
		if ce.WorkStarted == "" {
			ce.WorkStarted = workNo
		}
		return ce
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		return &cliError{Code: exitUsage, Kind: "usage", Message: ue.Message, NextAction: ue.Suggestion, WorkStarted: workNo}
	}
	var te transportErr
	if errors.As(err, &te) {
		ws := workNo
		if te.mutation {
			ws = workUnknown // the request may have arrived; nothing came back
		}
		return &cliError{Code: exitTemporary, Kind: "unreachable", Message: "the control plane did not answer: " + sanitize(te.err.Error()), WorkStarted: ws}
	}
	var he *hostedErr
	if errors.As(err, &he) {
		c := &cliError{Kind: he.Type, Message: sanitize(he.Message), HTTPStatus: he.Status, WorkStarted: workNo}
		if c.Kind == "" {
			c.Kind = "http_" + fmt.Sprint(he.Status)
		}
		switch {
		case he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden:
			c.Code = exitAuth
			c.NextAction = "ks login"
		case he.Status == http.StatusConflict || he.Status == http.StatusTooManyRequests || he.Status == http.StatusPaymentRequired:
			c.Code = exitConflict
		case he.Status == http.StatusUnprocessableEntity:
			c.Code = exitIntegrity
		case he.Status == http.StatusBadGateway || he.Status == http.StatusGatewayTimeout:
			// the control plane answered for a fleet it could not hear from:
			// the fleet may have done the work before the door closed
			c.Code = exitTemporary
			if he.Mutation {
				c.WorkStarted = workUnknown
			}
		case he.Status == http.StatusServiceUnavailable:
			c.Code = exitTemporary
		default:
			c.Code = exitFailed
		}
		return c
	}
	if errors.Is(err, errNoOperations) {
		return &cliError{Code: exitFailed, Kind: "no_operation_records", Message: err.Error(), WorkStarted: workNo}
	}
	msg := sanitize(err.Error())
	if strings.Contains(msg, "CHECKSUM MISMATCH") || strings.Contains(msg, "sha256") && strings.Contains(msg, "refused") {
		return &cliError{Code: exitIntegrity, Kind: "integrity", Message: msg, WorkStarted: workNo}
	}
	return &cliError{Code: exitFailed, Kind: "failed", Message: msg, WorkStarted: workNo}
}

// fail prints the error in the mode's shape and exits by the table.
func fail(err error) {
	ce := classify(err)
	if out.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"schema_version": 2, "request_id": out.requestID, "error": ce})
	} else {
		fmt.Fprintln(os.Stderr, "error:", ce.Message)
		if ce.OperationID != "" {
			fmt.Fprintln(os.Stderr, "operation:", ce.OperationID)
		}
		switch ce.WorkStarted {
		case workYes:
			fmt.Fprintln(os.Stderr, "Remote work started: yes.")
		case workUnknown:
			fmt.Fprintln(os.Stderr, "Remote work started: unknown. Do not rerun this command blindly.")
		default:
			fmt.Fprintln(os.Stderr, "Remote work started: no.")
		}
		if ce.NextAction != "" {
			fmt.Fprintln(os.Stderr, "Next:", ce.NextAction)
		}
	}
	os.Exit(ce.Code)
}

var (
	controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]|\x1b\[[0-9;?]*[ -/]*[@-~]`)
	secretShapes = regexp.MustCompile(`(?i)(ksk_[a-z0-9]+|sk-[a-z0-9_-]{8,}|bearer\s+[a-z0-9._-]+|ks_sk_[a-z0-9]+)`)
)

// sanitize strips terminal control sequences and redacts anything shaped
// like a credential from text the client did not write itself.
func sanitize(s string) string {
	s = controlChars.ReplaceAllString(s, "")
	return secretShapes.ReplaceAllString(s, "[redacted]")
}

// figure renders a possibly-absent number for humans: a missing value is
// "unavailable", a present zero is "0".
func figure(v any) string {
	switch n := v.(type) {
	case nil:
		return "unavailable"
	case float64:
		return commas(int64(n))
	case int64:
		return commas(n)
	case int:
		return commas(int64(n))
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return commas(i)
		}
		return n.String()
	case string:
		if n == "" {
			return "unavailable"
		}
		return n
	}
	return fmt.Sprint(v)
}
