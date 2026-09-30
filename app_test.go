package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func keysOf(t *testing.T, in string) []keyEvent {
	t.Helper()
	ch := make(chan keyEvent, 64)
	go readKeys(strings.NewReader(in), ch)
	var out []keyEvent
	for k := range ch {
		out = append(out, k)
	}
	return out
}

func TestAppKeysAreDecoded(t *testing.T) {
	got := keysOf(t, "hé\x1b[A\x1b[B\x1b[C\x1b[D\x1bOH\x1b[3~\r\x7f\x03\x04\x15\x17\t")
	want := []keyKind{keyRune, keyRune, keyUp, keyDown, keyRight, keyLeft, keyHome, keyDelete, keyEnter, keyBackspace, keyCtrlC, keyCtrlD, keyCtrlU, keyCtrlW, keyTab}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(got), len(want), got)
	}
	for i, k := range got {
		if k.kind != want[i] {
			t.Errorf("key %d: %v, want %v", i, k.kind, want[i])
		}
	}
	if got[1].r != 'é' {
		t.Errorf("a UTF-8 rune was split: %q", got[1].r)
	}
}

// A lone Esc is the Esc key (the interrupt), not the start of a sequence
// that swallows the next key.
func TestAppLoneEscIsTheEscKey(t *testing.T) {
	ch := make(chan keyEvent, 8)
	r, w := ioPipe()
	go readKeys(r, ch)
	w.Write([]byte{0x1b})
	select {
	case k := <-ch:
		if k.kind != keyEsc {
			t.Fatalf("a lone ESC read as %v", k.kind)
		}
	case <-time.After(time.Second):
		t.Fatal("a lone ESC never arrived as a key")
	}
	w.Write([]byte("a"))
	if k := <-ch; k.kind != keyRune || k.r != 'a' {
		t.Fatalf("the key after Esc: %+v", k)
	}
	w.Close()
}

// A bracketed paste is ONE event, whatever it contains: a pasted "/quit" or
// a line of "y" is text, never a command or an approval.
func TestAppPasteIsOneEventAndNeverACommand(t *testing.T) {
	got := keysOf(t, "\x1b[200~/quit\r\ny\r\nrun the tests\x1b[201~")
	if len(got) != 1 || got[0].kind != keyPaste || got[0].text != "/quit\ny\nrun the tests" {
		t.Fatalf("paste: %+v", got)
	}
}

// Lines printed while the live region is up go ABOVE it, and the region is
// redrawn below them: nothing printed tears the input or the status.
func TestAppScreenPrintsAboveTheLiveRegion(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b)
	s.width = func() int { return 40 }
	s.update(func() { s.status = "● Ready · /help"; s.input = []rune("hello"); s.cursor = 5 })
	b.Reset()
	s.Print("main  Found it.")
	out := b.String()
	iLine := strings.Index(out, "main  Found it.")
	iStatus := strings.LastIndex(out, "● Ready")
	iInput := strings.LastIndex(out, "> hello")
	if iLine < 0 || iStatus < iLine || iInput < iLine {
		t.Fatalf("the line is not above the redrawn live region: %q", out)
	}
	if !strings.Contains(out[:iLine], "\x1b[J") {
		t.Fatalf("the live region was not erased before the line: %q", out)
	}
}

func TestAppInputScrollsToKeepTheCursorVisible(t *testing.T) {
	s := newScreen(&bytes.Buffer{})
	s.input = []rune(strings.Repeat("x", 100) + "END")
	s.cursor = len(s.input)
	line, col := s.inputLine(40)
	if !strings.HasSuffix(line, "END") || displayWidth(line) > 40 || col > 40 {
		t.Fatalf("line %q col %d", line, col)
	}
	s.input = []rune("a\nb")
	if line, _ := s.inputLine(40); !strings.Contains(line, "a↵b") {
		t.Fatalf("a pasted line break is not shown: %q", line)
	}
}

func TestAppEditing(t *testing.T) {
	s := newScreen(&bytes.Buffer{})
	for _, r := range "run the tests" {
		s.edit(keyEvent{kind: keyRune, r: r})
	}
	s.edit(keyEvent{kind: keyCtrlW})
	if string(s.input) != "run the " {
		t.Fatalf("Ctrl-W: %q", string(s.input))
	}
	s.edit(keyEvent{kind: keyHome})
	s.edit(keyEvent{kind: keyDelete})
	if string(s.input) != "un the " {
		t.Fatalf("Home+Delete: %q", string(s.input))
	}
	if s.edit(keyEvent{kind: keyEnter}) {
		t.Fatal("Enter was consumed as an edit")
	}
	if got := s.take(); got != "un the " || !s.inputEmpty() {
		t.Fatalf("take: %q", got)
	}
}

