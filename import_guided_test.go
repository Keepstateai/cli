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
	// a real terminal as a person has it: not CI, colour and motion on
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+e.cfg, "COLUMNS=110"), e.env...)
	cmd.Env = append(cmd.Env, "CI=", "NO_COLOR=", "KS_COLOR=", "FORCE_COLOR=1", "TERM=xterm-256color")
	in, _ := cmd.StdinPipe()
	buf := &ptyBuf{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	seen := 0
	lastKey := time.Now()
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
		lastKey = time.Now()
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second): // its own deadline: a hang fails here, not at -timeout
		_ = cmd.Process.Kill()
		e.t.Fatalf("the command did not end within 30 s:\n%s", buf.String())
	}
	e.lastExit = time.Since(lastKey)
	in.Close()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	got := buf.String()
	raw := got
	got = ansiRe.ReplaceAllString(got, "")
	if dir := os.Getenv("KS_IMPORT_RENDER_DIR"); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		name := strings.NewReplacer("/", "_", " ", "_").Replace(e.t.Name())
		_ = os.WriteFile(filepath.Join(dir, name+".raw"), []byte(raw), 0o644)
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
	sent := kitBytes(int64(decodeJSONOut(t, res)["bytes"].(float64)))
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
	if !strings.Contains(got, "Plan digest") || !strings.Contains(flat(got), "Next: ks import claude-code --latest --execute ") || len(e.c.creations) != 0 {
		t.Fatalf("save the plan only: %d\n%s", code, got)
	}
	if !strings.Contains(got, "Total to send") {
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
	for _, want := range []string{"Cleaning", "Uploading", "100%", "Placing", "Imported into session", "Console", "ks agent open main --session session_abc123"} {
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

// Still over what a session can resume (after a trim, or with nothing to trim to): the
// choices are compact first, a written summary, cancel; the reserved place for
// compacting on KeepState is not shown.
func TestGuidedOverTheLimitOffersTheChoices(t *testing.T) {
	run := func(keys string) (*importEnv, string, int) {
		e := newImportEnv(t)
		e.ready()
		e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
		got, code := e.ptyImport([]keyStep{{"Pick a session", "\r"}, {"What now?", keys}}, "import", "claude-code")
		return e, got, code
	}
	// a summary: the plan is a summary and it is imported
	e, got, code := func() (*importEnv, string, int) {
		e := newImportEnv(t)
		e.ready()
		e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
		got, code := e.ptyImport([]keyStep{{"Pick a session", "\r"}, {"What now?", "2\r"}, {"Review the plan", "\r"}}, "import", "claude-code")
		return e, got, code
	}()
	f := flat(got)
	for _, want := range []string{"too big", "This conversation is too big for a session to resume", "a session can resume up to 1.0 KB", "Compact it in Claude Code first (recommended, costs nothing here)", "Bring a written summary instead (--brief)", "Cancel"} {
		if !strings.Contains(f, want) {
			t.Errorf("the screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(f, "Go without compacting") || strings.Contains(f, "on KeepState with") {
		t.Errorf("a choice that does not belong here:\n%s", got)
	}
	if code != 0 || len(e.c.applies) != 1 || asStr(asMap(e.c.applies[0]["declaration"])["mode"]) != "brief" {
		t.Fatalf("to a summary: %d\n%s", code, got)
	}
	// compact first: the instruction, the command to run again, nothing created
	e2, got2, code2 := run("\r")
	if code2 != 0 || len(e2.c.creations) != 0 || !strings.Contains(flat(got2), "/compact Keep: every decision") || !strings.Contains(flat(got2), "Next: ks import claude-code --session "+claudeSID) {
		t.Fatalf("compact first: %d\n%s", code2, got2)
	}
	// cancel
	e3, got3, code3 := run("3\r")
	if code3 != exitUsage || len(e3.c.creations) != 0 || !strings.Contains(got3, "Nothing was created") {
		t.Fatalf("cancel: %d\n%s", code3, got3)
	}
}

func TestGuidedAsksAboutFilesWithBothSizes(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	got, code := e.ptyImport([]keyStep{{"Bring the project files too?", "2\r"}, {"Review the plan", "\r"}}, "import", "claude-code", "--latest")
	if code != 0 || !strings.Contains(got, "conversation only") || !strings.Contains(got, "with the files") || !strings.Contains(flat(got), "the git repository") {
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

// ---- the first-read estimate and the big-conversation choices --------------------------

func fakeEstimate(notice bool) map[string]any {
	return map[string]any{"is_estimate": true, "method": "m", "tokens": 812345, "compacted": false, "basis": "b", "notice_bytes": 30000000, "notice_opus_microusd": 5000000,
		"notice": notice, "notice_reason": "the estimated first read on the dearest model is $6.50, over $5",
		"models": []any{
			map[string]any{"id": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5", "context_tokens": 1000000, "first_read_microusd": 3249380, "later_reply_microusd": 81235},
			map[string]any{"id": "claude-opus-5-5", "name": "Claude Opus 5.5", "context_tokens": 1000000, "first_read_microusd": 6498760, "later_reply_microusd": 162469},
		}}
}

func TestPlanShowsTheServicesEstimateAndTheClientDeclaresACount(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.c.estimate = fakeEstimate(false)
	out, _, code := e.run("import", "claude-code", "--latest", "--plan")
	text := flat(out)
	// every dollar figure is the service's: this client holds no price
	for _, want := range []string{"Estimate", "812,345 tokens", "$3.25 on Claude Sonnet 5.5", "$6.50 on Claude Opus 5.5", "each later reply about $0.081", "Estimates, not quotes"} {
		if code != 0 || !strings.Contains(text, want) {
			t.Fatalf("the plan lacks %q (%d):\n%s", want, code, out)
		}
	}
	if strings.Contains(text, "What now?") || strings.Contains(text, "big conversation") {
		t.Fatalf("under the threshold there is no notice:\n%s", out)
	}
	src := asMap(e.c.plans[0]["source"])
	if jsonInt(src["est_tokens"]) < 15000 {
		t.Fatalf("the declaration carries no count: %v", src)
	}
	// a written summary carries no count
	e.c.imu.Lock()
	e.c.plans = nil
	e.c.imu.Unlock()
	_, _, _ = e.run("import", "claude-code", "--latest", "--brief", "--plan")
	if _, has := asMap(e.c.plans[0]["source"])["est_tokens"]; has {
		t.Fatal("a summary declares a token count")
	}
}

func TestBigConversationChoicesInOrder(t *testing.T) {
	for _, tool := range []string{"claude-code", "codex"} {
		cs := igBigChoices(tool, &importPrep{SummaryChars: 5321}, &importEstimateAnswer{Models: []struct {
			ID                 string `json:"id"`
			Name               string `json:"name"`
			ContextTokens      int64  `json:"context_tokens"`
			FirstReadMicroUSD  int64  `json:"first_read_microusd"`
			LaterReplyMicroUSD int64  `json:"later_reply_microusd"`
			OverContext        bool   `json:"over_context"`
			Note               string `json:"note"`
		}{{Name: "Claude Sonnet 5.5", FirstReadMicroUSD: 3_000_000}}})
		var ids []string
		for _, c := range cs {
			ids = append(ids, c.ID)
		}
		if strings.Join(ids, ",") != "compact,brief,go,cancel" || !strings.Contains(cs[0].Text, "recommended, costs nothing here") || !strings.Contains(cs[1].Text, "5,321") || !strings.Contains(cs[2].Text, "$3.00 on Claude Sonnet 5.5") {
			t.Fatalf("%s: %v", tool, cs)
		}
	}
	// Cursor has no compact
	for _, c := range igBigChoices("cursor", &importPrep{}, &importEstimateAnswer{}) {
		if c.ID == "compact" {
			t.Fatal("Cursor is offered a compaction it does not have")
		}
	}
	for _, tool := range []string{"claude-code", "codex"} {
		text := flat(strings.Join(compactInstruction(tool), " "))
		for _, want := range []string{"/compact", "decision", "open tasks", "file paths", "commands", "error", "standing rules"} {
			if !strings.Contains(strings.ToLower(text), want) {
				t.Errorf("%s: the instruction lacks %q:\n%s", tool, want, text)
			}
		}
	}
	if !strings.Contains(strings.Join(compactInstruction("codex"), " "), "takes no instructions") {
		t.Error("the Codex variant does not say its /compact takes none")
	}
}

func bigEnv(t *testing.T) *importEnv {
	e := newImportEnv(t)
	e.ready()
	e.c.estimate = fakeEstimate(true)
	return e
}

func TestBigConversationCompactFirst(t *testing.T) {
	e := bigEnv(t)
	got, code := e.ptyImport([]keyStep{{"What now?", "\r"}}, "import", "claude-code", "--latest")
	text := flat(got)
	if code != 0 || len(e.c.creations) != 0 || !strings.Contains(text, "/compact Keep: every decision") || !strings.Contains(text, "Next: ks import claude-code --session "+claudeSID) {
		t.Fatalf("compact first: %d\n%s", code, got)
	}
	for _, want := range []string{"This is a big conversation", "Compact it in Claude Code first (recommended, costs nothing here)", "Bring a written summary instead (--brief)", "Go without compacting (first reply about $3.25 on Claude Sonnet 5.5, first reply about $6.50 on Claude Opus 5.5", "Cancel"} {
		if !strings.Contains(text, want) {
			t.Errorf("the notice lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(text, "Compact it in"), strings.Index(text, "Bring a written summary"); i < 0 || j < i {
		t.Fatal("the choices are not in order")
	}
}

func TestBigConversationBriefInstead(t *testing.T) {
	e := bigEnv(t)
	got, code := e.ptyImport([]keyStep{{"What now?", "2\r"}, {"Review the plan", "\r"}}, "import", "claude-code", "--latest")
	if code != 0 || len(e.c.applies) != 1 || asStr(asMap(e.c.applies[0]["declaration"])["mode"]) != "brief" {
		t.Fatalf("a summary instead: %d\n%s", code, got)
	}
}

func TestBigConversationGoWithoutCompactingAndCancel(t *testing.T) {
	e := bigEnv(t)
	got, code := e.ptyImport([]keyStep{{"What now?", "3\r"}, {"Review the plan", "\r"}}, "import", "claude-code", "--latest")
	if code != 0 || len(e.c.applies) != 1 || asStr(asMap(e.c.applies[0]["declaration"])["mode"]) != "conversation" {
		t.Fatalf("go without compacting: %d\n%s", code, got)
	}
	e2 := bigEnv(t)
	got, code = e2.ptyImport([]keyStep{{"What now?", "4\r"}}, "import", "claude-code", "--latest")
	if code != exitUsage || len(e2.c.creations) != 0 || !strings.Contains(got, "Nothing was created") {
		t.Fatalf("cancel: %d\n%s", code, got)
	}
}

func TestBigConversationNoticeIsOnlyAWarningWithoutAQuestion(t *testing.T) {
	e := bigEnv(t)
	out, _, code := e.run("import", "claude-code", "--latest", "--plan")
	if code != 0 || !strings.Contains(flat(out), "This is a big conversation") || strings.Contains(out, "What now?") {
		t.Fatalf("--plan: %d\n%s", code, out)
	}
}

// A real terminal, a slow link: the bar moves, shows the rate and the time
// left, and the last line stays on the screen.
func TestPtyProgressBarShowsRateAndTimeLeft(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(4)
	e.c.putDelay = 350 * time.Millisecond
	got, code := e.ptyImport(nil, "import", "claude-code", "--latest", "--no-files", "--yes")
	if code != 0 {
		t.Fatalf("%d\n%s", code, got)
	}
	for _, want := range []string{"Cleaning the conversation", "Uploading the conversation", "/s", "left", "100%", "Uploaded the conversation", "Placing the conversation on KeepState", "Placed the conversation", "Imported into session"} {
		if !strings.Contains(got, want) {
			t.Errorf("the terminal never showed %q:\n%s", want, got)
		}
	}
	// the screen's last words are the done card, and the bar's closing line is in the scrollback
	if i, j := strings.LastIndex(got, "Uploaded the conversation"), strings.LastIndex(got, "Imported into session"); i < 0 || j < i {
		t.Fatalf("order:\n%s", got)
	}
}
