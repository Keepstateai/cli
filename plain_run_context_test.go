// A plain `ks run` (a machine with no agent) in an organization's context:
// refused before anything is sent, naming the agent session that runs as the
// organization; and a context refusal that names its own next step is shown
// with that step, not always the step out of the context.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type plainRunCtl struct {
	mu    sync.Mutex
	asked []string
}

func (c *plainRunCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.asked = append(c.asked, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/capabilities":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-10-15T00:00:00Z", "price_book": "v1.3",
			"capabilities": []any{map[string]any{"id": "agent.workspace", "availability": "available", "summary": "agents", "surface": "api", "since_protocol": 2}}, "limits": map[string]any{}}})
	case r.URL.Path == "/api/v2/orgs/org_a":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 2, "limit": nil}}})
	case r.URL.Path == "/api/v2/agent-registry":
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"schema_version":2,"error":{"code":"ks_context_unsupported","type":"ks_context_unsupported","message":"not in this context","next_action":"ks agent list --all"}}`))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`))
	}
}

func TestPlainRunInAnOrganizationsContext(t *testing.T) {
	c := &plainRunCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	if out, errs, code := ksIn(t, bin, cfg, dir, "context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	out, _, code := ksIn(t, bin, cfg, dir, "run", "--json")
	var doc struct {
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			NextAction string `json:"next_action"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &doc)
	if code == 0 || doc.Error.NextAction != "ks run --agent" || !strings.Contains(doc.Error.Message, "agent session") {
		t.Errorf("a plain run in the organization's context: %d %+v\n%s", code, doc.Error, out)
	}
	c.mu.Lock()
	for _, a := range c.asked {
		if strings.Contains(a, "/api/sessions") {
			t.Errorf("a plain run in the organization's context asked the service %s", a)
		}
	}
	c.mu.Unlock()
	// a context refusal that names its own step is shown with it
	out, _, code = ksIn(t, bin, cfg, dir, "adviser", "discover", "--json")
	_ = json.Unmarshal([]byte(out), &doc)
	if code == 0 || doc.Error.NextAction != "ks agent list --all" {
		t.Errorf("a context refusal's own step: %d %+v\n%s", code, doc.Error, out)
	}
}
