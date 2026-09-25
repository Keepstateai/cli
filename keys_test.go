// KS-023 to KS-025 and KS-028 on the command line: the inventory never
// shows a secret, the secret enters only through standard input, a
// binding names a key by id, and preflight starts nothing. every
// client-authored surface (help, reference, doctor, verbs) carries no
// private marker.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type keysCtl struct {
	mu       sync.Mutex
	keys     []map[string]any
	requests []string
	secrets  []string
	credit   any // credit_microusd the fake preflight reports; nil is "unavailable"
}

func (c *keysCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"keys.inventory","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"},{"id":"preflight","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys":
		c.mu.Lock()
		defer c.mu.Unlock()
		env(200, map[string]any{"items": c.keys, "next_cursor": "", "observed_at": "x"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/keys":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.secrets = append(c.secrets, fmt.Sprint(body["secret"]))
		k := map[string]any{"id": "vlt_new00001", "provider": body["provider"], "alias": body["alias"], "last4": "9999", "enabled": true, "revision": 1, "created_at": "t", "updated_at": "t"}
		c.keys = append(c.keys, k)
		c.mu.Unlock()
		env(201, k)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v2/keys/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, k := range c.keys {
			if k["id"] == strings.TrimPrefix(r.URL.Path, "/api/v2/keys/") {
				k["enabled"] = body["enabled"]
				k["revision"] = k["revision"].(int) + 1
				env(200, k)
				return
			}
		}
		env(404, nil)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/deletion-plan"):
		env(201, map[string]any{"id": "plan_1", "plan": map[string]any{"bindings": map[string]any{"sessions": []any{map[string]any{"session_id": "s"}}}}})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v2/keys/"):
		env(200, map[string]any{"deleted": true, "bindings_dropped": 1})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{{"id": "fleetsession0000000000000000aaaa", "short_id": "fleetsession00", "name": "checkout", "runtime_state": "parked", "record_id": "session_rec1", "agent_activity": "unavailable", "task_state": "unavailable", "key_alias": "unbound", "observed_at": "x", "last_activity_at": "x", "created_at": "x", "image": "base", "budget_tokens": 1, "execution_epoch": 1}}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_rec1":
		env(200, map[string]any{"id": "session_rec1", "revision": 3})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_rec1/bindings":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		env(200, map[string]any{"session_id": "session_rec1", "keys": body["keys"], "revision": 4, "applied_at": "x", "expected_seen": body["expected_revision"]})
	case r.Method == "POST" && r.URL.Path == "/api/v2/preflight":
		env(200, map[string]any{"account_id": "acct_x", "cohort_state": "active", "keys": []any{}, "credit_microusd": c.credit, "currency": "USD", "registry_version": "t", "build": "b",
			"unavailable_capabilities": []string{"agent.workspace"}, "limits": map[string]any{}, "checks_supported": []string{"pytest"}, "blockers": []string{"no credit on the account"}, "ready": false, "ready_for": map[string]bool{"cruise": false, "agent": false}, "next_actions": map[string]string{"credit": "add credit in the console"}, "observed_at": "x"})
	default:
		env(404, nil)
	}
}

func TestKeyVerbsNeverCarryTheSecret(t *testing.T) {
	c := &keysCtl{keys: []map[string]any{{"id": "vlt_0123abcd", "provider": "anthropic", "alias": "prod", "last4": "TEST", "enabled": true, "revision": 1, "created_at": "t", "updated_at": "t"}}}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "list")
	if code != 0 || !strings.Contains(out, "vlt_0123abcd") || !strings.Contains(out, "…TEST") {
		t.Fatalf("list: %d %s", code, out)
	}
	// the secret enters through standard input and is sent once; it is not in any output
	secret := "sk-synthetic-0000000000000000-not-real"
	cmd := exec.Command(bin, "key", "add", "--provider", "anthropic", "--alias", "new", "--json")
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	cmd.Stdin = strings.NewReader(secret + "\n")
	outB, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(outB), `"id":"vlt_new00001"`) {
		t.Fatalf("add: %v %s", err, outB)
	}
	if strings.Contains(string(outB), secret) {
		t.Error("the secret was echoed")
	}
	if len(c.secrets) != 1 || c.secrets[0] != secret {
		t.Errorf("the service received %v", len(c.secrets))
	}
	// without a key on stdin nothing is sent
	cmd = exec.Command(bin, "key", "add", "--provider", "anthropic", "--no-input")
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	cmd.Stdin = strings.NewReader("")
	if _, err := cmd.CombinedOutput(); err == nil || len(c.secrets) != 1 {
		t.Error("an empty stdin stored something")
	}
	// disable by alias, delete with a plan first
	if out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "disable", "prod"); code != 0 || !strings.Contains(out, "disabled") {
		t.Errorf("disable: %d %s", code, out)
	}
	if out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "delete", "vlt_0123abcd"); code != 0 || !strings.Contains(out, "nothing deleted") {
		t.Errorf("delete without --yes: %d %s", code, out)
	}
	if out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "delete", "vlt_0123abcd", "--yes"); code != 0 || !strings.Contains(out, "deleted vlt_0123abcd") {
		t.Errorf("delete with --yes: %d %s", code, out)
	}
	// a session binding names the key by id, with the record's revision
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetsession00", "--key", "vlt_new00001", "--json")
	if code != 0 || !strings.Contains(out, `"anthropic":"vlt_new00001"`) || !strings.Contains(out, `"expected_seen":3`) {
		t.Errorf("bind: %d %s %s", code, out, errs)
	}
	// preflight reports and starts nothing
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "preflight")
	// blocked is exit 5 (KS-029: a blocked preflight is not a success a
	// script can mistake for ready); the blocker is still printed
	if code != exitConflict || !strings.Contains(out, "no credit on the account") || !strings.Contains(out, "not ready") {
		t.Errorf("preflight: %d %s", code, out)
	}
	for _, r := range c.requests {
		if strings.HasPrefix(r, "POST /api/v2/sessions") && !strings.HasSuffix(r, "/bindings") {
			t.Errorf("preflight or a key verb started something: %s", r)
		}
	}
}

