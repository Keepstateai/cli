//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"
)

// fakeClaude writes an executable named claude into a new directory and
// returns its path. body is a shell script. The fixture token is never a
// whole string in the script: it is printed from parts.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenParts is the fixture token as three printf arguments.
func tokenParts() string {
	tok := fixtureClaudeToken()
	return fmt.Sprintf("'%s' '%s' '%s'", tok[:6], tok[6:13], tok[13:])
}

// fakeSetupScript is a stand-in for claude setup-token: it records its own
// arguments, terminal size and environment to $FAKE_LOG (never the token),
// asks for a code, prints the token and a closing line.
func fakeSetupScript() string {
	return `echo "argv: $0 $*" > "$FAKE_LOG"
echo "size: $(stty size 2>/dev/null)" >> "$FAKE_LOG"
env | sed 's/=.*//' | sort | tr '\n' ' ' >> "$FAKE_LOG"
echo >> "$FAKE_LOG"
echo "sk-ant in the environment: $(env | grep -c 'sk-ant')" >> "$FAKE_LOG"
printf 'Opening your browser to sign in...\n'
printf 'Paste code here if prompted > '
read code
echo "code: ${#code}" >> "$FAKE_LOG"
printf '\n\nYour OAuth token (valid for 1 year):\n\n'
printf '%s%s%s\n' ` + tokenParts() + `
printf '\nStore this token securely.\n'
`
}

// treeHoldsToken says whether any file under dir holds eight characters in a
// row of the token (a whole token, or a piece worth hiding).
func treeHoldsToken(t *testing.T, dir, tok string) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() > 8<<20 {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if leaks(string(b), tok) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

func TestSetupTokenIsCapturedAndNeverShown(t *testing.T) {
	tok := fixtureClaudeToken()
	bin := fakeClaude(t, fakeSetupScript())
	log := filepath.Join(t.TempDir(), "fake.log")
	t.Setenv("FAKE_LOG", log)
	home := t.TempDir()
	t.Setenv("HOME", home)

	var display bytes.Buffer
	res, err := captureSetupToken(bin, strings.NewReader("123456\n"), nil, &display, 80, 24, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Token) != tok || !res.Hidden || res.ChildErr != nil {
		t.Fatalf("token ok=%v hidden=%v child=%v", string(res.Token) == tok, res.Hidden, res.ChildErr)
	}
	shown := display.String()
	if leaks(shown, tok) || strings.Contains(shown, "sk-ant-oat") {
		t.Fatalf("a piece of the token reached the screen:\n%q", shown)
	}
	for _, want := range []string{"Opening your browser", "Paste code here", claudePlanMask, "Store this token securely."} {
		if !strings.Contains(shown, want) {
			t.Errorf("the screen lost %q:\n%q", want, shown)
		}
	}
	// what the child saw: setup-token and nothing else, a wide terminal, the
	// code the person typed, and no token anywhere in its environment
	logged, _ := os.ReadFile(log)
	for _, want := range []string{"argv: " + bin + " setup-token", "size: 24 300", "code: 6", "sk-ant in the environment: 0"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the child's record lacks %q:\n%s", want, logged)
		}
	}
	if leaks(string(logged), tok) || !strings.Contains(string(logged), "sk-ant in the environment: 0") {
		t.Errorf("the token is in the child's argv or environment:\n%s", logged)
	}
	if hits := treeHoldsToken(t, home, tok); len(hits) > 0 {
		t.Errorf("the token was written under HOME: %v", hits)
	}
}

func TestSetupTokenGivesTheChildAWideTerminal(t *testing.T) {
	bin := fakeClaude(t, `echo "size: $(stty size 2>/dev/null)" > "$FAKE_LOG"`+"\n")
	log := filepath.Join(t.TempDir(), "fake.log")
	t.Setenv("FAKE_LOG", log)
	var display bytes.Buffer
	if _, err := captureSetupToken(bin, strings.NewReader(""), nil, &display, 40, 24, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "size: 24 300") {
		t.Errorf("a 40-column terminal gave the child %q, which would wrap a token", logged)
	}
}

