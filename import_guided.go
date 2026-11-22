// import_guided.go: the interactive import, for a person at a terminal.
//
//  1. Pick a session   (skipped by --latest and --session ID)
//  2. Bring the project files too?   (skipped by --with-files, --no-files,
//     and when the session names no project folder)
//  3. Review the plan: Import now / Save the plan only / Cancel   (skipped by --yes)
//  4. The import, with its progress, and what to do next
//
// It runs only at a terminal: with --no-input, --json or no terminal nothing
// is asked, and the command says which flags to pass. --plan and
// --execute DIGEST are the scripts' two steps and are not touched.
//
// The drawing here is local to the import, so the shared terminal kit can
// take it over later without a merge.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// guidedInteractive: a person is at both ends of the terminal, and has not
// said that nothing may be asked.
func guidedInteractive() bool {
	return stdinIsTerminal() && stderrIsTerminal() && !out.noInput && !out.json
}

func importNeedsPick(inv *Invocation, tool string) bool {
	if tool == "" {
		return true
	}
	if tool == "cursor" {
		return !inv.Bool("latest") && !inv.Set("chat")
	}
	return !inv.Bool("latest") && !inv.Set("session")
}

// ---- keys ------------------------------------------------------------------------------

type igKey int

const (
	igOther igKey = iota
	igUp
	igDown
	igEnter
	igEsc
	igCtrlC
	igDigit
)

// igRaw puts the terminal in a mode that gives keys one at a time.
type igRaw struct{ saved string }

func igStty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func igEnterRaw() *igRaw {
	saved, err := igStty("-g")
	if err != nil || saved == "" {
		return nil
	}
	if _, err := igStty("-icanon", "-echo", "-isig", "min", "1", "time", "0"); err != nil {
		return nil
	}
	rt := &igRaw{saved: saved}
	onExit(rt.restore)
	return rt
}

func (r *igRaw) restore() {
	if r != nil && r.saved != "" {
		_, _ = igStty(r.saved)
	}
}

// igReadKey reads one key. An escape that is not followed by more within a
// tenth of a second is the Escape key.
func igReadKey(r *igRaw) (igKey, byte) {
	var b [1]byte
	if n, err := os.Stdin.Read(b[:]); n == 0 || err != nil {
		return igCtrlC, 0
	}
	switch c := b[0]; {
	case c == '\r' || c == '\n':
		return igEnter, c
	case c == 3 || c == 4:
		return igCtrlC, c
	case c >= '0' && c <= '9':
		return igDigit, c
	case c == 'k':
		return igUp, c
	case c == 'j':
		return igDown, c
	case c == 27:
		_, _ = igStty("min", "0", "time", "1")
		var seq [2]byte
		n, _ := os.Stdin.Read(seq[:1])
		if n == 1 && seq[0] == '[' {
			n, _ = os.Stdin.Read(seq[1:2])
			_, _ = igStty("min", "1", "time", "0")
			if n == 1 {
				switch seq[1] {
				case 'A':
					return igUp, 0
				case 'B':
					return igDown, 0
				}
			}
			return igOther, 0
		}
		_, _ = igStty("min", "1", "time", "0")
		return igEsc, 27
	}
	return igOther, b[0]
}

// igChoose shows the items, one marked, and answers the index chosen with the
// arrow keys and Enter, or by typing its number; -1 for Escape or Ctrl-C.
func igChoose(title string, items []string, def int) int {
	w := os.Stderr
	fmt.Fprintln(w, title)
	rt := igEnterRaw()
	if rt == nil { // no raw keys: a numbered question answered with a line
		for i, it := range items {
			fmt.Fprintf(w, "  %d  %s\n", i+1, it)
		}
		fmt.Fprintf(w, "Choose 1-%d [%d]: ", len(items), def+1)
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil && strings.TrimSpace(line) == "" { // end of input (Ctrl-D) is a cancel, never an Enter
			return -1
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(items) {
			return -1
		}
		return n - 1
	}
	defer rt.restore()
	sel, typed := def, 0
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(w, "\x1b[%dA", len(items)+1)
		}
		for i, it := range items {
			mark, num := "  ", fmt.Sprintf("%d", i+1)
			if i == sel {
				mark = "❯ "
			}
			fmt.Fprintf(w, "\r\x1b[K%s%2s  %s\n", mark, num, it)
		}
		fmt.Fprint(w, "\r\x1b[K  ↑↓ move · Enter to choose · or type a number · Esc cancel\n")
	}
	draw(true)
	for {
		k, c := igReadKey(rt)
		switch k {
		case igUp:
			typed, sel = 0, (sel+len(items)-1)%len(items)
		case igDown:
			typed, sel = 0, (sel+1)%len(items)
		case igDigit:
			n := typed*10 + int(c-'0')
			if n < 1 || n > len(items) {
				n = int(c - '0')
			}
			if n >= 1 && n <= len(items) {
				typed, sel = n, n-1
			}
		case igEnter:
			fmt.Fprint(w, "\x1b[1A\r\x1b[K") // the hint goes; the marked list stays
			return sel
		case igEsc, igCtrlC:
			fmt.Fprint(w, "\x1b[1A\r\x1b[K")
			return -1
		}
		draw(false)
	}
}