// The status line says what the service says: STALE only when the service
// does, "unchanged" for an old resting report, spend unavailable (never $0),
// and who controls.
func TestAppStatusLine(t *testing.T) {
	var v liveViewDoc
	age := int64(300)
	q, p := 1, 2
	v.Status = &struct {
		Label       string `json:"label"`
		ObservedAt  string `json:"observed_at"`
		AgeSeconds  *int64 `json:"age_seconds"`
		Stale       bool   `json:"stale"`
		StaleAfterS int    `json:"stale_after_seconds"`
		Note        string `json:"note,omitempty"`
	}{Label: "Ready", AgeSeconds: &age}
	v.Footer.Queued, v.Footer.PendingApprovals = &q, &p
	line := appStatusLine(v, true)
	for _, want := range []string{"● Ready · unchanged 5m", "queue 1", "2 waiting for you", "not saved yet", "you control"} {
		if !strings.Contains(line, want) {
			t.Errorf("status %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "$0") || strings.Contains(line, "STALE") {
		t.Errorf("status %q invents a figure or a staleness", line)
	}
	v.Status.Stale = true
	if line := appStatusLine(v, false); !strings.Contains(line, "STALE") || !strings.Contains(line, "watching") {
		t.Errorf("stale, watching: %q", line)
	}
}

func TestAppApprovalBox(t *testing.T) {
	if approvalBox(nil, 80) != nil {
		t.Fatal("a box with nothing waiting")
	}
	rows := approvalBox([]approvalRow{{ID: "apr_1", Kind: "tool", Summary: "Bash: git push origin main", RequestedForTask: "task_1", ExpiresAt: "2026-09-29T12:00:00Z"}, {ID: "apr_2"}}, 60)
	all := strings.Join(rows, "\n")
	for _, want := range []string{"Permission", "[y] allow once", "[n] deny", "1 of 2 waiting", "task_1"} {
		if !strings.Contains(all, want) {
			t.Errorf("box lacks %q:\n%s", want, all)
		}
	}
	for _, r := range rows {
		if displayWidth(r) > 60 {
			t.Errorf("row wider than the terminal: %q", r)
		}
	}
}

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// The app end to end, in a pseudo-terminal, typed like a person: home lists
// the agent, Enter opens it, a typed line is sent as an instruction, Esc
// interrupts, a waiting permission request is a prompt that y approves (as
// SHOWN), /help answers, Ctrl-C twice leaves (releasing control) and q quits.
func TestAppEndToEndInAPseudoTerminal(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to drive a pseudo-terminal")
	}
	c := newAgentCtl()
	c.addApproval("apr_push1", "git push origin main", 1, "hash-push-1")
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := map[string]string{"XDG_CONFIG_HOME": cfg, "KS_HTTP_TIMEOUT_MS": "2000", "TERM": "xterm-256color"}
	spec := map[string]any{"argv": []string{bin}, "env": env, "timeout": 15, "steps": [][]string{
		{"Your agents:", "\r"},
		{"[y] allow once", "fix the login test\r"},
		{"task tsk_new1", "\x1b"},
		{"Esc:", "y"},
		{"approved", "/help\r"},
		{"/take", "/agent list\r"},
		{"$ ks agent list --session", "/nosuch\r"},
		{"is not a ks command", "/switch twin\r"},
		{"2 agents are named \"twin\"", "/switch agt_twin0001\r"},
		{"── twin", "\x03"},
		{"press Ctrl-C again", "\x03"},
		{"left twin", "q"},
	}}
	f := filepath.Join(t.TempDir(), "spec.json")
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(py, "testdata/ptydrive.py", f).CombinedOutput()
	if err != nil {
		t.Fatalf("the app did not behave as a person expects: %v\n%s", err, out)
	}
	t.Logf("screen transcript:\n%s", out)
	c.mu.Lock()
	defer c.mu.Unlock()
	joined := strings.Join(c.requests, "\n")
	for _, want := range []string{"POST /api/v2/agents/", "/decision", "DELETE /api/v2/control-leases/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the app never sent %q:\n%s", want, joined)
		}
	}
	if len(c.taskBodies) != 1 || !strings.Contains(c.taskBodies[0], "fix the login test") || !strings.Contains(c.taskBodies[0], `"origin":"live"`) {
		t.Errorf("the instruction sent: %v", c.taskBodies)
	}
	if len(c.decisionBodies) != 1 || !strings.Contains(c.decisionBodies[0], "approve") {
		t.Errorf("the decision sent: %v", c.decisionBodies)
	}
}

