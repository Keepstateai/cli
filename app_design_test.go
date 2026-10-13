package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// words reads terminal output as a person reads it: no styles, no box
// edges, one space between words.
func words(s string) string {
	s = ansiSeq.ReplaceAllString(s, "")
	s = strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// Prose is wrapped at word boundaries and never cut: every word arrives,
// no line is wider than asked, and a colour open at a break is closed at
// the end of the line and opened again on the next.
func TestAppWrapNeverCutsProse(t *testing.T) {
	text := "resuming starts the session's machine again, and runtime is charged from the moment it runs; the session's usage shows what has been charged so far"
	for _, width := range []int{20, 33, 60, 200} {
		lines := wrapText(text, width)
		for _, l := range lines {
			if displayWidth(l) > width {
				t.Errorf("width %d: a line of %d columns: %q", width, displayWidth(l), l)
			}
		}
		if got := strings.Join(lines, " "); got != text {
			t.Errorf("width %d: the words changed:\n%q\n%q", width, got, text)
		}
	}
	saved := th
	defer func() { th = saved }()
	th = appTheme{depth: color256}
	lines := wrapText(th.warn("one two three four five six"), 10)
	for i, l := range lines {
		if !strings.HasPrefix(l, "\x1b[") || !strings.HasSuffix(l, "\x1b[0m") {
			t.Errorf("line %d does not carry its colour: %q", i, l)
		}
	}
	if long := wrapText(strings.Repeat("x", 25), 10); len(long) != 3 || displayWidth(long[0]) != 10 {
		t.Errorf("a word longer than a line: %q", long)
	}
}

// Colour off prints the same words as colour on.
func TestAppSameWordsWithColourOff(t *testing.T) {
	saved := th
	defer func() { th = saved }()
	sess := inventoryRow{ShortID: "5335fa", Name: "api"}
	a := agentRow{Name: "dev1", Activity: "ready"}
	w := &agentWindow{Runtime: &openRuntime{State: "parked", Note: "the session is saved and stopped",
		Actions: []openAction{{Action: "resume_session", Discloses: "runtime is charged from the moment it runs"}}}}
	render := func() string {
		c, _ := parkedCard(sess, a, w, false, false)
		ap := approvalBox([]approvalRow{{ID: "apr_1", Kind: "shell", Summary: "git push"}}, 80)
		return strings.Join(c.render(80), "\n") + strings.Join(ap, "\n") + appHelp("dev1", 80)
	}
	th = appTheme{}
	off := render()
	th = appTheme{depth: colorTrue}
	on := render()
	if strings.Contains(off, "\x1b[") {
		t.Error("colour off printed a colour")
	}
	if ansiSeq.ReplaceAllString(on, "") != off {
		t.Errorf("colour on and off say different things:\n%s\n----\n%s", ansiSeq.ReplaceAllString(on, ""), off)
	}
}

// The paused card says the state in the client's words, says opening did not
// wake it, and shows the service's billing disclosure, once, before r is
// offered; the service's own note is details. Out of credit has its own card:
// add credit, then resume; adding credit alone does not resume.
func TestAppPausedCards(t *testing.T) {
	sess := inventoryRow{ShortID: "5335fa", Name: "api"}
	a := agentRow{Name: "dev1", Activity: "working"}
	disclose := "resuming starts the session's runtime again, and runtime is charged from the moment it runs"
	note := "the session is saved and stopped. Opening did not wake it; nothing runs"
	w := &agentWindow{Runtime: &openRuntime{State: "parked", Note: note,
		Actions: []openAction{{Action: "resume_session", Label: "Resume session", Discloses: disclose}}}}
	c, resumable := parkedCard(sess, a, w, false, false)
	got := words(strings.Join(c.render(80), "\n"))
	for _, want := range []string{"Session paused", "dev1's session is paused. Opening it did not wake it.", "Billing Resuming starts the session's runtime again", "r resume (billing starts)", "d details"} {
		if !strings.Contains(got, want) {
			t.Errorf("paused card lacks %q:\n%s", want, got)
		}
	}
	if !resumable || strings.Count(got, "charged from the moment it runs") != 1 || strings.Contains(got, "nothing runs") {
		t.Errorf("the disclosure once, the note only on d:\n%s", got)
	}
	if strings.Index(got, "Billing") > strings.Index(got, "r resume") {
		t.Errorf("the disclosure is not shown before the key that resumes:\n%s", got)
	}
	c, _ = parkedCard(sess, a, w, false, true)
	if got := words(strings.Join(c.render(80), "\n")); !strings.Contains(got, "Details The session is saved and stopped") {
		t.Errorf("d does not show the service's note:\n%s", got)
	}
	// a state this client does not know: the service's note is the explanation
	w.Runtime.State = "hibernating"
	c, _ = parkedCard(sess, a, w, false, false)
	if got := words(strings.Join(c.render(80), "\n")); !strings.Contains(got, "unknown (hibernating)") || !strings.Contains(got, "Details") {
		t.Errorf("an unknown state:\n%s", got)
	}
	w.Runtime.State = "parked"
	c, _ = parkedCard(sess, a, w, true, false)
	got = words(strings.Join(c.render(80), "\n"))
	for _, want := range []string{"Out of credit", "your account ran out of credit", "Adding credit alone does not resume it", "Last activity Working, as it was when it paused", "Billing"} {
		if !strings.Contains(got, want) {
			t.Errorf("out-of-credit card lacks %q:\n%s", want, got)
		}
	}
}

// A funds-paused agent never reads as running on the home list: its frozen
// activity is not its state, and it is listed with what needs you.
func TestAppHomeNeverShowsAFundsPausedAgentAsRunning(t *testing.T) {
	r := homeRow{sess: inventoryRow{ShortID: "9c41e0", Name: "billing", RuntimeState: "parked"},
		agent: &agentRow{Name: "ops", Activity: "working", SessionRuntime: &sessionRuntimeDoc{State: "parked", Reason: parkReasonFunds}}}
	icon, word, _ := homeState(r)
	if word != "Out of credit" || icon != iconPaused || !r.rowNeedsYou() {
		t.Errorf("funds-paused row: %s %s needs-you=%v", icon, word, r.rowNeedsYou())
	}
	line := words(homeLine(r, false, 100))
	if strings.Contains(line, "Working") || !strings.Contains(line, "⏸ ops") {
		t.Errorf("home line: %q", line)
	}
	parked := homeRow{sess: inventoryRow{ShortID: "5335fa", RuntimeState: "parked"}, agent: &agentRow{Name: "dev1", Activity: "waiting_approval",
		SessionRuntime: &sessionRuntimeDoc{State: "parked"}}}
	if _, word, _ := homeState(parked); word != "Paused" || parked.rowNeedsYou() {
		t.Errorf("a paused session's agent reads %q (needs you %v)", word, parked.rowNeedsYou())
	}
	plain := homeRow{sess: inventoryRow{ShortID: "697ca7", Name: "697ca7", RuntimeState: "running"}}
	if plain.rank() != 2 || !strings.Contains(words(homeLine(plain, false, 100)), "○ 697ca7") {
		t.Errorf("a plain session: rank %d %q", plain.rank(), homeLine(plain, false, 100))
	}
}

// Opening a paused session never wakes it, whatever keys are pressed but r;
// r resumes it, once, after the card with the billing disclosure was drawn.
func TestAppOpeningAPausedSessionNeverWakesIt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var mu sync.Mutex
	var resumes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/resume") {
			mu.Lock()
			resumes = append(resumes, r.URL.Path)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"runtime_state": "resuming"}})
			return
		}
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "ks_not_found", "message": "no"}})
	}))
	defer srv.Close()
	cr := hostedCreds{CTL: srv.URL, Token: "t"}
	sess := inventoryRow{ShortID: "5335fa", Name: "api", RecordID: "session_park"}
	a := agentRow{ID: "agt_1", Name: "dev1"}
	w := &agentWindow{Runtime: &openRuntime{State: "parked", Note: "saved and stopped",
		Actions: []openAction{{Action: "resume_session", Discloses: "runtime is charged from the moment it runs"}}}}
	run := func(ks ...keyEvent) string {
		var b bytes.Buffer
		scr := newScreen(&b)
		scr.width = func() int { return 90 }
		keys := make(chan keyEvent, len(ks))
		for _, k := range ks {
			keys <- k
		}
		close(keys)
		appParked(cr, scr, keys, sess, a, w)
		return b.String()
	}
	run(keyEvent{kind: keyRune, r: 'd'}, keyEvent{kind: keyRune, r: 'x'}, keyEvent{kind: keyEsc})
	mu.Lock()
	n := len(resumes)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("opening woke the session: %v", resumes)
	}
	out := run(keyEvent{kind: keyRune, r: 'r'})
	mu.Lock()
	n = len(resumes)
	mu.Unlock()
	shown := words(out)
	if n != 1 || strings.Index(shown, "charged from the moment it runs") < 0 || strings.Index(shown, "charged from the moment it runs") > strings.Index(shown, "Resumed api") {
		t.Fatalf("r resumed %d time(s); the disclosure must come first:\n%s", n, shown)
	}
}