// ---- the list ------------------------------------------------------------------------

type guidedRow struct {
	Tool    string
	ID      string
	Project string
	Title   string
	Mod     time.Time
	Size    int64 // what would go up after cleaning; -1 when it is a summary or unknown
	Over    bool
	Chat    bool
}

const guidedMaxRows = 15

func igShortHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		p = "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func igClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func igAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 2*time.Minute:
		return "open now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour && d >= 24*time.Hour:
		return "yesterday"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

type igStop struct{}

func (igStop) Error() string { return "stop" }

// firstPrompt is the first thing the person said in a session, from its first
// lines only.
func firstPrompt(t transcriptInfo) string {
	var found string
	n := 0
	_ = readImportLines(t.Path, func(line []byte) error {
		n++
		if n > 400 {
			return igStop{}
		}
		if t.Tool == "codex" {
			var r codexLight
			if jsonUnmarshalLoose(line, &r) && r.Type == "response_item" && r.Payload.Type == "message" && r.Payload.Role == "user" {
				if s, ok := humanText(r.Payload.Content); ok && strings.TrimSpace(s) != "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
					found = s
					return igStop{}
				}
			}
			return nil
		}
		var r claudeLight
		if jsonUnmarshalLoose(line, &r) && r.Type == "user" && !r.IsMeta && !r.IsSidechain && r.Message != nil && r.Message.Role == "user" {
			if s, ok := humanText(r.Message.Content); ok && strings.TrimSpace(s) != "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
				found = s
				return igStop{}
			}
		}
		return nil
	})
	return found
}

// igMeasure is what a session would send once cleaned (and trimmed to its last
// compaction when it is over what a session can resume).
func igMeasure(t transcriptInfo) int64 {
	dir, err := os.MkdirTemp("", "ks-import-size-")
	if err != nil {
		return -1
	}
	defer os.RemoveAll(dir)
	home, _ := os.UserHomeDir()
	rw := newPathRewriter(importRoots(t.Cwd, t.Cwds, home, importGuestWorkspace))
	sb := newSummaryBuilder(rw.apply)
	res, err := sanitizeTranscript(t, rw, sb, filepath.Join(dir, "t.jsonl"))
	if err != nil {
		return -1
	}
	if res.Bytes > importLimit() && res.Boundary >= 0 {
		if tr, terr := sanitizeTranscriptTrim(t, rw, nil, filepath.Join(dir, "t-trimmed.jsonl"), trimOpts{On: true, Start: res.Boundary}); terr == nil && tr.Bytes > 0 {
			return tr.Bytes
		}
	}
	return res.Bytes
}

func guidedRows(tool string) []guidedRow {
	var rows []guidedRow
	var tools []string
	if tool == "" {
		tools = []string{"claude-code", "codex"}
	} else if tool != "cursor" {
		tools = []string{tool}
	}
	if len(tools) > 0 {
		all := localTranscripts(tools...)
		for _, t := range all {
			rows = append(rows, guidedRow{Tool: t.Tool, ID: t.ID, Project: t.Cwd, Mod: t.ModTime, Size: -2, Title: "\x00" + t.Path})
		}
	}
	if tool == "" || tool == "cursor" {
		if cs, err := openCursorStore(cursorDefaultStore()); err == nil {
			if chats, cerr := cs.chats(); cerr == nil {
				for _, c := range chats {
					ms := c.UpdatedAt
					if ms == 0 {
						ms = c.CreatedAt
					}
					rows = append(rows, guidedRow{Tool: "cursor", ID: c.ID, Project: c.Folder, Mod: time.UnixMilli(ms), Size: -1, Title: c.Name, Chat: true})
				}
			}
			cs.Close()
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Mod.After(rows[j].Mod) })
	if len(rows) > guidedMaxRows {
		rows = rows[:guidedMaxRows]
	}
	// what each would send: read the newest ones' files, once
	infos := map[string]transcriptInfo{}
	for _, t := range localTranscripts("claude-code", "codex") {
		infos[t.Tool+"/"+t.ID] = t
	}
	for i := range rows {
		r := &rows[i]
		if r.Chat {
			continue
		}
		t, ok := infos[r.Tool+"/"+r.ID]
		if !ok {
			continue
		}
		r.Title = firstPrompt(t)
		fmt.Fprintf(os.Stderr, "\r\x1b[KMeasuring what each would send (%d of %d)...", i+1, len(rows))
		r.Size = igMeasure(t)
		r.Over = r.Size > importLimit()
	}
	fmt.Fprint(os.Stderr, "\r\x1b[K")
	return rows
}

