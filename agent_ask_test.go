// A person asks a connected adviser from the terminal: ks agent ask, and
// "@handle question" typed into a window. The handle names exactly one live
// connection (the adviser's name, or session/name when two connected
// agents share a name); a line that starts with @ is never sent to the
// agent as an instruction, and the reply is the adviser's own words,
// advice only and unverified.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAgentAskSendsAPersonConsultationAndWaitsForTheReply(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.consultWake = true // the adviser is paused: the question wakes it
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "ask", "main", "@reviewer is this approach sound?", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	bodies := append([]string(nil), c.consultBodies...)
	c.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("consultations sent: %v", bodies)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &body)
	if body["grant_id"] != "grant_rev" || body["question"] != "is this approach sound?" || body["request_id"] == "" || body["request_id"] == nil {
		t.Fatalf("the consultation body: %s", bodies[0])
	}
	for _, want := range []string{"asked reviewer for main", "waking the adviser", "reviewer's words, advice only, unverified",
		"Looks sound.", "Two notes:", "- keep the tests", "- name the rollback point"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s%s", want, out, errs)
		}
	}
	if c.sawRequest("POST /api/v2/agents/agt_main0001/tasks") {
		t.Error("a question to an adviser was sent to the agent as an instruction")
	}
	// --json: one object per line, the last the consultation itself
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "ask", "main", "@billing/ops is the invoice right?", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("--json: exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last map[string]any
	for _, l := range lines {
		var doc map[string]any
		if err := json.Unmarshal([]byte(l), &doc); err != nil {
			t.Fatalf("line is not JSON: %v (%q)", err, l)
		}
		last = doc
	}
	if last["state"] != "replied" || last["response"] == nil {
		t.Fatalf("the last line is not the replied consultation: %v", last)
	}
	c.mu.Lock()
	second := c.consultBodies[1]
	c.mu.Unlock()
	if !strings.Contains(second, `"grant_id":"grant_ops1"`) {
		t.Fatalf("session/name did not pick the one adviser: %s", second)
	}
}

func TestAgentAskRefusesAHandleThatNamesNobodyOrTwoAndNeverInstructs(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	dir := t.TempDir()
	for _, tc := range []struct{ line, want string }{
		{"@nobody hello", "no connected agent is called @nobody"},
		{"@ops hello", "two connected agents are called ops"},
		{"@reviewer", "write the question after @reviewer"},
		{"hello there", "starts with its handle"},
	} {
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "ask", "main", tc.line, "--session", agentSessionShort)
		if code != exitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%q: exit %d, want %q in:\n%s", tc.line, code, tc.want, errs)
		}
		if (tc.line == "@nobody hello" || tc.line == "@ops hello") && !strings.Contains(errs, "@billing/ops") {
			t.Errorf("%q: the known handles are not listed:\n%s", tc.line, errs)
		}
	}
	if c.sawRequest("POST /api/v2/agents/agt_main0001/consultations") || c.sawRequest("POST /api/v2/agents/agt_main0001/tasks") {
		t.Errorf("a refused line was sent: %v", c.seen())
	}
}

// In a window, a line that starts with @ goes to the connected adviser it
// names, and never to the agent as an instruction; one that names no
// adviser is refused with the known handles.
func TestAgentWindowRoutesAnAtLineToTheAdviserNeverToTheAgent(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.mu.Unlock()
	w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
	waitUntil(t, "an event", func() bool { return strings.Contains(w.so.String(), "42") })
	w.typeLine(t, "@nobody is this safe?")
	w.waitFor(t, "no connected agent is called @nobody")
	w.typeLine(t, "@reviewer is this safe?")
	w.waitFor(t, "asked reviewer for main")
	w.waitFor(t, "reviewer's words, advice only, unverified")
	w.waitFor(t, "- name the rollback point")
	if c.sawRequest("POST /api/v2/agents/agt_main0001/tasks") {
		t.Errorf("an @ line reached the agent as an instruction: %v", c.seen())
	}
	if !c.sawRequest("POST /api/v2/agents/agt_main0001/consultations") {
		t.Errorf("the question was not sent as a consultation: %v", c.seen())
	}
	w.detach(t)
}

// askToInterrupt starts ks agent ask against a fake whose consultation keeps
// waiting, and returns once the question has been asked: the process, what
// it printed, and the env it ran with. The wait bound is long, so only an
// interrupt ends the wait.
func askToInterrupt(t *testing.T, c *agentCtl, bin, cfg string, extraEnv []string, extraArgs ...string) (*exec.Cmd, *lockedBuf, *lockedBuf) {
	t.Helper()
	c.mu.Lock()
	c.consultHold = true
	c.mu.Unlock()
	args := append([]string{"agent", "ask", "main", "@reviewer is this approach sound?", "--session", agentSessionShort}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(append(append(os.Environ(), fastEnv(cfg)...), "KS_WAIT_TIMEOUT_MS=60000"), extraEnv...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	fixtureBarrier()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitUntil(t, "the question to be asked", func() bool {
		return strings.Contains(so.String(), "asked reviewer for main") || strings.Contains(so.String(), `"type":"asked"`)
	})
	return cmd, &so, &se
}

// exitOf waits for the process and answers its exit code, failing the test
// when it does not end within the bound.
func exitOf(t *testing.T, cmd *exec.Cmd, bound time.Duration, se *lockedBuf) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		fixtureBarrier()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatalf("wait: %v\n%s", err, se.String())
		}
		return 0
	case <-time.After(bound):
		_ = cmd.Process.Kill()
		t.Fatalf("ks agent ask did not end within %s\n%s", bound, se.String())
		return -1
	}
}

