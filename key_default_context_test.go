// ks key default in an organization's context: the organization's defaults,
// said so; a member's change is refused in the service's words with no step
// to sign in again; in the personal context, yours, with no context sent.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type keyDefaultsCtl struct {
	mu    sync.Mutex
	asked []string
}

func (c *keyDefaultsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.asked = append(c.asked, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	ctx := r.URL.Query().Get("context")
	w.Header().Set("Content-Type", "application/json")
	doc := func(d string) map[string]any {
		return map[string]any{"defaults": map[string]any{"anthropic": d}, "revision": 1,
			"managed": map[string]any{"available": false, "note": "not in this release", "providers": map[string]any{}}}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-10-15T00:00:00Z", "price_book": "v1.3",
			"capabilities": []any{map[string]any{"id": "managed_inference", "availability": "available", "summary": "managed", "surface": "api", "since_protocol": 2},
				map[string]any{"id": "agent.workspace", "availability": "available", "summary": "agents", "surface": "api", "since_protocol": 2}, map[string]any{"id": "keys.inventory", "availability": "available", "summary": "keys", "surface": "api", "since_protocol": 2}}, "limits": map[string]any{}}})
	case r.URL.Path == "/api/v2/orgs/org_a":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 2, "limit": nil}}})
	case r.URL.Path == "/api/v2/account/model-keys" && r.Method == "PUT" && ctx != "":
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"schema_version":2,"error":{"code":"ks_forbidden","type":"ks_forbidden","message":"the organization's default key per provider is set by its owners and admins; your role there (member) reads it","next_action":"ks key default"}}`))
	case r.URL.Path == "/api/v2/account/model-keys" && ctx == "org_a":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": doc("managed")})
	case r.URL.Path == "/api/v2/account/model-keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": doc("own")})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`))
	}
}

func TestKeyDefaultInAnOrganizationsContext(t *testing.T) {
	c := &keyDefaultsCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	if out, errs, code := ksIn(t, bin, cfg, dir, "context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, dir, "key", "default")
	if code != 0 || !strings.HasPrefix(out, "Context: organization org_a") || !strings.Contains(out, "KeepState's key") {
		t.Errorf("ks key default in the organization's context: %d\n%s%s", code, out, errs)
	}
	out, _, code = ksIn(t, bin, cfg, dir, "key", "default", "anthropic", "own", "--json")
	var d struct {
		Error struct {
			Code       string `json:"code"`
			NextAction string `json:"next_action"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &d)
	if code == 0 || d.Error.Code != "ks_forbidden" || strings.Contains(d.Error.NextAction, "ks login") || !strings.Contains(d.Error.Message, "owners and admins") {
		t.Errorf("a member changes the organization's defaults: %d %+v\n%s", code, d.Error, out)
	}
	c.mu.Lock()
	for _, a := range c.asked {
		if strings.Contains(a, "/api/v2/account/model-keys") && !strings.Contains(a, "context=org_a") {
			t.Errorf("asked %s in the organization's context without it", a)
		}
	}
	c.asked = nil
	c.mu.Unlock()
	if _, _, code := ksIn(t, bin, cfg, dir, "context", "use", "personal"); code != 0 {
		t.Fatal("context use personal")
	}
	out, _, code = ksIn(t, bin, cfg, dir, "key", "default")
	if code != 0 || strings.Contains(out, "Context:") || !strings.Contains(out, "your own key") {
		t.Errorf("ks key default in the personal context: %d\n%s", code, out)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.asked {
		if strings.Contains(a, "context=") {
			t.Errorf("asked %s in the personal context with a context", a)
		}
	}
}