func (r guidedRow) line(width int) string {
	size := "summary"
	switch {
	case r.Over:
		size = kitBytes(r.Size) + " too big"
	case r.Size >= 0:
		size = kitBytes(r.Size)
	case r.Size == -2 || r.Chat:
		size = "summary"
	}
	title := r.Title
	if title == "" {
		title = "(no first message)"
	}
	proj := igClip(igShortHome(r.Project), 22)
	if proj == "" {
		proj = "(no folder)"
	}
	when := igAgo(r.Mod)
	if r.Tool == "cursor" {
		when += " · best effort"
	}
	fixed := 12 + 2 + 22 + 2 + 2 + 16 + 2 + 12 + 6
	tw := width - fixed
	if tw < 12 {
		tw = 12
	}
	return fmt.Sprintf("%-12s %-22s %-*s  %-16s  %s", r.Tool, proj, tw, "“"+igClip(title, tw-2)+"”", when, size)
}

func igCols() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 60 {
			return n
		}
	}
	if out, err := igStty("size"); err == nil {
		var rows, cols int
		if _, err := fmt.Sscanf(out, "%d %d", &rows, &cols); err == nil && cols >= 60 {
			return cols
		}
	}
	return 100
}

// ---- the flow ------------------------------------------------------------------------------

func igDeclined() {
	fmt.Println("Nothing was created.")
	runExitHooks()
	os.Exit(exitUsage)
}

func guidedCommand(tool string) *Command {
	return importRow(tool) // not the registry's own row: that would make the registry refer to itself
}

