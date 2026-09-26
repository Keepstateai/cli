// KS-032 on the command line: ks agent open resolves a name through the
// service and chooses nothing among several; a session that is not running
// is shown with its runtime actions and never woken; only --resume resumes.
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

type openCtl struct {
	mu      sync.Mutex
	calls   []string
	resumes int
	opens   int
}

func (c *openCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	target := func(id, sess, sname string) map[string]any {
		return map[string]any{"agent_id": id, "agent_name": "main", "is_primary": true, "activity": "ready", "session_id": sess, "session_name": sname,
			"runtime_state": "parked", "project_name": "payments", "key_routes": []string{"anthropic"}}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/agent-targets":
		switch r.URL.Query().Get("name") {
		case "main":
			env(map[string]any{"resolution": "ambiguous", "candidates": []any{target("agent_1", "session_1", "checkout"), target("agent_2", "session_2", "billing")}})
		case "reviewer":
			t := target("agent_1", "session_1", "checkout")
			t["agent_name"] = "reviewer"
			env(map[string]any{"resolution": "resolved", "target": t, "candidates": []any{t}})
		default:
			env(map[string]any{"resolution": "not_found", "candidates": []any{}, "create": "ks run --agent --agent-name ghost"})
		}
	case r.URL.Path == "/api/v2/sessions":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetopn0000000000000000000000001", "short_id": "fleetopn0000", "name": "checkout", "runtime_state": "parked", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents/agent_1/open":
		c.opens++
		env(map[string]any{"agent": map[string]any{"id": "agent_1", "name": "reviewer", "activity": "ready"}, "lease": nil, "queue_depth": 0,
			"runtime": map[string]any{"session_id": "session_1", "state": "parked", "running": false, "woken": false,
				"note": "the session is saved and stopped. Opening did not wake it; nothing runs and no runtime is charged until you resume it",
				"actions": []any{
					map[string]any{"action": "resume_session", "label": "Resume session", "request": "POST /api/v2/sessions/session_1/resume", "discloses": "runtime is charged from the moment it runs"},
					map[string]any{"action": "view_saved_transcript", "label": "View saved transcript", "request": "GET /api/v2/sessions/session_1/events"},
				}}})
	case r.URL.Path == "/api/v2/sessions/session_1/resume":
		c.resumes++
		env(map[string]any{"runtime_state": "resuming"})
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no route"}}`)
	}
}

func TestOpenResolvesByNameAndNeverWakesTheSession(t *testing.T) {
	c := &openCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	// ambiguous, no terminal: refused with both listed, nothing opened
	_, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "open", "main")
	if code != exitUsage || !strings.Contains(errs, "checkout") || !strings.Contains(errs, "billing") || !strings.Contains(errs, "none is chosen") || c.opens != 0 {
		t.Fatalf("ambiguous: %d\n%s", code, errs)
	}
	// not found: never created
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "open", "ghost"); code != exitUsage || !strings.Contains(errs, "never creates") || c.opens != 0 {
		t.Fatalf("not found: %d\n%s", code, errs)
	}
	// resolved; the session is parked: shown with its actions, not woken
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "open", "reviewer")
	if code != 0 || !strings.Contains(out, "was NOT woken") || !strings.Contains(out, "Resume session") || !strings.Contains(out, "runtime is charged") ||
		!strings.Contains(out, "--resume") || !strings.Contains(errs, "target: session") || c.resumes != 0 {
		t.Fatalf("parked: %d resumes=%d\n%s%s", code, c.resumes, out, errs)
	}
	// --resume: the resume route, and only then
	_, errs, _ = auditExec(t, bin, cfg, dir, env, "agent", "open", "reviewer", "--resume", "--no-follow")
	if c.resumes != 1 || !strings.Contains(errs, "because you asked with --resume") {
		t.Fatalf("--resume: resumes=%d\n%s", c.resumes, errs)
	}
}
