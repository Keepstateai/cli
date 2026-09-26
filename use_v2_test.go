// BACKLOG-171 client: legacy kill, wake and fork on an AGENT session are
// refused by the service (409 ks_agent_session_use_v2, the engine never
// asked); the client shows that refusal first, by name, then follows the
// pointer to the workspace verb. A kill never becomes a deletion: it
// becomes a deletion PLAN and the deletion stays plan-plus-confirm.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type useV2Ctl struct {
	mu       sync.Mutex
	requests []string
}

func (c *useV2Ctl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	refuse := func(next string) {
		w.WriteHeader(409)
		_ = enc.Encode(map[string]string{"error": "ks_agent_session_use_v2",
			"message":     "this is an agent workspace session, and its lifecycle is changed only through the workspace routes. Nothing was changed",
			"next_action": next})
	}
	switch {
	case r.Method == "DELETE" && r.URL.Path == "/api/sessions/session_ag":
		refuse("DELETE /api/v2/sessions/session_ag")
	case r.Method == "POST" && r.URL.Path == "/api/sessions/session_ag/resume":
		refuse("POST /api/v2/sessions/session_ag/resume")
	case r.Method == "POST" && r.URL.Path == "/api/sessions/session_ag/fork":
		refuse("POST /api/v2/sessions/session_ag/fork-plan")
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_ag/deletion-plan":
		_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "dpl_1", "expires_at": "2026-09-26T13:00:00Z",
			"plan": map[string]any{"agents": map[string]any{"count": 1, "effect": "removed"}, "retention": "saved content retained"}}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_ag/resume":
		_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"runtime_state": "resuming"}})
	default:
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func (c *useV2Ctl) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]string(nil), c.requests...)
	c.requests = nil
	return out
}

func TestLegacyLifecycleOnAnAgentSessionFollowsThePointer(t *testing.T) {
	c := &useV2Ctl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	// wake: refused by name first, then the same resume on the v2 route
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "wake", "session_ag")
	if code != 0 || strings.TrimSpace(out) != "session_ag" {
		t.Fatalf("wake: exit %d\n%s%s", code, out, errs)
	}
	if i, j := strings.Index(errs, "[ks_agent_session_use_v2]"), strings.Index(errs, "resuming it through the workspace route"); i < 0 || j < 0 || i > j {
		t.Errorf("the refusal is not shown first:\n%s", errs)
	}
	if got := c.seen(); len(got) != 2 || got[1] != "POST /api/v2/sessions/session_ag/resume" {
		t.Errorf("wake requests: %v", got)
	}

	// kill: a deletion PLAN only; nothing deleted; the confirm command named
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "kill", "session_ag", "--force")
	if code != exitConflict {
		t.Fatalf("kill: exit %d\n%s", code, errs)
	}
	for _, want := range []string{"[ks_agent_session_use_v2]", "deletion plan dpl_1 for session session_ag: NOTHING was deleted",
		"session session_ag was NOT killed", "ks session delete session_ag --execute dpl_1 --confirm session_ag"} {
		if !strings.Contains(errs, want) {
			t.Errorf("kill lacks %q:\n%s", want, errs)
		}
	}
	for _, r := range c.seen() {
		if strings.HasPrefix(r, "DELETE /api/v2/") {
			t.Fatalf("a kill became a deletion: %s", r)
		}
	}

	// fork: never a saved point chosen for you; the plan command named
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "fork", "session_ag", "-n", "2")
	if code != exitConflict || !strings.Contains(errs, "session session_ag was NOT forked") ||
		!strings.Contains(errs, "ks session fork session_ag --plan --checkpoint <saved point> --children 2") {
		t.Errorf("fork: exit %d\n%s", code, errs)
	}
	if got := c.seen(); len(got) != 1 {
		t.Errorf("fork sent more than the refused request: %v", got)
	}
}