// importGuided runs the four steps. inv is the command's own invocation (or
// nil when no tool was named).
func importGuided(inv *Invocation, tool string) {
	cr, ok := hostedToken()
	if !ok {
		fail(&cliError{Code: exitAuth, Kind: "not_signed_in", Message: "Not signed in. Run: ks login", NextAction: "ks login"})
	}
	rowCmd := guidedCommand(firstNonEmptyStr(tool, "claude-code"))
	requireCapability(cr, rowCmd)

	// 1. pick
	if importNeedsPick(inv, tool) {
		rows := guidedRows(tool)
		if len(rows) == 0 {
			fail(&cliError{Code: exitFailed, Kind: "no_sessions", Message: "no Claude Code, Codex or Cursor session was found on this machine. Nothing was sent", NextAction: "ks import list --cursor"})
		}
		width := igCols()
		items := make([]string, len(rows))
		for i, r := range rows {
			items[i] = r.line(width - 6)
		}
		i := igChoose("Import a conversation into KeepState. Pick a session; nothing is sent until you confirm.", items, 0)
		if i < 0 {
			igDeclined()
		}
		row := rows[i]
		tool = row.Tool
		if inv == nil {
			var uerr *UsageError
			inv, uerr = guidedCommand(tool).parse(nil)
			if uerr != nil {
				uerr.print()
				os.Exit(2)
			}
		}
		delete(inv.set, "latest")
		if row.Chat {
			inv.set["chat"], inv.strs["chat"] = true, row.ID
		} else {
			inv.set["session"], inv.strs["session"] = true, row.ID
		}
	}

	// 2. files
	importDeferOverLimit = !out.yes // asked below; with --yes there is nobody to ask: it is refused
	p := prepareImport(inv, tool)
	closeP := func() { p.Close() }
	defer func() { closeP() }()
	if p.OverLimit && p.Mode == "conversation" {
		// still over what a session can resume after the trim, or nothing to trim to
		_, er := cmdUI()
		why := "It holds no compaction to trim to."
		if p.Trimmed {
			why = "It was trimmed to its last compaction, and that part alone is still over."
		}
		er.Warn("This conversation is too big for a session to resume", fmt.Sprintf("It is %s after cleaning, and a session can resume up to %s. %s Nothing was sent or created.", kitBytes(p.Bytes), kitBytes(importLimit()), why))
		cs := igOverChoices(tool, p)
		items := make([]string, len(cs))
		for i, c := range cs {
			items[i] = c.Text
		}
		i := igChoose("What now?", items, 0)
		if i < 0 || cs[i].ID == "cancel" {
			igDeclined()
		}
		switch cs[i].ID {
		case "compact":
			o, _ := cmdUI()
			o.Blank()
			o.Title("", toneOK, "Compact it in "+importToolLabel(tool)+", then import again")
			for _, l := range compactInstruction(tool) {
				o.lines("  " + l)
			}
			o.Blank()
			o.NextWhy("ks import "+tool+" "+importFlagsWith(inv, p.Info.ID), "Run this after the compaction has finished.")
			return
		case "brief":
			inv.set["brief"] = true
			p.Close()
			p = prepareImport(inv, tool)
		}
	}
	if !inv.Bool("with-files") && !inv.Bool("no-files") && !out.yes { // --yes takes the default: the conversation only
		if top, plan, ok := guidedFilesProbe(inv, p); ok {
			total := p.Bytes
			conv := fmt.Sprintf("conversation only %s", kitBytes(total))
			withf := fmt.Sprintf("with the files %s (the repository %s, %s)", kitBytes(total+plan.Content), kitBytes(plan.Content), nounCount(len(plan.Entries), "file", "files"))
			i := igChoose(fmt.Sprintf("Bring the project files too? The git repository at %s.", igShortHome(top)), []string{conv, withf}, 0)
			if i < 0 {
				igDeclined()
			}
			if i == 1 {
				inv.set["with-files"] = true
				p.Close()
				p = prepareImport(inv, tool)
			}
		}
	}

	// 3. review
	set := importSettingsOf(inv, tool)
	svc := serverImportPlan(cr, p.Decl)
	showReview := func() {
		printImportPlan(p, set, svc)
		o, _ := cmdUI()
		sent := p.Bytes
		detail := ""
		if p.Files != nil {
			sent += p.Files.Content
			detail = fmt.Sprintf(" (the conversation %s and the files %s)", kitBytes(p.Bytes), kitBytes(p.Files.Content))
		}
		o.Blank()
		o.Facts([2]string{"Total to send", kitBytes(sent) + detail},
			[2]string{"Stays behind", "what you say in the old session after this point"})
		o.Blank()
	}
	showReview()
	if !out.yes {
		// a big conversation: the notice and its choices come before the review
		if e := svc.Plan.Estimate; e != nil && e.Notice && p.Mode == "conversation" {
			switch igBigChoice(inv, tool, p, e) {
			case "compact":
				return
			case "brief":
				inv.set["brief"] = true
				p.Close()
				p = prepareImport(inv, tool)
				svc = serverImportPlan(cr, p.Decl)
				set = importSettingsOf(inv, tool)
				showReview()
			case "go":
			default:
				igDeclined()
			}
		}
		i := igChoose("Review the plan above.", []string{"Import now", "Save the plan only (prints the digest and the command to run it later)", "Cancel"}, 0)
		switch i {
		case 0:
		case 1:
			o, _ := cmdUI()
			o.Facts([2]string{"Plan digest", svc.Digest})
			o.Blank()
			o.NextWhy("ks import "+tool+" "+importReplayArgs(inv, "--execute "+svc.Digest), "Does exactly this plan.")
			return
		default:
			igDeclined()
		}
	}

	// 4. the import
	importExecute(cr, inv, p, set, svc)
}

// importChoice is one line of the big-conversation notice: what it says and
// what choosing it means. A later option (compacting on KeepState with a
// cheaper model) is one more entry in igBigChoices, with its own id.
type importChoice struct {
	ID   string
	Text string
}

// igBigChoices are the choices, in the order they are shown. Compacting in
// Claude Code (or Codex) first comes first because it is the recommended one
// and costs nothing here; Cursor has no compact, so that choice is left out.
// igOverChoices are the choices for a conversation that is still over what a
// session can resume (after a trim, or with nothing to trim to): compact it where
// it came from, a written summary, cancel. The place for "Compact it on KeepState
// with <model>" is the comment below.
func igOverChoices(tool string, p *importPrep) []importChoice {
	var cs []importChoice
	if tool != "cursor" {
		cs = append(cs, importChoice{"compact", "Compact it in " + importToolLabel(tool) + " first (recommended, costs nothing here)"})
	}
	cs = append(cs, importChoice{"brief", fmt.Sprintf("Bring a written summary instead (--brief), about %s characters", commas(p.SummaryChars))})
	// the place for "Compact it on KeepState with <model> (about $X)": not built yet
	return append(cs, importChoice{"cancel", "Cancel"})
}

