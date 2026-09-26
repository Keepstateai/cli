// BACKLOG-150: a session the funds interlock parked reads, on every
// surface, "paused: out of credit — add credit (console), then resume" and
// never running. The fake control plane serves exactly the fields the
// service added: the session record's park_reason, the agent's and the
// task's session_runtime, and the live view's header.park_reason with the
// frozen-status note and the add_credits action.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const (
	fundsNote = "the account's credits ran out, so the funds interlock saved this session and paused it (it was never killed). Nothing runs and nothing is charged while it is parked; the agent's last reported state is frozen at the saved point. Add credits, then resume the session"
	fundsNext = "POST /api/billing/checkout"
)

// parkCtl is a recording fake control plane for one session whose agent
// was mid-instruction when the credits ran out.
type parkCtl struct {
	mu          sync.Mutex
	requests    []string
	reason      string // the record's park_reason
	recordState string // the record's runtime_state
	recordsFail bool   // the record list answers 503
}

func (c *parkCtl) runtime() any {
	if c.recordState == "running" {
		return nil
	}
	rt := map[string]any{"state": c.recordState, "reason": c.reason}
	if c.reason == "funds_interlock" {
		rt["note"], rt["next_action"] = fundsNote, fundsNext
	} else {
		rt["note"] = "the session is parked; its agent's last reported state is from before the pause, and nothing runs until it is resumed"
		rt["next_action"] = "POST /api/v2/sessions/session_rec1/resume"
	}
	return rt
}

