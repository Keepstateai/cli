// The console's words beside the raw state, wherever a session or agent
// state is printed: Running, Paused, Saving, Starting, Ended, Failed, Needs
// recovery, Status unknown; and the list's final Ended group.
package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStateWordsRenderTheConsolesWordBesideTheRawState(t *testing.T) {
	for _, c := range []struct{ kind, raw, want string }{
		{"session_runtime", "running", "Running (running)"},
		{"session_runtime", "checkpointing", "Running (checkpointing)"},
		{"session_runtime", "parked", "Paused (parked)"},
		{"session_runtime", "stopping", "Saving (stopping)"},
		{"session_runtime", "provisioning", "Starting (provisioning)"},
		{"session_runtime", "resuming", "Starting (resuming)"},
		{"session_runtime", "dead", "Ended (dead)"},
		{"session_runtime", "deleting", "Ended (deleting)"},
		{"session_runtime", "deleted", "Ended (deleted)"},
		{"session_runtime", "failed", "Failed (failed)"},
		{"session_runtime", "recovery_required", "Needs recovery (recovery_required)"},
		{"session_runtime", "", "Status unknown (unavailable)"},
		{"session_runtime", "weird", "Status unknown (weird)"},
		{"agent_activity", "working", "Working (working)"},
		{"agent_activity", "ready", "Ready (ready)"},
		{"agent_activity", "idle", "Ready (idle)"},
		{"agent_activity", "waiting_approval", "Waiting for approval (waiting_approval)"},
		{"agent_activity", "waiting_provider", "Waiting for provider (waiting_provider)"},
		{"agent_activity", "running_tool", "Running tool (running_tool)"},
		{"agent_activity", "waiting_advice", "Waiting for advice (waiting_advice)"},
		{"agent_activity", "paused", "Paused (paused)"},
		{"agent_activity", "stopped", "Paused (stopped)"},
		{"agent_activity", "failed", "Failed (failed)"},
		{"agent_activity", "recovery_required", "Needs recovery (recovery_required)"},
		{"agent_activity", "starting", "Starting (starting)"},
		{"agent_activity", "", "Status unknown (unavailable)"},
		{"agent_activity", "dancing", "Status unknown (dancing)"},
	} {
		if got := stateWords(c.kind, c.raw); got != c.want {
			t.Errorf("stateWords(%s, %q) = %q, want %q", c.kind, c.raw, got, c.want)
		}
	}
}

// The home list says Paused for an agent whose session is parked, never its
// frozen activity, and the console's word beside the raw state everywhere.
func TestAppHomeSaysPausedForAParkedAgent(t *testing.T) {
	parked := homeRow{sess: inventoryRow{ShortID: "5335fa", Name: "api", RuntimeState: "parked"}, agent: &agentRow{Name: "dev1", Activity: "working"}}
	if _, word, _ := homeState(parked); word != "Paused (parked)" {
		t.Errorf("a parked agent reads %q", word)
	}
	line := words(homeLine(parked, false, 100))
	if strings.Contains(line, "Working") || !strings.Contains(line, "Paused (parked)") {
		t.Errorf("home line: %q", line)
	}
	frozen := homeRow{sess: inventoryRow{ShortID: "5335fa", RuntimeState: "running"}, agent: &agentRow{Name: "dev1", Activity: "working", SessionRuntime: &sessionRuntimeDoc{State: "parked"}}}
	if _, word, _ := homeState(frozen); word != "Paused (parked)" {
		t.Errorf("an agent whose own read says its session is parked reads %q", word)
	}
	running := homeRow{sess: inventoryRow{ShortID: "3f2a1c", RuntimeState: "running"}, agent: &agentRow{Name: "main", Activity: "working"}}
	if _, word, _ := homeState(running); word != "Working (working)" {
		t.Errorf("a working agent reads %q", word)
	}
	ended := homeRow{sess: inventoryRow{ShortID: "3f2a1c", RuntimeState: "dead"}, agent: &agentRow{Name: "main", Activity: "ready"}}
	if _, word, _ := homeState(ended); word != "Ended (dead)" {
		t.Errorf("an ended session's agent reads %q", word)
	}
}

// ks session list shows ended sessions in a final Ended group by default,
// with the console's word beside each state, within 80 columns.
func TestSessionListGroupsEndedSessionsLast(t *testing.T) {
	c := &inventoryCtl{n: 4, dead: 2}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS=80"), "session", "list")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"Running (running)", "Paused (parked)", "\nEnded", "Ended (dead)", "6 session(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, l := range lines[:len(lines)-2] { // the table; the two closing hints are prose
		if len([]rune(l)) > 80 {
			t.Errorf("line wider than 80 columns: %q", l)
		}
	}
	ended := strings.Index(out, "\nEnded")
	if ended < strings.LastIndex(out, "Running (running)") || ended > strings.Index(out, "Ended (dead)") {
		t.Errorf("the Ended group is not last:\n%s", out)
	}
	// a state filter is one table, no group
	out, _, _ = auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS=80"), "session", "list", "--state", "running")
	if strings.Contains(out, "\nEnded") || strings.Contains(out, "(dead)") {
		t.Errorf("a filtered list carries the Ended group:\n%s", out)
	}
	out, _, _ = auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS=80"), "session", "list", "--state", "dead")
	if !strings.Contains(out, "Ended (dead)") || strings.Contains(out, "\nEnded\n") {
		t.Errorf("--state dead: %s", out)
	}
}