func TestAppConversationReadsLikeAChat(t *testing.T) {
	ev := func(payload string) journalEvent {
		return journalEvent{SubjectType: "agent", SubjectID: "agent_1", Payload: json.RawMessage(payload)}
	}
	cases := []struct {
		payload, want string
		shown         bool
	}{
		{`{"type":"agent.transcript","kind":"assistant_text","text":"Found it.\nPatching now."}`, "main   Found it.\n       Patching now.", true},
		{`{"type":"agent.transcript","kind":"tool_started","tool_name":"Bash","text":"go test ./..."}`, "  ⎿ Bash: go test ./...", true},
		{`{"type":"agent.transcript","kind":"tool_finished","tool_name":"Bash","text":"ok"}`, "  ✓ Bash", true},
		{`{"type":"agent.transcript","kind":"tool_finished","tool_name":"Bash","failed":true,"text":"exit 1"}`, "  ✗ Bash: exit 1", true},
		// this person's own message was shown on Enter; not twice
		{`{"type":"agent.transcript","kind":"instruction","author_type":"human","author_id":"acct_me","text":"fix it"}`, "", false},
		{`{"type":"agent.transcript","kind":"instruction","author_type":"agent","author_id":"agent_2","text":"check this"}`, "agent agent_2", true},
		{`{"type":"task.finished","state":"failed","summary":"the runner stopped"}`, "✗ failed", true},
		{`{"type":"agent.activity","activity":"working"}`, "", false},
		{`{"type":"approval.requested"}`, "", false},
	}
	for _, c := range cases {
		got, ok := appEventLine(ev(c.payload), "main", "acct_me")
		if ok != c.shown || (c.shown && !strings.Contains(got, c.want)) {
			t.Errorf("%s: %q shown=%v, want %q shown=%v", c.payload, got, ok, c.want, c.shown)
		}
	}
}

// Parity: every ks command runs as a / command in the app, or is excluded
// with a stated reason; a command added later cannot be silently missing.
func TestAppEveryCommandIsASlashCommand(t *testing.T) {
	ctx := slashContext{sessionID: "session_rec1", agentName: "main"}
	for _, c := range registry {
		if c.Group {
			continue
		}
		argv, got, err := slashArgv("/"+c.Name()+" --help", ctx)
		if why, excluded := slashExcluded[c.Name()]; excluded {
			if err == nil || !strings.Contains(err.Error(), why) || strings.TrimSpace(why) == "" {
				t.Errorf("/%s is excluded without saying why: %v", c.Name(), err)
			}
			continue
		}
		if err != nil || got != c || strings.Join(argv, " ") != c.Name()+" --help" {
			t.Errorf("/%s --help runs %v (%v), want %q", c.Name(), argv, err, c.Name()+" --help")
		}
	}
	for short, path := range slashShortcuts {
		if c, _, _ := lookup(registry, path); c == nil || c.Group {
			t.Errorf("/%s names %v, which is not a command", short, path)
		}
		for _, c := range registry {
			if c.Path[0] == short {
				t.Errorf("the shortcut /%s hides the ks command or group %q", short, c.Path[0])
				break
			}
		}
		if _, clash := windowCommands[short]; clash {
			t.Errorf("/%s is both a shortcut and a window command", short)
		}
	}
	for w := range windowCommands {
		if c, _, _ := lookup(registry, []string{w}); c != nil {
			t.Errorf("the window command /%s hides the ks command %q", w, c.Name())
		}
	}
}

// The window fills in its own session and agent, and only where the command
// takes them and they were not given.
func TestAppSlashFillsInTheWindowsContext(t *testing.T) {
	ctx := slashContext{sessionID: "session_rec1", agentName: "main"}
	for _, c := range []struct{ line, want string }{
		{"/agent status", "agent status main --session session_rec1"},
		{"/stop", "agent stop main --session session_rec1"},
		{"/queue", "agent queue show main --session session_rec1"},
		{"/agent status twin", "agent status twin --session session_rec1"},
		{"/task list --session other1", "task list --session other1"},
		{"/usage", "session usage session_rec1"},
		{"/keys", "key list"},
		{"/adviser connect helper --confirm s/helper", "adviser connect helper --confirm s/helper --agent main --session session_rec1"},
		{"/tasks", "task list --agent main --session session_rec1"},
		{"/adviser list --agent other", "adviser list --agent other --session session_rec1"},
		{`/agent tell main "run the tests"`, "agent tell main run the tests --session session_rec1"},
	} {
		argv, _, err := slashArgv(c.line, ctx)
		if err != nil || strings.Join(argv, " ") != c.want {
			t.Errorf("%s: %q (%v), want %q", c.line, strings.Join(argv, " "), err, c.want)
		}
	}
	for _, line := range []string{"/agent open main", "/attach x", "/login", "/agent", "/nosuch", `/agent tell main "unclosed`} {
		if argv, _, err := slashArgv(line, ctx); err == nil {
			t.Errorf("%s runs %v; it must be refused with a reason", line, argv)
		}
	}
}

