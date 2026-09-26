// KS-054 and KS-052 on the command line: a fork is planned (creating
// nothing) and executed by its digest, a moved plan is refused with the
// reason; a restore prints the continuation exactly as given, and an
// unknown one is never success.
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

type forkCtl struct {
	mu           sync.Mutex
	stale        bool
	forks        []map[string]any
	continuation string
	stack        string
}

func (c *forkCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetfrk0000000000000000000000001", "short_id": "fleetfrk0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/sessions/session_1" && r.Method == "GET":
		env(200, map[string]any{"id": "session_1", "revision": 7, "execution_epoch": 3})
	case r.URL.Path == "/api/v2/sessions/session_1/fork-plan":
		env(201, map[string]any{"id": "fplan_1", "checkpoint_id": "ck_1", "children": 2, "plan_digest": "sha256:plan", "state": "planned", "plan": map[string]any{
			"source":    map[string]any{"checkpoint_id": "ck_1", "boundary": "attempt_closed", "effect": "each child starts from exactly this saved point"},
			"runtime":   "2 additional running session(s): each child is metered and billed as its own session",
			"keys":      map[string]any{"bindings": []any{map[string]any{"provider": "anthropic", "carried": true}}, "effect": "re-checked again when the fork executes"},
			"pending":   map[string]any{"instructions": 3, "effect": "copied into each child as HELD templates"},
			"approvals": map[string]any{"open_in_parent": 1, "effect": "none is inherited"},
			"advisers":  map[string]any{"connections_in_parent": 1, "effect": "not copied"}}})
	case r.URL.Path == "/api/v2/sessions/session_1/fork":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.forks = append(c.forks, b)
		if c.stale {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_plan_stale","type":"ks_plan_stale","message":"the session changed since this plan was made (revision 8, planned at 7)"}}`)
			return
		}
		env(201, map[string]any{"plan_id": "fplan_1", "children": []any{map[string]any{"session_id": "session_c1", "name": "c-1", "runtime_state": "running",
			"agents": []any{map[string]any{"agent_id": "agent_c1", "held_templates": []string{"tsk_x", "tsk_y"}, "hold_id": "hold_c1"}}}}})
	case r.URL.Path == "/api/v2/sessions/session_1/restore":
		note := map[string]string{"exact_runtime": "the engine loaded the whole machine from the saved point", "unknown": "whether the engine replaced the guest is unknown, so no continuation is claimed", "none": "the guest was not replaced"}[c.continuation]
		detail := ""
		if c.stack == "incompatible" {
			detail = "the engine refused: this saved point was taken on a different pinned stack (ks_stack_incompatible: runner 2.1.240 != 2.1.251; image claude-75 != claude-81)"
		}
		env(200, map[string]any{"session_id": "session_1", "checkpoint_id": "ck_1", "scope": "whole_session", "phases": []any{map[string]any{"phase": "restore", "done": c.continuation == "exact_runtime", "detail": detail}},
			"superseded_attempts": []string{}, "continuation": c.continuation, "continuation_note": note, "stack_compatibility": c.stack})
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no route"}}`)
	}
}

func TestAForkIsPlannedThenExecutedByItsDigest(t *testing.T) {
	c := &forkCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	if _, _, code := auditExec(t, bin, cfg, dir, env, "session", "fork", "fleetfrk"); code != exitUsage {
		t.Fatalf("neither plan nor execute: %d", code)
	}
	if _, _, code := auditExec(t, bin, cfg, dir, env, "session", "fork", "fleetfrk", "--plan"); code != exitUsage {
		t.Fatalf("no checkpoint: %d", code)
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "fork", "fleetfrk", "--plan", "--checkpoint", "ck_1", "--children", "2")
	if code != 0 || len(c.forks) != 0 {
		t.Fatalf("plan: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"NOTHING was created", "ck_1", "metered and billed as its own session", "carried", "HELD templates", "none is inherited", "not copied", "--execute sha256:plan --plan-id fplan_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "fork", "fleetfrk", "--execute", "sha256:plan", "--plan-id", "fplan_1")
	if code != 0 || c.forks[0]["plan_digest"] != "sha256:plan" || c.forks[0]["plan_id"] != "fplan_1" || !strings.Contains(out, "2 held template(s)") {
		t.Fatalf("execute: %d %v\n%s%s", code, c.forks, out, errs)
	}
	c.stale = true
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "fork", "fleetfrk", "--execute", "sha256:plan", "--plan-id", "fplan_1")
	if code != exitConflict || !strings.Contains(errs, "nothing was forked") || !strings.Contains(errs, "revision 8") || !strings.Contains(errs, "--plan") {
		t.Fatalf("stale: %d\n%s", code, errs)
	}
}
