// ks run and the session verbs in an organization's context, against a fake
// control plane: the organization's preflight and key, the organization's
// session made with org_id, your own keys never read, the service's refusal
// for an organization with no credit said in its words, and the session
// verbs resolving among the organization's records.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgRunCtl struct {
	mu      sync.Mutex
	sent    []string
	orgKeys []any
}

func (c *orgRunCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"code": typ, "type": typ, "message": msg, "retryable": false, "work_started": "no", "next_action": ""}})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		_, _ = w.Write([]byte(`{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.4","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"},{"id":"session.show","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`))
	case r.URL.Path == "/api/v2/orgs/org_a":
		env(200, map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 2}})
	case r.URL.Path == "/api/v2/preflight":
		env(200, map[string]any{"account_id": "acct_principal", "keys": c.orgKeys, "blockers": []any{}, "checks": []any{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions":
		env(201, map[string]any{"id": "session_org1", "revision": 1, "primary_agent_id": "agent_org1", "org_id": "org_a"})
	case strings.HasSuffix(r.URL.Path, "/bindings"):
		env(200, map[string]any{"session_id": "session_org1", "keys": map[string]any{"anthropic": "vlt_org"}, "managed": []any{}, "revision": 2})
	case strings.HasSuffix(r.URL.Path, "/provision"):
		refuse(402, "ks_organization_no_credit", "this organization has no credit, and its session's time is billed to the organization, never to a member; nothing was created")
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("context") == "org_a":
		env(200, map[string]any{"items": []any{map[string]any{"id": "session_org1abcdef", "name": "build", "runtime_state": "parked", "revision": 3, "primary_agent_id": "agent_org1"}}, "next_cursor": ""})
	case strings.HasPrefix(r.URL.Path, "/api/v2/sessions/session_org1abcdef"):
		env(200, map[string]any{"id": "session_org1abcdef", "name": "build", "runtime_state": "parked", "org_id": "org_a", "runner_model": "claude-sonnet-5-5"})
	case r.URL.Path == "/api/v2/keys" || r.URL.Path == "/api/v2/account/model-keys":
		env(200, map[string]any{"items": []any{map[string]any{"id": "vlt_mine", "provider": "anthropic", "last4": "MINE", "enabled": true}}, "next_cursor": ""})
	default:
		refuse(404, "ks_not_found", "no such resource")
	}
}

func (c *orgRunCtl) asked(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

func TestOrganizationRunAndSessionVerbs(t *testing.T) {
	c := &orgRunCtl{orgKeys: []any{}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	if _, _, code := run("context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d", code)
	}
	// the organization has no key: nothing is made, your keys are not read
	out, errs, code := run("run", "--agent", "--no-input")
	if code != exitConflict || !strings.Contains(errs, "has no enabled anthropic key") || !strings.Contains(errs, "your own keys are never used") || !strings.Contains(errs, "Nothing was created") {
		t.Fatalf("a run in an organization with no key: %d\n%s%s", code, out, errs)
	}
	if len(c.asked("POST /api/v2/sessions ")) != 0 {
		t.Errorf("a session was made: %v", c.asked("POST /api/v2/sessions"))
	}
	if p := c.asked("POST /api/v2/preflight"); len(p) != 1 || !strings.Contains(p[0], `"org_id":"org_a"`) {
		t.Errorf("the preflight did not name the organization: %v", p)
	}
	// a key of yours named for the organization's session is refused here
	if _, errs, code := run("run", "--agent", "--no-input", "--key", "vlt_mine"); code != exitUsage || !strings.Contains(errs, "not a key of the organization") {
		t.Errorf("--key with your own key in an organization's context: %d %s", code, errs)
	}
	// with an organization key: the session is the organization's, the
	// organization's key is bound, and the service's money refusal is said
	c.mu.Lock()
	c.orgKeys = []any{map[string]any{"id": "vlt_org", "provider": "anthropic", "last4": "ORGK", "enabled": true}}
	c.mu.Unlock()
	out, errs, code = run("run", "--agent", "--no-input", "--name", "build")
	created := c.asked("POST /api/v2/sessions ")
	if len(created) != 1 || !strings.Contains(created[0], `"org_id":"org_a"`) {
		t.Fatalf("the organization's session: %v\n%s%s", created, out, errs)
	}
	if b := c.asked("POST /api/v2/sessions/session_org1/bindings"); len(b) != 1 || !strings.Contains(b[0], "vlt_org") || strings.Contains(b[0], "vlt_mine") {
		t.Errorf("the binding: %v", b)
	}
	if code == 0 || !strings.Contains(errs+out, "this organization has no credit") {
		t.Errorf("the organization's money refusal was not said: %d\n%s%s", code, out, errs)
	}
	if k := c.asked("GET /api/v2/keys"); len(k) != 0 {
		t.Errorf("your own keys were read for an organization's run: %v", k)
	}
	if k := c.asked("GET /api/v2/account/model-keys"); len(k) != 0 {
		t.Errorf("your own key defaults were read for an organization's run: %v", k)
	}
	// the session verbs: the organization's records, the context said
	out, errs, code = run("session", "list")
	if code != 0 || !strings.Contains(out, "Context: organization org_a") || !strings.Contains(out, "session_org1a") {
		t.Fatalf("session list in the context: %d %s %s", code, out, errs)
	}
	out, errs, code = run("session", "show", "session_org1ab")
	if code != 0 || !strings.Contains(out, "session_org1abcdef") {
		t.Errorf("session show by a prefix of an organization's session: %d\n%s%s", code, out, errs)
	}
	// back to personal: the fleet inventory, as always
	if _, _, code := run("context", "use", "personal"); code != 0 {
		t.Fatal("context use personal")
	}
	before := len(c.asked("GET /api/v2/sessions?"))
	_, _, _ = run("session", "list")
	fleet := false
	for _, x := range c.asked("GET /api/v2/sessions?")[before:] {
		fleet = fleet || strings.Contains(x, "source=fleet")
	}
	if !fleet {
		t.Errorf("the personal session list did not read the fleet inventory: %v", c.asked("GET /api/v2/sessions?")[before:])
	}
}
