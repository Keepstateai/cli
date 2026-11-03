// The herdr bridge: inside a herdr pane a stream reports its agent's state
// with herdr pane report-agent and releases the pane on leaving; outside
// herdr it runs nothing. A fake herdr records each call's arguments.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHerdr writes a herdr that appends its arguments (one call per line,
// arguments separated by |) to a log, after sleeping for the seconds in
// its delay file.
func fakeHerdr(t *testing.T) (bin, log, delay string) {
	t.Helper()
	dir := t.TempDir()
	bin, log, delay = filepath.Join(dir, "herdr"), filepath.Join(dir, "calls.log"), filepath.Join(dir, "delay")
	script := "#!/bin/sh\nd=$(cat " + delay + " 2>/dev/null)\n[ -n \"$d\" ] && sleep \"$d\"\n" +
		"line=\"\"\nfor a in \"$@\"; do line=\"$line|$a\"; done\nprintf '%s\\n' \"$line\" >> " + log + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log, delay
}

// herdrVerb keeps the calls of one herdr verb ("report-agent",
// "report-metadata", "release-agent").
func herdrVerb(calls []string, verb string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "|pane|"+verb+"|") {
			out = append(out, c)
		}
	}
	return out
}

func herdrCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func inHerdr(t *testing.T, bin string) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "p_7")
	t.Setenv("HERDR_BIN_PATH", bin)
}

func herdrSeq(c string) string {
	_, after, _ := strings.Cut(c, "|--seq|")
	return strings.SplitN(after, "|", 2)[0]
}

func TestHerdrBridgeRunsNothingOutsideHerdr(t *testing.T) {
	bin, log, _ := fakeHerdr(t)
	for _, env := range [][3]string{{"", "p_7", bin}, {"1", "", bin}, {"1", "p_7", ""}} {
		t.Setenv("HERDR_ENV", env[0])
		t.Setenv("HERDR_PANE_ID", env[1])
		t.Setenv("HERDR_BIN_PATH", env[2])
		h := newHerdrReporter("main", nil)
		h.report("working", "", "Working.")
		h.release()
		if h != nil {
			t.Errorf("%v: a reporter outside herdr", env)
		}
	}
	if calls := herdrCalls(t, log); len(calls) != 0 {
		t.Errorf("herdr was run: %v", calls)
	}
}

func TestHerdrBridgeMapsStatesAndSendsTheResumeCommand(t *testing.T) {
	bin, log, _ := fakeHerdr(t)
	inHerdr(t, bin)
	for _, tc := range []struct{ state, kind, msg, want, label string }{
		{"working", "", "Working.", "|--state|working|--message|Working.|", "working=Working"},
		{"blocked", "permission", "Run: npm test", "|--state|blocked|--message|Needs you: permission. Run: npm test|", "blocked=Needs you: permission"},
		{"blocked", "question", "Held.", "|--state|blocked|--message|Needs you: a question. Held.|", "blocked=Needs you: a question"},
		{"blocked", "auth", "Choose a key.", "|--state|blocked|--message|Needs you: sign-in or key. Choose a key.|", "blocked=Needs you: sign-in or key"},
		{"blocked", "", "Paused: out of credit.", "|--state|blocked|--message|Needs you. Paused: out of credit.|", "blocked=Needs you"},
		{"error", "", "The runner failed.", "|--state|blocked|--message|Error: The runner failed.|", "blocked=Error"},
		{"done", "", "Finished.", "|--state|idle|--message|Finished.|", "idle=Done"},
		{"idle", "", "Ready.", "|--state|idle|--message|Ready.|", "idle=Idle"},
	} {
		_ = os.Remove(log)
		h := newHerdrReporter("main", agentResumeArgv("main", "agentsession00"))
		h.report(tc.state, tc.kind, tc.msg)
		waitUntil(t, "the report and its label", func() bool { return len(herdrCalls(t, log)) == 2 })
		got := herdrCalls(t, log)[0]
		if !strings.HasPrefix(got, "|pane|report-agent|p_7|--source|keepstate|--agent|main (KeepState)|--state|") ||
			!strings.Contains(got, tc.want) || !strings.HasSuffix(got, "|--|ks|agent|open|main|--session|agentsession00") || !strings.Contains(got, "|--seq|") {
			t.Errorf("%s/%s: %s", tc.state, tc.kind, got)
		}
		// herdr shows no --message; the sidebar's words go as the state's label
		label := herdrCalls(t, log)[1]
		if !strings.HasPrefix(label, "|pane|report-metadata|p_7|--source|keepstate|--agent|main (KeepState)|--state-label|"+tc.label+"|--seq|") {
			t.Errorf("%s/%s label: %s", tc.state, tc.kind, label)
		}
		h.release()
		calls := herdrCalls(t, log)
		if len(calls) != 4 || !strings.HasPrefix(calls[2], "|pane|release-agent|p_7|--source|keepstate|--agent|main (KeepState)|--seq|") ||
			!strings.HasPrefix(calls[3], "|pane|report-metadata|p_7|--source|keepstate|--agent|main (KeepState)|--clear-state-labels|--seq|") {
			t.Errorf("release: %v", calls)
		}
	}
	// a name that is not safe on a command line is reported without a resume command
	if argv := agentResumeArgv("it's mine", "s"); argv != nil {
		t.Errorf("resume for an unsafe name: %v", argv)
	}
}

