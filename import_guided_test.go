//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type ptyBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *ptyBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *ptyBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// keyStep waits for text to appear on the screen, then types keys.
type keyStep struct{ waitFor, keys string }

const (
	keyUpSeq   = "\x1b[A"
	keyDownSeq = "\x1b[B"
	keyEscSeq  = "\x1b"
)

// ptyImport runs ks under script(1), which gives it a real terminal, and
// types the scripted keys as each screen appears. It answers what the
// terminal received and the exit code.
func (e *importEnv) ptyImport(steps []keyStep, args ...string) (string, int) {
	e.t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		e.t.Skip("script(1) is not installed")
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("script", append([]string{"-q", "/dev/null", e.bin}, args...)...)
	} else {
		cmd = exec.Command("script", "-q", "-e", "-c", e.bin+" "+strings.Join(args, " "), "/dev/null")
	}
	cmd.Dir = e.dir
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+e.cfg, "COLUMNS=110"), e.env...)
	in, _ := cmd.StdinPipe()
	buf := &ptyBuf{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	seen := 0
	for _, st := range steps {
		deadline := time.Now().Add(60 * time.Second)
		for {
			s := buf.String()
			if i := strings.Index(s[seen:], st.waitFor); i >= 0 {
				seen += i + len(st.waitFor)
				break
			}
			select {
			case err := <-done:
				e.t.Fatalf("the command ended before %q appeared (%v):\n%s", st.waitFor, err, buf.String())
			default:
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				e.t.Fatalf("%q never appeared:\n%s", st.waitFor, buf.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(150 * time.Millisecond) // the screen is up and the program is reading keys
		_, _ = in.Write([]byte(st.keys))
	}
	err := <-done
	in.Close()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	got := buf.String()
	if dir := os.Getenv("KS_IMPORT_RENDER_DIR"); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		name := strings.NewReplacer("/", "_", " ", "_").Replace(e.t.Name())
		_ = os.WriteFile(filepath.Join(dir, name+".raw"), []byte(got), 0o644)
	}
	return got, code
}

func TestGuidedCancelAtTheList(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	got, code := e.ptyImport([]keyStep{{"Pick a session", keyEscSeq}}, "import")
	if code != exitUsage || !strings.Contains(got, "Nothing was created") || len(e.c.creations) != 0 || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("cancel: %d\n%s", code, got)
	}
	if !strings.Contains(got, "claude-code") || !strings.Contains(got, "open now") || !strings.Contains(got, "“") {
		t.Fatalf("the list lacks the tool, when and first prompt:\n%s", got)
	}
}

func TestGuidedListShowsWhatEachWouldSend(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	got, _ := e.ptyImport([]keyStep{{"Pick a session", keyEscSeq}}, "import", "claude-code")
	// the size after cleaning, not the size on disk
	res, _, _ := e.run("import", "claude-code", "--latest", "--plan", "--json")
	sent := barBytes(int64(decodeJSONOut(t, res)["bytes"].(float64)))
	if !strings.Contains(got, sent) {
		t.Fatalf("the row does not show %s, what would go up:\n%s", sent, got)
	}
}

func TestGuidedPlanOnlyWithNoProjectFolder(t *testing.T) {
	e := newImportEnv(t)
	e.ready() // the session's folder (/Users/alice/projects/shop) is not on this machine
	got, code := e.ptyImport([]keyStep{{"Review the plan", keyDownSeq + "\r"}}, "import", "claude-code", "--latest")
	if code != 0 || strings.Contains(got, "Bring the project files too?") {
		t.Fatalf("no project folder, so no question about files: %d\n%s", code, got)
	}
	if !strings.Contains(got, "plan digest: ") || !strings.Contains(got, "Next: ks import claude-code --latest --execute ") || len(e.c.creations) != 0 {
		t.Fatalf("save the plan only: %d\n%s", code, got)
	}
	if !strings.Contains(got, "Total to send:") {
		t.Fatalf("the plan card lacks the total:\n%s", got)
	}
}

func TestGuidedImportNow(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(2)
	got, code := e.ptyImport([]keyStep{{"Pick a session", "\r"}, {"Review the plan", "\r"}}, "import")
	if code != 0 || len(e.c.creations) != 1 || len(e.c.applies) != 1 {
		t.Fatalf("import now: %d (%d sessions)\n%s", code, len(e.c.creations), got)
	}
	for _, want := range []string{"Cleaning", "Uploading", "100%", "Placing", "Imported Claude Code session", "console: ", "ks agent open main --session session_abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("the screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Uploading  ") && !strings.Contains(got, "100%  ") {
		t.Errorf("the bar does not end at 100%%")
	}
}

func TestGuidedCancelAtTheReview(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	got, code := e.ptyImport([]keyStep{{"Review the plan", keyDownSeq + keyDownSeq + "\r"}}, "import", "claude-code", "--latest")
	if code != exitUsage || !strings.Contains(got, "Nothing was created") || len(e.c.creations) != 0 {
		t.Fatalf("cancel at the review: %d\n%s", code, got)
	}
}

func TestGuidedOverTheLimitOffersTheSummary(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
	got, code := e.ptyImport([]keyStep{{"Pick a session", "\r"}, {"Import a written summary of it instead?", "2\r"}, {"Review the plan", "\r"}}, "import", "claude-code")
	if !strings.Contains(got, "too big") {
		t.Fatalf("the row is not marked:\n%s", got)
	}
	if code != 0 || len(e.c.applies) != 1 || asStr(asMap(e.c.applies[0]["declaration"])["mode"]) != "brief" {
		t.Fatalf("over the limit, to a summary: %d\n%s", code, got)
	}
	// declining the summary changes nothing
	e2 := newImportEnv(t)
	e2.ready()
	e2.env = append(e2.env, "KS_IMPORT_MAX_BYTES=1000")
	got, code = e2.ptyImport([]keyStep{{"Pick a session", "\r"}, {"Import a written summary of it instead?", "\r"}}, "import", "claude-code")
	if code != exitUsage || len(e2.c.creations) != 0 || !strings.Contains(got, "Nothing was created") {
		t.Fatalf("declined the summary: %d\n%s", code, got)
	}
}

func TestGuidedAsksAboutFilesWithBothSizes(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	got, code := e.ptyImport([]keyStep{{"Bring the project files too?", "2\r"}, {"Review the plan", "\r"}}, "import", "claude-code", "--latest")
	if code != 0 || !strings.Contains(got, "conversation only") || !strings.Contains(got, "with the files") || !strings.Contains(got, "Files: the git repository") {
		t.Fatalf("with the files: %d\n%s", code, got)
	}
	if e.c.upload("folder") == nil {
		t.Fatalf("the files were not uploaded:\n%s", got)
	}
}

func TestGuidedFlagsSkipTheirSteps(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	// --no-files skips the question, --yes skips the review
	got, code := e.ptyImport(nil, "import", "claude-code", "--latest", "--no-files", "--yes")
	if code != 0 || strings.Contains(got, "Bring the project files too?") || strings.Contains(got, "Review the plan") || !strings.Contains(got, "Imported") {
		t.Fatalf("--no-files --yes: %d\n%s", code, got)
	}
	if e.c.upload("folder") != nil {
		t.Fatal("files went up although --no-files was given")
	}
}

func TestGuidedNeverAsksWithoutATerminalOrWithNoInput(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	for _, args := range [][]string{{"import"}, {"import", "claude-code"}, {"import", "claude-code", "--no-input"}} {
		_, errs, code := e.run(args...)
		if code != exitUsage || !strings.Contains(errs, "--latest") && !strings.Contains(errs, "--yes") || len(e.c.creations) != 0 {
			t.Fatalf("%v: %d\n%s", args, code, errs)
		}
	}
	// at a terminal with --no-input: the same, and no question
	got, code := e.ptyImport(nil, "import", "claude-code", "--no-input")
	if code != exitUsage || strings.Contains(got, "Pick a session") || !strings.Contains(got, "--latest") {
		t.Fatalf("--no-input at a terminal: %d\n%s", code, got)
	}
}
