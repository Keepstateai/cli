// The key verbs in an organization's context: every request names the
// context, so the organization's keys are listed, stored, changed and deleted
// in the organization's vault and never in yours; in your personal context
// the requests are exactly what they were.
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

type orgKeysCtl struct {
	mu   sync.Mutex
	sent []string
}

func (c *orgKeysCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	org := r.URL.Query().Get("context") == "org_a"
	key := func(id, provider string) map[string]any {
		return map[string]any{"id": id, "provider": provider, "alias": "", "last4": "ABCD", "enabled": true, "revision": 3}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		_, _ = w.Write([]byte(`{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.4","capabilities":[{"id":"keys.inventory","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`))
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a":
		env(200, map[string]any{"id": "org_a", "name": "Acme", "state": "active", "revision": 3, "seats": map[string]any{"used": 1, "limit": nil}, "your_role": "admin", "your_seat": "assigned"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys" && org:
		env(200, map[string]any{"items": []any{key("vlt_org", "anthropic")}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys":
		env(200, map[string]any{"items": []any{key("vlt_mine", "anthropic")}, "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/keys":
		id := "vlt_mine"
		if org {
			id = "vlt_org2"
		}
		env(201, key(id, "openai"))
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/keys/vlt_org" && org:
		k := key("vlt_org", "anthropic")
		k["enabled"] = false
		env(200, k)
	case r.Method == "POST" && r.URL.Path == "/api/v2/keys/vlt_org/deletion-plan" && org:
		env(201, map[string]any{"id": "plan_1", "plan": map[string]any{"bindings": map[string]any{"sessions": []any{map[string]any{"session_id": "s1"}}}}})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/keys/vlt_org" && org:
		env(200, map[string]any{"deleted": true, "key_id": "vlt_org", "bindings_dropped": 1})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgKeysCtl) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.sent, "\n")
}
func (c *orgKeysCtl) reset() { c.mu.Lock(); defer c.mu.Unlock(); c.sent = nil }

func TestKeyVerbsInAnOrganizationsContext(t *testing.T) {
	c := &orgKeysCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// personal: exactly the requests of before, no context named
	out, errs, code := run("key", "list")
	if code != 0 || !strings.Contains(out, "vlt_mine") || strings.Contains(c.all(), "context=") {
		t.Fatalf("personal key list: %d\n%s%s\n%s", code, out, errs, c.all())
	}
	if out, errs, code := run("context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	c.reset()
	out, errs, code = run("key", "list")
	if code != 0 || !strings.Contains(out, "vlt_org") || strings.Contains(out, "vlt_mine") || !strings.Contains(out, "The organization org_a's own keys") {
		t.Fatalf("the organization's key list: %d\n%s%s", code, out, errs)
	}
	out, errs, code = run("key", "disable", "vlt_org")
	if code != 0 || !strings.Contains(c.all(), "PATCH /api/v2/keys/vlt_org?context=org_a") || !strings.Contains(out, "disabled") {
		t.Fatalf("disable the organization's key: %d\n%s%s\n%s", code, out, errs, c.all())
	}
	out, errs, code = run("key", "delete", "vlt_org", "--yes")
	if code != 0 || !strings.Contains(c.all(), "POST /api/v2/keys/vlt_org/deletion-plan?context=org_a") || !strings.Contains(c.all(), "DELETE /api/v2/keys/vlt_org?plan_id=plan_1&context=org_a") {
		t.Fatalf("delete the organization's key: %d\n%s%s\n%s", code, out, errs, c.all())
	}
	// a key of yours is not the organization's: nothing is sent to change it
	c.reset()
	if _, errs, code := run("key", "disable", "vlt_mine"); code == 0 || !strings.Contains(errs, "your own keys are not the organization's") || strings.Contains(c.all(), "PATCH") {
		t.Errorf("your key through the organization's context: %d %s\n%s", code, errs, c.all())
	}
	// every request of the key verbs in this context named it
	for _, l := range strings.Split(c.all(), "\n") {
		if strings.Contains(l, "/api/v2/keys") && !strings.Contains(l, "context=org_a") {
			t.Errorf("a key request in the organization's context did not name it: %s", l)
		}
	}
}
