// recovery_test: the guards for the four verbs a person uses when an agent's
// queue has stopped moving. A scripted control plane on the loopback serves
// the capability registry, the session, its agents, their instructions and
// the hold on the queue, and every case below is measured through the built
// binary against it.
//
// The properties under test are the ones somebody would be hurt by if they
// were wrong. A recovery view states what the service recorded and says so
// where it recorded nothing, rather than composing a plausible sentence. A
// resume releases what was waiting and NEVER runs the blocked instruction
// again. A decision is bound to the state that was displayed, so a queue
// whose blocked instruction resolved — or whose session was restored —
// between the reading and the deciding is shown and REFUSED with nothing
// sent. And an option the service does not offer is visible with its reason,
// because an option nobody lists is an option nobody can weigh.
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

const (
	recoveryHold     = "hold_1"
	recoveryBlocking = "tsk_2"
	recoveryAgent    = "agt_main0001"
)

// recoveryCtl is the control plane in miniature for a held queue. Every
// field is read and written under the mutex.
type recoveryCtl struct {
	mu         sync.Mutex
	capability string
	requests   []string
	decisions  []string

	state         string // active | released
	cause         string
	revision      int64
	sessionEpoch  int64
	blockingState string
	summary       string
	held          []string
	decision      string
	finding       string
	noHold        bool   // this agent's queue is not held at all
	changeOnRead  bool   // the hold changes the moment it has been read
	decisionFault string // the typed refusal the decision route answers with
}

func newRecoveryCtl() *recoveryCtl {
	return &recoveryCtl{capability: "available", state: "active", cause: "task_failed", revision: 3, sessionEpoch: 1,
		blockingState: "failed", summary: "the test command exited 1 after writing three files; the work did not finish",
		held: []string{"tsk_3", "tsk_4"}}
}

func (c *recoveryCtl) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func (c *recoveryCtl) sentDecisions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.decisions...)
}

