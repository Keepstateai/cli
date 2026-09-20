// KS-023 to KS-025 and KS-028 on the command line: the inventory never
// shows a secret, the secret enters only through standard input, a
// binding names a key by id, and preflight starts nothing. DISC-07: every
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
		env(200, map[string]any{"account_id": "acct_x", "cohort_state": "active", "keys": []any{}, "credit_microusd": nil, "currency": "USD", "registry_version": "t", "build": "b",
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
	if code != 0 || !strings.Contains(out, "blocker: no credit") || !strings.Contains(out, "not ready") {
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
