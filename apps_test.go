package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type appsCtl struct {
	mu    sync.Mutex
	calls []string
	keys  []string
	gone  bool
}

func (c *appsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	if r.Method == "DELETE" {
		c.keys = append(c.keys, r.Header.Get("Idempotency-Key"))
	}
	gone := c.gone
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/connected-apps":
		items := []map[string]any{}
		if !gone {
			items = append(items, map[string]any{"id": "gr_0001", "client": "Claude", "verification": "Claude by Anthropic, verified by KeepState", "verified": true,
				"sends_codes_to": "claude.ai", "context": "personal", "context_name": "Personal", "scopes": []string{"ks:read", "ks:act"},
				"scope_words": []string{"See your sessions.", "Make changes you can undo at once."}, "connected_at": "2026-11-23T10:00:00Z", "last_used_at": ""})
		}
		env(200, map[string]any{"items": items, "next_cursor": "", "observed_at": "x"})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/connected-apps/gr_0001":
		c.mu.Lock()
		c.gone = true
		c.mu.Unlock()
		env(200, map[string]any{"id": "gr_0001", "client": "Claude"})
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no connected app of yours has that id", "retryable": false, "work_started": "no", "next_action": "ks apps list"}})
	}
}

func TestAppsListShowsTheAppAndWhatItMayDo(t *testing.T) {
	c := &appsCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "apps", "list")
	if code != 0 {
		t.Fatalf("list: %d %s %s", code, out, errs)
	}
	for _, want := range []string{"gr_0001", "Claude", "Personal", "never", "claude.ai", "See your sessions.", "ks apps disconnect <id>"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	j, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "apps", "list", "--json")
	if code != 0 || !strings.Contains(j, `"count"`) || !strings.Contains(j, `"gr_0001"`) {
		t.Errorf("--json: %d %s", code, j)
	}
}

func TestAppsDisconnectEndsItWithAnIdempotencyKey(t *testing.T) {
	c := &appsCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "apps", "disconnect", "gr_0001")
	if code != 0 || !strings.Contains(out, "disconnected gr_0001 (Claude)") || !strings.Contains(out, "every request it had asked you to confirm") {
		t.Fatalf("disconnect: %d %s %s", code, out, errs)
	}
	c.mu.Lock()
	keys := append([]string{}, c.keys...)
	c.mu.Unlock()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "ksop_") {
		t.Errorf("the disconnect did not carry one idempotency key: %v", keys)
	}
	if out, _, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "apps", "list"); !strings.Contains(out, "no app is connected") {
		t.Errorf("after: %s", out)
	}
	// an unknown id is refused in the service's words
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "apps", "disconnect", "gr_nope")
	if code == 0 || !strings.Contains(errs, "no connected app of yours has that id") {
		t.Errorf("an unknown app: %d %s", code, errs)
	}
}
