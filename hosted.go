// Hosted mode (AZ-4): once `ks login` stores a token, the session verbs
// route to the KeepState control plane over HTTPS with that token instead
// of a local ksd. This is what lets a stranger's Mac drive a real session
// on the fleet — the same verbs, a different destination.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type hostedCreds struct {
	CTL       string `json:"ctl"`
	Token     string `json:"token"`
	TokenID   string `json:"token_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Source    string `json:"-"` // which file it came from
}

// hostedToken reads the credential stored by `ks login` — a 0600 file in
// the user's config dir on every platform. This client is hosted-only:
// without a login there is nothing to talk to. A second source is read
// when the first is absent: ~/.keepstate/hosted.json {ctl, token}, the
// endpoint file the gates and the bench write.
func hostedToken() (hostedCreds, bool) {
	paths := []string{tokenPath()}
	if p := legacyTokenPath(); p != "" {
		paths = append(paths, p)
	}
	for _, p := range paths {
		var c hostedCreds
		b, err := os.ReadFile(p)
		if err != nil {
			if !os.IsNotExist(err) {
				// present but unreadable: say so, never fall through to a
				// weaker source as if nothing were there
				fmt.Fprintf(os.Stderr, "the stored credential %s cannot be read (%v); fix its permissions or run: ks login\n", p, err)
				os.Exit(2)
			}
			continue
		}
		if json.Unmarshal(b, &c) != nil || c.Token == "" || c.CTL == "" {
			continue
		}
		ctl, err := validateControlPlane(c.CTL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "the stored credential %s names an unsafe control plane: %v\nRun: ks logout, then ks login --ctl <https URL>\n", p, err)
			os.Exit(2)
		}
		c.CTL = ctl
		c.Source = p
		return c, true
	}
	return hostedCreds{}, false
}

// hostedDo sends one authenticated request and returns the response
// unread, for callers that stream a body (an artifact download). The
// caller closes resp.Body.
func hostedDo(cr hostedCreds, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, strings.TrimRight(cr.CTL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cr.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := streamClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("control plane unreachable: %w", err)
	}
	return resp, nil
}

// hostedError turns a non-2xx response into a clean message. The control
// plane answers in two shapes: {"message": ...} (the session verbs, e.g.
// the kill guard's actionable text) and {"error": {"type", "message"}}
// (the job routes); a bare {"error": "..."} is read too. Anything else is
// the status line and the raw body.
func hostedError(method, path string, resp *http.Response, raw []byte) error {
	var e struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return fmt.Errorf("%s", e.Message)
		}
		var typed struct{ Type, Message string }
		if len(e.Error) > 0 && json.Unmarshal(e.Error, &typed) == nil && typed.Message != "" {
			if typed.Type != "" {
				return fmt.Errorf("%s (%s)", typed.Message, typed.Type)
			}
			return fmt.Errorf("%s", typed.Message)
		}
		var s string
		if len(e.Error) > 0 && json.Unmarshal(e.Error, &s) == nil && s != "" {
			return fmt.Errorf("%s", s)
		}
	}
	return fmt.Errorf("hosted %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
}

// hostedCall makes an authenticated broker request. path is like
// "/api/sessions" or "/api/sessions/<id>/meter". body is marshalled as
// JSON, except a []byte, which is sent as the JSON it already is.
func hostedCall(cr hostedCreds, method, path string, body any, out any) error {
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		enc, _ := json.Marshal(b)
		r = bytes.NewReader(enc)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cr.CTL, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cr.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ordinaryClient().Do(req)
	if err != nil {
		return fmt.Errorf("control plane unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return hostedError(method, path, resp, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ---------------------------------------------------------------------
// the session verbs, each reading its parsed invocation and nothing else
// ---------------------------------------------------------------------

// hostedRun sends only what was supplied: an omitted budget is absent from
// the request, so the control plane applies the account's own default; a
// supplied one is sent exactly. Zero and negative never get this far: the
// parser refuses them.
func hostedRun(cr hostedCreds, inv *Invocation) {
	req := map[string]any{}
	if inv.Set("image") {
		req["Image"] = inv.Str("image")
	}
	if inv.Set("budget-tokens") {
		req["Budget"] = inv.Int("budget-tokens")
	}
	var sess map[string]any
	if err := hostedMutate(cr, "POST", "/api/sessions", req, &sess); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "hosted session %v: image=%v state=%v (on %s)\n", sess["id"], sess["image"], sess["state"], cr.CTL)
	fmt.Println(sess["id"])
}

func hostedKill(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	path := "/api/sessions/" + id
	if inv.Bool("force") {
		path += "?force=1"
	}
	if err := hostedMutate(cr, "DELETE", path, nil, nil); err != nil {
		die(err)
	}
	fmt.Println("killed", id)
}

func hostedWake(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var sess map[string]any
	if err := hostedMutate(cr, "POST", "/api/sessions/"+id+"/resume", nil, &sess); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "hosted session %v resumed\n", id)
	fmt.Println(id)
}

func hostedCheckpoint(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	if err := hostedMutate(cr, "POST", "/api/sessions/"+id+"/checkpoint", map[string]bool{"Stop": inv.Bool("stop")}, nil); err != nil {
		die(err)
	}
	fmt.Println("checkpointed", id)
}

func hostedMeter(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var mtr map[string]any
	if err := hostedCall(cr, "GET", "/api/sessions/"+id+"/meter", nil, &mtr); err != nil {
		die(err)
	}
	if inv.Bool("json") {
		b, _ := json.Marshal(mtr)
		fmt.Println(string(b))
		return
	}
	fmt.Printf("session %v · spent %s / budget %s tokens · billed calls %s · key source(s): %v\n",
		mtr["session"], commas(asInt(mtr["spent"])), commas(asInt(mtr["budget"])), commas(asInt(mtr["billed_calls"])), mtr["key_sources"])
}

// hostedExec sends the command in one of two documented forms. Without
// --shell, every argument after the session reaches the program exactly as
// typed: the vector is rendered as a POSIX shell string in which each word
// is quoted, so the guest's shell splits it back into the same vector
// (spaces, empty arguments, $, ;, quotes, newlines and Unicode included).
// With --shell, exactly one argument is sent verbatim for the guest's shell
// to interpret: pipes, globs and variables are its business. A single
// argument that looks like a shell line, sent without --shell, is refused
// rather than guessed: it would run as a program named after the whole
// line, which nobody means.
func hostedExec(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var cmd string
	switch {
	case inv.Bool("shell"):
		if len(inv.Rest) != 1 {
			(&UsageError{Cmd: inv.Cmd, Message: fmt.Sprintf("--shell takes exactly one argument, the shell line; got %d.", len(inv.Rest))}).print()
			os.Exit(2)
		}
		cmd = inv.Rest[0]
	case len(inv.Rest) == 1 && looksLikeShellLine(inv.Rest[0]):
		(&UsageError{Cmd: inv.Cmd,
			Message:    fmt.Sprintf("%q looks like a shell line, not a program name.", inv.Rest[0]),
			Suggestion: "Pass --shell before the session to have its shell interpret it, or write each argument separately after --."}).print()
		os.Exit(2)
	default:
		cmd = shellJoin(inv.Rest)
	}
	var res map[string]any
	if err := hostedCall(cr, "POST", "/api/sessions/"+id+"/exec", map[string]string{"Cmd": cmd}, &res); err != nil {
		die(err)
	}
	if out, ok := res["output"].(string); ok {
		fmt.Print(out)
	}
	if e, ok := res["error"].(string); ok && e != "" {
		fmt.Fprintln(os.Stderr, "exec:", e)
		os.Exit(1)
	}
}

// shellJoin renders an argument vector as one POSIX shell string that a
// shell splits back into exactly that vector. A word made only of
// characters no shell treats specially is left bare; anything else is
// single-quoted, with each single quote closed, escaped and reopened.
func shellJoin(args []string) string {
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = shellQuote(a)
	}
	return strings.Join(words, " ")
}

func shellQuote(a string) string {
	if a == "" {
		return "''"
	}
	safe := true
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// looksLikeShellLine: whitespace or a shell metacharacter inside a single
// argument, which is the old joined form arriving as one word.
func looksLikeShellLine(a string) bool {
	return strings.ContainsAny(a, " \t\n|&;<>()$`\\\"'*?[]#~{}")
}