// The needs-attention card in the conversation never reads as ready or
// finished, and says how to decide.
func TestAppRecoveryCardNeverReadsReady(t *testing.T) {
	e := journalEvent{SubjectID: "tsk_mig", Payload: json.RawMessage(`{"type":"task.finished","state":"reconciliation_required","summary":"the runner stopped"}`)}
	got := words(strings.Join(appRecoveryCard(e, "main", "abc123"), "\n"))
	for _, want := range []string{"Needs attention", "It is not ready, and the instruction did not finish", "To decide /queue (ks agent queue show main --session abc123)"} {
		if !strings.Contains(got, want) {
			t.Errorf("recovery card lacks %q:\n%s", want, got)
		}
	}
	if line, ok := appEventLine(e, "main", ""); ok {
		t.Errorf("the stopped instruction also printed a line: %q", line)
	}
	act := journalEvent{Payload: json.RawMessage(`{"type":"agent.activity","activity":"recovery_required"}`)}
	if got := words(strings.Join(appRecoveryCard(act, "main", "abc123"), "\n")); !strings.Contains(got, "not ready") {
		t.Errorf("activity recovery: %s", got)
	}
}

// / commands are case-insensitive, their arguments are not, and one that is
// not a command suggests the nearest.
func TestAppSlashCommandsIgnoreCase(t *testing.T) {
	ctx := slashContext{sessionID: "session_rec1", agentName: "main"}
	for line, want := range map[string]string{
		"/Stop":                            "agent stop main --session session_rec1",
		"/Agent List":                      "agent list --session session_rec1",
		`/agent tell main "Run The Tests"`: "agent tell main Run The Tests --session session_rec1",
	} {
		argv, _, err := slashArgv(line, ctx)
		if err != nil || strings.Join(argv, " ") != want {
			t.Errorf("%s: %q (%v), want %q", line, strings.Join(argv, " "), err, want)
		}
	}
	if _, _, err := slashArgv("/stpo", ctx); err == nil || !strings.Contains(err.Error(), "Did you mean /stop?") {
		t.Errorf("/stpo: %v", err)
	}
}

