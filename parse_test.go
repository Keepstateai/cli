// parse_test: the permanent guards of KS-002. A malformed command line is
// refused before login, before any request and before any local write, with
// exit 2 and at most one suggestion that is never applied; a well-formed one
// sends exactly what was typed. Every case runs the built binary against a
// recording control plane, so "zero requests" is an observation, and the
// property test drives the parser in-process ten thousand times.
//
// The recorder is proven able to see a request first (the control at the
// top of each binary-level test), so a green run is a measurement.
package main

import (
	"encoding/json"
	"math/rand"
	"net/http/httptest"
	"strings"
	"testing"
)

func startAuditRecorder(t *testing.T) (*auditRecorder, string, string) {
	t.Helper()
	rec := &auditRecorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	auditExec(t, bin, cfg, t.TempDir(), nil, "run")
	if len(rec.drain()) == 0 {
		t.Fatal("control: `ks run` made no request; the recorder cannot observe")
	}
	return rec, bin, cfg
}

// QA-002-1: --buget 1000, --budget abc and a missing value each produce
// zero requests and no session; plus the other refusals of the schema.
func TestStrictParsingRefusesBeforeAnyRequest(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"run", "--buget", "1000"}, []string{"Unknown option: --buget", "Did you mean --budget-tokens?", "No session was started."}},
		{[]string{"run", "--budget", "abc"}, []string{"not a whole number", "No session was started."}},
		{[]string{"run", "--budget"}, []string{"requires a value", "No session was started."}},
		{[]string{"run", "--budget=1000", "--budget-tokens", "2000"}, []string{"more than once", "No session was started."}},
		{[]string{"run", "extra"}, []string{"Unexpected argument", "No session was started."}},
		{[]string{"run", "--budget=1000", "--image"}, []string{"requires a value"}},
		{[]string{"kill"}, []string{"Missing argument: <session>", "Nothing was destroyed."}},
		{[]string{"checkpoint", "s1", "--stop=yes"}, []string{"takes no value", "No checkpoint was taken."}},
		{[]string{"checkpoint", "s1", "--stpo"}, []string{"Unknown option: --stpo", "Did you mean --stop?"}},
		{[]string{"fork", "s1", "-n", "abc"}, []string{"not a whole number", "Nothing was forked."}},
		{[]string{"fork", "s1", "-x"}, []string{"Unknown option: -x"}},
		{[]string{"exec", "s1"}, []string{"Missing argument: <command...>", "No command was sent."}},
		{[]string{"cruise", "init", "--spend", "abc"}, []string{"not a dollar amount", "No draft was written."}},
		{[]string{"cruise", "init", "--spend", "0"}, []string{"above zero", "No draft was written."}},
		{[]string{"cruise", "init", "--sped", "2"}, []string{"Unknown option: --sped", "Did you mean --spend?"}},
		{[]string{"cruise", "logs"}, []string{"Missing argument: <job>"}},
		{[]string{"rn"}, []string{"unknown verb", "Did you mean run?"}},
		{[]string{"cruise", "innit"}, []string{"unknown cruise verb", "Did you mean cruise init?"}},
		{[]string{"--budget", "1000"}, []string{"Unknown option: --budget"}},
	}
	for _, c := range cases {
		name := strings.Join(c.args, " ")
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, c.args...)
		if code != 2 {
			t.Errorf("`ks %s` exited %d, want 2\n%s%s", name, code, out, errs)
		}
		for _, w := range c.want {
			if !strings.Contains(errs, w) {
				t.Errorf("`ks %s` stderr lacks %q:\n%s", name, w, errs)
			}
		}
		if n := strings.Count(errs, "Did you mean"); n > 1 {
			t.Errorf("`ks %s` made %d suggestions; at most one", name, n)
		}
		if got := rec.drain(); len(got) != 0 {
			t.Errorf("`ks %s` made %d request(s): %v", name, len(got), got)
		}
	}
}

// QA-002-2: 1000abc, 1e3, negative values and integer overflow fail
// without truncation, in-process and through the binary.
func TestIntegerFormsFailWithoutTruncation(t *testing.T) {
	bad := []string{"1000abc", "1e3", "0", "-5", "-0", "99999999999999999999", "+1000", "1_000", " 1000", "1000 ", "1,000", "0x10", "1.0", "٣", "", "-"}
	for _, s := range bad {
		if n, err := parseWhole(s); err == nil && n > 0 {
			t.Errorf("parseWhole(%q) = %d, accepted", s, n)
		}
	}
	for _, s := range []string{"1000", "1", "9223372036854775807"} {
		if _, err := parseWhole(s); err != nil {
			t.Errorf("parseWhole(%q) refused: %v", s, err)
		}
	}
	rec, bin, cfg := startAuditRecorder(t)
	for _, s := range bad {
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, "run", "--budget-tokens", s)
		if code != 2 {
			t.Errorf("`ks run --budget-tokens %q` exited %d, want 2: %s", s, code, errs)
		}
		if got := rec.drain(); len(got) != 0 {
			t.Errorf("`ks run --budget-tokens %q` made a request: %v", s, got)
		}
	}
}

