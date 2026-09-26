package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Before these verbs the only way to read an instruction's
// standing was the recovery view, which answers why a queue is STUCK — and
// the control plane's own refusals named `ks task list` and `ks task show`
// on nine different paths. Advice naming a command nobody can type is a
// route the product only imagines it has.

// The queue reads in the order it was COMMITTED, with every agent of the
// session, and it never wanders into another session's work.
func TestTaskListShowsTheQueueInCommittedOrder(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "list", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task list: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"INSTRUCTION", "POS", "STATE", "ORIGIN", "AUTHOR", "tsk_1", recoveryBlocking, "tsk_3", "tsk_4"} {
		if !strings.Contains(out, want) {
			t.Errorf("the queue listing lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tsk_elsewhere") {
		t.Errorf("another agent's instruction appeared in this session's queue:\n%s", out)
	}
	// committed order, not arrival order of the rows
	pos := func(id string) int { return strings.Index(out, id) }
	if !(pos("tsk_1") < pos(recoveryBlocking) && pos(recoveryBlocking) < pos("tsk_3") && pos("tsk_3") < pos("tsk_4")) {
		t.Errorf("the queue is not in committed order:\n%s", out)
	}
	assertOnlyPublishedRoutes(t, c.seen())
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("a read verb sent a decision: %v", d)
	}
	for _, req := range c.seen() {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "PUT") || strings.HasPrefix(req, "DELETE") || strings.HasPrefix(req, "PATCH") {
			t.Errorf("a read verb sent a mutation: %q", req)
		}
	}
}

