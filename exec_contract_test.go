// exec_contract_test: KS-003's argument boundary for the one verb that
// carries another program's arguments. The control plane's route takes a
// single command string that the guest's shell splits, so lossless
// forwarding is a quoting contract, and this file measures it against a
// real POSIX shell rather than asserting it.
package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// awkward is every class of argument the audit named: spaces, an empty
// argument, a dollar sign, a semicolon, quotes of both kinds, a backslash,
// a newline, Unicode, a glob and a leading dash that is not a KS option.
var awkward = []string{"a b", "", "$HOME", "a;b", "it's", `say "hi"`, `back\slash`, "line\nbreak", "café 🙂", "*.py", "--help", "-", "x=y", "tab\there"}

// VER-003-2: the quoted string, given to a POSIX shell, yields exactly the
// original vector. printf prints each argument NUL-terminated so an empty
// argument and a newline inside one are both visible.
func TestShellJoinRoundTripsThroughSh(t *testing.T) {
	args := append([]string{"printf", `%s\0`}, awkward...)
	cmd := exec.Command("sh", "-c", shellJoin(args))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", shellJoin(args), err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if strings.Join(got, "\x01") != strings.Join(awkward, "\x01") {
		t.Fatalf("the shell rebuilt %q, want %q\nfrom %s", got, awkward, shellJoin(args))
	}
	// bare words stay bare, so a simple command line reads as typed
	if s := shellJoin([]string{"python", "--help"}); s != "python --help" {
		t.Errorf("shellJoin(python --help) = %q", s)
	}
	if s := shellJoin([]string{"printf", "%s", "a b"}); s != "printf %s 'a b'" {
		t.Errorf("shellJoin = %q", s)
	}
}

// QA-003-2's exec form and QA-003-3's client half: a program's --help
// reaches the program; KeepState's own help is only in the option region.
func TestExecArgumentContract(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	sent := func(args ...string) (string, int, string) {
		t.Helper()
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
		got := rec.drain()
		if len(got) == 0 {
			return "", code, out + errs
		}
		var body map[string]string
		_ = json.Unmarshal([]byte(got[0].Body), &body)
		return body["Cmd"], code, out + errs
	}
	// argv form, with and without the separator
	for _, args := range [][]string{{"exec", "s1", "--", "python", "--help"}, {"exec", "s1", "python", "--help"}} {
		if cmd, code, out := sent(args...); code != 0 || cmd != "python --help" || strings.Contains(out, "usage:") {
			t.Errorf("`ks %s`: exit %d, sent %q\n%s", strings.Join(args, " "), code, cmd, out)
		}
	}
	// every awkward argument survives as the program's own argument
	full := append([]string{"exec", "s1", "--", "prog"}, awkward...)
	cmd, code, _ := sent(full...)
	if code != 0 {
		t.Fatalf("awkward exec exited %d", code)
	}
	back, err := exec.Command("sh", "-c", "set -- "+strings.TrimPrefix(cmd, "prog ")+`; printf '%s\0' "$@"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(back), "\x00"), "\x00")
	if strings.Join(got, "\x01") != strings.Join(awkward, "\x01") {
		t.Errorf("the guest shell would see %q, want %q", got, awkward)
	}
	// the shell form: one line, verbatim, --shell before the session
	if cmd, code, _ := sent("exec", "--shell", "s1", "ls | wc -l"); code != 0 || cmd != "ls | wc -l" {
		t.Errorf("--shell: exit %d, sent %q", code, cmd)
	}
	// the ambiguous legacy form is refused, named, and sends nothing
	if cmd, code, out := sent("exec", "s1", "ls | wc -l"); code != 2 || cmd != "" || !strings.Contains(out, "--shell") || !strings.Contains(out, "No command was sent.") {
		t.Errorf("ambiguous line: exit %d, sent %q\n%s", code, cmd, out)
	}
	if cmd, code, _ := sent("exec", "--shell", "s1", "ls", "-l"); code != 2 || cmd != "" {
		t.Errorf("--shell with two arguments: exit %d, sent %q", code, cmd)
	}
	// --shell AFTER the session is the program's argument, not KS's flag
	if cmd, code, _ := sent("exec", "s1", "--shell", "x"); code != 0 || cmd != "--shell x" {
		t.Errorf("late --shell: exit %d, sent %q", code, cmd)
	}
	// a single simple word is unambiguous and sent bare
	if cmd, code, _ := sent("exec", "s1", "uptime"); code != 0 || cmd != "uptime" {
		t.Errorf("single word: exit %d, sent %q", code, cmd)
	}
}

// VER-003-1: help tests enumerated from the REGISTRY, not from the
// manifest, so a command the manifest has not learned about yet is still
// covered: every command and alias, three help forms, its own usage line
// printed, zero requests.
func TestEveryRegisteredCommandHelps(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	n := 0
	for _, c := range registry {
		if c.Group {
			continue
		}
		paths := append([][]string{c.Path}, c.Aliases...)
		for _, p := range paths {
			for _, form := range []string{"--help", "-h", "help"} {
				args := append(append([]string{}, p...), form)
				out, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
				n++
				if code != 0 {
					t.Errorf("`ks %s` exited %d\n%s", strings.Join(args, " "), code, errs)
				}
				if !strings.Contains(out, c.Usage()) {
					t.Errorf("`ks %s` did not print its usage line %q:\n%s", strings.Join(args, " "), c.Usage(), out)
				}
				if c.Effects == "" || !strings.Contains(out, c.Effects) {
					t.Errorf("`ks %s` did not print its effects (every command names them):\n%s", strings.Join(args, " "), out)
				}
				if got := rec.drain(); len(got) != 0 {
					t.Errorf("`ks %s` made a request: %v", strings.Join(args, " "), got)
				}
			}
		}
	}
	t.Logf("%d help invocations from the registry, zero requests", n)
}
