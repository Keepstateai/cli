package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The one shape of a list: every list screen, at the widths a person has
// (120, 80 and 60 columns), with colour and without, and into a pipe. What is
// asserted is what a person sees: nothing wider than the terminal (but a
// command, which is never cut), no escape bytes without colour, no raw JSON,
// no cut id, the empty state in a sentence. The plain screens at the three
// widths are the goldens in testdata/ux.
//
//	KS_UPDATE_GOLDEN=1 go test -run TestListScreens .

type screenCase struct {
	name   string
	screen func(empty bool) listScreen
}

func sampleAgents() []agentRow {
	live := true
	return []agentRow{
		{ID: "agt_main00000001", Name: "main", IsPrimary: true, Activity: "working", ActiveTaskID: "tsk_0123456789abcdef", Controller: "held", Consultation: "busy", SupervisorLive: &live},
		{ID: "agt_review0000002", Name: "reviewer-with-a-long-name", Activity: "idle"},
		{ID: "agt_docs000000003", Name: "docs", Activity: "waiting_approval", SessionRuntime: &sessionRuntimeDoc{State: "parked", Reason: "funds_interlock", Note: "the account's credits ran out"}},
	}
}

func sampleInventory() []inventoryRow {
	return []inventoryRow{
		{ID: "session_0123456789abcdef", ShortID: "session_0123", Name: "shop import", RuntimeState: "running", AgentActivity: "working", TaskState: "running", LastActivityAt: "2026-11-23T10:15:00Z", LastCheckpointID: "cp_1", LastCheckpointAt: "2026-11-23T09:00:00Z", KeyAlias: "work key"},
		{ID: "session_abcdef0123456789", ShortID: "session_abcd", Name: "docs", RuntimeState: "parked", AgentActivity: "idle", TaskState: "2 queued", LastActivityAt: "2026-11-22T18:00:00Z"},
		{ID: "session_dead000000000000", ShortID: "session_dead", Name: "old", RuntimeState: "dead", AgentActivity: "stopped", TaskState: "none", LastActivityAt: "2026-11-01T08:00:00Z"},
	}
}