// A confirmation a / command asks for becomes the command that confirms it.
func TestAppConfirmedCommand(t *testing.T) {
	for _, c := range []struct {
		argv []string
		next string
		want string
	}{
		{[]string{"result", "apply", "r1"}, "ks result apply r1 --confirm 3f2a1c9b8d7e", "result apply r1 --confirm 3f2a1c9b8d7e"},
		{[]string{"adviser", "connect", "helper"}, "pass --confirm s/helper", "adviser connect helper --confirm s/helper"},
		{[]string{"agent", "view"}, `--confirm "Save and pause"`, "agent view --confirm Save and pause"},
		{[]string{"x"}, "retry later", ""},
	} {
		if got := strings.Join(confirmedArgv(c.argv, c.next), " "); got != c.want {
			t.Errorf("%v + %q: %q, want %q", c.argv, c.next, got, c.want)
		}
	}
	if got := asSlash("ks session delete 6e766c --execute p1 --confirm 6e766c (or ks session list)"); got != "/session delete 6e766c --execute p1 --confirm 6e766c (or /session list)" {
		t.Errorf("asSlash: %q", got)
	}
}

// A deletion plan reads as a few plain sentences, ending in what cannot be
// undone; the plan's internals stay in the whole plan.
func TestDeletionSummaryIsPlain(t *testing.T) {
	plan := map[string]any{
		"agents":          map[string]any{"count": float64(1), "effect": "removed; each keeps its history"},
		"tasks":           map[string]any{"queued_or_held": float64(2), "running": float64(1)},
		"results":         map[string]any{"count": float64(1)},
		"controllers":     map[string]any{"live": float64(1), "effect": "released; every fence is invalid"},
		"admissions":      map[string]any{"reserved": float64(0)},
		"runtime_cleanup": "the fleet session f1 is stopped for good",
		"erasure":         map[string]any{"expected": "complete"},
		"cost":            "no charge for the deletion",
	}
	got := strings.Join(deletionSummary(plan), " ")
	for _, want := range []string{"Its machine is stopped for good.", "It removes 1 agent (each keeps its history), 2 waiting instructions and 1 result.",
		"1 running instruction is asked to stop.", "Its saved content is erased.", "This cannot be undone."} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q: %s", want, got)
		}
	}
	for _, internal := range []string{"fence", "admission", "controller", "f1"} {
		if strings.Contains(got, internal) {
			t.Errorf("summary shows %q: %s", internal, got)
		}
	}
}