func TestHerdrBridgeSendsOnlyTheNewestReportAndNeverBlocks(t *testing.T) {
	bin, log, delay := fakeHerdr(t)
	inHerdr(t, bin)
	if err := os.WriteFile(delay, []byte("0.6"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHerdrReporter("main", nil)
	start := time.Now()
	h.report("working", "", "one")
	time.Sleep(150 * time.Millisecond) // "one" is now running in herdr
	for _, m := range []string{"two", "three", "four", "five"} {
		h.report("working", "", m)
	}
	if d := time.Since(start); d > 350*time.Millisecond {
		t.Errorf("reporting waited for herdr: %s", d)
	}
	waitUntil(t, "two reports", func() bool { return len(herdrVerb(herdrCalls(t, log), "report-agent")) == 2 })
	time.Sleep(800 * time.Millisecond)
	calls := herdrVerb(herdrCalls(t, log), "report-agent")
	if len(calls) != 2 || !strings.Contains(calls[0], "|one|") || !strings.Contains(calls[1], "|five|") {
		t.Errorf("calls %v, want the first and the newest", calls)
	}
	// every seq rises across both verbs: the label of a report never sorts before it
	all := herdrCalls(t, log)
	for i := 1; i < len(all); i++ {
		if a, b := herdrSeq(all[i-1]), herdrSeq(all[i]); !(len(b) > len(a) || len(b) == len(a) && b > a) {
			t.Errorf("seq did not rise: %s then %s", all[i-1], all[i])
		}
	}
	// seq always rises
	seq := func(c string) string {
		_, after, _ := strings.Cut(c, "|--seq|")
		return strings.SplitN(after, "|", 2)[0]
	}
	if len(calls) == 2 && !(len(seq(calls[1])) >= len(seq(calls[0])) && seq(calls[1]) > seq(calls[0])) {
		t.Errorf("seq did not rise: %s then %s", seq(calls[0]), seq(calls[1]))
	}
	h.release()

	// a herdr that hangs: reports still return at once, and leaving waits
	// at most the time limit
	if err := os.WriteFile(delay, []byte("30"), 0o644); err != nil {
		t.Fatal(err)
	}
	h = newHerdrReporter("main", nil)
	start = time.Now()
	h.report("working", "", "x")
	h.report("blocked", "permission", "y")
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("a hanging herdr blocked a report: %s", d)
	}
	start = time.Now()
	h.release()
	if d := time.Since(start); d > herdrTimeout+time.Second {
		t.Errorf("leaving waited %s for a hanging herdr", d)
	}
}

func TestAgentOpenInsideHerdrReportsAndReleasesThePane(t *testing.T) {
	hbin, log, _ := fakeHerdr(t)
	c, bin, cfg := statusFixture(t, openFrames, "")
	c.gap = 150 * time.Millisecond // a live stream: herdr hears each change before the next
	env := append(fastEnv(cfg), "HERDR_ENV=1", "HERDR_PANE_ID=p_7", "HERDR_BIN_PATH="+hbin)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), env, "agent", "open", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	calls := herdrCalls(t, log)
	if len(calls) < 4 || !strings.HasPrefix(calls[len(calls)-2], "|pane|release-agent|p_7|") ||
		!strings.HasPrefix(calls[len(calls)-1], "|pane|report-metadata|p_7|--source|keepstate|--agent|main (KeepState)|--clear-state-labels|") {
		t.Fatalf("calls %v", calls)
	}
	for _, c := range calls[:len(calls)-2] {
		if !strings.HasPrefix(c, "|pane|report-agent|p_7|--source|keepstate|--agent|main (KeepState)|") &&
			!strings.HasPrefix(c, "|pane|report-metadata|p_7|--source|keepstate|--agent|main (KeepState)|--state-label|") {
			t.Errorf("call %s", c)
		}
		if strings.Contains(c, "\x1b") {
			t.Errorf("an escape reached herdr: %q", c)
		}
	}
	// stdout is not a terminal here, so no OSC was written; herdr still heard
	if strings.Contains(stdout, "]7501") {
		t.Errorf("OSC written to a pipe: %q", stdout)
	}
}