func hostedFork(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	n := int64(1)
	if inv.Set("children") {
		n = inv.Int("children")
	}
	path := fmt.Sprintf("/api/sessions/%s/fork?n=%d", id, n)
	if inv.Set("steer") {
		b, err := os.ReadFile(inv.Str("steer"))
		if err != nil {
			die(err)
		}
		// one steer, applied to every child (the hosted form keeps it
		// simple; per-branch steer files are a bench-only affordance).
		steers, _ := json.Marshal([]string{string(b)})
		path += "&steers=" + urlQueryEscape(string(steers))
	}
	var children []map[string]any
	if err := hostedMutate(cr, "POST", path, nil, &children); err != nil {
		die(err)
	}
	for _, c := range children {
		fmt.Fprintf(os.Stderr, "child %v: parent=%v (on the fleet)\n", c["id"], c["parent"])
		fmt.Println(c["id"])
	}
}

func hostedAttachCmd(cr hostedCreds, inv *Invocation) {
	if err := hostedAttach(cr, inv.Arg(0)); err != nil {
		die(err)
	}
}

// asInt coerces a JSON number (decoded as float64) or int to int64. Token
// counts are whole numbers; JSON has no int type, so spend/budget arrive
// as float64 and must never be printed raw (2e+06 is not a customer
// surface — founder ruling 2026-08-31).
func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// commas renders an integer with thousands separators: 2000000 -> 2,000,000.
func commas(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