// Everything the client writes for a person (help, the reference, doctor,
// every verb's own help, the command manifest) speaks about the client and
// the customer API only: no service-internal path, no internal ticket id,
// no service component name, no environment switch, no workstation path.
func TestClientSurfacesCarryNoPrivateMarkers(t *testing.T) {
	markers := regexp.MustCompile(`/internal/|\b[A-Z]{2}-\d{3}\b|\bksd\b|\bksgw\b|firecracker|\b[A-Z]{2,4}_[A-Z_]{4,}\b|/Users/[a-z]|/home/[a-z]`)
	srv := httptest.NewServer(&keysCtl{})
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	outputs := map[string]string{}
	for _, args := range [][]string{{"--help"}, {"reference"}, {"doctor"}} {
		out, errs, _ := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		outputs[strings.Join(args, " ")] = out + errs
	}
	for _, c := range registry {
		if c.Group {
			continue
		}
		args := append(append([]string{}, c.Path...), "--help")
		out, errs, _ := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		outputs[strings.Join(args, " ")] = out + errs
	}
	b, _ := os.ReadFile(filepath.Join(".", "commands.json"))
	outputs["commands.json"] = string(b)
	for name, text := range outputs {
		if m := markers.FindAllString(text, -1); len(m) > 0 {
			t.Errorf("%s carries %v", name, m)
		}
	}
}

// TestPreflightShowsCreditInDollars: the service reports credit in
// microdollars, and preflight printed that integer with the currency code
// after it, so -31673162 microdollars read "credit -31,673,162 USD": a
// balance of -$31.67 shown a million times too large. Found against
// production on 2026-09-25.
func TestPreflightShowsCreditInDollars(t *testing.T) {
	for _, tc := range []struct {
		credit    any
		want, not string
	}{
		{-31673162, "credit -$31.673162", "31,673,162"},
		{2000000, "credit $2.00", "2,000,000"},
		{nil, "credit unavailable", "$"},
	} {
		c := &keysCtl{credit: tc.credit}
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		out, _, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "preflight")
		srv.Close()
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "account ") {
				line = l
			}
		}
		if !strings.Contains(line, tc.want) || strings.Contains(line, tc.not) {
			t.Errorf("credit %v: preflight printed %q, want it to contain %q and not %q", tc.credit, line, tc.want, tc.not)
		}
	}
}

// KS-029 on the client: each check is shown with its status, the workspace
// is sized locally against the service's limit, and the exit says ready (0),
// blocked (5) or not known (4).
type checksCtl struct {
	checks []any
	limit  float64
}

func (c *checksCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"preflight","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case "/api/v2/preflight":
		blockers := []any{}
		ready := true
		for _, x := range c.checks {
			m := x.(map[string]any)
			if m["status"] == "block" {
				blockers = append(blockers, m["detail"])
			}
			if m["status"] != "pass" {
				ready = false
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": map[string]any{
			"account_id": "acct_x", "cohort_state": "active", "credit_microusd": 5000000, "registry_version": "t",
			"limits": map[string]any{"workspace_bytes": c.limit}, "checks": c.checks, "blockers": blockers, "ready": ready}})
	default:
		w.WriteHeader(404)
	}
}

func TestPreflightShowsEveryCheckAndExitsByReadiness(t *testing.T) {
	pass := func(n string) map[string]any { return map[string]any{"check": n, "status": "pass", "detail": n + " ok"} }
	for _, tc := range []struct {
		name   string
		checks []any
		limit  float64
		files  map[string]string
		exit   int
		want   []string
	}{
		{"ready", []any{pass("account"), pass("key"), pass("credit")}, 1 << 20, map[string]string{"a.py": "x"}, 0, []string{"key          PASS", "workspace    PASS", "ready"}},
		{"no key", []any{pass("account"), map[string]any{"check": "key", "status": "block", "detail": "no enabled anthropic key on the account", "next_action": "ks key add --provider anthropic"}, pass("credit")}, 1 << 20, nil, exitConflict,
			[]string{"key          BLOCK", "→ ks key add --provider anthropic", "not ready; clear the blockers"}},
		{"unsupported runner", []any{pass("account"), pass("key"), pass("credit"), map[string]any{"check": "runner", "status": "block", "detail": "agent mode is not available on this service: not certified", "next_action": "ks doctor"}}, 1 << 20, nil, exitConflict,
			[]string{"runner       BLOCK", "agent mode is not available"}},
		{"oversize workspace", []any{pass("account"), pass("key"), pass("credit")}, 10, map[string]string{"big.py": strings.Repeat("x", 100)}, exitConflict,
			[]string{"workspace    BLOCK", "over the 10-byte limit"}},
		{"credit not checkable", []any{pass("account"), pass("key"), map[string]any{"check": "credit", "status": "unavailable", "detail": "the credit balance could not be read"}}, 1 << 20, nil, exitTemporary,
			[]string{"credit       UNAVAILABLE", "readiness is not known"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(&checksCtl{checks: tc.checks, limit: tc.limit})
			defer srv.Close()
			bin, cfg := buildAndAuth(t, srv)
			dir := t.TempDir()
			for n, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, errOut, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "preflight")
			if code != tc.exit {
				t.Fatalf("exit %d, want %d\n%s\n%s", code, tc.exit, out, errOut)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}