// QA-002-3: valid equals and space forms send exactly 1000, including
// through the legacy alias; an omitted budget is absent from the request.
func TestValidBudgetFormsSendExactlyThousand(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	forms := [][]string{{"--budget", "1000"}, {"--budget=1000"}, {"--budget-tokens", "1000"}, {"--budget-tokens=1000"}, {"--image", "base", "--budget-tokens", "1000"}}
	for _, f := range forms {
		args := append([]string{"run"}, f...)
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
		if code != 0 {
			t.Fatalf("`ks %s` exited %d\n%s%s", strings.Join(args, " "), code, out, errs)
		}
		got := rec.drain()
		if len(got) != 1 || got[0].Method != "POST" || got[0].Path != "/api/sessions" {
			t.Fatalf("`ks %s` made %v, want exactly POST /api/sessions", strings.Join(args, " "), got)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got[0].Body), &body); err != nil {
			t.Fatal(err)
		}
		if string(body["Budget"]) != "1000" {
			t.Errorf("`ks %s` sent Budget %q, want exactly 1000", strings.Join(args, " "), body["Budget"])
		}
	}
	// omitted stays omitted
	auditExec(t, bin, cfg, t.TempDir(), nil, "run")
	got := rec.drain()
	if len(got) != 1 || strings.Contains(got[0].Body, "Budget") {
		t.Errorf("an omitted budget was sent: %v", got)
	}
	// an alias verb parses its own flags too
	auditExec(t, bin, cfg, t.TempDir(), nil, "save", "sess-1", "--stop")
	got = rec.drain()
	if len(got) != 1 || got[0].Path != "/api/sessions/sess-1/checkpoint" || !strings.Contains(got[0].Body, `"Stop":true`) {
		t.Errorf("`ks save sess-1 --stop` sent %v", got)
	}
	// -n on fork, space and short forms
	auditExec(t, bin, cfg, t.TempDir(), nil, "fork", "sess-1", "-n", "3")
	got = rec.drain()
	if len(got) != 1 || got[0].Path != "/api/sessions/sess-1/fork" {
		t.Errorf("`ks fork sess-1 -n 3` sent %v", got)
	}
}

// The option region ends at "--" or after a Rest command's positionals:
// what follows is literal, help words included.
func TestDoubleDashEndsOptionRegion(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	for _, args := range [][]string{{"exec", "s1", "--", "python", "--help"}, {"exec", "s1", "python", "--help"}, {"exec", "s1", "echo", "help"}} {
		out, _, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
		got := rec.drain()
		if code != 0 || len(got) != 1 || got[0].Path != "/api/sessions/s1/exec" {
			t.Errorf("`ks %s`: exit %d, requests %v\n%s", strings.Join(args, " "), code, got, out)
			continue
		}
		if strings.Contains(out, "usage:") {
			t.Errorf("`ks %s` printed KeepState's usage instead of sending the command", strings.Join(args, " "))
		}
		wantCmd := strings.Join(args[2:], " ")
		if args[2] == "--" {
			wantCmd = strings.Join(args[3:], " ")
		}
		var body map[string]string
		_ = json.Unmarshal([]byte(got[0].Body), &body)
		if body["Cmd"] != wantCmd {
			t.Errorf("`ks %s` sent Cmd %q, want %q", strings.Join(args, " "), body["Cmd"], wantCmd)
		}
	}
	// "help" BEFORE the positionals is help, and makes no request
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), nil, "exec", "--help")
	if code != 0 || len(rec.drain()) != 0 || !strings.Contains(out, "ks exec") {
		t.Errorf("`ks exec --help`: exit %d\n%s", code, out)
	}
}

// VER-002-2: ten thousand malformed argument sequences through the parser
// without a panic; a hundred of the refused ones through the binary without
// a request.
func TestMalformedSequencesNeverPanicOrRequest(t *testing.T) {
	alphabet := []string{"run", "kill", "exec", "fork", "cruise", "init", "status", "save", "--budget", "--budget-tokens", "--budget=", "--budget=1000",
		"--image", "--stop", "--force", "--json", "-n", "-x", "--", "-", "--help", "help", "-h", "1000", "1e3", "abc", "-5", "0", "", "=", "--=",
		"--spend", "$2", "2.5", "--paths", "a", "b", "--ladder", "x:y,", "🙂", "\x00", "--\x00", "sess-1", "--steer", "/dev/null", "---", "--budget--tokens"}
	rng := rand.New(rand.NewSource(20260919))
	refused := [][]string{}
	for i := 0; i < 10000; i++ {
		n := rng.Intn(9)
		args := make([]string, n)
		for j := range args {
			args[j] = alphabet[rng.Intn(len(alphabet))]
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", args, r)
				}
			}()
			if len(args) == 0 || isHelpWord(args[0]) || args[0] == "--version" || args[0] == "-v" {
				return // handled before lookup: help and version are commands, not malformed input
			}
			c, rest, _ := lookup(registry, args)
			if c == nil || c.Group {
				refused = append(refused, args)
				return
			}
			if _, uerr := c.parse(rest); uerr != nil {
				refused = append(refused, args)
			}
		}()
	}
	if len(refused) < 100 {
		t.Fatalf("only %d of 10000 sequences were refused; the alphabet is too tame to prove anything", len(refused))
	}
	rec, bin, cfg := startAuditRecorder(t)
	rng.Shuffle(len(refused), func(i, j int) { refused[i], refused[j] = refused[j], refused[i] })
	for _, args := range refused[:100] {
		clean := true
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				clean = false // an OS argument cannot carry NUL; in-process only
			}
		}
		if !clean {
			continue
		}
		_, _, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
		if got := rec.drain(); len(got) != 0 {
			t.Errorf("refused sequence %q made a request: %v", args, got)
		}
		if code != 2 {
			t.Errorf("refused sequence %q exited %d, want 2", args, code)
		}
	}
	t.Logf("%d of 10000 random sequences refused in-process; 100 of them re-run as the binary with zero requests", len(refused))
}
