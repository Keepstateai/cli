// Cruise drafts in an organization's context: every draft and generated-tests
// call goes to the service in the chosen context, the list says its context
// and shows the organization's drafts, never the personal ones; in the
// personal context no context is sent.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type draftCtxCtl struct {
	mu    sync.Mutex
	asked []string
}

func (c *draftCtxCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.asked = append(c.asked, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	ctx := r.URL.Query().Get("context")
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	draft := func(id, goal string) map[string]any {
		return map[string]any{"id": id, "revision": 1, "state": "ready", "source": map[string]any{"kind": "upload"}, "options": map[string]any{"goal": goal},
			"files": map[string]any{"included": 2, "bytes": 100}, "expires_at": "2026-11-05T00:00:00Z"}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		env(map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-10-15T00:00:00Z", "price_book": "v1.3",
			"capabilities": []any{map[string]any{"id": "cruise.drafts", "availability": "available", "summary": "drafts", "surface": "api", "since_protocol": 2},
				map[string]any{"id": "cruise.generated_tests", "availability": "available", "summary": "tests", "surface": "api", "since_protocol": 2}}, "limits": map[string]any{}})
	case r.URL.Path == "/api/v2/orgs/org_a":
		env(map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 2, "limit": nil}})
	case r.URL.Path == "/api/v2/cruise-drafts" && ctx == "org_a":
		env(map[string]any{"items": []any{draft("crd_org1", "ACMEDRAFT")}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/cruise-drafts":
		env(map[string]any{"items": []any{draft("crd_mine1", "PERSONALDRAFT")}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/cruise-drafts/crd_org1/tests":
		env(map[string]any{"draft_id": "crd_org1", "source": "repository", "state": "none"})
	case r.URL.Path == "/api/v2/cruise-drafts/crd_org1" && r.Method == "DELETE":
		env(draft("crd_org1", "ACMEDRAFT"))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`))
	}
}

func TestCruiseDraftsInAnOrganizationsContext(t *testing.T) {
	c := &draftCtxCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	if out, errs, code := ksIn(t, bin, cfg, dir, "context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, dir, "cruise", "draft", "list")
	if code != 0 || !strings.HasPrefix(out, "Context: organization org_a") || !strings.Contains(out, "crd_org1") || strings.Contains(out, "crd_mine1") {
		t.Errorf("cruise draft list in the organization's context: %d\n%s%s", code, out, errs)
	}
	_, _, _ = ksIn(t, bin, cfg, dir, "cruise", "tests", "show", "crd_org1")
	_, _, _ = ksIn(t, bin, cfg, dir, "cruise", "draft", "discard", "crd_org1")
	c.mu.Lock()
	for _, a := range c.asked {
		if strings.Contains(a, "/api/v2/cruise-drafts") && !strings.Contains(a, "context=org_a") {
			t.Errorf("asked %s in the organization's context without it", a)
		}
	}
	c.asked = nil
	c.mu.Unlock()
	if _, _, code := ksIn(t, bin, cfg, dir, "context", "use", "personal"); code != 0 {
		t.Fatal("context use personal")
	}
	out, _, code = ksIn(t, bin, cfg, dir, "cruise", "draft", "list")
	if code != 0 || strings.Contains(out, "Context:") || !strings.Contains(out, "crd_mine1") || strings.Contains(out, "crd_org1") {
		t.Errorf("cruise draft list in the personal context: %d\n%s", code, out)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.asked {
		if strings.Contains(a, "context=") {
			t.Errorf("asked %s in the personal context with a context", a)
		}
	}
}