func igBigChoices(tool string, p *importPrep, e *importEstimateAnswer) []importChoice {
	var cs []importChoice
	if tool != "cursor" {
		cs = append(cs, importChoice{"compact", "Compact it in " + importToolLabel(tool) + " first (recommended, costs nothing here)"})
	}
	cs = append(cs, importChoice{"brief", fmt.Sprintf("Bring a written summary instead (--brief), about %s characters", commas(p.SummaryChars))})
	// the place for "Compact it on KeepState with <model> (about $X)": not built yet
	cs = append(cs, importChoice{"go", "Go without compacting (" + igFirstReads(e) + ")"})
	cs = append(cs, importChoice{"cancel", "Cancel"})
	return cs
}

func igFirstReads(e *importEstimateAnswer) string {
	var parts []string
	for _, m := range e.Models {
		parts = append(parts, fmt.Sprintf("first reply about $%s on %s", microsShown(m.FirstReadMicroUSD), m.Name))
	}
	return strings.Join(parts, ", ") + ", estimates"
}

// igBigChoice shows the notice and the choices and answers the id chosen.
func igBigChoice(inv *Invocation, tool string, p *importPrep, e *importEstimateAnswer) string {
	cs := igBigChoices(tool, p, e)
	items := make([]string, len(cs))
	for i, c := range cs {
		items[i] = c.Text
	}
	i := igChoose("What now?", items, 0)
	if i < 0 {
		return "cancel"
	}
	id := cs[i].ID
	if id == "compact" {
		o, _ := cmdUI()
		o.Blank()
		o.Title("", toneOK, "Compact it in "+importToolLabel(tool)+", then import again")
		for _, l := range compactInstruction(tool) {
			o.lines("  " + l)
		}
		o.Blank()
		o.NextWhy("ks import "+tool+" "+importFlagsWith(inv, p.Info.ID), "Run this after the compaction has finished.")
	}
	return id
}

// importFlagsWith is the flags of the command again, naming the session by id.
func importFlagsWith(inv *Invocation, id string) string {
	return strings.Join(append(importFlagParts(inv, map[string]bool{"latest": true, "session": true, "chat": true}), "--session", id), " ")
}

// compactInstruction is what to paste into the original session so that its
// compaction keeps what matters. Claude Code's /compact takes instructions;
// Codex's takes none, so there they are sent as a message first.
func compactInstruction(tool string) []string {
	keep := "Keep: every decision and the reason for it; the open tasks and where each stands; the file paths, function names and settings that matter; the commands that were run and what they printed when it mattered; every error and how it was fixed; and my standing rules and preferences, word for word. Leave out: greetings, repeated attempts that went nowhere, and long tool output that was already acted on."
	if tool == "codex" {
		return []string{
			"In the Codex session you want to import, send this message first:",
			"",
			"  Before we compact: write down, in your next reply, " + strings.ToLower(keep[:1]) + keep[1:],
			"",
			"Then type /compact (Codex's /compact takes no instructions, so the message above is what it keeps in view). Wait for it to finish.",
		}
	}
	return []string{
		"In the Claude Code session you want to import, paste this and press Enter:",
		"",
		"  /compact " + keep,
		"",
		"Wait for it to finish.",
	}
}

// guidedFilesProbe finds the project's git repository and its upload size,
// without failing when there is none (then the question is not asked).
func guidedFilesProbe(inv *Invocation, p *importPrep) (string, *uploadPlan, bool) {
	cwd := p.Info.Cwd
	if inv.Set("project") {
		if abs, err := filepath.Abs(inv.Str("project")); err == nil {
			cwd = abs
		}
	}
	if cwd == "" {
		return "", nil, false
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", nil, false
	}
	top, ok := gitOut(cwd, "rev-parse", "--show-toplevel")
	if !ok {
		return "", nil, false
	}
	top, _ = filepath.EvalSymlinks(top)
	return top, planUpload(top, "", true), true
}

// jsonUnmarshalLoose reads a line, answering whether it was JSON.
func jsonUnmarshalLoose(line []byte, v any) bool { return json.Unmarshal(line, v) == nil }
