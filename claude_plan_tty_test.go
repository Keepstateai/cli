//go:build unix

package main

// The sign-in on a real terminal: the binary runs under script(1), which
// gives it a pseudo-terminal for standard input and output, with a stand-in
// claude first on PATH (or none). What the person would have seen is the
// bytes script(1) captured.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ptyStep writes input to the terminal once the terminal has shown waitFor
// (or at once when waitFor is empty), after delay.
type ptyStep struct {
	waitFor string
	delay   time.Duration
	input   string
}

type ptyOutBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *ptyOutBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *ptyOutBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// inPTYSteps runs bin under script(1) at 80 columns, feeds the steps in order,
// and returns what the terminal received and the exit code.
func inPTYSteps(t *testing.T, env []string, steps []ptyStep, bin string, args ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is not installed: no PTY without a dependency")
	}
	inner := append([]string{"sh", "-c", `stty cols 80 rows 24 2>/dev/null; exec "$0" "$@"`, bin}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" || strings.HasSuffix(runtime.GOOS, "bsd") {
		cmd = exec.CommandContext(ctx, "script", append([]string{"-q", "/dev/null"}, inner...)...)
	} else {
		quoted := make([]string, len(inner))
		for i, a := range inner {
			quoted[i] = shellQuote(a)
		}
		cmd = exec.CommandContext(ctx, "script", "-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null")
	}
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	buf := &ptyOutBuf{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		for _, st := range steps {
			if st.waitFor != "" {
				for end := time.Now().Add(40 * time.Second); !strings.Contains(buf.String(), st.waitFor) && time.Now().Before(end); {
					time.Sleep(50 * time.Millisecond)
				}
			}
			time.Sleep(st.delay)
			_, _ = stdin.Write([]byte(st.input))
		}
	}()
	werr := cmd.Wait()
	code := 0
	if ee, ok := werr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return buf.String(), code
}

// inPTYWithInput is one write after one pause.
func inPTYWithInput(t *testing.T, env []string, input string, delay time.Duration, bin string, args ...string) (string, int) {
	t.Helper()
	return inPTYSteps(t, env, []ptyStep{{delay: delay, input: input}}, bin, args...)
}

func ttyEnv(env []string, path string, extra ...string) []string {
	return append(append(append([]string(nil), env...), "PATH="+path, "TERM=xterm-256color"), extra...)
}

// The control for the no-echo tests: with nothing asking for silence, this
// harness shows what is typed.
func TestTheTerminalHarnessEchoesWhatIsTypedUnlessSilenced(t *testing.T) {
	out, _ := inPTYWithInput(t, nil, "visible-word\r\x04", 700*time.Millisecond, "/bin/cat")
	if !strings.Contains(out, "visible-word") {
		t.Skipf("this harness does not feed a terminal's input, so the no-echo tests below would prove nothing: %q", out)
	}
}

func TestLoginClaudeRunsClaudeCodesSignInAndNeverShowsTheToken(t *testing.T) {
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	bin, cfg, home, env := planEnv(t, c)
	log := filepath.Join(t.TempDir(), "fake.log")
	fake := fakeClaude(t, fakeSetupScript())
	env = ttyEnv(env, filepath.Dir(fake)+":/usr/bin:/bin", "FAKE_LOG="+log)

	out, code := inPTYWithInput(t, env, "123456\r", 2500*time.Millisecond, bin, "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if leaks(out, tok) || strings.Contains(out, "sk-ant-oat") {
		t.Fatalf("a piece of the token reached the terminal:\n%q", out)
	}
	for _, want := range []string{"Opening your browser", "Paste code here", claudePlanMask, "Store this token securely.", "Signed in to your Claude plan", "…" + tok[len(tok)-4:]} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal never showed %q:\n%s", want, out)
		}
	}
	logged, _ := os.ReadFile(log)
	for _, want := range []string{"argv: " + fake + " setup-token", "size: 50 300", "code: 6", "sk-ant in the environment: 0"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the stand-in's record lacks %q:\n%s", want, logged)
		}
	}
	if leaks(string(logged), tok) {
		t.Errorf("the token is in the child's argv or environment:\n%s", logged)
	}
	posts := c.posts("/api/v2/keys")
	if len(posts) != 1 || strings.Count(posts[0], tok) != 1 || !strings.Contains(posts[0], `"provider":"claude_plan"`) {
		t.Fatalf("the service received %d requests; want one carrying the token once", len(posts))
	}
	for _, dir := range []string{home, cfg, filepath.Dir(log)} {
		if hits := treeHoldsToken(t, dir, tok); len(hits) > 0 {
			t.Errorf("the token was written to a file under %s: %v", dir, hits)
		}
	}
	// the terminal comes back in a usable state: the last thing printed is the
	// card's next step, followed by a newline, not an unfinished raw screen
	if !strings.HasSuffix(strings.TrimRight(out, " "), "\n") {
		t.Errorf("the output ends mid-line: %q", out[max(0, len(out)-60):])
	}
}