func (c *recoveryCtl) set(fn func(*recoveryCtl)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// holdDoc is the hold as the reader's route answers it, including the three
// options and the one that is not offered.
func (c *recoveryCtl) holdDoc() map[string]any {
	doc := map[string]any{
		"id": recoveryHold, "session_id": agentSessionRecord, "agent_id": recoveryAgent,
		"state": c.state, "cause": c.cause, "revision": c.revision,
		"blocking_task": recoveryBlocking, "blocking_task_state": c.blockingState,
		"reason": "the attempt failed; the instructions committed after it may have been written expecting its result",
		"epoch":  int64(1), "session_epoch": c.sessionEpoch,
		"held_tasks":  c.held,
		"consequence": fmt.Sprintf("%d queued instruction(s) wait; no worker may start one until this hold is decided", len(c.held)),
	}
	if c.summary != "" {
		doc["blocking_task_summary"] = c.summary
	}
	if c.state == "active" {
		doc["choices"] = []map[string]any{
			{"decision": "release_successors", "available": true,
				"effect":      fmt.Sprintf("the %d instruction(s) this hold stopped return to the queue in the order they were committed, and a worker may start them", len(c.held)),
				"state_after": "the blocking record keeps the outcome it has now (" + c.blockingState + "); it is not repeated and its outcome is not overwritten by the decision",
				"cost":        "the released instructions run and are metered like any other work; recording the decision itself costs nothing"},
			{"decision": "keep_held", "available": true,
				"effect":      "the hold stands, and what you established is recorded beside it",
				"state_after": "nothing moves: the held instructions keep their id, their committed order and their content, and stay held",
				"cost":        "nothing runs, so nothing is metered"},
			{"decision": "retry_from_safe_point", "available": false,
				"effect":             "a new attempt under the blocking task, resuming from a named save point",
				"state_after":        "unchanged: this service records no such attempt",
				"cost":               "not stated: an attempt whose boundary cannot be named cannot be costed either",
				"unavailable_reason": "no save point is recorded for this task, so a new attempt could not name the boundary it would resume from"},
		}
	} else {
		doc["choices"] = []map[string]any{}
		doc["consequence"] = "later work was permitted to continue; the record that stopped the queue kept the outcome it had and was not repeated"
	}
	// a decision is recorded beside the hold whether or not it released
	// anything: keeping a queue held is a decision somebody made
	if c.decision != "" {
		doc["decision"] = c.decision
		doc["decided_by"] = "acct_1"
		doc["decided_at"] = "2026-09-21T12:00:00Z"
		doc["finding"] = c.finding
	}
	return doc
}

var recoveryTaskRows = []map[string]any{
	{"id": "tsk_1", "agent_id": recoveryAgent, "submission_id": "sub_1", "state": "succeeded", "queue_seq": 1, "origin": "live", "content_ref": "ref_1", "created_at": "2026-09-21T11:50:00Z"},
	{"id": recoveryBlocking, "agent_id": recoveryAgent, "submission_id": "sub_2", "state": "failed", "queue_seq": 2, "origin": "live", "content_ref": "ref_2", "created_at": "2026-09-21T11:55:00Z"},
	{"id": "tsk_3", "agent_id": recoveryAgent, "submission_id": "sub_3", "state": "held", "queue_seq": 3, "origin": "live", "content_ref": "ref_3", "created_at": "2026-09-21T11:56:00Z"},
	{"id": "tsk_4", "agent_id": recoveryAgent, "submission_id": "sub_4", "state": "held", "queue_seq": 4, "origin": "live", "content_ref": "ref_4", "created_at": "2026-09-21T11:57:00Z"},
	// an instruction of an agent that is not in the session under test
	{"id": "tsk_elsewhere", "agent_id": "agt_other0001", "submission_id": "sub_9", "state": "held", "queue_seq": 1, "origin": "live", "content_ref": "ref_9", "created_at": "2026-09-21T11:58:00Z"},
}

func (c *recoveryCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	fault := func(code int, kind, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": message}})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		c.mu.Lock()
		availability := c.capability
		c.mu.Unlock()
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "2026-09-21T12:00:00Z", "price_book": "v1.3",
			"capabilities": []map[string]any{
				{"id": "agent.workspace", "availability": availability, "summary": "agent windows", "surface": "api", "note": "not open to accounts yet"},
			}, "limits": map[string]any{}})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{{
			"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort, "name": "checkout", "runtime_state": "running",
			"fleet_state": "running", "record_id": agentSessionRecord, "agent_activity": "working", "task_state": "2 held",
			"observed_at": "x", "last_activity_at": "x", "created_at": "x", "image": "base", "budget_tokens": 500000, "execution_epoch": 1,
		}}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": agentRows, "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		var items []map[string]any
		for _, t := range recoveryTaskRows {
			if t["agent_id"] == r.URL.Query().Get("agent_id") {
				items = append(items, t)
			}
		}
		if items == nil {
			items = []map[string]any{}
		}
		env(200, map[string]any{"items": items, "observed_at": "x"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/tasks/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/tasks/")
		for _, t := range recoveryTaskRows {
			if t["id"] == id {
				env(200, t)
				return
			}
		}
		fault(404, "ks_not_found", "no such task")
	case r.Method == "GET" && r.URL.Path == "/api/v2/queue-holds":
		c.mu.Lock()
		if c.noHold {
			c.mu.Unlock()
			env(200, map[string]any{"items": []map[string]any{}, "observed_at": "x"})
			return
		}
		doc := c.holdDoc()
		if c.changeOnRead {
			// the blocked instruction resolves the instant the hold has been
			// read: this is the race the comparison exists to lose safely,
			// and it deliberately does NOT move the hold's own revision
			c.blockingState = "succeeded"
			c.summary = "the registry shows version 4.2: it did publish after all"
		}
		c.mu.Unlock()
		env(200, map[string]any{"items": []map[string]any{doc}, "observed_at": "x"})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v2/queue-holds/"):
		raw, _ := readAllBody(r)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.decisions = append(c.decisions, string(raw))
		forced := c.decisionFault
		rev, epoch, state := c.revision, c.sessionEpoch, c.state
		c.mu.Unlock()
		switch {
		case forced != "":
			fault(409, forced, "this queue cannot be decided as it was read")
			return
		case state != "active":
			fault(409, "ks_hold_not_active", "this hold was already decided")
			return
		case int64(body["expected_revision"].(float64)) != rev:
			fault(409, "ks_revision_conflict", "the hold changed since it was read")
			return
		case int64(body["epoch"].(float64)) != epoch:
			fault(409, "ks_epoch_mismatch", "that is not the session's current generation")
			return
		}
		decision, _ := body["decision"].(string)
		finding, _ := body["finding"].(string)
		c.mu.Lock()
		c.decision = decision
		c.finding = finding
		c.revision++
		released := []string{}
		if decision == "release_successors" {
			c.state = "released"
			released = c.held
			c.held = []string{}
		}
		doc := c.holdDoc()
		c.mu.Unlock()
		if decision == "release_successors" {
			doc["released_tasks"] = released
		}
		env(200, doc)
	default:
		fault(404, "ks_not_found", "no such route")
	}
}

func recoveryFixture(t *testing.T) (*recoveryCtl, string, string) {
	t.Helper()
	c := newRecoveryCtl()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return c, bin, cfg
}

// The recovery view states what is blocking, what that instruction last
// reported, what is waiting behind it, and what each option would do —
// including the option the service does not offer, with its reason. The whole
// queue stays visible, failed work included.
func TestQueueShowNamesWhatBlocksWhatWaitsAndWhatEachChoiceWouldDo(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "show", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("queue show: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"is HELD",
		"failed instruction " + recoveryBlocking + " (failed)",
		"the test command exited 1 after writing three files",
		"your choices",
		"release_successors",
		"ks agent queue resume main --session " + agentSessionShort,
		"keep_held",
		"retry_from_safe_point",
		"NOT OFFERED",
		"no save point is recorded",
		"the queue in full (4 instruction(s)",
		"-> ", // the blocking instruction is marked in the queue
		"tsk_3",
		"held",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the recovery view lacks %q:\n%s", want, out)
		}
	}
	// a known failure is never dressed up as an unknown
	if strings.Contains(out, "unresolved instruction") {
		t.Errorf("a known failure is reported as unresolved:\n%s", out)
	}
	// it is a read: nothing was decided
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("a read verb sent %d decisions: %v", len(d), d)
	}
	for _, req := range c.seen() {
		if strings.HasPrefix(req, "POST ") {
			t.Errorf("a read verb sent a mutation: %v", c.seen())
		}
	}

	// the same read as one JSON document
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "show", "main", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("queue show --json: exit %d\n%s", code, out)
	}
	data := parseEnvelope(t, out)["data"].(map[string]any)
	if data["held"] != true {
		t.Errorf("held: %v", data["held"])
	}
	hold := data["hold"].(map[string]any)
	if hold["blocking_task"] != recoveryBlocking || hold["blocking_task_state"] != "failed" {
		t.Errorf("hold: %v", hold)
	}
	if got := hold["choices"].([]any); len(got) != 3 {
		t.Errorf("choices: %v", got)
	}
	if got := data["tasks"].([]any); len(got) != 4 {
		t.Errorf("the queue is not reported in full: %v", got)
	}
}