func TestSetupTokenChildThatEndsWithoutAToken(t *testing.T) {
	bin := fakeClaude(t, "printf 'sign-in was cancelled\\n'\nexit 3\n")
	var display bytes.Buffer
	res, err := captureSetupToken(bin, strings.NewReader(""), nil, &display, 80, 24, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != nil || res.Hidden || res.ChildErr == nil {
		t.Errorf("token=%v hidden=%v child=%v", res.Token != nil, res.Hidden, res.ChildErr)
	}
	if !strings.Contains(display.String(), "sign-in was cancelled") {
		t.Errorf("the child's words were lost: %q", display.String())
	}
}

func TestSetupTokenALineThatWrapsInsideTheChildIsHiddenAndNotKept(t *testing.T) {
	// the child folds the token at 40 columns on a pty it was told is wide:
	// the filter cannot tell the fold from the end of the token
	tok := fixtureClaudeToken()
	bin := fakeClaude(t, fmt.Sprintf("printf '%%s\\n%%s\\n%%s\\n\\n' '%s' '%s' '%s'\nprintf 'after\\n'\n", tok[:40], tok[40:80], tok[80:]))
	var display bytes.Buffer
	res, err := captureSetupToken(bin, strings.NewReader(""), nil, &display, 80, 24, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if leaks(display.String(), tok) {
		t.Errorf("a piece of the folded token reached the screen: %q", display.String())
	}
	if res.Token != nil || res.Uncertain != "cut" {
		t.Errorf("a token cut by the child's fold: kept=%v reason=%q", res.Token != nil, res.Uncertain)
	}
	if !res.Hidden {
		t.Error("the token was hidden, and the result should say so")
	}
	if !strings.Contains(display.String(), "after") {
		t.Errorf("the text after the token was lost: %q", display.String())
	}
}

func TestSetupTokenIsStoppedWhenTheBrowserStepTakesTooLong(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_PID", pidFile)
	bin := fakeClaude(t, "echo $$ > \"$FAKE_PID\"\nsleep 60\n")
	var display bytes.Buffer
	start := time.Now()
	_, err := captureSetupToken(bin, strings.NewReader(""), nil, &display, 80, 24, 1500*time.Millisecond)
	if err != errSetupTimedOut {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the run was not cut short")
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 0 {
		t.Fatalf("no pid recorded: %q", raw)
	}
	for i := 0; i < 40; i++ {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the child %d is still running after the stop", pid)
}

// The person's own terminal is raw during the run and exactly as it was
// afterwards, on a clean run and on a failed one.
func TestSetupTokenPutsThePersonsTerminalBack(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"a clean run", "sleep 1\nprintf 'done\\n'\n"},
		{"a child that fails", "sleep 1\nexit 7\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := ptyPair()
			if err != nil {
				t.Skip("no pseudo-terminal here: " + err.Error())
			}
			defer master.Close()
			defer slave.Close()
			fd := int(slave.Fd())
			before, err := term.GetState(fd)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%v", *before)

			sawRaw := make(chan bool, 1)
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						sawRaw <- false
						return
					default:
					}
					if st, e := term.GetState(fd); e == nil && fmt.Sprintf("%v", *st) != want {
						sawRaw <- true
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()

			bin := fakeClaude(t, tc.script)
			var display bytes.Buffer
			_, err = captureSetupToken(bin, strings.NewReader(""), slave, &display, 80, 24, 30*time.Second)
			close(stop)
			if err != nil {
				t.Fatal(err)
			}
			after, err := term.GetState(fd)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%v", *after); got != want {
				t.Errorf("the terminal was left changed:\nbefore %s\nafter  %s", want, got)
			}
			if !<-sawRaw {
				t.Error("the terminal was never put in raw mode, so keystrokes would have been echoed and cooked")
			}
		})
	}
}

// A child that ignores Ctrl-C is stopped by three of them, and a single one
// reaches the child as it would in any terminal.
func TestSetupTokenIsStoppedByThreeCtrlC(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_PID", pidFile)
	bin := fakeClaude(t, "trap '' INT\necho $$ > \"$FAKE_PID\"\nsleep 60\n")
	var display bytes.Buffer
	start := time.Now()
	_, err := captureSetupToken(bin, &delayedInput{after: 700 * time.Millisecond, data: "\x03\x03\x03"}, nil, &display, 80, 24, 30*time.Second)
	if err != errSetupInterrupted {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the run was not stopped")
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for i := 0; i < 40 && pid > 0; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the child %d is still running", pid)
}

func TestSetupTokenOneCtrlCReachesTheChild(t *testing.T) {
	// the child ends on its own SIGINT: the terminal's own handling, not ours
	bin := fakeClaude(t, "trap 'printf \"got-int\\n\"; exit 9' INT\nprintf 'ready\\n'\nsleep 30 &\nwait\n")
	var display bytes.Buffer
	res, err := captureSetupToken(bin, &delayedInput{after: 600 * time.Millisecond, data: "\x03"}, nil, &display, 80, 24, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChildErr == nil || !strings.Contains(display.String(), "got-int") {
		t.Errorf("child=%v screen=%q", res.ChildErr, display.String())
	}
}

// delayedInput gives its data once, after a pause, then blocks like a
// keyboard that is not pressed.
type delayedInput struct {
	after time.Duration
	data  string
	done  bool
}

func (d *delayedInput) Read(p []byte) (int, error) {
	if d.done {
		select {}
	}
	d.done = true
	time.Sleep(d.after)
	return copy(p, d.data), nil
}