func screenCases() []screenCase {
	return []screenCase{
		{"import-list", func(empty bool) listScreen {
			if empty {
				return importListScreen(nil, "")
			}
			return importListScreen([]importListEntry{
				{Tool: "claude-code", ID: "2d1eb651-e108-498a-88b4-dc7659a1a38a", Project: "/home/you/work/projects/BulkPulse-main", Date: "2026-11-23T20:17:00Z", Bytes: 537_000_000, Messages: 12041},
				{Tool: "codex", ID: "01a1c2d3-e4f5-7a6b-8c9d-0e1f2a3b4c5d", Project: "/home/you/keepstate", Date: "2026-11-22T09:00:00Z", Bytes: 3_100_000, Messages: 88},
				{Tool: "cursor", ID: "3f2a1c", Project: "", Date: "2026-11-20T09:00:00Z", Messages: 14, Note: "best effort"},
			}, "")
		}},
		{"sessions", func(empty bool) listScreen {
			if empty {
				return sessionListScreen(nil, "", "", "", 80)
			}
			return sessionListScreen(sampleInventory(), "", "", "", 80)
		}},
		{"agents", func(empty bool) listScreen {
			if empty {
				return agentListScreen("sess_0123", nil)
			}
			return agentListScreen("sess_0123", sampleAgents())
		}},
		{"agents-all", func(empty bool) listScreen {
			if empty {
				return allAgentsScreen(agentStatusList{})
			}
			return allAgentsScreen(agentStatusList{Items: []statusRecord{
				{AgentID: "agt_1", Name: "checkout", SessionID: "ses_one", SessionName: "api", State: "blocked", Kind: "permission", Msg: "shell: npm test", NeedsYou: true, Action: &statusAction{Label: "Approve or deny", Verb: "ks agent approve apr_1 --session ses_one"}},
				{AgentID: "agt_2", Name: "build", SessionID: "ses_two", SessionName: "web", State: "working", Msg: "Running a tool: shell, then writing the result to the workspace and checking it"},
				{AgentID: "agt_3", Name: "review", SessionID: "ses_one", SessionName: "api", State: "idle", Msg: "Ready."},
			}})
		}},
		{"tasks", func(empty bool) listScreen {
			if empty {
				return taskListScreen("sess_0123", nil, 0)
			}
			qs := []taskQueue{{Agent: "main", AgentID: "agt_1", Tasks: []taskRow{
				{ID: "tsk_0123456789abcdef0123", QueueSeq: 1, State: "running", Origin: "console", AuthorType: "account", AuthorID: "acct_1", CreatedAt: "2026-11-23T10:00:00Z"},
				{ID: "tsk_0123456789abcdef0124", QueueSeq: 2, State: "queued", Origin: "cli", AuthorType: "account", AuthorID: "acct_1", CreatedAt: "2026-11-23T10:05:00Z"},
			}}, {Agent: "docs", AgentID: "agt_2", Tasks: []taskRow{}}, {Agent: "ghost", AgentID: "agt_3", Unreadable: "the service did not answer"}}
			return taskListScreen("sess_0123", qs, 2)
		}},
		{"results", func(empty bool) listScreen {
			if empty {
				return resultListScreen("session sess_0123", nil, nil)
			}
			return resultListScreen("session sess_0123", []resultRow{
				{ID: "res_0123456789", Name: "quarterly-report-final-version-2.pdf", Bytes: 1234567, TaskID: "tsk_1", SHA256: "0123456789abcdef0123456789abcdef"},
			}, []string{"task tsk_gone: not found"})
		}},
		{"files", func(empty bool) listScreen {
			if empty {
				return fileListScreen(fileList{Path: "."}, "in session sess_0123")
			}
			return fileListScreen(fileList{Path: "src", Total: 3, Entries: []fileEntry{
				{Name: "main.go", Type: "file", Mode: "0644", Size: 1204},
				{Name: "internal", Type: "dir", Mode: "0755"},
				{Name: ".env", Type: "file", Mode: "0600", Size: 40, Redacted: true},
			}}, "in session sess_0123")
		}},
		{"queue", func(empty bool) listScreen {
			if empty {
				return pendingScreen("Queue of agent main in session sess_0123", pendingView{QueueRevision: "qrev_1"})
			}
			return pendingScreen("Queue of agent main in session sess_0123", pendingView{QueueRevision: "qrev_1",
				Active: &pendingItem{TaskID: "tsk_run", State: "running", SummaryShown: true, Summary: "deploy the site", SubmitterType: "account", SubmitterID: "acct_1", AgeSeconds: 90},
				Pending: []pendingItem{
					{TaskID: "tsk_a", Position: 1, State: "queued", SummaryShown: true, Summary: "write the tests for the new import choice and run them on every width", SubmitterType: "account", SubmitterID: "acct_1", AgeSeconds: 80},
					{TaskID: "tsk_b", Position: 2, State: "held", HeldReason: "waiting for approval", SubmitterType: "agent", SubmitterID: "agt_2", AgeSeconds: 70},
				}})
		}},
		{"keys", func(empty bool) listScreen {
			if empty {
				return keyListScreen("", nil, nil, true, "")
			}
			return keyListScreen("", []customerKey{
				{ID: "vlt_mine0001", Provider: "anthropic", Alias: "work", Last4: "abcd", Enabled: true},
				{ID: "vlt_mine0002", Provider: "openai", Last4: "wxyz"},
			}, []string{"KeepState's key for anthropic (--key keepstate): list price plus 4%, from purchased credit; the default for new sessions."}, false, "")
		}},
		{"apps", func(empty bool) listScreen {
			if empty {
				return appsListScreen(nil)
			}
			return appsListScreen([]connectedApp{{ID: "gr_0001", Client: "Claude", Verification: "verified", SendsCodesTo: "claude.ai", ContextName: "Personal", ConnectedAt: "2026-11-01T00:00:00Z", LastUsedAt: "2026-11-20T12:00:00Z",
				ScopeWords: []string{"read your sessions and agents", "start and stop sessions after you confirm each one on a console page"}}})
		}},
	}
}