// Where the service recorded nothing, the view says nothing was recorded. It
// does not compose a plausible sentence to fill the space.
func TestQueueShowSaysNothingWasRecordedRatherThanInventingIt(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.summary = "" })
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "queue", "show", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("queue show: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "it reported     nothing was recorded") {
		t.Errorf("a missing report is not reported as missing:\n%s", out)
	}
}

// A resume releases what was held, under the revision and the generation that
// were on the screen, and says plainly that the blocked instruction was not
// run again.
func TestQueueResumeReleasesWhatWasHeldAndNeverRestartsTheBlockedInstruction(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "resume", "main",
		"--session", agentSessionShort, "--finding", "read the build log: it stopped before publishing anything")
	if code != 0 {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"resumed the queue of agent main", "2 instruction(s) may run again", "tsk_3", "tsk_4",
		"still reads failed and was NOT run again"} {
		if !strings.Contains(out, want) {
			t.Errorf("the resume report lacks %q:\n%s", want, out)
		}
	}
	sent := c.sentDecisions()
	if len(sent) != 1 {
		t.Fatalf("the resume sent %d decisions, want exactly 1: %v", len(sent), sent)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(sent[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["decision"] != "release_successors" {
		t.Errorf("decision: %v", body["decision"])
	}
	if body["expected_revision"] != float64(3) {
		t.Errorf("the decision is not bound to the revision that was read: %v", body["expected_revision"])
	}
	if body["epoch"] != float64(1) {
		t.Errorf("the decision does not name the generation it was prepared under: %v", body["epoch"])
	}
	if f, _ := body["finding"].(string); !strings.Contains(f, "stopped before publishing") {
		t.Errorf("what was established is not recorded with the decision: %v", body["finding"])
	}
}

// A decision over unsettled work records what was established. Without it
// nothing is sent at all.
func TestARecoveryDecisionWithoutAFindingSendsNothing(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"agent", "queue", "resume", "main", "--session", agentSessionShort},
		{"agent", "queue", "hold", "main", "--session", agentSessionShort},
		{"task", "resume", recoveryBlocking, "--session", agentSessionShort},
	} {
		out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != exitUsage {
			t.Fatalf("%v: exit %d (want %d)\n%s%s", args, code, exitUsage, out, errs)
		}
		if !strings.Contains(errs, "--finding") {
			t.Errorf("%v: the refusal does not name the flag:\n%s", args, errs)
		}
	}
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("a refused command sent %d decisions: %v", len(d), d)
	}
}

