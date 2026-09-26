// KS-038: agent list shows controller and consultation standing; removal
// goes through a plan; a busy agent is refused with its work named and ks
// agent stop suggested.
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

type manageCtl struct {
	mu      sync.Mutex
	busy    bool
	deletes []string
	creates int
}

func (c *manageCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	busy := func() {
		w.WriteHeader(409)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_agent_busy","type":"ks_agent_busy","message":"this agent has work in flight (tsk_run), and removing it would end that work unannounced","next_action":"POST /api/v2/agents/agent_1/stop"}}`)
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetmgt0000000000000000000000001", "short_id": "fleetmgt0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents" && r.Method == "GET":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "helper", "activity": "working", "controller": "held", "consultation": "busy"}}})
	case r.URL.Path == "/api/v2/agents" && r.Method == "POST":
		c.creates++
		w.WriteHeader(409)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_conflict","type":"ks_conflict","message":"this session already has a primary agent"}}`)
	case r.URL.Path == "/api/v2/agents/agent_1/removal-plan":
		if c.busy {
			busy()
			return
		}
		env(201, map[string]any{"id": "dplan_1", "expires_at": "x", "plan": map[string]any{"tasks": map[string]any{"queued_or_held": 2, "running": 0, "effect": "queued and held are cancelled"}, "session": "preserved"}})
	case r.URL.Path == "/api/v2/agents/agent_1" && r.Method == "DELETE":
		c.deletes = append(c.deletes, r.URL.Query().Get("plan_id"))
		if c.busy {
			busy()
			return
		}
		env(200, map[string]any{"removed": true, "session_preserved": true, "tasks_cancelled": 2})
	default:
		w.WriteHeader(404)
	}
}

func TestAgentStandingAndRemovalThroughAPlan(t *testing.T) {
	c := &manageCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, _, code := auditExec(t, bin, cfg, dir, env, "agent", "list", "--session", "fleetmgt")
	if code != 0 || !strings.Contains(out, "control held · advice busy") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	if _, _, code := auditExec(t, bin, cfg, dir, env, "agent", "remove", "helper", "--session", "fleetmgt"); code != exitUsage || len(c.deletes) != 0 {
		t.Fatalf("neither: %d", code)
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "remove", "helper", "--session", "fleetmgt", "--plan")
	if code != 0 || !strings.Contains(out, "NOTHING was removed") || !strings.Contains(out, "--execute dplan_1") || len(c.deletes) != 0 {
		t.Fatalf("plan: %d\n%s%s", code, out, errs)
	}
	if out, _, code := auditExec(t, bin, cfg, dir, env, "agent", "remove", "helper", "--session", "fleetmgt", "--execute", "dplan_1"); code != 0 || c.deletes[0] != "dplan_1" || !strings.Contains(out, "preserved") {
		t.Fatalf("execute: %d %v\n%s", code, c.deletes, out)
	}
	c.mu.Lock()
	c.busy = true
	c.mu.Unlock()
	for _, args := range [][]string{{"--plan"}, {"--execute", "dplan_1"}} {
		_, errs, code := auditExec(t, bin, cfg, dir, env, append([]string{"agent", "remove", "helper", "--session", "fleetmgt"}, args...)...)
		if code != exitConflict || !strings.Contains(errs, "tsk_run") || !strings.Contains(errs, "ks agent stop helper") || !strings.Contains(errs, "nothing was removed") {
			t.Errorf("busy %v: %d\n%s", args, code, errs)
		}
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "create", "second", "--session", "fleetmgt"); code != exitConflict || !strings.Contains(errs, "nothing was created") {
		t.Fatalf("create conflict: %d\n%s", code, errs)
	}
}
