// KS-083 client: ks session migrate shows the preview, refuses blockers by
// name, is confirmed only by the preview's digest, applies exactly it and
// reads the record back. KS-090: the rollout and recovery refusals are
// shown by name and are definite (nothing was done, nothing is retried).
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

type migrateCtl struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	blocked  bool
	fail     bool
	mode     string
}

func (c *migrateCtl) preview() map[string]any {
	p := map[string]any{"session_id": "session_leg1", "mode": "legacy", "eligible": !c.blocked, "blockers": []string{},
		"runner":       map[string]any{"compatible": true, "detail": "no engine runs this session; its next start uses the agent runner"},
		"workspace":    map[string]any{"engine_linked": false, "runtime_state": "parked"},
		"key_bindings": []string{"anthropic"}, "saved_points": map[string]any{"count": 2, "latest": "2026-09-26T09:00:00Z"},
		"preserved": map[string]any{"tasks": 3, "agents": 0, "adviser_grants": 1}, "rollback": "saved point ckpt_9",
		"script_changes": []any{
			map[string]any{"command": "ks exec / POST /api/sessions/{id}/exec", "before": "runs a shell command in the session", "after": "refused, 409 ks_agent_session_no_shell"},
			map[string]any{"command": "ks checkpoint", "before": "saves the session", "after": "unchanged"},
		},
		"preview_digest": "3f2a1c9b8d7e" + strings.Repeat("0", 52)}
	if c.blocked {
		p["runner"] = map[string]any{"compatible": false, "detail": "an engine is running this session without an agent adapter; save and stop it first"}
		p["workspace"] = map[string]any{"engine_linked": true, "runtime_state": "running"}
		p["blockers"] = []string{"an engine is running this session without an agent adapter; save and stop it first"}
	}
	return p
}

func (c *migrateCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.bodies = append(c.bodies, string(b))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = enc.Encode(map[string]any{"schema_version": 2, "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		_ = enc.Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "retryable": code == 503, "work_started": "no"}})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "x", "price_book": "v1.3", "limits": map[string]any{},
			"capabilities": []map[string]any{{"id": "agent.workspace", "availability": "available", "summary": "s", "surface": "api"}}})
	case r.URL.Path == "/api/v2/sessions/session_leg1/migration":
		env(200, c.preview())
	case r.URL.Path == "/api/v2/sessions/session_leg1/migrate":
		if c.fail {
			w.WriteHeader(409)
			_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"outcome": "failed_restored", "mode": "legacy"},
				"error": map[string]any{"type": "ks_migration_failed", "message": "the conversion failed and changed nothing: the session is still legacy"}})
			return
		}
		c.mu.Lock()
		c.mode = "agent"
		c.mu.Unlock()
		env(200, map[string]any{"session_id": "session_leg1", "outcome": "migrated", "mode": "agent", "rollback_point": "ckpt_9", "agent_id": "agt_new",
			"note": "the session is an agent session with its primary agent", "preserved_tasks": 3, "preserved_adviser_grants": 1, "preserved_members": 2})
	case r.URL.Path == "/api/v2/sessions/session_leg1":
		c.mu.Lock()
		mode := c.mode
		c.mu.Unlock()
		env(200, map[string]any{"id": "session_leg1", "mode": mode, "primary_agent_id": "agt_new"})
	case r.URL.Path == "/api/v2/preflight":
		env(200, map[string]any{"checks": []any{}, "blockers": []any{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions":
		refuse(503, "ks_rollout_paused", "new agent sessions are paused while an incident is handled; your existing sessions, their status, cancellation and saved results are unaffected")
	case r.Method == "POST" && r.URL.Path == "/api/sessions":
		w.WriteHeader(410)
		_ = enc.Encode(map[string]any{"error": map[string]string{"type": "ks_legacy_retired", "message": "new legacy sessions are retired; your existing sessions keep their status, saves and results"}})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/sessions/"):
		w.WriteHeader(503)
		_ = enc.Encode(map[string]any{"error": map[string]string{"type": "ks_read_only_recovery", "message": "this control-plane instance is in read-only recovery (a newer build wrote the store); reads work"}})
	default:
		refuse(404, "ks_not_found", "no route")
	}
}

func (c *migrateCtl) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for i, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r+" "+c.bodies[i])
		}
	}
	return out
}