// New evidence resolves the blocked instruction while a person is looking at
// the recovery view. The decision they then ask for is REFUSED and NOTHING IS
// SENT: it was prepared against a queue that no longer exists, and the client
// does not quietly adopt the state the service reports now.
func TestARecoveryDecisionIsRefusedWhenTheQueueChangedSinceItWasShown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(c *recoveryCtl)
		want  string
	}{
		{"the blocked instruction resolved", func(c *recoveryCtl) {
			c.blockingState = "succeeded"
			c.summary = "the registry shows version 4.2: it did publish after all"
		}, "succeeded"},
		{"different work is waiting behind it", func(c *recoveryCtl) { c.held = []string{"tsk_3"} }, "1 instruction"},
		{"the session was restored underneath it", func(c *recoveryCtl) { c.sessionEpoch = 2 }, "generation 2"},
		{"the hold itself moved on", func(c *recoveryCtl) { c.revision = 4 }, "revision 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, bin, cfg := recoveryFixture(t)
			dir := t.TempDir()
			// the person reads the queue: THIS is the display the decision is
			// compared against
			if out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "show", "main", "--session", agentSessionShort); code != 0 {
				t.Fatalf("queue show: exit %d\n%s%s", code, out, errs)
			}
			c.set(tc.after)
			out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "resume", "main",
				"--session", agentSessionShort, "--finding", "nothing downstream depends on it")
			if code != exitConflict {
				t.Fatalf("resume over a changed queue: exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
			}
			for _, want := range []string{"changed after it was shown", "nothing was released", "no decision was sent", tc.want} {
				if !strings.Contains(errs, want) {
					t.Errorf("the refusal lacks %q:\n%s", want, errs)
				}
			}
			if d := c.sentDecisions(); len(d) != 0 {
				t.Fatalf("a queue that changed since it was shown was decided anyway: %v", d)
			}
		})
	}
}

// The same race with nothing displayed beforehand: the decision path shows
// the queue itself, reads it again, and refuses when the second read differs.
// A terminal that has looked at nothing is more careful, never less.
func TestARecoveryDecisionIsRefusedWhenTheQueueChangesBetweenTheDisplayAndTheSend(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.changeOnRead = true })
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "queue", "resume", "main",
		"--session", agentSessionShort, "--finding", "nothing downstream depends on it")
	if code != exitConflict {
		t.Fatalf("resume: exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
	}
	if !strings.Contains(errs, "changed after it was shown") {
		t.Errorf("the refusal does not say what happened:\n%s", errs)
	}
	if d := c.sentDecisions(); len(d) != 0 {
		t.Fatalf("a queue that changed under the decision was decided anyway: %v", d)
	}
}

// The service refuses a decision prepared before a restore. The client says
// so in words a person can act on, and does not send it again.
func TestARecoveryDecisionRefusedByTheServiceIsExplainedAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		fault string
		want  string
	}{
		{"ks_epoch_mismatch", "was restored while this was being decided"},
		{"ks_revision_conflict", "moved on while it was being decided"},
		{"ks_hold_not_active", "already decided elsewhere"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			c, bin, cfg := recoveryFixture(t)
			c.set(func(c *recoveryCtl) { c.decisionFault = tc.fault })
			out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "queue", "resume", "main",
				"--session", agentSessionShort, "--finding", "nothing downstream depends on it")
			if code != exitConflict {
				t.Fatalf("exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("the refusal lacks %q:\n%s", tc.want, errs)
			}
			if d := c.sentDecisions(); len(d) != 1 {
				t.Errorf("a refused decision was sent %d times; it is never retried automatically", len(d))
			}
		})
	}
}