// Interrupting ks agent ask while it waits cancels the question, with the
// request the console's Cancel sends, and exits as an interrupted command.
// SIGTERM does the same.
func TestAgentAskInterruptCancelsTheQuestion(t *testing.T) {
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		c, bin, cfg := agentFixture(t)
		cmd, so, se := askToInterrupt(t, c, bin, cfg, nil)
		if err := cmd.Process.Signal(s); err != nil {
			t.Fatal(err)
		}
		if code := exitOf(t, cmd, 15*time.Second, se); code != exitInterrupt {
			t.Errorf("%v: exit %d, want %d\n%s%s", s, code, exitInterrupt, so.String(), se.String())
		}
		if !strings.Contains(so.String(), "cancelled the question to reviewer (consultation csl_0001): the request is closed, a reply to it is refused") {
			t.Errorf("%v: no cancelled line:\n%s%s", s, so.String(), se.String())
		}
		c.mu.Lock()
		bodies := append([]string(nil), c.cancelBodies...)
		c.mu.Unlock()
		if len(bodies) != 1 || !strings.Contains(bodies[0], `"reason":"cancelled from the terminal (interrupted)"`) {
			t.Errorf("%v: cancel requests: %v", s, bodies)
		}
		if c.sawRequest("POST /api/v2/agents/agt_main0001/tasks") {
			t.Errorf("%v: the interrupt sent the agent an instruction", s)
		}
	}
	// --json: the outcome is one line in the shape of the other ask lines
	c, bin, cfg := agentFixture(t)
	cmd, so, se := askToInterrupt(t, c, bin, cfg, nil, "--json")
	_ = cmd.Process.Signal(os.Interrupt)
	if code := exitOf(t, cmd, 15*time.Second, se); code != exitInterrupt {
		t.Errorf("--json: exit %d\n%s", code, so.String())
	}
	lines := strings.Split(strings.TrimSpace(so.String()), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("the last line is not JSON: %v\n%s", err, so.String())
	}
	if last["type"] != "cancelled" || last["consultation"] != "csl_0001" || last["state"] != "cancelled" {
		t.Errorf("--json: the outcome line: %v", last)
	}
}

// A reply that arrived before the cancel is not cancelled, and the line
// says so and where to read it.
func TestAgentAskInterruptAfterTheReplyCancelsNothing(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.cancelMode = "replied"
	c.mu.Unlock()
	cmd, so, se := askToInterrupt(t, c, bin, cfg, nil)
	_ = cmd.Process.Signal(os.Interrupt)
	if code := exitOf(t, cmd, 15*time.Second, se); code != exitInterrupt {
		t.Errorf("exit %d\n%s%s", code, so.String(), se.String())
	}
	if !strings.Contains(so.String(), "the reply had already arrived, so nothing was cancelled (consultation csl_0001); ks advice show csl_0001") {
		t.Errorf("no already-replied line:\n%s%s", so.String(), se.String())
	}
	if strings.Contains(so.String(), "cancelled the question") {
		t.Errorf("a refused cancel was reported as done:\n%s", so.String())
	}
}

// A cancel the control plane refused leaves the question standing, and one
// that failed in a way that says nothing certain may or may not have
// arrived: each line says which, and neither claims a cancel.
func TestAgentAskInterruptSaysWhenTheCancelDidNotHappen(t *testing.T) {
	for mode, want := range map[string]string{
		"refused": "the cancel of consultation csl_0001 was refused (no such consultation (ks_not_found)): the question stands, and reviewer may still answer it; ks advice show csl_0001",
		"error":   "the cancel of consultation csl_0001 could not be confirmed (",
	} {
		c, bin, cfg := agentFixture(t)
		c.mu.Lock()
		c.cancelMode = mode
		c.mu.Unlock()
		cmd, so, se := askToInterrupt(t, c, bin, cfg, nil)
		_ = cmd.Process.Signal(os.Interrupt)
		if code := exitOf(t, cmd, 15*time.Second, se); code != exitInterrupt {
			t.Errorf("%s: exit %d\n%s%s", mode, code, so.String(), se.String())
		}
		if !strings.Contains(so.String(), want) {
			t.Errorf("%s: want %q in:\n%s%s", mode, want, so.String(), se.String())
		}
		if mode == "error" && !strings.Contains(so.String(), "it may not have arrived, and reviewer may still answer the question") {
			t.Errorf("error: the unconfirmed line does not say the question may still be answered:\n%s", so.String())
		}
		if strings.Contains(so.String(), "cancelled the question") {
			t.Errorf("%s: a cancel that did not happen was reported as done:\n%s", mode, so.String())
		}
	}
}

// A second interrupt while the cancel is in flight stops at once, saying
// the cancel's outcome is not known; it never waits out the request.
func TestAgentAskSecondInterruptStopsAtOnce(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	c.mu.Lock()
	c.cancelMode, c.cancelHang = "hang", hang
	c.mu.Unlock()
	// a long request bound, so only the second interrupt can end the wait
	cmd, so, se := askToInterrupt(t, c, bin, cfg, []string{"KS_HTTP_TIMEOUT_MS=60000"})
	_ = cmd.Process.Signal(os.Interrupt)
	waitUntil(t, "the cancel to be sent", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.cancelBodies) == 1
	})
	start := time.Now()
	_ = cmd.Process.Signal(os.Interrupt)
	if code := exitOf(t, cmd, 5*time.Second, se); code != exitInterrupt {
		t.Errorf("exit %d\n%s%s", code, so.String(), se.String())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the second interrupt took %s to stop the command", d)
	}
	if !strings.Contains(so.String(), "stopped without waiting for the cancel's answer: whether consultation csl_0001 was cancelled is not known") {
		t.Errorf("no abandoned line:\n%s%s", so.String(), se.String())
	}
}