// --json is the shape automation reads; it carries the rows, not prose.
func TestTaskListJSONCarriesTheRowsAndTheCount(t *testing.T) {
	_, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "list", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("task list --json: exit %d\n%s%s", code, out, errs)
	}
	var env struct {
		Data struct {
			Tasks  int `json:"tasks"`
			Queues []struct {
				Agent string `json:"agent"`
				Tasks []struct {
					ID       string `json:"id"`
					QueueSeq int64  `json:"queue_seq"`
					State    string `json:"state"`
				} `json:"tasks"`
			} `json:"queues"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("task list --json is not JSON: %v\n%s", err, out)
	}
	doc := env.Data
	if doc.Tasks != 4 {
		t.Errorf("task count %d, want 4\n%s", doc.Tasks, out)
	}
	var main struct {
		Agent string `json:"agent"`
		Tasks []struct {
			ID       string `json:"id"`
			QueueSeq int64  `json:"queue_seq"`
			State    string `json:"state"`
		} `json:"tasks"`
	}
	found := false
	for _, q := range doc.Queues {
		if q.Agent == "main" {
			main, found = q, true
		}
	}
	if !found {
		t.Fatalf("no queue for the agent under test: %+v", doc.Queues)
	}
	if len(main.Tasks) != 4 || main.Tasks[0].QueueSeq != 1 || main.Tasks[3].QueueSeq != 4 {
		t.Errorf("rows: %+v", main.Tasks)
	}
}

// A queue that could not be READ is not an empty queue. This is the same
// mistake that let a release strand every instruction it was holding, asked
// again at the surface a person reads.
func TestTaskListNeverReportsAnUnreadableQueueAsEmpty(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.tasksFault = "ks_internal"
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "list", "--session", agentSessionShort)
	if code == 0 {
		t.Fatalf("an unreadable queue exited 0:\n%s%s", out, errs)
	}
	joined := out + errs
	for _, want := range []string{"could not be READ", "not known to be empty"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "no instructions") {
		t.Errorf("an unreadable queue was reported as empty:\n%s", joined)
	}
	// and the machine-readable form is ONE envelope, not a listing followed
	// by an error: two documents on stdout is a listing a parser would read
	// as whole.
	jout, jerr, jcode := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "list", "--session", agentSessionShort, "--json")
	if jcode == 0 {
		t.Fatalf("an unreadable queue exited 0 in --json:\n%s%s", jout, jerr)
	}
	dec := json.NewDecoder(strings.NewReader(jout))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, jout)
	}
	if _, ok := first["error"]; !ok {
		t.Errorf("--json did not answer with an error envelope:\n%s", jout)
	}
	var second map[string]any
	if err := dec.Decode(&second); err == nil {
		t.Errorf("--json printed a second document after the first:\n%s", jout)
	}
}

// One instruction in full, with its exact bytes quoted from the content
// store rather than from the task row, which holds only a reference.
func TestTaskShowQuotesTheExactInstructionAndNamesTheHold(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"instruction " + recoveryBlocking,
		"publish the release",
		"then tag it",
		"THIS instruction is what holds the queue",
		"queue position 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("task show lacks %q:\n%s", want, out)
		}
	}
	assertOnlyPublishedRoutes(t, c.seen())
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("a read verb sent a decision: %v", d)
	}
}

// C05's fourth column belongs to an independent verifier. An instruction
// that finished without one reads finished and NEVER verified, and this
// client does not derive the word from the state beside it.
func TestTaskShowNeverPromotesFinishedToVerified(t *testing.T) {
	_, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", "tsk_1", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "no independent verification") {
		t.Errorf("a finished instruction did not say it lacks verification:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "verified") && !strings.Contains(out, "never verified") {
		t.Errorf("a finished instruction was shown as verified:\n%s", out)
	}
}

// C03 asks for attempt history. This service records a current attempt and
// no history; where it records none, that is what is said. A plausible
// attempt rendered here would be read as a promise that `ks task resume`
// works, and that verb refuses precisely because no attempt exists.
func TestTaskShowInventsNoAttemptHistory(t *testing.T) {
	_, bin, cfg := recoveryFixture(t)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "records no attempt under this instruction") {
		t.Errorf("the absent attempt was not stated as absent:\n%s", out)
	}
	for _, invented := range []string{"attempt 1", "attempt 0", "attempt #"} {
		if strings.Contains(strings.ToLower(out), invented) {
			t.Errorf("an attempt was invented (%q):\n%s", invented, out)
		}
	}
}

// Content that could not be read is not an empty instruction.
func TestTaskShowNeverReportsUnreadableContentAsEmpty(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.contentGone = true
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	joined := out + errs
	if !strings.Contains(joined, "could not be READ") || !strings.Contains(joined, "not known to be empty") {
		t.Errorf("unreadable content was not stated as unreadable:\n%s", joined)
	}
}

// --session is a GUARD, not a lookup key: an instruction belonging to
// another session's agent is refused rather than shown.
func TestTaskShowRefusesAnotherSessionsInstruction(t *testing.T) {
	_, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", "tsk_elsewhere", "--session", agentSessionShort)
	if code == 0 {
		t.Fatalf("another session's instruction was shown:\n%s%s", out, errs)
	}
}

// Both verbs are gated on the capability the control plane publishes, and
// neither reaches a route when it is not available.
func TestTaskReadVerbsRespectTheCapabilityRegistry(t *testing.T) {
	for _, verb := range [][]string{
		{"task", "list", "--session", agentSessionShort},
		{"task", "show", recoveryBlocking, "--session", agentSessionShort},
	} {
		c, bin, cfg := recoveryFixture(t)
		c.mu.Lock()
		c.capability = "unavailable"
		c.mu.Unlock()
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), verb...)
		if code == 0 {
			t.Fatalf("%v ran against a control plane that does not offer it:\n%s%s", verb, out, errs)
		}
		if !strings.Contains(out+errs, "agent.workspace") {
			t.Errorf("%v did not name the capability it needs:\n%s%s", verb, out, errs)
		}
		for _, req := range c.seen() {
			if strings.Contains(req, "/api/v2/tasks") {
				t.Errorf("%v reached a task route despite the capability being unavailable: %q", verb, req)
			}
		}
	}
}

// A refused close is a condition an authorized person can END. It records
// that the outcome could NOT be established, and it can record nothing else:
// a person writing a verdict they did not measure is exactly what the
// service's refusal existed to prevent.
func TestTaskReconcileRecordsAnUnknownAndNeverAVerdict(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "reconcile", recoveryBlocking, "--session", agentSessionShort,
		"--finding", "checked the registry by hand: nothing was published")
	if code != 0 {
		t.Fatalf("task reconcile: exit %d\n%s%s", code, out, errs)
	}
	sent := c.sentReconciles()
	if len(sent) != 1 {
		t.Fatalf("reconciles sent: %v", sent)
	}
	for _, want := range []string{`"expected_revision"`, `"epoch"`, `"finding"`} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("the reconciliation was not bound by %s: %s", want, sent[0])
		}
	}
	for _, want := range []string{"reconciliation_required", "could NOT be established",
		"not a success and not a", "held behind it 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	// the STATE it reports is never a verdict. The prose may name both words
	// while saying it is neither; what must never happen is the recorded
	// state being one of them.
	if strings.Contains(out, "now reads succeeded") || strings.Contains(out, "now reads failed") {
		t.Errorf("a reconciliation reported a verdict:\n%s", out)
	}
	jout, _, jcode := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "reconcile", recoveryBlocking, "--session", agentSessionShort,
		"--finding", "checked by hand", "--json")
	if jcode != 0 {
		t.Fatalf("--json: exit %d\n%s", jcode, jout)
	}
	d := parseEnvelope(t, jout)["data"].(map[string]any)
	task := d["task"].(map[string]any)
	if task["state"] != "reconciliation_required" {
		t.Errorf("the recorded state is %v, not reconciliation_required", task["state"])
	}
	assertOnlyPublishedRoutes(t, c.seen())
}

// The record is worth nothing without what was established beside it, so the
// finding is required and nothing is sent without one.
func TestTaskReconcileRequiresAFindingAndSendsNothingWithoutOne(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "reconcile", recoveryBlocking, "--session", agentSessionShort)
	if code == 0 {
		t.Fatalf("a reconciliation with no finding succeeded:\n%s%s", out, errs)
	}
	if !strings.Contains(out+errs, "--finding is required") {
		t.Errorf("the refusal does not say what is missing:\n%s%s", out, errs)
	}
	if r := c.sentReconciles(); len(r) != 0 {
		t.Errorf("a reconciliation was sent with no finding: %v", r)
	}
}

// A close the service REFUSED records nothing against the instruction, and
// until a second refusal raises a hold it appears on no recovery surface at
// all. Somebody who cannot see it cannot act on it, so `ks task show` reads
// the journal the service already publishes and names the recovery action.
func TestTaskShowMakesARefusedCloseVisibleAndNamesTheRecoveryAction(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.refusedCloses = 1 })
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"refused closes 1", "still running and nothing was recorded",
		"not downgraded", "ks task reconcile " + recoveryBlocking} {
		if !strings.Contains(out, want) {
			t.Errorf("a refused close was not made visible (%q):\n%s", want, out)
		}
	}
}

// The attempt history is READ, not derived, and a retry is additive: the
// earlier attempt keeps its identity, its worker and its outcome.
func TestTaskShowReadsTheAttemptHistoryBack(t *testing.T) {
	_, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"attempts       1", "att_2", "index 1", "closed failed", "worker runner-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the attempt history lacks %q:\n%s", want, out)
		}
	}
}

// A journal that could not be READ is not a journal with no refusals in it.
func TestTaskShowNeverReadsAFailedJournalAsNoRefusals(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) { c.eventsFault = "ks_internal" })
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "journal could not be READ") || !strings.Contains(out, "not the same as none") {
		t.Errorf("an unreadable journal was not stated as unreadable:\n%s", out)
	}
	if strings.Contains(out, "refused closes none") {
		t.Errorf("an unreadable journal was reported as no refusals:\n%s", out)
	}
}

// A saved point is listed with what it actually covers. A reader must not be
// able to come away thinking one instruction is being rolled back.
func TestSessionCheckpointsSaysWhatARestoreActuallyCovers(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "checkpoints", agentSessionShort)
	if code != 0 {
		t.Fatalf("session checkpoints: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"SAVED POINT", "ckpt_newest", "valid", "ckpt_older", "superseded",
		"WHOLE-SESSION", "EVERY agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("the saved-point listing lacks %q:\n%s", want, out)
		}
	}
	assertOnlyPublishedRoutes(t, c.seen())
}

// The view C02 asks for is the JOURNAL, and the agent's conversation is
// published to it. A view that rendered those entries as a compacted JSON
// payload would make a person parse the thing the view exists to display.
//
// The words stay INERT throughout: they are the agent's, stored as data,
// and nothing here reads one as an instruction.
func TestTheAgentWindowRendersTheConversationRatherThanItsPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []string
		absent  []string
	}{
		{"what the agent said",
			`{"type":"agent.transcript","kind":"assistant_text","text":"I read the login flow and it uses a signed cookie."}`,
			[]string{"agent", "I read the login flow"}, []string{`"type"`, `"kind"`}},
		{"what a person asked for",
			`{"type":"agent.transcript","kind":"instruction","text":"explain the login flow"}`,
			[]string{"asked", "explain the login flow"}, []string{`"kind"`}},
		{"a tool starting",
			`{"type":"agent.transcript","kind":"tool_started","tool_name":"bash","text":"go test ./..."}`,
			[]string{"tool", "bash started", "go test"}, nil},
		{"a tool that failed",
			`{"type":"agent.transcript","kind":"tool_finished","tool_name":"bash","failed":true,"text":"exit 1"}`,
			[]string{"tool", "bash FAILED", "exit 1"}, nil},
		{"a tool nobody named",
			`{"type":"agent.transcript","kind":"tool_started","text":"something"}`,
			[]string{"an unnamed tool started"}, nil},
		{"an entry the service clipped",
			`{"type":"agent.transcript","kind":"assistant_text","text":"a long answer","clipped":true}`,
			[]string{"clipped by the service"}, nil},
		{"a kind this client does not know is shown, never dropped",
			`{"type":"agent.transcript","kind":"some_future_kind","text":"a new thing"}`,
			[]string{"some_future_kind", "a new thing"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := agentEventLine(journalEvent{StreamSeq: 7, ObservedAt: "2026-09-22T11:22:33Z",
				SubjectType: "agent", SubjectID: "agt_1", Payload: []byte(c.payload)})
			for _, w := range c.want {
				if !strings.Contains(line, w) {
					t.Errorf("the rendered line lacks %q: %s", w, line)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(line, a) {
					t.Errorf("the raw payload leaked into the view (%q): %s", a, line)
				}
			}
			if !strings.Contains(line, "11:22:33") {
				t.Errorf("the line lost its place in time: %s", line)
			}
		})
	}
}

// A multi-line answer is kept to one line in a live window; the rest is in
// the journal and in --json, which is where a reader goes for it. The line
// must say it was shortened rather than silently truncating.
func TestTheWindowShortensALongAnswerVisibly(t *testing.T) {
	line := agentEventLine(journalEvent{StreamSeq: 1, ObservedAt: "2026-09-22T00:00:00Z",
		Payload: []byte(`{"type":"agent.transcript","kind":"assistant_text","text":"first line\nsecond line\nthird"}`)})
	if !strings.Contains(line, "first line") || !strings.Contains(line, "…") {
		t.Errorf("a multi-line answer was not visibly shortened: %s", line)
	}
	if strings.Contains(line, "second line") {
		t.Errorf("the window printed more than one line for one entry: %s", line)
	}
}

// Everything that is not a conversation entry renders exactly as it did.
func TestNonTranscriptEventsRenderUnchanged(t *testing.T) {
	line := agentEventLine(journalEvent{StreamSeq: 3, ObservedAt: "2026-09-22T09:08:07Z",
		SubjectType: "approval", SubjectID: "apr_1",
		Payload: []byte(`{"type":"approval.requested","human_scope":"edit README.md"}`)})
	for _, want := range []string{"approval", "apr_1", "edit README.md", "!!"} {
		if !strings.Contains(line, want) {
			t.Errorf("an approval event lost %q: %s", want, line)
		}
	}
}

// The history line and the current-attempt line must not contradict each
// other. This client once said "it keeps no history of earlier ones" on the
// same screen that listed two attempts, because the sentence was written
// when the service kept none and was never revisited when it began to.
func TestTaskShowDoesNotDenyAHistoryItIsAboutToPrint(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.set(func(c *recoveryCtl) {
		c.attempts = append(c.attempts, map[string]any{
			"id": "att_9", "task_id": recoveryBlocking, "attempt_index": 2, "execution_epoch": 2,
			"dispatch_id": "dsp_9", "state": "intent", "evidence_required": true,
			"receipt_ids": []string{}, "retry_of": "att_2", "checkpoint_id": "ckpt_newest",
			"authorized_by": "hold_1"})
	})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task show: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "attempts       2") {
		t.Fatalf("the history was not printed:\n%s", out)
	}
	if strings.Contains(out, "keeps no history") || strings.Contains(out, "no history of earlier ones") {
		t.Errorf("the screen denies a history it prints two lines later:\n%s", out)
	}
}

// ---------------------------------------------------------------------
// KS-044: ks task cancel. The request is the easy half; the WORDING is the
// half that can lie, so most of what follows is about what is printed.
// ---------------------------------------------------------------------

// The route the client calls must be the one the service declares. This
// pins it, so a divergence between the two slices fails in the client's own
// suite rather than as a 404 during acceptance on the integrated candidate.
func TestTaskCancelUsesTheRouteTheRegistryPublishes(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	_, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("task cancel: exit %d", code)
	}
	if got := len(c.sentCancels()); got != 1 {
		t.Fatalf("cancels sent: %d, want exactly 1", got)
	}
	want := "POST /api/v2/tasks/" + recoveryBlocking + "/cancel"
	found := false
	for _, req := range c.seen() {
		if strings.HasPrefix(req, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("the client did not call %q; it called %v", want, c.seen())
	}
	assertOnlyPublishedRoutes(t, c.seen())
}

// Bound to what was read, and sent once. A cancellation prepared before a
// restore must not land after one, and a second request is a second chance
// to duplicate work.
func TestTaskCancelIsBoundToRevisionAndGenerationAndSentOnce(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	if _, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort); code != 0 {
		t.Fatalf("exit %d", code)
	}
	sent := c.sentCancels()
	if len(sent) != 1 {
		t.Fatalf("cancels sent: %v", sent)
	}
	for _, want := range []string{`"expected_revision"`, `"epoch"`} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("the cancellation was not bound by %s: %s", want, sent[0])
		}
	}
}

// `cancelling` means the stopping outcome is NOT known. The client must say
// so, and must never say a tool's or a provider's completed work was undone.
func TestTaskCancelWordsCancellingAsARequestAndNeverClaimsAnUndo(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.cancelState = "cancelling"
	c.cancelUnknown = []string{"2026-09-23T18:00:00Z"} // becomes signal_deadline
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"cancelling", "REQUESTED", "not established yet",
		"does not say the work stopped", "effects already in flight stay unresolved",
		"2026-09-23T18:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("a requested cancellation did not say %q:\n%s", want, out)
		}
	}
	// A substring check cannot tell a claim from its denial: the client's own
	// honest line is "nothing above was undone", which contains "was undone".
	// So each phrase is judged per LINE, and a line carrying a negator is the
	// disclaimer rather than the claim. The first version of this test failed
	// on the client's correct wording.
	for _, never := range []string{"was undone", "reversed", "rolled back", "the work stopped"} {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, never) {
				continue
			}
			negated := false
			for _, n := range []string{"nothing", "not ", "never", "does not", "no "} {
				if strings.Contains(strings.ToLower(line), n) {
					negated = true
					break
				}
			}
			if !negated {
				t.Errorf("the client claimed %q, which it cannot know:\n%s", never, line)
			}
		}
	}
}

// A completion that genuinely won the race is the outcome that stands. A
// false "cancelled" over real work is the worst answer available.
func TestTaskCancelDoesNotClaimACancellationItLost(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.cancelState = "succeeded"
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "the cancellation did not replace it") {
		t.Errorf("a lost race was not reported as the outcome that stands:\n%s", out)
	}
	if strings.Contains(out, "it will not run") {
		t.Errorf("the client called a completed instruction cancelled:\n%s", out)
	}
}

// Cancelling one instruction is not permission to continue the rest.
func TestTaskCancelLeavesTheHeldQueueHeldAndSaysSo(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"stay held", "not a queue", "ks agent queue resume"} {
		if !strings.Contains(out, want) {
			t.Errorf("cancelling did not say the held queue stays held (%q):\n%s", want, out)
		}
	}
	if d := c.sentDecisions(); len(d) != 0 {
		t.Errorf("cancelling sent a queue decision: %v", d)
	}
}

// No instruction named, nothing sent.
func TestTaskCancelRefusesWithoutAnInstructionAndSendsNothing(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", "--session", agentSessionShort)
	if code == 0 {
		t.Fatalf("a cancellation with no instruction succeeded:\n%s%s", out, errs)
	}
	if got := len(c.sentCancels()); got != 0 {
		t.Errorf("it sent %d cancellation(s) without an instruction", got)
	}
}

// KS-044 QA-044-2: a runner that ignores the interrupt. Once C04's interrupt
// wait has passed the service answers with explicit recovery actions, and
// the client must print every one of them -- and must still not say the work
// stopped, nor take any action on its own.
func TestTaskCancelPrintsTheRecoveryActionsForAnUnconfirmedStop(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.cancelState = "cancelling"
	c.cancelRecovery = map[string]any{
		"requested_at": "2026-09-26T10:00:00Z", "interrupt_wait_seconds": 10,
		"overdue_at": "2026-09-26T10:00:10Z", "overdue": true,
		"note": "the stop has not been confirmed within the 10 s interrupt wait; it is NOT stopped",
		"options": []map[string]any{
			{"action": "wait", "request": "GET /api/v2/tasks/" + recoveryBlocking, "effect": "keeps the stop request standing", "does_not": "does not claim anything stopped"},
			{"action": "stop_runtime", "request": "POST /api/v2/sessions/s/pause", "effect": "stops the machine", "does_not": "does not delete the session"},
			{"action": "record_unknown", "request": "POST /api/v2/tasks/" + recoveryBlocking + "/reconcile", "effect": "records reconciliation_required", "does_not": "does not record cancelled"},
		},
	}
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"NOT STOPPED", "10 s interrupt wait", "none is taken for you",
		"wait", "GET /api/v2/tasks/" + recoveryBlocking,
		"stop_runtime", "POST /api/v2/sessions/s/pause",
		"record_unknown", "POST /api/v2/tasks/" + recoveryBlocking + "/reconcile",
		"does not: does not record cancelled"} {
		if !strings.Contains(out, want) {
			t.Errorf("the recovery was not printed (%q missing):\n%s", want, out)
		}
	}
	if n := len(c.reconciles); n != 0 {
		t.Errorf("the client took a recovery action on its own: %d reconcile calls", n)
	}
	// and machine output carries the whole object
	jout, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("--json exit %d\n%s%s", code, jout, errs)
	}
	if !strings.Contains(jout, `"record_unknown"`) || !strings.Contains(jout, `"overdue":true`) {
		t.Errorf("--json does not carry the recovery object:\n%s", jout)
	}
}

// Inside the wait there is nothing to choose yet, and the client must not
// invent a menu.
func TestTaskCancelOffersNothingInsideTheInterruptWait(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.cancelState = "cancelling"
	c.cancelRecovery = map[string]any{"requested_at": "2026-09-26T10:00:00Z", "interrupt_wait_seconds": 10,
		"overdue_at": "2026-09-26T10:00:10Z", "overdue": false, "note": "pending", "options": []any{}}
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg),
		"task", "cancel", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "2026-09-26T10:00:10Z") || strings.Contains(out, "NOT STOPPED") || strings.Contains(out, "choose one") {
		t.Errorf("inside the interrupt wait the client must name the deadline and offer nothing:\n%s", out)
	}
}

// KS-044: the same recovery, read back later through ks task show (GET
// /api/v2/tasks/{id} carries cancel_recovery), with the ks command for each
// action; and recording the outcome unknown inside the interrupt wait is
// refused by the service and recorded nowhere (exit 5).
func TestTaskShowCarriesTheCancelRecoveryAndReconcileWaitsOutTheInterrupt(t *testing.T) {
	c, bin, cfg := recoveryFixture(t)
	c.mu.Lock()
	c.taskRecovery = map[string]any{
		"requested_at": "2026-09-26T10:00:00Z", "interrupt_wait_seconds": 10, "overdue_at": "2026-09-26T10:00:10Z", "overdue": true,
		"note": "the stop has not been confirmed within the 10 s interrupt wait",
		"options": []map[string]any{
			{"action": "stop_runtime", "request": "POST /api/v2/sessions/s/pause", "effect": "stops the machine", "does_not": "does not delete the session"},
			{"action": "record_unknown", "request": "POST /api/v2/tasks/" + recoveryBlocking + "/reconcile", "effect": "records reconciliation_required", "does_not": "does not record cancelled"},
		},
	}
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "show", recoveryBlocking, "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"NOT STOPPED", "ks: ks agent pause", "ks: ks task reconcile " + recoveryBlocking + " --session " + agentSessionShort} {
		if !strings.Contains(out, want) {
			t.Errorf("task show does not carry %q:\n%s", want, out)
		}
	}
	c.mu.Lock()
	c.notStuck = true
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "reconcile", recoveryBlocking, "--session", agentSessionShort, "--finding", "checked the registry")
	if code != exitConflict || !strings.Contains(errs, "interrupt wait has not passed") {
		t.Fatalf("reconcile inside the wait: exit %d\n%s", code, errs)
	}
}
