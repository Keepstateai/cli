package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every stty invocation must wire the child's stdin to ours.
//
// This is a source guard, not a behavioural one, and it exists because the
// behavioural version cannot be written here: proving raw mode is entered
// needs a pty, and the Go standard library will not make one without
// golang.org/x/sys or creack/pty. This client's zero dependencies are a
// deliberate property, so that proof lives in an external probe and the
// gap is named rather than papered over with a test that asserts nothing.
//
// What CAN be guarded dependency-free is the exact defect that occurred.
// `stty` reads the terminal from its own standard input. An exec.Command
// with no Stdin set hands the child /dev/null, where every stty call fails
// with "stdin isn't a terminal" -- and both call sites discarded the
// error, so rawMode() silently never entered raw mode and termSize()
// silently answered 80x24 for every window. The code was cited as
// evidence that the capability existed; it existed and did not run.
func TestEverySttyCallWiresStdin(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`exec\.Command\("stty"`)
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if !call.MatchString(line) {
				continue
			}
			found++
			// The assignment must be wired within the few lines that
			// follow, before the command is run.
			wired := false
			for j := i; j < len(lines) && j < i+6; j++ {
				if strings.Contains(lines[j], ".Stdin = os.Stdin") {
					wired = true
					break
				}
			}
			if !wired {
				t.Errorf("%s:%d: this stty child gets /dev/null, where stty always fails.\n"+
					"    %s\n"+
					"    Set cmd.Stdin = os.Stdin, or call it through sttyOut/sttyRun.", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	// A guard that finds nothing to check is not a passing guard.
	if found == 0 {
		t.Fatal("no stty call sites were found at all: this test is no longer checking anything")
	}
	t.Logf("stty call sites checked: %d", found)
}
