package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// nextLines are the commands a run said to run next ("Next: ks ...").
func nextLines(errs string) []string {
	var out []string
	for _, l := range strings.Split(errs, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "Next: ks ") {
			out = append(out, strings.TrimPrefix(t, "Next: "))
		}
	}
	return out
}

func (e *importEnv) runNext(cmd string) (string, string, int) {
	e.t.Helper()
	f := strings.Fields(cmd)
	if len(f) < 2 || f[0] != "ks" {
		e.t.Fatalf("not a ks command: %q", cmd)
	}
	return e.run(f[1:]...)
}

func (e *importEnv) ready() {
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
}

// The limit is 2 GB.
func TestImportLimitIsTwoGB(t *testing.T) {
	if importMaxBytes != 2_000_000_000 {
		t.Fatalf("the import limit is %d, want 2,000,000,000", importMaxBytes)
	}
}

// A bare command plans, shows the plan, and without a terminal or --yes stops
// there, naming the command to run; nothing is created.
func TestImportBareCommandShowsThePlanAndSaysWhichCommandToRun(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	out, errs, code := e.run("import", "claude-code", "--latest", "--no-input")
	if code != exitUsage || strings.Contains(errs, "planned first") || !strings.Contains(out, "Import plan") || !strings.Contains(errs, "--yes") {
		t.Fatalf("bare, no input: %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "Nothing was created") || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("a session was created, or it is not said that none was:\n%s", errs)
	}
	nx := nextLines(errs)
	if len(nx) != 1 || !strings.HasSuffix(nx[0], "--yes") {
		t.Fatalf("next: %v", nx)
	}
	// the same without a terminal at all
	if _, _, code = e.run("import", "claude-code", "--latest"); code != exitUsage || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("bare, no terminal: %d", code)
	}
}

// --yes plans and executes in one run, from the one reading of the file.
func TestImportBareYesPlansAndExecutes(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	out, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported Claude Code session") {
		t.Fatalf("--yes: %d\n%s\n%s", code, out, errs)
	}
	if len(e.c.creations) != 1 || len(e.c.applies) != 1 || e.c.count("POST /api/v2/imports") != 2 { // the plan, then the apply
		t.Fatalf("creations %d applies %d log %v", len(e.c.creations), len(e.c.applies), e.c.seenLog())
	}
	if !strings.Contains(out, "Import plan") {
		t.Fatalf("the plan is shown before it runs:\n%s", out)
	}
}

// --json without --yes is the plan's document, byte for byte; with --yes it is
// the execute document.
func TestImportBareJSONIsStable(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	planOut, _, c1 := e.run("import", "claude-code", "--latest", "--plan", "--json")
	bareOut, _, c2 := e.run("import", "claude-code", "--latest", "--json")
	id := regexp.MustCompile(`"request_id":"[^"]*"`)
	planOut, bareOut = id.ReplaceAllString(planOut, ""), id.ReplaceAllString(bareOut, "")
	if c1 != 0 || c2 != 0 || planOut != bareOut {
		t.Fatalf("bare --json differs from --plan --json: %d %d\n%s\n%s", c1, c2, planOut, bareOut)
	}
	if len(e.c.creations) != 0 {
		t.Fatal("a session was created by a plan")
	}
	out, errs, code := e.run("import", "claude-code", "--latest", "--json", "--yes")
	if code != 0 || decodeJSONOut(t, out)["import_id"] == nil && decodeJSONOut(t, out)["session_id"] == nil {
		t.Fatalf("--json --yes: %d\n%s\n%s", code, out, errs)
	}
}

