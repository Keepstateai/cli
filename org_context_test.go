// The context verbs and an organization's session records against a fake
// control plane: the context is checked, kept locally per control plane,
// always shown, and read by ks organization sessions.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgContextCtl struct {
	mu    sync.Mutex
	asked []string
}

func (c *orgContextCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, r.Method+" "+r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	env := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/v2/orgs/org_a":
		env(map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 3, "limit": nil}})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("context") == "org_a":
		if r.URL.Query().Get("cursor") == "" {
			env(map[string]any{"items": []any{map[string]any{"id": "session_1", "name": "build", "runtime_state": "provisioning", "org_id": "org_a", "created_by": "acct_me"}}, "next_cursor": "2"})
		} else {
			env(map[string]any{"items": []any{map[string]any{"id": "session_2", "name": "deploy", "runtime_state": "provisioning", "org_id": "org_a", "created_by": "acct_admin", "transferred_from": "acct_gone"}}, "next_cursor": ""})
		}
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func TestOrganizationContext(t *testing.T) {
	c := &orgContextCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	if out, _, code := run("context", "show"); code != 0 || !strings.Contains(out, "Context: personal") {
		t.Fatalf("the default context: %d %s", code, out)
	}
	// with no context, the organization must be named
	if _, _, code := run("organization", "sessions"); code != exitUsage {
		t.Errorf("organization sessions with no context: %d", code)
	}
	out, errs, code := run("context", "use", "org_a")
	if code != 0 || !strings.Contains(out, "Context: Acme (org_a), as member") {
		t.Fatalf("context use: %d\n%s%s", code, out, errs)
	}
	if out, _, code := run("context", "show"); code != 0 || !strings.Contains(out, "org_a") {
		t.Errorf("context show after use: %d %s", code, out)
	}
	// the organization's records, both pages, the context said
	out, _, code = run("organization", "sessions")
	if code != 0 || !strings.Contains(out, "Context: organization org_a") || !strings.Contains(out, "session_1") || !strings.Contains(out, "session_2") || !strings.Contains(out, "transferred from acct_gone") {
		t.Fatalf("organization sessions: %d %s", code, out)
	}
	// an organization you do not belong to is refused and changes nothing
	if _, errs, code := run("context", "use", "org_nope"); code == 0 || !strings.Contains(errs, "no such resource") {
		t.Errorf("context use of a stranger's organization: %d %s", code, errs)
	}
	if out, _, _ := run("context", "show"); !strings.Contains(out, "org_a") {
		t.Errorf("a refused context use changed the context: %s", out)
	}
	if _, _, code := run("context", "use", "somewhere"); code != exitUsage {
		t.Errorf("a context that is neither: %d", code)
	}
	if out, _, code := run("context", "use", "personal"); code != 0 || !strings.Contains(out, "personal") {
		t.Errorf("back to personal: %d %s", code, out)
	}
	if out, _, _ := run("context", "show"); !strings.Contains(out, "Context: personal") {
		t.Errorf("after personal: %s", out)
	}
}
