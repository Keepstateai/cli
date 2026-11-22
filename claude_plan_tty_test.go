//go:build unix

package main

// The sign-in on a real terminal: the binary runs under script(1), which
// gives it a pseudo-terminal for standard input and output, with a stand-in
// claude first on PATH (or none). What the person would have seen is the
// bytes script(1) captured.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// inPTYWithInput runs bin under script(1) at 80 columns, writes input to its
// terminal after delay, and returns what the terminal received and the exit
// code.
func inPTYWithInput(t *testing.T, env []string, input string, delay time.Duration, bin string, args ...string) (string, int) {
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
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(delay)
		_, _ = stdin.Write([]byte(input))
	}()
	werr := cmd.Wait()
	code := 0
	if ee, ok := werr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return buf.String(), code
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

func TestLoginClaudeWithoutClaudeCodeAsksForAPasteThatShowsNothing(t *testing.T) {
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	bin, cfg, home, env := planEnv(t, c)
	env = ttyEnv(env, "/usr/bin:/bin")
	out, code := inPTYWithInput(t, env, tok+"\r", 2500*time.Millisecond, bin, "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if leaks(out, tok) || strings.Contains(out, "sk-ant-oat") {
		t.Fatalf("the pasted token was shown:\n%q", out)
	}
	for _, want := range []string{"Claude Code is not installed on this computer.", "claude setup-token", "Nothing is shown as you paste", "Token:", "Signed in to your Claude plan"} {
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