// At a terminal the person is asked, and the answer decides.
func TestImportAsksAtATerminal(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil || runtime.GOOS == "windows" {
		t.Skip("script(1) is not installed")
	}
	for _, answer := range []string{"n", "y"} {
		e := newImportEnv(t)
		e.ready()
		args := []string{"import", "claude-code", "--latest"}
		var cmd *exec.Cmd
		if runtime.GOOS == "darwin" {
			cmd = exec.Command("script", append([]string{"-q", "/dev/null", e.bin}, args...)...)
		} else {
			cmd = exec.Command("script", "-q", "-e", "-c", e.bin+" "+strings.Join(args, " "), "/dev/null")
		}
		cmd.Dir = e.dir
		cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+e.cfg), e.env...)
		// the answer waits in the terminal's input; the pipe stays open until the
		// run ends, because its end would be sent as end-of-file
		in, _ := cmd.StdinPipe()
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		_ = cmd.Start()
		_, _ = in.Write([]byte(answer + "\n"))
		_ = cmd.Wait()
		in.Close()
		got := b.String()
		if !strings.Contains(got, "Import this conversation? [y/N]") {
			t.Fatalf("answer %q: no question:\n%s", answer, got)
		}
		made := len(e.c.creations)
		if answer == "n" && (made != 0 || !strings.Contains(got, "Nothing was created")) {
			t.Fatalf("declined: %d created\n%s", made, got)
		}
		if answer == "y" && (made != 1 || !strings.Contains(got, "Imported Claude Code session")) {
			t.Fatalf("accepted: %d created\n%s", made, got)
		}
	}
}

// Over the limit: plain words, nothing sent, and the one command named parses
// and works as it is given.
func TestImportOverTheLimitSaysTheOneCommandAndItWorks(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != exitConflict || !strings.Contains(errs, "the limit is") || !strings.Contains(errs, "Nothing was sent") {
		t.Fatalf("over the limit: %d\n%s", code, errs)
	}
	nx := nextLines(errs)
	if len(nx) != 1 || !strings.Contains(nx[0], "--brief") || !strings.Contains(nx[0], "--session") {
		t.Fatalf("next: %v\n%s", nx, errs)
	}
	out, errs2, code := e.runNext(nx[0] + " --yes")
	if code != 0 || !strings.Contains(out, "Imported") {
		t.Fatalf("the command as given: %d\n%s\n%s", code, out, errs2)
	}
	// without --yes it is a plan and a stop, never the old refusal
	_, errs3, _ := e.runNext(nx[0])
	if strings.Contains(errs3, "planned first") || strings.Contains(errs3, "unknown flag") {
		t.Fatalf("the command as given is refused:\n%s", errs3)
	}
}

// Every Next: command an import says parses.
func TestEveryImportNextCommandParses(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	var all []string
	_, errs, _ := e.run("import", "claude-code", "--latest", "--no-input")
	all = append(all, nextLines(errs)...)
	digest := planDigest(t, e, "import", "claude-code", "--latest")
	_ = digest
	_, errs, _ = e.run("import", "claude-code", "--latest", "--execute", strings.Repeat("0", 64))
	all = append(all, nextLines(errs)...)
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
	_, errs, _ = e.run("import", "claude-code", "--latest")
	all = append(all, nextLines(errs)...)
	if len(all) < 3 {
		t.Fatalf("only %d next commands: %v", len(all), all)
	}
	for _, cmd := range all {
		_, errs, code := e.runNext(cmd)
		if code == exitUsage && (strings.Contains(errs, "planned first") || strings.Contains(errs, "unknown flag") || strings.Contains(errs, "one of the two")) {
			t.Errorf("%q does not parse: %d\n%s", cmd, code, errs)
		}
		if strings.Contains(cmd, "<") {
			t.Errorf("%q holds a placeholder", cmd)
		}
	}
}

// A conversation written in the last two minutes is open somewhere: the plan says
// what happens to what is said after it.
func TestImportSaysWhenTheConversationIsOpenRightNow(t *testing.T) {
	const note = "This conversation is open in Claude Code right now; what you say after this point stays behind."
	e := newImportEnv(t)
	p := e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	out, _, code := e.run("import", "claude-code", "--latest", "--plan")
	if code != 0 || !strings.Contains(out, note) {
		t.Fatalf("a file just written: %d\n%s", code, out)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	out, _, code = e.run("import", "claude-code", "--latest", "--plan")
	if code != 0 || strings.Contains(out, "open in Claude Code right now") {
		t.Fatalf("a file an hour old: %d\n%s", code, out)
	}
	_ = filepath.Join
	_ = fmt.Sprint
}