func TestLoginClaudeWhenTheMachineIsUnavailableAsksForAPasteThatShowsNothing(t *testing.T) {
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	c.signin = &signinScript{startFail: &planFail{503, "ks_fleet_unavailable", "no fleet"}}
	bin, cfg, home, env := planEnv(t, c)
	env = ttyEnv(env, "/usr/bin:/bin")
	out, code := inPTYWithInput(t, env, tok+"\r", 2500*time.Millisecond, bin, "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if leaks(out, tok) || strings.Contains(out, "sk-ant-oat") {
		t.Fatalf("the pasted token was shown:\n%q", out)
	}
	for _, want := range []string{"could not start a sign-in machine just now", "claude setup-token", "Nothing is shown as you paste", "Token:", "Signed in to your Claude plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal never showed %q:\n%s", want, out)
		}
	}
	posts := c.posts("/api/v2/keys")
	if len(posts) != 1 || strings.Count(posts[0], tok) != 1 {
		t.Fatalf("the service received %d requests", len(posts))
	}
	for _, dir := range []string{home, cfg} {
		if hits := treeHoldsToken(t, dir, tok); len(hits) > 0 {
			t.Errorf("the token was written under %s: %v", dir, hits)
		}
	}
}

func TestLoginClaudeWhenClaudeCodeEndsWithoutATokenStoresNothing(t *testing.T) {
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	fake := fakeClaude(t, "printf 'sign-in cancelled\\n'\nexit 1\n")
	env = ttyEnv(env, filepath.Dir(fake)+":/usr/bin:/bin")
	out, code := inPTYWithInput(t, env, "", 100*time.Millisecond, bin, "login", "claude")
	if code != exitFailed || !strings.Contains(out, "did not finish") || !strings.Contains(out, "Next: ks login claude") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n := len(c.posts("/api/v2/keys")); n != 0 {
		t.Errorf("%d requests after a sign-in that made no token", n)
	}
}

func TestLoginClaudeWhenTheTokenCannotBeReadSafelyAsksForAPasteInstead(t *testing.T) {
	// a token cut by a fold the filter cannot read: hidden, not kept, said in
	// plain words, and the hidden paste takes over; a pasted token is stored
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	fake := fakeClaude(t, fmt.Sprintf("printf '%%s\n%%s\n%%s\n\n' '%s' '%s' '%s'\n", tok[:40], tok[40:80], tok[80:]))
	env = ttyEnv(env, filepath.Dir(fake)+":/usr/bin:/bin")
	out, code := inPTYWithInput(t, env, tok+"\r", 2500*time.Millisecond, bin, "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"could not read the token safely", "hidden and not kept", "claude setup-token", "Token:", "Signed in to your Claude plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal never showed %q:\n%s", want, out)
		}
	}
	if leaks(out, tok[13:]) {
		t.Errorf("a piece of a token reached the terminal:\n%q", out)
	}
	posts := c.posts("/api/v2/keys")
	if len(posts) != 1 || strings.Count(posts[0], tok) != 1 {
		t.Errorf("%d requests; the one that stored a token must carry the pasted one once", len(posts))
	}
}

// ---- the sign-in machine on a terminal -------------------------------------------

func ttyPlanEnv(t *testing.T, c *planCtl) (string, string, string, []string) {
	bin, cfg, home, env := planEnv(t, c)
	// no Claude Code on PATH: the machine is the way
	return bin, cfg, home, ttyEnv(env, "/usr/bin:/bin")
}

func TestMachineCodePromptShowsNothingAndLeavesNothing(t *testing.T) {
	c := newPlanCtl()
	c.signin = goodSignin()
	bin, cfg, home, env := ttyPlanEnv(t, c)
	out, code := inPTYSteps(t, env, []ptyStep{
		{waitFor: "Open it in your browser?", delay: 200 * time.Millisecond, input: "n\r"},
		{waitFor: "Code (nothing is shown", delay: 300 * time.Millisecond, input: machineCode + "\r"},
	}, bin, "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, machineCode) {
		t.Fatalf("the code was shown on the terminal:\n%q", out)
	}
	for _, want := range []string{signinTestLink, "Code (nothing is shown as you paste)", "Signed in to your Claude plan", "…9999"} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal never showed %q:\n%s", want, out)
		}
	}
	if _, codes, _ := c.signinState(); len(codes) != 1 || codes[0] != machineCode {
		t.Errorf("the service received %v", codes)
	}
	for _, dir := range []string{home, cfg} {
		if hits := treeHoldsText(t, dir, machineCode); len(hits) > 0 {
			t.Errorf("the code was written to a file: %v", hits)
		}
	}
}

