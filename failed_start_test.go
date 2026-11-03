// A start whose machine was never made: the control plane marks the
// session failed and keeps its name, so the same command again would be
// refused. The run says what happened in the service's words and gives one
// way on: delete that session, then start again. It never says "run it
// again" on its own, and sends no instruction.
package main

import (
	"strings"
	"testing"
)

// failedStartNote is the control plane's own answer when no machine could
// be created for the session.
const failedStartNote = "No machine could be created for this session, so nothing ran and nothing is billed. The session is marked failed; delete it, then start it again"

func TestAgentRunAFailedStartSaysDeleteItThenStartAgain(t *testing.T) {
	c := newInteropCtl()
	c.noMachine = true
	_, _, run := interopEnv(t, c)
	_, errs, code := run(nil, "agent", "run", "--no-parent", "--name", "helper", "list the open TODOs")
	if code != exitFailed {
		t.Fatalf("exit %d, want %d\n%s", code, exitFailed, errs)
	}
	if !strings.Contains(errs, "nothing ran and nothing is billed") {
		t.Errorf("the service's words are not said:\n%s", errs)
	}
	if !strings.Contains(errs, "Next: ks session delete session_1, then ks agent run --name helper") {
		t.Errorf("the one way on is not delete, then start again:\n%s", errs)
	}
	if strings.Contains(errs, "ks agent run again") || strings.Contains(errs, "ks doctor") {
		t.Errorf("a failed start still says to run it again:\n%s", errs)
	}
	if c.count("POST /api/v2/agents/agent_1/tasks") != 0 {
		t.Errorf("an instruction was sent to an agent that never started")
	}
	// the same in --json: the next action a script reads
	out, _, code := run(nil, "agent", "run", "--no-parent", "--name", "helper2", "t", "--json")
	if code != exitFailed || !strings.Contains(out+errs, "ks session delete session_2, then ks agent run --name helper2") {
		t.Errorf("--json: exit %d\n%s", code, out)
	}
}

func TestRunAgentAFailedStartSaysDeleteItThenStartAgain(t *testing.T) {
	prov := map[string]any{"session_id": "session_abc123", "agent_id": "agent_main1", "ready": false, "supervised": false, "agent_activity": "unavailable", "cleaned_up": false,
		"note":   failedStartNote,
		"phases": []any{map[string]any{"phase": "create_guest", "done": false, "detail": "no machine could be created for this session"}}}
	c := &runCtl{workspace: "available", prov: prov, record: map[string]any{"id": "session_abc123", "name": "checkout", "runtime_state": "failed", "revision": 3}}
	syncFixture(t, &c.mu)
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--name", "checkout", "--task", "x")
	if code != exitFailed {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, exitFailed, out, errOut)
	}
	if !strings.Contains(errOut, "nothing ran and nothing is billed") || !strings.Contains(errOut, "Next: ks session delete session_abc123, then ks run --agent --name checkout") {
		t.Errorf("the failed start's words and its one way on:\n%s", errOut)
	}
	if strings.Contains(errOut, "to try again") || strings.Contains(errOut, "ks doctor") {
		t.Errorf("a failed start still says to try again:\n%s", errOut)
	}
	if c.asked("POST /api/v2/agents/") {
		t.Errorf("a task was submitted to an agent that never started")
	}
}