// a cut cell is a word with an ellipsis in it; a last-four starts with one
var cutCell = regexp.MustCompile(`[A-Za-z0-9/~]…`)

var uxAnsi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func renderScreen(sc listScreen, width int, themed, pipe bool) string {
	var buf bytes.Buffer
	u := testUI(&buf, width, themed, newFakeClock())
	u.pipe = pipe
	u.List(sc)
	return buf.String()
}

func TestListScreensFitEveryWidth(t *testing.T) {
	for _, c := range screenCases() {
		for _, empty := range []bool{false, true} {
			for _, width := range []int{120, 80, 60} {
				for _, themed := range []bool{false, true} {
					out := renderScreen(c.screen(empty), width, themed, false)
					for _, l := range strings.Split(out, "\n") {
						plain := uxAnsi.ReplaceAllString(l, "")
						if displayWidth(plain) > width && !strings.Contains(plain, "ks ") && !longToken(plain) {
							t.Errorf("%s (empty %v) at %d columns: %q is %d wide", c.name, empty, width, plain, displayWidth(plain))
						}
					}
					if !themed && strings.Contains(out, "\x1b") {
						t.Errorf("%s: escape bytes without colour: %q", c.name, out)
					}
					if strings.Contains(out, `{"`) || strings.Contains(out, "map[") || strings.Contains(out, "%!") {
						t.Errorf("%s: raw data on screen:\n%s", c.name, out)
					}
					if cutCell.MatchString(out) {
						t.Errorf("%s at %d columns: a cell was cut:\n%s", c.name, width, out)
					}
				}
			}
		}
	}
}

func longToken(l string) bool {
	for _, w := range strings.Fields(l) {
		if displayWidth(w) >= 48 {
			return true
		}
	}
	return false
}

// Into a pipe nothing is cut and nothing is wrapped: one row, one line.
func TestListScreensIntoAPipeAreWhole(t *testing.T) {
	for _, c := range screenCases() {
		out := renderScreen(c.screen(false), 40, false, true)
		for _, l := range strings.Split(out, "\n") {
			if cutCell.MatchString(l) {
				t.Errorf("%s: a cell was cut for a pipe: %q", c.name, l)
			}
		}
	}
	// a long row stays one line
	sc := resultListScreen("session s", []resultRow{{ID: "res_1", Name: strings.Repeat("long-name-", 8), Bytes: 12, TaskID: "tsk_1", SHA256: "abcdef0123456789"}}, nil)
	out := renderScreen(sc, 40, false, true)
	if !strings.Contains(out, strings.Repeat("long-name-", 8)) {
		t.Errorf("the name was cut for a pipe:\n%s", out)
	}
}

// An empty list is a sentence, said once, and no table.
func TestEmptyListsSaySoInWords(t *testing.T) {
	for _, c := range screenCases() {
		out := renderScreen(c.screen(true), 80, false, false)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 3 || strings.TrimSpace(lines[1]) != "" {
			t.Errorf("%s: empty screen is not a title, a blank and a sentence:\n%s", c.name, out)
		}
		if !regexp.MustCompile(`(?i)\b(no|none|nothing|there are no|has no|have no|you have no)\b`).MatchString(out) {
			t.Errorf("%s: the empty state does not say so in words:\n%s", c.name, out)
		}
	}
}

// The plain screens at 120, 80 and 60 columns are the goldens.
func TestListScreenGoldens(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	t.Setenv("CODEX_HOME", "/cfg/codex")
	for _, c := range screenCases() {
		var b strings.Builder
		for _, empty := range []bool{false, true} {
			for _, width := range []int{120, 80, 60} {
				state := "list"
				if empty {
					state = "empty"
				}
				fmt.Fprintf(&b, "=== %s, %s, %d columns ===\n", c.name, state, width)
				b.WriteString(renderScreen(c.screen(empty), width, false, false))
			}
		}
		name := filepath.Join("testdata", "ux", c.name+".txt")
		if os.Getenv("KS_UPDATE_GOLDEN") == "1" {
			_ = os.MkdirAll(filepath.Dir(name), 0o755)
			if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v (KS_UPDATE_GOLDEN=1 writes it)", name, err)
		}
		if string(want) != b.String() {
			t.Errorf("%s differs from what the screen draws now (KS_UPDATE_GOLDEN=1 rewrites it)\n--- got ---\n%s", name, b.String())
		}
	}
}