func TestMachineCtrlCCancelsTheMachineAtEveryStage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		views   []map[string]any
		waitFor string
	}{
		{"while the machine starts", []map[string]any{sview("starting", 0, nil)}, "Starting sign-in process, generating sign in URL"},
		{"at the link", []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink})}, "Open it in your browser?"},
		{"at the code prompt", []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink})}, "Code (nothing is shown"},
	} {
		c := newPlanCtl()
		c.signin = &signinScript{views: tc.views}
		bin, _, _, env := ttyPlanEnv(t, c)
		steps := []ptyStep{}
		if tc.waitFor == "Code (nothing is shown" {
			steps = append(steps, ptyStep{waitFor: "Open it in your browser?", delay: 200 * time.Millisecond, input: "n\r"})
		}
		steps = append(steps, ptyStep{waitFor: tc.waitFor, delay: 300 * time.Millisecond, input: "\x03"})
		out, code := inPTYSteps(t, env, steps, bin, "login", "claude")
		if code != 130 || !strings.Contains(out, "Cancelled. The sign-in machine is gone and nothing was saved.") || !c.signinWas() {
			t.Errorf("%s: exit %d, cancelled on the service: %v\n%s", tc.name, code, c.signinWas(), out)
		}
	}
}

func TestMachineRefusedCodeOffersToStartAgain(t *testing.T) {
	refused := sview("failed", 0, map[string]any{"error": map[string]any{"code": "ks_signin_code_refused", "words": "Claude did not accept that code. Start again to get a new link, and paste the new code.", "next": "ks login claude"}})
	c := newPlanCtl()
	c.signin = &signinScript{views: []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink}), sview("checking", 3, nil), refused}}
	bin, _, _, env := ttyPlanEnv(t, c)
	out, code := inPTYSteps(t, env, []ptyStep{
		{waitFor: "Open it in your browser?", delay: 200 * time.Millisecond, input: "n\r"},
		{waitFor: "Code (nothing is shown", delay: 300 * time.Millisecond, input: machineCode + "\r"},
		{waitFor: "Start again with a new link?", delay: 200 * time.Millisecond, input: "n\r"},
	}, bin, "login", "claude")
	if code != exitFailed || !strings.Contains(out, "did not accept that code") || !strings.Contains(out, "Next: ks login claude") || strings.Contains(out, machineCode) {
		t.Errorf("exit %d\n%s", code, out)
	}
	if starts, _, _ := c.signinState(); starts != 1 {
		t.Errorf("a second sign-in was started although the answer was no (%d starts)", starts)
	}
}

func TestMachineRefusedCodeStartsAgainWhenAsked(t *testing.T) {
	refused := sview("failed", 0, map[string]any{"error": map[string]any{"code": "ks_signin_code_refused", "words": "Claude did not accept that code.", "next": "ks login claude"}})
	c := newPlanCtl()
	c.signin = &signinScript{views: []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink}), sview("checking", 3, nil), refused}}
	bin, _, _, env := ttyPlanEnv(t, c)
	out, _ := inPTYSteps(t, env, []ptyStep{
		{waitFor: "Open it in your browser?", delay: 200 * time.Millisecond, input: "n\r"},
		{waitFor: "Code (nothing is shown", delay: 300 * time.Millisecond, input: machineCode + "\r"},
		{waitFor: "Start again with a new link?", delay: 200 * time.Millisecond, input: "y\r"},
		{waitFor: "Open it in your browser?", delay: 200 * time.Millisecond, input: "\x03"},
	}, bin, "login", "claude")
	starts, _, _ := c.signinState()
	if starts != 2 {
		t.Errorf("starts = %d, want a second sign-in after yes\n%s", starts, out)
	}
}