// Inside the app, a command's refusal reads in the app's words (no contract
// lines), and one that needs a confirmation hands it to the app to ask.
func TestAppChildRefusalsReadInTheAppsWords(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	_ = c
	env := append(fastEnv(cfg), "KS_IN_APP=1")
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "agent", "stop", "main", "--session", agentSessionShort, "--no-input")
	if code != exitUsage || !strings.Contains(errs, appConfirmMarker) || !strings.Contains(errs, `--confirm main`) {
		t.Errorf("agent stop in the app: %d\n%q", code, errs)
	}
	_, errs, _ = auditExec(t, bin, cfg, t.TempDir(), env, "agent", "status", "ghost", "--session", agentSessionShort)
	if !strings.Contains(errs, iconFailed+" ") || strings.Contains(errs, "Remote work started") || strings.Contains(errs, "Next: ks ") {
		t.Errorf("a refusal in the app:\n%s", errs)
	}
	// the same refusal outside the app keeps its contract lines
	_, errs, _ = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "status", "ghost", "--session", agentSessionShort)
	if !strings.Contains(errs, "Remote work started: no.") {
		t.Errorf("a refusal on the command line lost its contract lines:\n%s", errs)
	}
}

// The window's header rule is exactly the terminal's width, however long
// the names are.
func TestAppWindowHeaderFitsTheTerminal(t *testing.T) {
	long := inventoryRow{ShortID: "6e766c", Name: strings.Repeat("a-very-long-session-name-", 8)}
	for _, width := range []int{40, 80, 110} {
		for _, name := range []string{"main", strings.Repeat("agent", 20)} {
			h := strings.TrimPrefix(appWindowHeader(name, long, width), "\n")
			if displayWidth(h) != width {
				t.Errorf("width %d, agent %d chars: header is %d columns", width, len(name), displayWidth(h))
			}
		}
	}
}

// Every icon means one thing; a time in the past is said to have passed.
func TestAppApprovalExpiry(t *testing.T) {
	if got := approvalWhen(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); !strings.HasSuffix(got, "(passed)") {
		t.Errorf("past expiry: %q", got)
	}
	if got := approvalWhen(time.Now().Add(9*time.Minute + 30*time.Second).UTC().Format(time.RFC3339)); !strings.Contains(got, "(in 9m)") {
		t.Errorf("future expiry: %q", got)
	}
}

// ks kill on a session the client knows is an agent session goes straight to
// its deletion plan (never the legacy route first), and deletes nothing
// unconfirmed; ks session delete with no flags plans.
func TestKillOfAKnownAgentSessionPlansWithoutTheLegacyRoute(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch {
		case r.URL.Path == "/api/capabilities":
			_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"registry_version": "t", "build": "b", "fetched_at": "x", "price_book": "v1.3",
				"capabilities": []map[string]any{{"id": "deletion.plans", "availability": "available", "summary": "s", "surface": "api"}, {"id": "session.list", "availability": "available", "summary": "s", "surface": "api"}}, "limits": map[string]any{}}})
		case r.URL.Path == "/api/v2/sessions" && r.Method == "GET":
			_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"items": []any{map[string]any{
				"id": "6e766cfleet00000", "short_id": "6e766c", "name": "test3", "runtime_state": "running", "record_id": "session_ag"}}, "next_cursor": ""}})
		case r.URL.Path == "/api/v2/sessions/session_ag/deletion-plan":
			_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "dpl_1", "expires_at": "2026-09-26T13:00:00Z",
				"plan": map[string]any{"agents": map[string]any{"count": 1, "effect": "removed"}, "controllers": map[string]any{"live": 1, "effect": "released; every fence is invalid"},
					"runtime_cleanup": "the fleet session 6e766cfleet00000 is stopped for good", "erasure": map[string]any{"expected": "complete"}}}})
		default:
			w.WriteHeader(404)
			_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "kill", "6e766c")
	if code != exitConflict || !strings.Contains(errs, "Deletion plan for session 6e766c. Nothing has been deleted.") ||
		!strings.Contains(errs, "ks session delete 6e766c --execute dpl_1 --confirm 6e766c") || strings.Contains(errs, "fence") {
		t.Errorf("kill: %d\n%s", code, errs)
	}
	mu.Lock()
	for _, r := range seen {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("kill sent %s", r)
		}
	}
	seen = nil
	mu.Unlock()
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "delete", "6e766c")
	if code != 0 || !strings.Contains(out, "Nothing has been deleted") || !strings.Contains(out, "--execute dpl_1 --confirm 6e766c") {
		t.Errorf("session delete with no flags does not plan: %d\n%s", code, out)
	}
}