// A verb's human text goes through outf and outln (so a line wider than the
// terminal wraps on word boundaries) and its tables through the kit (so a
// table fits the width): no new fmt.Printf or fmt.Println on standard output
// in a verb, and no new fixed-width table row. The files that write raw
// bytes on purpose (the protocol server, shell completion, the terminal
// screens, the live window) are named here, each by its exact name; verbatim
// content goes through outRaw.
func TestVerbsPrintThroughTheKit(t *testing.T) {
	raw := map[string]bool{
		"agent_view.go":             true,
		"app.go":                    true,
		"appaway.go":                true,
		"appconfirm.go":             true,
		"appnew.go":                 true,
		"appslash.go":               true,
		"appstates.go":              true,
		"appterm.go":                true,
		"apptheme.go":               true,
		"appui.go":                  true,
		"appwindow.go":              true,
		"attach_client.go":          true,
		"claude_plan.go":            true,
		"claude_plan_capture.go":    true,
		"claude_plan_mask.go":       true,
		"claude_plan_org.go":        true,
		"claude_plan_pty_darwin.go": true,
		"claude_plan_pty_linux.go":  true,
		"claude_plan_pty_other.go":  true,
		"claude_plan_pty_unix.go":   true,
		"claude_signin.go":          true,
		"command.go":                true,
		"completion.go":             true,
		"cruise_watch.go":           true,
		"help.go":                   true,
		"herdr.go":                  true,
		"herdr_pane.go":             true,
		"herdr_report.go":           true,
		"import_guided.go":          true,
		"import_progress.go":        true,
		"import_screens.go":         true,
		"listui.go":                 true,
		"login.go":                  true,
		"main.go":                   true,
		"mcp.go":                    true,
		"mcp_nosabotage.go":         true,
		"mcp_sabotage.go":           true,
		"mcp_tools.go":              true,
		"osc7501.go":                true,
		"output.go":                 true,
		"recovery_screen.go":        true,
		"skill.go":                  true,
		"stream_restart.go":         true,
		"termwidth.go":              true,
		"ui.go":                     true,
		"update.go":                 true,
		"viewsize.go":               true,
	}
	files, _ := filepath.Glob("*.go")
	printf := regexp.MustCompile(`\bfmt\.(Printf|Println)\(`)
	table := regexp.MustCompile(`\bout(f|ln)\(\s*(fmt\.Sprintf\()?"%-[0-9]+[sd] `)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if raw[f] {
			continue
		}
		b, _ := os.ReadFile(f)
		for i, l := range strings.Split(string(b), "\n") {
			if printf.MatchString(l) {
				t.Errorf("%s:%d: fmt.Printf or fmt.Println in a verb: use outf or outln (a line wider than the terminal wraps)\n  %s", f, i+1, strings.TrimSpace(l))
			}
			if table.MatchString(l) {
				t.Errorf("%s:%d: a fixed-width table row: build a listScreen (a table fits the terminal)\n  %s", f, i+1, strings.TrimSpace(l))
			}
		}
	}
}