func TestSessionMigratePreviewConfirmApplyReadBack(t *testing.T) {
	c := &migrateCtl{mode: "legacy"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	// without a terminal: the preview, then a confirmation required; --yes is not one
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "migrate", "session_leg1", "--yes")
	for _, want := range []string{"migration preview for session session_leg1 (now legacy), digest 3f2a1c9b8d7e", "key bindings   anthropic (provider names only)",
		"rollback       saved point ckpt_9", "refused, 409 ks_agent_session_no_shell", "--yes does not confirm it", "ks session migrate session_leg1 --confirm 3f2a1c9b8d7e"} {
		if !strings.Contains(errs, want) {
			t.Errorf("unconfirmed lacks %q:\n%s", want, errs)
		}
	}
	if code != exitUsage || len(c.sent()) != 0 {
		t.Fatalf("unconfirmed: exit %d, sent %v", code, c.sent())
	}
	// a digest of another preview: refused, nothing sent
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "migrate", "session_leg1", "--confirm", "aaaaaaaaaaaa")
	if code != exitConflict || !strings.Contains(errs, "is not the preview shown now") || len(c.sent()) != 0 {
		t.Fatalf("stale confirm: exit %d, sent %v\n%s", code, c.sent(), errs)
	}
	// confirmed: exactly the previewed digest is sent, and the record read back
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "migrate", "session_leg1", "--confirm", "3f2a1c9b8d7e")
	if code != 0 {
		t.Fatalf("migrate: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"session session_leg1: migrated (mode agent)", "agent          agt_new", "rollback point ckpt_9",
		"preserved      3 task(s), 1 adviser connection(s), 2 member(s)", "read back: the session record now reads mode agent, primary agent agt_new"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	sent := c.sent()
	if len(sent) != 1 || !strings.Contains(sent[0], `"preview_digest":"3f2a1c9b8d7e`+strings.Repeat("0", 52)+`"`) {
		t.Errorf("sent %v", sent)
	}
}

func TestSessionMigrateRefusesBlockersAndReportsAFailure(t *testing.T) {
	c := &migrateCtl{blocked: true, mode: "legacy"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "migrate", "session_leg1", "--confirm", "3f2a1c9b8d7e")
	if code != exitConflict || !strings.Contains(errs, "BLOCKED:") || !strings.Contains(errs, "save and stop it first") ||
		!strings.Contains(errs, "ks checkpoint session_leg1 --stop") || len(c.sent()) != 0 {
		t.Fatalf("blocked: exit %d, sent %v\n%s", code, c.sent(), errs)
	}
	c.mu.Lock()
	c.blocked, c.fail = false, true
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "migrate", "session_leg1", "--confirm", "3f2a1c9b8d7e")
	if code != exitFailed || !strings.Contains(errs, "the conversion failed and changed nothing") {
		t.Errorf("failed: exit %d\n%s", code, errs)
	}
}

func TestKS090RefusalsAreNamedAndDefinite(t *testing.T) {
	c := &migrateCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	cases := []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"run", "--agent"}, exitTemporary, []string{"the service has paused new actions of this kind", "[ks_rollout_paused]", "status, cancellation and results still work"}},
		{[]string{"run"}, exitFailed, []string{"[ks_legacy_retired]", "ks run --agent"}},
		{[]string{"kill", "sess_x"}, exitTemporary, []string{"read-only recovery", "[ks_read_only_recovery]", "reading (status, lists, results) still works"}},
	}
	for _, k := range cases {
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), k.args...)
		if code != k.code {
			t.Errorf("%v: exit %d, want %d\n%s", k.args, code, k.code, errs)
		}
		for _, w := range k.want {
			if !strings.Contains(errs, w) {
				t.Errorf("%v lacks %q:\n%s", k.args, w, errs)
			}
		}
		// definite: no operation lookup, no "unknown" outcome
		if strings.Contains(errs, "unknown") && strings.Contains(errs, "Remote work started: unknown") {
			t.Errorf("%v was treated as uncertain:\n%s", k.args, errs)
		}
	}
	for _, r := range c.requests {
		if strings.Contains(r, "/api/operations/") {
			t.Errorf("a definite refusal was looked up as an uncertain operation: %s", r)
		}
	}
}
