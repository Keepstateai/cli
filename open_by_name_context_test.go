// ks agent open NAME --session SESSION in an organization's context: the
// service resolves the name in the organization's session (by id or name),
// never against your personal sessions, and the context is sent.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgOpenCtl struct {
	mu    sync.Mutex
	calls []string
}

func (c *orgOpenCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/orgs/org_a":
		env(map[string]any{"id": "org_a", "name": "Acme", "your_role": "owner", "your_seat": "assigned", "seats": map[string]any{"used": 2, "limit": nil}})
	case r.URL.Path == "/api/v2/agent-targets" && q.Get("context") == "org_a" && q.Get("session") == "billing":
		t := map[string]any{"agent_id": "agent_org2", "agent_name": "main", "is_primary": true, "activity": "ready", "session_id": "session_org2", "session_name": "billing",
			"runtime_state": "parked", "key_routes": []string{"anthropic"}}
		env(map[string]any{"resolution": "resolved", "target": t, "candidates": []any{t}})
	case r.URL.Path == "/api/v2/agents/agent_org2/open":
		env(map[string]any{"agent": map[string]any{"id": "agent_org2", "name": "main", "activity": "ready"}, "lease": nil, "queue_depth": 0,
			"runtime": map[string]any{"session_id": "session_org2", "state": "parked", "running": false, "woken": false,
				"note": "the session is saved and stopped. Opening did not wake it", "actions": []any{}}})
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`)
	}
}

func TestOpenByNameInASessionOfTheOrganization(t *testing.T) {
	c := &orgOpenCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	if out, errs, code := ksIn(t, bin, cfg, dir, "context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, dir, "agent", "open", "main", "--session", "billing", "--no-follow")
	if code != 0 || !strings.Contains(out+errs, "session_org2") {
		t.Errorf("ks agent open main --session billing in the organization's context: %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.calls {
		if (strings.HasPrefix(a, "GET /api/v2/sessions") && !strings.Contains(a, "context=org_a")) || strings.HasPrefix(a, "GET /api/sessions") {
			t.Errorf("the organization's session was looked up among your personal sessions: %s", a)
		}
		if strings.HasPrefix(a, "GET /api/v2/agent-targets") && !strings.Contains(a, "context=org_a") {
			t.Errorf("asked %s in the organization's context without it", a)
		}
	}
}