func TestLoginClaudeOrgPlanKeysAndRemove(t *testing.T) {
	c := newPlanCtl()
	c.orgRows = []map[string]any{
		{"member_id": "mem_you", "who": map[string]any{"name": "Ada Lovelace", "email": "ada@example.com"}, "last4": "9999", "state": "ok", "expires_at": "2027-11-22T10:00:00Z", "since": "2026-11-22T10:00:00Z", "you": true},
		{"member_id": "mem_two", "who": map[string]any{"name": "Grace Hopper", "email": "grace@example.com"}, "last4": "1234", "state": "expiring", "expires_at": "2026-12-03T10:00:00Z", "since": "2026-11-01T10:00:00Z"},
	}
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "", "organization", "plan-keys", "--org", "org_acme")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"MEMBER", "Ada Lovelace (you)", "…9999", "working", "Grace Hopper", "…1234", "ends soon", "3 Dec 2026"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list does not say %q:\n%s", want, out)
		}
	}
	// removal names the member, asks, and ends only that permission
	_, errs, code = runPlan(t, bin, env, "", "organization", "plan-keys", "remove", "grace@example.com", "--org", "org_acme", "--no-input")
	if code != exitUsage || !strings.Contains(errs, "--yes") || len(c.removedList()) != 0 {
		t.Fatalf("a removal went ahead without a yes: exit %d removed %v\n%s", code, c.removedList(), errs)
	}
	out, errs, code = runPlan(t, bin, env, "", "organization", "plan-keys", "remove", "grace@example.com", "--org", "org_acme", "--yes")
	if rm := c.removedList(); code != 0 || len(rm) != 1 || rm[0] != "/api/v2/orgs/org_acme/plan-keys/mem_two" {
		t.Fatalf("exit %d removed %v\n%s%s", code, rm, out, errs)
	}
	if !strings.Contains(out, "Ended Grace Hopper's Claude plan") {
		t.Errorf("the card does not say what ended:\n%s", out)
	}
	_, errs, code = runPlan(t, bin, env, "", "organization", "plan-keys", "remove", "nobody", "--org", "org_acme", "--yes")
	if code != exitUsage || !strings.Contains(errs, "no member with a Claude plan") {
		t.Errorf("an unknown member: exit %d\n%s", code, errs)
	}
}

func TestRunOnAMembersPlanInAnOrganization(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]any
		ok   bool
		says string
	}{
		{"the member put a plan there", []map[string]any{{"member_id": "mem_you", "last4": "9999", "state": "ok", "expires_at": "2027-11-22T10:00:00Z", "you": true}}, true, ""},
		{"only someone else did", []map[string]any{{"member_id": "mem_two", "last4": "1234", "state": "ok", "expires_at": "2027-11-22T10:00:00Z"}}, false, "no claude plan in this organization"},
		{"no plan at all", nil, false, "ks login claude --org"},
	} {
		c := planRunCtl()
		c.orgRows = tc.rows
		bin, cfg, _, env := planEnv(t, c)
		ctx, _ := json.Marshal(map[string]string{c.url: "org_acme"})
		if err := os.WriteFile(filepath.Join(cfg, "keepstate", "context.json"), ctx, 0o600); err != nil {
			t.Fatal(err)
		}
		out, errs, code := runPlan(t, bin, env, "", "run", "--agent", "--key", "claude-plan")
		if tc.ok {
			var body map[string]any
			posts := c.posts("/api/v2/sessions")
			if code != 0 || len(posts) != 1 || json.Unmarshal([]byte(posts[0]), &body) != nil || body["claude_plan"] != true || body["org_id"] != "org_acme" {
				t.Errorf("%s: exit %d body %v\n%s%s", tc.name, code, posts, out, errs)
			}
			continue
		}
		if code == 0 || !strings.Contains(strings.ToLower(errs), tc.says) || c.asked("POST /api/v2/sessions") {
			t.Errorf("%s: exit %d\n%s", tc.name, code, errs)
		}
	}
}

// Ctrl-C while the start has not answered yet: once the machine's id arrives it
// is cancelled and said; if the start never answers, the words say the machine
// may still be starting.
func TestMachineCtrlCDuringTheStartCancelsTheMachineWhenItsIdArrives(t *testing.T) {
	c := newPlanCtl()
	c.signin = &signinScript{startDelay: 250 * time.Millisecond, views: []map[string]any{sview("starting", 0, nil)}}
	bin, _, _, env := ttyPlanEnv(t, c)
	out, code := inPTYSteps(t, env, []ptyStep{{waitFor: "Sign in to your Claude plan", delay: 20 * time.Millisecond, input: "\x03"}}, bin, "login", "claude")
	if code != 130 || !c.signinWas() || !strings.Contains(out, "Cancelled. The sign-in machine is gone and nothing was saved.") {
		t.Errorf("exit %d, cancelled on the service: %v\n%s", code, c.signinWas(), out)
	}
}

func TestMachineCtrlCWhenTheStartNeverAnswersSaysTheMachineMayStillBeStarting(t *testing.T) {
	c := newPlanCtl()
	c.signin = &signinScript{startDelay: 3 * time.Second, views: []map[string]any{sview("starting", 0, nil)}}
	bin, _, _, env := ttyPlanEnv(t, c)
	out, code := inPTYSteps(t, env, []ptyStep{{waitFor: "Sign in to your Claude plan", delay: 20 * time.Millisecond, input: "\x03"}}, bin, "login", "claude")
	if code != 130 || !strings.Contains(out, "may still be starting") || !strings.Contains(out, "ends on its own") || strings.Contains(out, "The sign-in machine is gone") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
