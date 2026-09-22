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
