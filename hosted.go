// Hosted mode (AZ-4): once `ks login` stores a token, the session verbs
// route to the KeepState control plane over HTTPS with that token instead
// of a local ksd. This is what lets a stranger's Mac drive a real session
// on the fleet — the same verbs, a different destination.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type hostedCreds struct {
	CTL   string `json:"ctl"`
	Token string `json:"token"`
}

// hostedToken reads the credential stored by `ks login` — a 0600 file in
// the user's config dir on every platform. This client is hosted-only:
// without a login there is nothing to talk to. A second source is read
// when the first is absent: ~/.keepstate/hosted.json {ctl, token}, the
// endpoint file the gates and the bench write.
func hostedToken() (hostedCreds, bool) {
	var c hostedCreds
	paths := []string{filepath.Join(configHome(), "keepstate", "token.json")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".keepstate", "hosted.json"))
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if json.Unmarshal(b, &c) == nil && c.Token != "" && c.CTL != "" {
			return c, true
		}
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := hostedDo(cr, method, path, "application/json", r)
	if err != nil {
		return err
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

// runHosted dispatches the hosted session verbs. Returns handled=false for
// verbs with no hosted form (the caller falls back to local ksd).
func runHosted(cr hostedCreds, verb string) bool {
	switch verb {
	case "run":
		budget := flagValue("--budget", "0")
		req := map[string]any{"Image": flagValue("--image", ""), "Budget": mustInt(budget)}
		var sess map[string]any
		if err := hostedCall(cr, "POST", "/api/sessions", req, &sess); err != nil {
			die(err)
		}
		fmt.Fprintf(os.Stderr, "hosted session %v: image=%v state=%v (on %s)\n", sess["id"], sess["image"], sess["state"], cr.CTL)
		fmt.Println(sess["id"])
	case "kill":
		id := arg(2, "ks kill <session> [--force]")
		path := "/api/sessions/" + id
		if hasFlag("--force") {
			path += "?force=1"
		}
		if err := hostedCall(cr, "DELETE", path, nil, nil); err != nil {
			die(err)
		}
		fmt.Println("killed", id)
	case "resume", "wake":
		id := arg(2, "ks wake <session>")
		var sess map[string]any
		if err := hostedCall(cr, "POST", "/api/sessions/"+id+"/resume", nil, &sess); err != nil {
			die(err)
		}
		fmt.Fprintf(os.Stderr, "hosted session %v resumed\n", id)
		fmt.Println(id)
	case "checkpoint", "save":
		id := arg(2, "ks checkpoint <session> [--stop]")
		if err := hostedCall(cr, "POST", "/api/sessions/"+id+"/checkpoint", map[string]bool{"Stop": hasFlag("--stop")}, nil); err != nil {
			die(err)
		}
		fmt.Println("checkpointed", id)
	case "meter":
		id := arg(2, "ks meter <session> [--json]")
		var mtr map[string]any
		if err := hostedCall(cr, "GET", "/api/sessions/"+id+"/meter", nil, &mtr); err != nil {
			die(err)
		}
		if hasFlag("--json") {
			b, _ := json.Marshal(mtr)
			fmt.Println(string(b))
			return true
		}
		fmt.Printf("session %v · spent %s / budget %s tokens · billed calls %s · key source(s): %v\n",
			mtr["session"], commas(asInt(mtr["spent"])), commas(asInt(mtr["budget"])), commas(asInt(mtr["billed_calls"])), mtr["key_sources"])
	case "exec":
		id := arg(2, "ks exec <session> <command...>")
		if len(os.Args) < 4 {
			die(fmt.Errorf("usage: ks exec <session> <command...>"))
		}
		cmd := strings.Join(os.Args[3:], " ")
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
	case "fork":
		id := arg(2, "ks fork <session> [-n N] [--steer FILE]")
		n := flagValue("-n", "1")
		path := "/api/sessions/" + id + "/fork?n=" + n
		if f := flagValue("--steer", ""); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				die(err)
			}
			// one steer, applied to every child (the hosted form keeps it
			// simple; per-branch steer files are a bench-only affordance).
			steers, _ := json.Marshal([]string{string(b)})
			path += "&steers=" + urlQueryEscape(string(steers))
		}
		var children []map[string]any
		if err := hostedCall(cr, "POST", path, nil, &children); err != nil {
			die(err)
		}
		for _, c := range children {
			fmt.Fprintf(os.Stderr, "child %v: parent=%v (on the fleet)\n", c["id"], c["parent"])
			fmt.Println(c["id"])
		}
	case "attach":
		id := arg(2, "ks attach <session>")
		if err := hostedAttach(cr, id); err != nil {
			die(err)
		}
	default:
		return false
	}
	return true
}

func mustInt(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
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
