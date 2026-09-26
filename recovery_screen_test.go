// KS-031 (QA-031-2, VER-031-2): after a runner stops mid-instruction every
// client surface reads recovery, never Ready or Finished, and the recovery
// screen says from the records whether exact saved state was restored and
// which conversation the execution ran in. The fake control plane serves
// exactly what the service records after the crash close
// (ctl/workspace_crashview_test.go).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type crashCtl struct {
	mu            sync.Mutex
	requests      []string
	activity      string // the agent's activity: recovery_required, or ready once its supervisor is back
	journalFails  bool
	fromSaved     string // the crashed execution's checkpoint_id (a retry from a saved point)
	ranIn         string // the crashed execution's runner_session_id
	certifiedLast string // the last conversation a runner announced
}

const crashSummary = "UNKNOWN acceptance: the runner stopped reporting after it had begun this instruction. Tool Bash (toolu_x1) was started and its result was never observed, so what it did to the workspace is unknown."

func (c *crashCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	fault := func(code int, kind, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"type": kind, "message": msg}})
	}
	crashed := map[string]any{"id": "tsk_mig", "agent_id": "agt_main0001", "state": "reconciliation_required", "queue_seq": 1, "origin": "cli",
		"created_at": "2026-09-26T10:00:00Z", "revision": 3, "author_type": "user", "author_id": "u1", "current_attempt_id": "att_1"}
	held := map[string]any{"id": "tsk_deploy", "agent_id": "agt_main0001", "state": "held", "queue_seq": 2, "origin": "cli",
		"created_at": "2026-09-26T10:00:01Z", "revision": 2, "author_type": "user", "author_id": "u1", "held_reason": "queue held behind tsk_mig"}
	switch {
	case r.URL.Path == "/api/capabilities":
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "2026-09-26T12:00:00Z", "price_book": "v1.3",
			"capabilities": []map[string]any{
				{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
				{"id": "agent.workspace", "availability": "available", "summary": "agent windows", "surface": "api"},
			}, "limits": map[string]any{}})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{{
			"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort, "name": "checkout", "runtime_state": "running",
			"record_id": agentSessionRecord, "agent_activity": c.activity, "task_state": "reconciliation_required",
			"key_alias": "prod", "observed_at": "2026-09-26T12:00:00Z", "last_activity_at": "2026-09-26T11:00:00Z",
			"created_at": "2026-09-26T09:00:00Z", "image": "base", "budget_tokens": 500000, "execution_epoch": 1,
		}}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []map[string]any{{"id": "agt_main0001", "session_id": agentSessionRecord, "name": "main", "is_primary": true,
			"activity": c.activity, "observed_at": "2026-09-26T11:00:00Z", "revision": 4}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		env(200, map[string]any{"items": []map[string]any{crashed, held}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks/tsk_mig":
		env(200, crashed)
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks/tsk_mig/attempts":
		att := map[string]any{"id": "att_1", "task_id": "tsk_mig", "attempt_index": 1, "execution_epoch": 1, "worker_id": "runner-a",
			"state": "ambiguous", "closed_state": "reconciliation_required", "runner_session_id": c.ranIn, "summary": crashSummary}
		if c.fromSaved != "" {
			att["checkpoint_id"] = c.fromSaved
		}
		env(200, map[string]any{"items": []map[string]any{att}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/queue-holds":
		env(200, map[string]any{"items": []map[string]any{{
			"id": "hold_1", "session_id": agentSessionRecord, "agent_id": "agt_main0001", "state": "active", "cause": "task_unknown",
			"revision": 1, "blocking_task": "tsk_mig", "blocking_task_state": "reconciliation_required", "blocking_task_summary": crashSummary,
			"reason": "the outcome of tsk_mig is unknown", "epoch": 1, "session_epoch": 1, "held_tasks": []string{"tsk_deploy"},
			"consequence": "1 instruction waits",
			"choices": []map[string]any{
				{"decision": "release_successors", "available": true, "effect": "the held instructions start; tsk_mig is not repeated", "state_after": "tsk_mig stays reconciliation_required", "cost": "runtime while they run"},
				{"decision": "keep_held", "available": true, "effect": "nothing starts", "state_after": "held", "cost": "none"},
			}}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/"+agentSessionRecord+"/events":
		if c.journalFails {
			fault(503, "ks_unavailable", "the journal is not readable right now")
			return
		}
		items := []map[string]any{
			{"stream_seq": 5, "subject_type": "agent", "subject_id": "agt_main0001", "observed_at": "2026-09-26T10:30:00Z",
				"payload": map[string]any{"type": "agent.conversation_certified", "agent_id": "agt_main0001", "was": "", "now": c.certifiedLast}},
			{"stream_seq": 9, "subject_type": "agent", "subject_id": "agt_main0001", "observed_at": "2026-09-26T11:00:00Z",
				"payload": map[string]any{"type": "agent.activity", "activity": "recovery_required", "epoch": 1}},
			{"stream_seq": 10, "subject_type": "task", "subject_id": "tsk_mig", "observed_at": "2026-09-26T11:00:01Z",
				"payload": map[string]any{"type": "task.finished", "agent_id": "agt_main0001", "state": "reconciliation_required", "summary": crashSummary,
					"worker_id": "runner-a", "epoch": 1, "attempt_id": "att_1",
					"outcome": map[string]any{"runner_turn": "errored", "work": "unknown", "acceptance": "uncertain",
						"unsettled": []map[string]any{{"kind": "tool", "id": "toolu_x1", "detail": "started, never observed finishing"}}}}},
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	default:
		fault(404, "ks_not_found", "not served by this fake")
	}
}

func crashFixture(t *testing.T, c *crashCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func runOK(t *testing.T, bin, cfg string, args ...string) string {
	t.Helper()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
	if code != 0 {
		t.Fatalf("%v: exit %d\n%s%s", args, code, out, errs)
	}
	return out + errs
}

func TestAfterARunnerCrashTheRecoveryScreenShowsAndNothingReadsReadyOrFinished(t *testing.T) {
	c := &crashCtl{activity: "recovery_required", ranIn: "conv-aaaa", certifiedLast: "conv-aaaa"}
	bin, cfg := crashFixture(t, c)

	out := runOK(t, bin, cfg, "agent", "queue", "show", "main", "--session", agentSessionShort)
	for _, want := range []string{
		"!! RECOVERY: the runner stopped in the middle of an instruction. This agent is NOT ready and the instruction is NOT finished.",
		"instruction    tsk_mig reads reconciliation_required: what it did is UNKNOWN",
		"outcome        runner turn errored · work unknown · acceptance uncertain",
		"unsettled      tool toolu_x1: started, never observed finishing",
		"execution      att_1 reads ambiguous (worker runner-a, generation 1)",
		"runtime state  NOT restored: no saved point was applied",
		"ran in         conversation conv-aaaa",
		"certified      conversation conv-aaaa, announced by the runner at 2026-09-26T10:30:00Z; the execution ran in this one",
		"start/continue not shown",
		"your choices",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queue show lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "   decide ") {
		t.Errorf("the recovery view points at itself:\n%s", out)
	}

	out = runOK(t, bin, cfg, "agent", "status", "main", "--session", agentSessionShort)
	for _, want := range []string{"RECOVERY       its runner stopped", "queue hold     HELD behind tsk_mig, whose outcome is UNKNOWN"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"activity       ready", "succeeded", "finished"} {
		if strings.Contains(out, bad) {
			t.Errorf("status reads %q after a crash:\n%s", bad, out)
		}
	}

	// --json carries the screen's facts as fields
	js := runOK(t, bin, cfg, "agent", "queue", "show", "main", "--session", agentSessionShort, "--json")
	var doc struct {
		Data struct {
			Screen crashFacts `json:"recovery_screen"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(js[:strings.LastIndex(js, "}")+1]), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, js)
	}
	if s := doc.Data.Screen; s.Outcome == nil || s.Outcome.Acceptance != "uncertain" || s.Attempt == nil || s.Attempt.RunnerSessionID != "conv-aaaa" || !strings.HasPrefix(s.ModeNotShown, "not shown") {
		t.Errorf("recovery_screen: %+v", doc.Data.Screen)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a read sent %q", r)
		}
	}
}

// A supervisor that comes back reads ready; the queue is still held over the
// unknown, and status says so beside the word ready.
func TestAReturningSupervisorReadingReadyDoesNotHideTheHeldUnknown(t *testing.T) {
	bin, cfg := crashFixture(t, &crashCtl{activity: "ready", ranIn: "conv-aaaa", certifiedLast: "conv-aaaa"})
	out := runOK(t, bin, cfg, "agent", "status", "main", "--session", agentSessionShort)
	if !strings.Contains(out, "queue hold     HELD behind tsk_mig, whose outcome is UNKNOWN") {
		t.Errorf("ready hides the held unknown:\n%s", out)
	}
}

// A retry from a named saved point restored exact saved state; the screen
// says so, and says when the conversation differs from the certified one.
func TestTheScreenSaysWhenExactSavedStateWasRestored(t *testing.T) {
	bin, cfg := crashFixture(t, &crashCtl{activity: "recovery_required", fromSaved: "ckpt_7", ranIn: "conv-new", certifiedLast: "conv-aaaa"})
	out := runOK(t, bin, cfg, "agent", "queue", "show", "main", "--session", agentSessionShort)
	for _, want := range []string{"runtime state  RESTORED: this execution started from saved point ckpt_7", "the execution ran in a DIFFERENT one"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// A journal that cannot be read costs the outcome, and says so.
func TestAnUnreadableJournalIsSaidOnTheScreen(t *testing.T) {
	bin, cfg := crashFixture(t, &crashCtl{activity: "recovery_required", journalFails: true, ranIn: "conv-aaaa"})
	out := runOK(t, bin, cfg, "agent", "queue", "show", "main", "--session", agentSessionShort)
	for _, want := range []string{"the journal could not be READ, so the close's outcome is not shown", "certified      not shown: the journal could not be read"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// The live window shows the screen on the events, and nothing for others.
func TestTheWindowShowsTheRecoveryScreenOnTheCrashEvents(t *testing.T) {
	fin := journalEvent{SubjectType: "task", SubjectID: "tsk_mig", Payload: json.RawMessage(`{"type":"task.finished","state":"reconciliation_required","summary":"UNKNOWN acceptance","attempt_id":"att_1","outcome":{"runner_turn":"errored","work":"unknown","acceptance":"uncertain","unsettled":[{"kind":"tool","id":"toolu_x1","detail":"started, never observed finishing"}]}}`)}
	lines := strings.Join(recoveryScreenFor(fin, "main", "abc123"), "\n")
	for _, want := range []string{"!! RECOVERY", "tsk_mig reads reconciliation_required", "tool toolu_x1", "decide         ks agent queue show main --session abc123"} {
		if !strings.Contains(lines, want) {
			t.Errorf("window screen lacks %q:\n%s", want, lines)
		}
	}
	act := journalEvent{SubjectType: "agent", Payload: json.RawMessage(`{"type":"agent.activity","activity":"recovery_required","epoch":1}`)}
	if l := strings.Join(recoveryScreenFor(act, "main", "abc123"), "\n"); !strings.Contains(l, "NOT ready") {
		t.Errorf("activity screen: %s", l)
	}
	ok := journalEvent{SubjectType: "task", Payload: json.RawMessage(`{"type":"task.finished","state":"succeeded"}`)}
	if l := recoveryScreenFor(ok, "main", "abc123"); l != nil {
		t.Errorf("a finished instruction shows a recovery screen: %v", l)
	}
}
