// KS-041 on the command line: an instruction for a parked session is
// accepted and HELD and the session is not woken; only --resume resumes it,
// after acceptance, and a session that needs more than a resume is not
// "resumed" by the flag.
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

type tellCtl struct {
	mu      sync.Mutex
	next    string // the runtime next_action the receipt names; "" = running
	resumes int
	posts   int
}

func (c *tellCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleettel0000000000000000000000001", "short_id": "fleettel0000", "name": "checkout", "runtime_state": "parked", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.URL.Path == "/api/v2/agents/agent_1/tasks" && r.Method == "POST":
		c.posts++
		row := map[string]any{"id": "tsk_1", "agent_id": "agent_1", "state": "queued", "queue_seq": 3}
		if c.next != "" {
			row["runtime"] = map[string]any{"session_id": "session_1", "state": "parked", "woken": false, "next_action": c.next,
				"note": "accepted and held in this agent's queue; the session was not woken, so nothing runs and no model is called until the session is resumed"}
		}
		env(201, row)
	case r.URL.Path == "/api/v2/sessions/session_1/resume" && r.Method == "POST":
		c.resumes++
		env(200, map[string]any{"runtime_state": "resuming"})
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`)
	}
}

func TestAnInstructionForAParkedSessionIsHeldAndOnlyResumeWakesIt(t *testing.T) {
	c := &tellCtl{next: "POST /api/v2/sessions/session_1/resume"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "agent", "tell", "main", "run the tests", "--session", "fleettel")
	if code != 0 || !strings.Contains(out, "accepted and HELD") || !strings.Contains(out, "was NOT woken") || !strings.Contains(out, "ks agent resume main") || c.resumes != 0 {
		t.Fatalf("held: %d resumes=%d\n%s%s", code, c.resumes, out, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, t.TempDir(), env, "agent", "tell", "main", "run the lint", "--session", "fleettel", "--resume")
	if code != 0 || c.resumes != 1 || !strings.Contains(out, "because you asked with --resume") {
		t.Fatalf("--resume: %d resumes=%d\n%s%s", code, c.resumes, out, errs)
	}
	// a session that needs provisioning is not "resumed" by the flag
	c.mu.Lock()
	c.next = "POST /api/v2/sessions/session_1/provision"
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), env, "agent", "tell", "main", "run the build", "--session", "fleettel", "--resume", "--json")
	if code != exitConflict || c.resumes != 1 {
		t.Fatalf("not resumable: %d resumes=%d\n%s", code, c.resumes, errs)
	}
}