func TestAppTabCompletesSlashCommands(t *testing.T) {
	if got, _ := completeSlash("/que"); got != "/queue " {
		t.Errorf("/que -> %q", got)
	}
	got, cands := completeSlash("/agent q")
	if !strings.HasPrefix(got, "/agent queue ") || len(cands) < 2 {
		t.Errorf("/agent q -> %q %v", got, cands)
	}
	if got, cands := completeSlash("hello"); got != "hello" || cands != nil {
		t.Errorf("plain text was completed: %q %v", got, cands)
	}
}

func TestAppWhileYouWereAway(t *testing.T) {
	ev := func(seq int64, payload string) journalEvent {
		return journalEvent{StreamSeq: seq, Payload: json.RawMessage(payload)}
	}
	events := []journalEvent{
		ev(10, `{"type":"task.finished","state":"succeeded"}`), // before: not counted
		ev(11, `{"type":"task.finished","state":"succeeded"}`),
		ev(12, `{"type":"task.finished","state":"succeeded"}`),
		ev(13, `{"type":"task.finished","state":"failed"}`),
		ev(14, `{"type":"result.recorded","name":"report.md"}`),
		ev(15, `{"type":"result.recorded","name":"ks-changeset.json"}`),
	}
	s := awaySummary(events, 10, 1, "2026-09-29T14:02:00Z")
	for _, want := range []string{"While you were away", "2 instructions finished", "1 did not finish", "1 result (/results)", "1 permission request waits for you"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q lacks %q", s, want)
		}
	}
	if awaySummary(events, 15, 0, "") != "" {
		t.Error("a summary with nothing to say")
	}
	// the mark is per control plane and agent, only ever moves forward
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	cr := hostedCreds{CTL: "https://ctl.test"}
	markSeen(cr, "agt_1", 20)
	markSeen(cr, "agt_1", 15)
	if m, ok := lastSeen(cr, "agt_1"); !ok || m.Seq != 20 {
		t.Fatalf("the mark moved back or was lost: %+v %v", m, ok)
	}
	if _, ok := lastSeen(hostedCreds{CTL: "https://other"}, "agt_1"); ok {
		t.Fatal("a mark leaked across control planes")
	}
}

func TestAppAgentsThatNeedYouComeFirst(t *testing.T) {
	for _, a := range []string{"waiting_approval", "recovery_required", "failed"} {
		if !needsYou(a) {
			t.Errorf("%s does not need you", a)
		}
	}
	for _, a := range []string{"ready", "working", "starting", "paused"} {
		if needsYou(a) {
			t.Errorf("%s needs you", a)
		}
	}
	line := homeLine(homeRow{sess: inventoryRow{ShortID: "3f2a1c", Name: "checkout"}, agent: &agentRow{Name: "main", Activity: "waiting_approval"}}, false, 100)
	if !strings.Contains(line, "⚑ main") {
		t.Errorf("an agent waiting on you is not marked: %q", line)
	}
}

func TestAppFillsTheAgentOnlyWhereItNamesAnExistingAgent(t *testing.T) {
	ctx := slashContext{sessionID: "session_rec1", agentName: "main"}
	for _, c := range []struct{ line, want string }{
		{`/agent tell "run the tests"`, "agent tell main run the tests --session session_rec1"},
		{"/agent rename helper", "agent rename main helper --session session_rec1"},
		{"/agent create helper", "agent create helper --session session_rec1"},
	} {
		argv, _, err := slashArgv(c.line, ctx)
		if err != nil || strings.Join(argv, " ") != c.want {
			t.Errorf("%s: %q (%v), want %q", c.line, strings.Join(argv, " "), err, c.want)
		}
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 2 * time.Minute: "2m", 65 * time.Minute: "1h 5m", 26 * time.Hour: "1d 2h"} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
