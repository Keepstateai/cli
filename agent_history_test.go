// The conversation of a paused or ended agent, and the tail of a long one.
// ks agent history prints the journal's conversation in any session state,
// with the whole text of every entry; a window's backlog is the END of the
// journal, read by paging forward to it, not the oldest page.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func transcriptRow(kind, text string, extra map[string]any) map[string]any {
	p := map[string]any{"type": "agent.transcript", "kind": kind, "text": text}
	for k, v := range extra {
		p[k] = v
	}
	return map[string]any{"event_id": "ev_" + kind, "subject_type": "agent", "subject_id": "agt_main0001", "observed_at": "2026-09-22T11:05:00Z", "payload": p}
}

func TestAgentHistoryPrintsTheWholeConversationInAnySessionState(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.stopAfter = -1 // the session reads stopping, then parked: not running, and never woken here
	c.journal = []map[string]any{
		transcriptRow("instruction", "run the tests\nthen lint everything", map[string]any{"task_id": "tsk_1", "author_type": "human", "author_id": "acct_me"}),
		transcriptRow("assistant_text", "On it.\nSecond line of the answer.", nil),
		transcriptRow("tool_started", "go test ./...", map[string]any{"tool_name": "Bash"}),
		transcriptRow("tool_finished", "ok", map[string]any{"tool_name": "Bash"}),
		transcriptRow("consultation", "is the migration reversible?", map[string]any{"consultation_id": "csl_9", "author_type": "agent", "author_id": "agt_rev00001"}),
		transcriptRow("advice", "yes, if you keep the down script", map[string]any{"consultation_id": "csl_9", "author_type": "adviser", "author_id": "agt_rev00001"}),
		{"event_id": "ev_model", "subject_type": "session", "subject_id": agentSessionRecord, "observed_at": "2026-09-22T11:06:00Z",
			"payload": map[string]any{"type": "session.model-changed", "from": "claude-haiku-4-5-20251001", "to": "claude-sonnet-5", "actor": "acct_me", "in_effect": "from the next resume"}},
		{"event_id": "ev_other", "subject_type": "agent", "subject_id": "agt_twin0001", "observed_at": "2026-09-22T11:07:00Z",
			"payload": map[string]any{"type": "agent.transcript", "kind": "assistant_text", "text": "another agent's words"}},
		{"event_id": "ev_park", "subject_type": "session", "subject_id": agentSessionRecord, "observed_at": "2026-09-22T11:08:00Z",
			"payload": map[string]any{"type": "session.parked", "runtime_state": "parked", "reason": ""}},
	}
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "history", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"you", "run the tests", "then lint everything", "main", "On it.", "Second line of the answer.",
		"Bash started", "agt_rev00001 asked: is the migration reversible?", "advice from agt_rev00001", "yes, if you keep the down script",
		"model changed from claude-haiku-4-5-20251001 to claude-sonnet-5", "from the next resume", "paused"} {
		if !strings.Contains(out, want) {
			t.Errorf("the history lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "another agent's words") {
		t.Errorf("another agent's words are in this agent's history:\n%s", out)
	}
	// newest last: the instruction before the answer, the answer before the advice
	if !(strings.Index(out, "run the tests") < strings.Index(out, "On it.") && strings.Index(out, "On it.") < strings.Index(out, "yes, if you keep")) {
		t.Errorf("the history is not oldest first:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("an entry was cut to its first line:\n%s", out)
	}
	if c.sawRequest("POST ") || c.sawRequest("GET /api/v2/sessions/"+agentSessionRecord+"/events/stream") {
		t.Errorf("a read of the history sent a mutation or opened a stream: %v", c.seen())
	}
	// --last keeps the end
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "history", "main", "--session", agentSessionShort, "--last", "2")
	if code != 0 || strings.Contains(out, "run the tests") || !strings.Contains(out, "paused") || !strings.Contains(out, "model changed") {
		t.Errorf("--last 2: exit %d\n%s", code, out)
	}
	// --json: one journal row per line
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "history", "main", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("--json: exit %d\n%s", code, out)
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var doc map[string]any
		if err := json.Unmarshal([]byte(l), &doc); err != nil || doc["stream_seq"] == nil {
			t.Errorf("line is not a journal row: %v (%q)", err, l)
		}
	}
}

// The journal is long and the window resumes near its end: the backlog is
// the last entries BEFORE the resume point, read by paging forward to it,
// never the oldest page's tail.
func TestTheWindowBacklogIsTheTailOfALongJournal(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.backlog, c.resumeAt = 1200, 1150
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort)
	joined := out + errs
	if code != 0 && !strings.Contains(joined, "what already happened") {
		t.Fatalf("agent open: exit %d\n%s", code, joined)
	}
	for _, want := range []string{"an earlier line 1149", "an earlier line 1120", "1120 earlier entries are in the journal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the backlog is not the journal's tail (%q):\n%s", want, joined)
		}
	}
	for _, not := range []string{"an earlier line 399\n", "an earlier line 1119\n", "an earlier line 1150"} {
		if strings.Contains(joined, not) {
			t.Errorf("the backlog shows %q:\n%s", strings.TrimSpace(not), joined)
		}
	}
	c.mu.Lock()
	pages := c.eventPages
	c.mu.Unlock()
	if pages < 2 {
		t.Errorf("the journal was read in %d page(s); a long journal is paged forward to the resume point", pages)
	}
}