// Leaving a queue held is a decision somebody records, not the absence of
// one. It releases nothing.
func TestQueueHoldRecordsThatItStaysHeldAndReleasesNothing(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "queue", "hold", "main",
		"--session", agentSessionShort, "--finding", "the release may have gone out; waiting on the registry")
	if code != 0 {
		t.Fatalf("hold: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"stays held behind failed instruction " + recoveryBlocking, "waiting on the registry", "recorded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the hold report lacks %q:\n%s", want, out)
		}
	}
	sent := c.sentDecisions()
	if len(sent) != 1 {
		t.Fatalf("the hold sent %d decisions, want 1: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], `"decision":"keep_held"`) {
		t.Errorf("the decision sent was not keep_held: %s", sent[0])
	}
}

// ks task resume is the same decision named from the other end. It resumes
// what is held behind the instruction you name, refuses an instruction that
// is not the one holding the queue (and names the one that is), and refuses
// where nothing is held at all.
func TestTaskResumeNamesTheBlockedInstructionAndRefusesAnythingElse(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	dir := t.TempDir()

	// an instruction that is not what holds the queue
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "task", "resume", "tsk_3",
		"--session", agentSessionShort, "--finding", "it looks independent to me")
	if code != exitConflict {
		t.Fatalf("resume of a non-blocking instruction: exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
	}
	for _, want := range []string{"is not what holds this queue", recoveryBlocking, "nothing was resumed"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, errs)
		}
	}
	if d := c.sentDecisions(); len(d) != 0 {
		t.Fatalf("a refused resume decided something: %v", d)
	}

	// the instruction that IS what holds the queue
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "task", "resume", recoveryBlocking,
		"--session", agentSessionShort, "--finding", "read the build log: nothing was published")
	if code != 0 {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "still reads failed and was NOT run again") {
		t.Errorf("the report does not say the named instruction was left alone:\n%s", out)
	}
	if d := c.sentDecisions(); len(d) != 1 || !strings.Contains(d[0], `"decision":"release_successors"`) {
		t.Fatalf("decisions sent: %v", d)
	}

	// an instruction of an agent that is not in the session that was named:
	// refused before any hold is read, because naming one session and
	// deciding another's queue is never what somebody meant
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "task", "resume", "tsk_elsewhere",
		"--session", agentSessionShort, "--finding", "looks fine to me")
	if code != exitUsage {
		t.Fatalf("resume of another session's instruction: exit %d (want %d)\n%s%s", code, exitUsage, out, errs)
	}
	if !strings.Contains(errs, "has no agent named") {
		t.Errorf("the refusal does not say the instruction is not this session's:\n%s", errs)
	}

	// nothing held: nothing to resume, and nothing changed
	c.set(func(c *recoveryCtl) { c.noHold = true })
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "task", "resume", recoveryBlocking,
		"--session", agentSessionShort, "--finding", "just checking")
	if code != exitConflict {
		t.Fatalf("resume with no hold: exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
	}
	if !strings.Contains(errs, "its queue is not held") {
		t.Errorf("the refusal lacks its reason:\n%s", errs)
	}
}

// A queue nobody is holding is a fact the view states plainly, with the queue
// still listed in full.
func TestQueueShowOnAQueueThatIsNotHeldSaysSoAndStillListsIt(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.noHold = true })
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "queue", "show", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("queue show: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"is not held", "the queue in full (4 instruction(s)", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the view lacks %q:\n%s", want, out)
		}
	}
}

// Every one of these verbs refuses cleanly while the control plane reports
// the capability unavailable — which is what it reports today — and none of
// them reaches any other route first.
func TestTheRecoveryVerbsRefuseWhileTheCapabilityIsUnavailable(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.capability = "unavailable" })
	dir := t.TempDir()
	for _, args := range [][]string{
		{"agent", "queue", "show", "main", "--session", agentSessionShort},
		{"agent", "queue", "resume", "main", "--session", agentSessionShort, "--finding", "x"},
		{"agent", "queue", "hold", "main", "--session", agentSessionShort, "--finding", "x"},
		{"task", "resume", recoveryBlocking, "--session", agentSessionShort, "--finding", "x"},
	} {
		out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != exitFailed {
			t.Fatalf("%v: exit %d (want %d)\n%s%s", args, code, exitFailed, out, errs)
		}
		if !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "unavailable") {
			t.Errorf("%v: the refusal does not name the capability and its state:\n%s", args, errs)
		}
	}
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("a disabled verb decided something: %v", d)
	}
	for _, req := range c.seen() {
		if req != "GET /api/capabilities" {
			t.Errorf("a disabled verb reached %q", req)
		}
	}
}