func (c *parkCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	rt := c.runtime()
	task := map[string]any{"id": "tsk_live", "agent_id": "agt_main0001", "state": "running", "queue_seq": 3, "origin": "cli",
		"created_at": "2026-09-26T10:00:00Z", "updated_at": "2026-09-26T10:01:00Z", "revision": 2, "author_type": "user", "author_id": "u1"}
	queued := map[string]any{"id": "tsk_next", "agent_id": "agt_main0001", "state": "queued", "queue_seq": 4, "origin": "cli",
		"created_at": "2026-09-26T10:02:00Z", "revision": 1, "author_type": "user", "author_id": "u1"}
	switch {
	case r.URL.Path == "/api/capabilities":
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "2026-09-26T12:00:00Z", "price_book": "v1.3",
			"capabilities": []map[string]any{
				{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
				{"id": "agent.workspace", "availability": "available", "summary": "agent windows", "surface": "api"},
			}, "limits": map[string]any{}})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		// the fleet's own word is "stopping"; the record's facts are the
		// agent's last word (working) and its task's (running): all frozen
		env(200, map[string]any{"items": []map[string]any{{
			"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort, "name": "checkout", "runtime_state": "stopping",
			"record_id": agentSessionRecord, "agent_activity": "working", "task_state": "running",
			"key_alias": "prod", "observed_at": "2026-09-26T12:00:00Z", "last_activity_at": "2026-09-26T11:00:00Z",
			"created_at": "2026-09-26T09:00:00Z", "image": "base", "budget_tokens": 500000, "execution_epoch": 1,
		}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/sessions":
		if c.recordsFail {
			fault(503, "ks_unavailable", "the records are not readable right now")
			return
		}
		rec := map[string]any{"id": agentSessionRecord, "name": "checkout", "runtime_state": c.recordState, "revision": 5}
		if c.reason != "" {
			rec["park_reason"] = c.reason
		}
		env(200, map[string]any{"items": []map[string]any{rec}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		a := map[string]any{"id": "agt_main0001", "session_id": agentSessionRecord, "name": "main", "is_primary": true,
			"activity": "working", "observed_at": "2026-09-26T11:00:00Z", "active_task_id": "tsk_live", "revision": 4}
		if rt != nil {
			a["session_runtime"] = rt
		}
		env(200, map[string]any{"items": []map[string]any{a}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		env(200, map[string]any{"items": []map[string]any{task, queued}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks/tsk_live":
		if rt != nil {
			task["session_runtime"] = rt
		}
		env(200, task)
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents/agt_main0001/view":
		v := map[string]any{
			"header": map[string]any{"session_id": agentSessionRecord, "session_name": "checkout", "agent_id": "agt_main0001", "agent_name": "main",
				"controller": "none", "runtime_state": c.recordState, "activity": "working", "observed_at": "2026-09-26T11:00:00Z", "park_reason": c.reason},
			"status":       map[string]any{"label": "working", "observed_at": "2026-09-26T11:00:00Z", "stale": true, "stale_after_seconds": 15, "note": fundsNote + ". last known state"},
			"current_task": map[string]any{"id": "tsk_live", "label": "Running"},
			"next_action":  map[string]any{"action": "add_credits", "label": "Add credits", "request": fundsNext, "discloses": "adding credits does not resume the session by itself"},
		}
		env(200, v)
	default:
		fault(404, "ks_not_found", "not served by this fake")
	}
}

func parkFixture(t *testing.T, c *parkCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

// runningWord finds a session or task that reads running on its own.
var runningWord = regexp.MustCompile(`(?m)(^\s*(state|session|activity)\s+running)|(\s(running|Running)\s*$)`)

func TestFundsParkReadsPausedOnEverySurface(t *testing.T) {
	c := &parkCtl{reason: "funds_interlock", recordState: "stopping"}
	bin, cfg := parkFixture(t, c)
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errs)
		}
		return out + errs
	}
	cases := map[string][]string{
		"session list": {"session", "list"},
		"session show": {"session", "show", agentSessionShort},
		"agent status": {"agent", "status", "main", "--session", agentSessionShort},
		"agent list":   {"agent", "list", "--session", agentSessionShort},
		"agent view":   {"agent", "view", "main", "--session", agentSessionShort},
		"task show":    {"task", "show", "tsk_live", "--session", agentSessionShort},
		"task list":    {"task", "list", "--session", agentSessionShort},
	}
	for name, args := range cases {
		out := run(args...)
		if name == "agent view" {
			if !strings.Contains(out, "PAUSED: out of credit — add credit (console), then resume") {
				t.Errorf("%s does not say the pause:\n%s", name, out)
			}
			if !strings.Contains(out, "next: add credit in the console (POST /api/billing/checkout), then resume") {
				t.Errorf("%s does not name the add-credit action:\n%s", name, out)
			}
			if !strings.Contains(out, "frozen at the saved point") || !strings.Contains(out, "tsk_live paused (frozen)") {
				t.Errorf("%s does not say the status is frozen:\n%s", name, out)
			}
		} else if !strings.Contains(out, fundsPausedLine) {
			t.Errorf("%s does not read %q:\n%s", name, fundsPausedLine, out)
		}
		if m := runningWord.FindString(out); m != "" {
			t.Errorf("%s reads running (%q) for a funds-parked session:\n%s", name, m, out)
		}
	}
	// the tables show the pause in their cells, not only in a footnote
	if out := run("session", "list"); !regexp.MustCompile(`agentsession00\s+paused\s+frozen\s+paused`).MatchString(out) {
		t.Errorf("session list cells:\n%s", out)
	}
	if out := run("task", "list", "--session", agentSessionShort); !regexp.MustCompile(`tsk_live\s+3 paused`).MatchString(out) ||
		!regexp.MustCompile(`tsk_next\s+4 paused`).MatchString(out) {
		t.Errorf("task list cells:\n%s", out)
	}
	// status puts the pause FIRST and the agent's word as frozen history
	out := run("agent", "status", "main", "--session", agentSessionShort)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], fundsPausedLine) {
		t.Errorf("the pause is not the first thing status says:\n%s", out)
	}
	for _, want := range []string{"FROZEN at working", "add credit     in the console (the service's action: POST /api/billing/checkout)", "then resume    ks agent resume main --session " + agentSessionShort, "NOT moving"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	// --json carries the service's fields as read
	js := run("agent", "status", "main", "--session", agentSessionShort, "--json")
	data := parseEnvelope(t, js)["data"].(map[string]any)
	rt, _ := data["agent"].(map[string]any)["session_runtime"].(map[string]any)
	if rt["reason"] != "funds_interlock" || rt["next_action"] != fundsNext {
		t.Errorf("status --json session_runtime: %v", rt)
	}
	js = run("session", "show", agentSessionShort, "--json")
	if d := parseEnvelope(t, js)["data"].(map[string]any); d["park_reason"] != "funds_interlock" {
		t.Errorf("session show --json park_reason: %v", d["park_reason"])
	}
	js = run("task", "show", "tsk_live", "--session", agentSessionShort, "--json")
	tk := parseEnvelope(t, js)["data"].(map[string]any)["task"].(map[string]any)
	if r, _ := tk["session_runtime"].(map[string]any); r["reason"] != "funds_interlock" {
		t.Errorf("task show --json session_runtime: %v", tk["session_runtime"])
	}
	// every request was a read
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a read sent %q", r)
		}
	}
}

// An ordinary park (a person parked it) says parked with the service's note
// and resume action, and is not called out of credit.
func TestOrdinaryParkIsNotCalledOutOfCredit(t *testing.T) {
	bin, cfg := parkFixture(t, &parkCtl{recordState: "parked"})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "status", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if strings.Contains(out, "out of credit") {
		t.Errorf("an ordinary park reads out of credit:\n%s", out)
	}
	for _, want := range []string{"session        parked: the session is parked", "next           POST /api/v2/sessions/session_rec1/resume", "FROZEN at working"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// A running session carries no session_runtime and reads as before.
func TestRunningSessionIsUnchanged(t *testing.T) {
	bin, cfg := parkFixture(t, &parkCtl{recordState: "running"})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "status", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if strings.Contains(out, "paused") || strings.Contains(out, "FROZEN") || !strings.Contains(out, "activity       working") {
		t.Errorf("a running session is shown as paused:\n%s", out)
	}
}

// When the records cannot be read, the list says why a park is unknown; it
// never reports the session as not parked for credit.
func TestUnreadableParkReasonIsSaid(t *testing.T) {
	bin, cfg := parkFixture(t, &parkCtl{reason: "funds_interlock", recordState: "stopping", recordsFail: true})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "list")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "why a session is parked could not be read") || !strings.Contains(out, "may be out of credit") {
		t.Errorf("the unreadable reason is not said:\n%s", out)
	}
}

// The live window turns the journal's session.parked event for a funds park
// into the pause line, so the window never goes quiet over it.
func TestWindowEventLineForFundsPark(t *testing.T) {
	e := journalEvent{StreamSeq: 42, ObservedAt: "2026-09-26T12:00:00Z", SubjectType: "session", SubjectID: agentSessionRecord,
		Payload: json.RawMessage(`{"type":"session.parked","reason":"funds_interlock","runtime_state":"stopping","next_action":"POST /api/billing/checkout"}`)}
	if l := agentEventLine(e); !strings.HasPrefix(l, "!! ") || !strings.Contains(l, fundsPausedLine) {
		t.Errorf("event line: %q", l)
	}
	e.Payload = json.RawMessage(`{"type":"session.parked","reason":"","runtime_state":"parked"}`)
	if l := agentEventLine(e); strings.Contains(l, "out of credit") {
		t.Errorf("an ordinary park event reads out of credit: %q", l)
	}
}