// A line a verb prints as text wraps on word boundaries at the terminal's
// width and hangs under its value; a command is left whole; into a pipe
// nothing moves.
func TestTextLinesWrapOnWordBoundaries(t *testing.T) {
	var buf bytes.Buffer
	u := testUI(&buf, 40, false, newFakeClock())
	long := "  decided       approved by the owner at the time the agent asked, with a long reason"
	got := u.fitLine(long)
	if len(got) < 2 {
		t.Fatalf("a long card line did not wrap: %q", got)
	}
	for i, l := range got {
		if displayWidth(l) > 40 {
			t.Errorf("line %d is %d wide: %q", i, displayWidth(l), l)
		}
		if i > 0 && !strings.HasPrefix(l, strings.Repeat(" ", len("  decided       "))) {
			t.Errorf("line %d does not hang under the value: %q", i, l)
		}
	}
	if strings.Join(strings.Fields(strings.Join(got, " ")), " ") != strings.Join(strings.Fields(long), " ") {
		t.Errorf("words were lost or cut: %q", got)
	}
	cmd := "  run it: ks check run chk_0123456789abcdef (it stops for your trust the first time)"
	if g := u.fitLine(cmd); len(g) != 1 || g[0] != cmd {
		t.Errorf("a line with a command was wrapped: %q", g)
	}
	u.pipe = true
	if g := u.fitLine(long); len(g) != 1 || g[0] != long {
		t.Errorf("a line into a pipe was wrapped: %q", g)
	}
}

func TestProseNeverBreaksAWord(t *testing.T) {
	path := "/home/you/.config/keepstate/templates/a-very-long-folder-name/reviewer.md"
	lines := wrapProse("Your own roles go in "+path+" and then the instructions.", 30)
	found := false
	for _, l := range lines {
		if l == path {
			found = true
		}
		if strings.Contains(l, path[:20]) && l != path {
			t.Errorf("the path shares a line: %q", l)
		}
	}
	if !found {
		t.Errorf("the path was broken: %q", lines)
	}
}

// Shell lines, blocks and words that cannot fit are left whole, never
// reflowed or broken in the middle of a word.
func TestShellLinesAndWideWordsAreLeftWhole(t *testing.T) {
	var buf bytes.Buffer
	u := testUI(&buf, 40, false, newFakeClock())
	for _, l := range []string{
		"    rm -f /usr/local/bin/ks && rm -rf /home/you/.config/keepstate",
		"  run: cd /somewhere && make && make install and then restart the shell",
		"  label        " + strings.Repeat("x", 60),
		"  a sentence with an unbroken " + strings.Repeat("y", 45) + " in it",
	} {
		if g := u.fitLine(l); len(g) != 1 || g[0] != l {
			t.Errorf("reflowed or broken: %q -> %q", l, g)
		}
	}
}

// A stacked row keeps its cells apart.
func TestStackedRowsKeepTheirCellBoundaries(t *testing.T) {
	var buf bytes.Buffer
	u := testUI(&buf, 50, false, newFakeClock())
	u.Table([]Col{{Title: "A"}, {Title: "B"}, {Title: "C"}, {Title: "D", Second: true}},
		[][]string{{"alpha value", "beta value", strings.Repeat("c", 30), "dd"}})
	if !strings.Contains(buf.String(), "alpha value · beta value") {
		t.Errorf("cells ran together:\n%s", buf.String())
	}
}

// A table is cut for no one: a long copyable value stacks instead.
func TestCopyableValuesAreNeverCutInATable(t *testing.T) {
	var buf bytes.Buffer
	u := testUI(&buf, 40, false, newFakeClock())
	email := "a.person.with.a.long.name@a-long-company-domain.example"
	u.Table([]Col{{Title: "Request"}, {Title: "Email"}, {Title: "State"}}, [][]string{{"req_1", email, "waiting"}})
	if !strings.Contains(buf.String(), email) || strings.Contains(buf.String(), "…") {
		t.Errorf("the address was cut:\n%s", buf.String())
	}
}

// COLUMNS says how wide a terminal is, not that nobody is reading the
// output: into a pipe a list is whole whatever COLUMNS says.
func TestAPipeIsWholeWhateverColumnsSays(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	e.env = append(e.env, "COLUMNS=40")
	out, _, code := e.run("import", "list")
	if code != 0 {
		t.Fatalf("%d", code)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, claudeSID) && !strings.Contains(l, oldRoot) {
			t.Errorf("the row was cut or stacked for a pipe: %q", l)
		}
	}
}
