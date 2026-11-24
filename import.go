// import.go: ks import, a Claude Code, Codex CLI or Cursor conversation
// brought into a KeepState session.
//
//	ks import list [--cursor] [--store FILE] [--json]
//	ks import claude-code|codex [--latest | --session ID] [--project DIR]
//	      [--with-files | --with-git | --without-git] [--name NAME] [--model M] [--key own|keepstate|ALIAS]
//	      [--budget DOLLARS] [--harness H] [--brief] [--plan | --execute DIGEST]
//	ks import cursor [--latest | --chat ID] [--store FILE] ... (a written summary)
//
// Everything that decides what is sent happens here, on this machine: the
// transcript is read, taken apart, its project paths said as the new
// workspace's, its secrets replaced, and the person is shown the plan before
// anything is created. The service judges the same declaration and answers a
// digest; --execute names that digest, so what runs is exactly what was
// shown, and a conversation that moved since is refused rather than sent.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- where the sessions are ----------------------------------------------------

func claudeProjectsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func codexSessionsDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "sessions")
}

// transcriptFiles lists the transcript files of a tool, without reading them.
func transcriptFiles(tool string) []string {
	var out []string
	switch tool {
	case "claude-code":
		dirs, _ := os.ReadDir(claudeProjectsDir())
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			fs, _ := filepath.Glob(filepath.Join(claudeProjectsDir(), d.Name(), "*.jsonl"))
			out = append(out, fs...)
		}
	case "codex":
		_ = filepath.WalkDir(codexSessionsDir(), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

// localSessions reads every transcript of the tools asked for and sorts them
// into the person's own sessions (newest first) and helper threads. Codex
// writes a rollout file for each helper thread (a spawned subagent, an approval
// reviewer); those are not sessions the person can import, so they are kept
// apart. Each session carries the count of helper threads that work for it.
func localSessions(tools ...string) (tops, helpers []transcriptInfo) {
	for _, tool := range tools {
		for _, p := range transcriptFiles(tool) {
			info, err := scanTranscript(tool, p)
			if err != nil || info.ID == "" {
				continue
			}
			if info.Helper {
				helpers = append(helpers, info)
			} else {
				tops = append(tops, info)
			}
		}
	}
	sort.SliceStable(tops, func(i, j int) bool { return tops[i].ModTime.After(tops[j].ModTime) })
	idx := map[string]int{}
	for i, t := range tops {
		idx[t.Tool+"/"+t.ID] = i
	}
	byID := map[string]transcriptInfo{}
	for _, h := range helpers {
		byID[h.Tool+"/"+h.ID] = h
	}
	for _, h := range helpers {
		root := helperRoot(h, byID)
		if i, ok := idx[h.Tool+"/"+root]; ok {
			tops[i].Helpers++
		}
	}
	return tops, helpers
}

// helperRoot is the session a helper thread works for: its parent, or, when the
// parent is itself a helper, the one at the top.
func helperRoot(h transcriptInfo, byID map[string]transcriptInfo) string {
	root := h.ParentID
	for hops := 0; hops < 8; hops++ {
		up, ok := byID[h.Tool+"/"+root]
		if !ok || up.ParentID == "" || up.ParentID == root {
			break
		}
		root = up.ParentID
	}
	if root == "" {
		root = h.ID
	}
	return root
}

// localTranscripts reads every session of the tools asked for, newest first.
// Helper threads are not among them.
func localTranscripts(tools ...string) []transcriptInfo {
	tops, _ := localSessions(tools...)
	return tops
}

// helperStandIns: a helper thread whose session's own file is not on this machine
// is shown under one row for that session, so it is not lost and not repeated.
func helperStandIns(tops, helpers []transcriptInfo) []transcriptInfo {
	have := map[string]bool{}
	for _, t := range tops {
		have[t.Tool+"/"+t.ID] = true
	}
	byID := map[string]transcriptInfo{}
	for _, h := range helpers {
		byID[h.Tool+"/"+h.ID] = h
	}
	var out []transcriptInfo
	at := map[string]int{}
	for _, h := range helpers {
		root := helperRoot(h, byID)
		if have[h.Tool+"/"+root] {
			continue
		}
		k := h.Tool + "/" + root
		i, ok := at[k]
		if !ok {
			out = append(out, transcriptInfo{Tool: h.Tool, ID: root, Orphan: true, Cwd: h.Cwd, ModTime: h.ModTime})
			i = len(out) - 1
			at[k] = i
		}
		o := &out[i]
		o.Helpers++
		o.Messages += h.Messages
		o.Size += h.Size
		if h.ModTime.After(o.ModTime) {
			o.ModTime, o.Cwd = h.ModTime, h.Cwd
		}
	}
	return out
}

type importListEntry struct {
	Tool     string `json:"tool"`
	ID       string `json:"id"`
	Project  string `json:"project"`
	Date     string `json:"date"`
	Bytes    int64  `json:"bytes"`
	Messages int    `json:"messages"`
	Turns    int    `json:"turns"`
	Helpers  int    `json:"helper_threads,omitempty"`
	Version  string `json:"version,omitempty"`
	Model    string `json:"model,omitempty"`
	Note     string `json:"note,omitempty"`
}

func importList(inv *Invocation) {
	var rows []importListEntry
	tops, helpers := localSessions("claude-code", "codex")
	for _, t := range append(tops, helperStandIns(tops, helpers)...) {
		e := importListEntry{Tool: t.Tool, ID: t.ID, Project: t.Cwd, Date: t.ModTime.UTC().Format(time.RFC3339), Bytes: t.Size, Messages: t.Messages, Turns: t.Turns, Helpers: t.Helpers, Version: t.Version, Model: t.Model}
		if t.Orphan {
			e.Note = "its own file is not on this machine"
		}
		rows = append(rows, e)
	}
	cursorNote := ""
	if inv.Bool("cursor") || inv.Set("store") {
		store := firstNonEmptyStr(inv.Str("store"), cursorDefaultStore())
		cs, err := openCursorStore(store)
		if err != nil {
			cursorNote = "Cursor: " + sanitize(err.Error())
		} else {
			chats, cerr := cs.chats()
			cs.Close()
			if cerr != nil {
				cursorNote = "Cursor: " + sanitize(cerr.Error())
			}
			for _, c := range chats {
				ms := c.UpdatedAt
				if ms == 0 {
					ms = c.CreatedAt
				}
				rows = append(rows, importListEntry{Tool: "cursor", ID: c.ID, Project: c.Folder, Date: time.UnixMilli(ms).UTC().Format(time.RFC3339), Messages: c.Bubbles, Model: c.Model, Note: "best effort"})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Date > rows[j].Date })
	// a session with no messages is hidden unless --all asks for it
	hidden := 0
	if !inv.Bool("all") {
		kept := rows[:0:0]
		for _, r := range rows {
			if r.Messages == 0 {
				hidden++
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	doc := map[string]any{"sessions": rows}
	if hidden > 0 {
		doc["hidden_empty"] = hidden
	}
	if cursorNote != "" {
		doc["cursor_note"] = cursorNote
	}
	emit(doc, func() {
		o, _ := cmdUI()
		o.List(importListScreen(rows, cursorNote, hidden))
	})
}

// importListScreen is ks import list as a list screen.
func importListScreen(rows []importListEntry, cursorNote string, hidden int) listScreen {
	s := listScreen{Title: "Sessions on this machine", Detail: listCount(len(rows), "session", "sessions"),
		Cols: []Col{{Title: "Tool"}, {Title: "Session"}, {Title: "Project"}, {Title: "Last used", Second: true}, {Title: "Size", Right: true, Second: true}, {Title: "Messages", Right: true, Second: true}}}
	for _, r := range rows {
		tool := r.Tool
		var notes []string
		if r.Note != "" {
			notes = append(notes, r.Note)
		}
		if r.Helpers > 0 {
			notes = append(notes, nounCount(r.Helpers, "helper thread", "helper threads"))
		}
		if len(notes) > 0 {
			tool += " (" + strings.Join(notes, ", ") + ")"
		}
		id := r.ID
		size, msgs := "", commas(int64(r.Messages))+" messages"
		if r.Bytes > 0 {
			size = kitBytes(r.Bytes)
		}
		project := visible(sanitize(r.Project))
		if project == "" {
			project = "unknown folder"
		}
		s.Rows = append(s.Rows, []string{tool, id, project, strings.Replace(strings.TrimSuffix(r.Date, "Z")[:16], "T", " ", 1), size, msgs})
	}
	if hidden > 0 && len(rows) > 0 {
		s.Lead = []string{nounCount(hidden, "empty session", "empty sessions") + " hidden; ks import list --all"}
	}
	if len(rows) == 0 && hidden > 0 {
		s.Empty = "There are no sessions with messages on this machine: " + nounCount(hidden, "empty session is", "empty sessions are") + " hidden (ks import list --all shows them)."
	} else if len(rows) == 0 {
		s.Empty = "No Claude Code or Codex session was found on this machine. Claude Code keeps them in " + visible(claudeProjectsDir()) + ", Codex in " + visible(codexSessionsDir()) + "."
	}
	if cursorNote != "" {
		s.Notes = append(s.Notes, cursorNote)
	}
	if len(rows) > 0 {
		s.Notes = append(s.Notes, "Any unique start of a session's id works with --session.")
		s.Next = []string{"ks import claude-code --latest"}
	}
	return s
}

// ---- choosing one --------------------------------------------------------------

func importUsage(msg, next string) {
	fail(&cliError{Code: exitUsage, Kind: "usage", Message: msg, NextAction: next})
}

// chooseTranscript answers the one session --latest or --session names.
func chooseTranscript(tool string, inv *Invocation) transcriptInfo {
	if inv.Bool("latest") == inv.Set("session") {
		importUsage("name the session to import: --latest (the newest for this project) or --session ID, and add --yes to import it without being asked. Nothing was sent", "ks import list")
	}
	all, helpers := localSessions(tool)
	if inv.Set("session") {
		id := inv.Str("session")
		// A helper thread's own id is refused in plain words: what runs is the
		// session it works for, which is the one to import.
		byID := map[string]transcriptInfo{}
		for _, h := range helpers {
			byID[h.Tool+"/"+h.ID] = h
		}
		for _, h := range helpers {
			if h.ID == id {
				parent := helperRoot(h, byID)
				importUsage(fmt.Sprintf("%s is a helper thread that %s started for the session %s, not a session of its own. Import that session instead. Nothing was sent", sanitize(id), tool, sanitize(parent)), "ks import "+tool+" --session "+sanitize(parent))
			}
		}
		var hits []transcriptInfo
		for _, t := range all {
			if t.ID == id {
				return t
			}
			if len(id) >= 4 && strings.HasPrefix(t.ID, id) {
				hits = append(hits, t)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0]
		case 0:
			importUsage(fmt.Sprintf("no %s session on this machine starts with %q. Nothing was sent", tool, sanitize(id)), "ks import list")
		}
		importUsage(fmt.Sprintf("%q starts %d %s sessions; give more of the id. Nothing was sent", sanitize(id), len(hits), tool), "ks import list")
	}
	if len(all) == 0 {
		fail(&cliError{Code: exitFailed, Kind: "no_sessions", Message: fmt.Sprintf("no %s session was found on this machine. Nothing was sent", tool), NextAction: "ks import list"})
	}
	// the newest is never an empty session
	var withText []transcriptInfo
	for _, t := range all {
		if t.Messages > 0 {
			withText = append(withText, t)
		}
	}
	if len(withText) > 0 {
		all = withText
	}
	project := inv.Str("project")
	if project == "" {
		project, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(project); err == nil {
		project = abs
	}
	for _, t := range all {
		if t.Cwd != "" && (under(t.Cwd, project) || under(project, t.Cwd)) {
			return t
		}
	}
	progress("no %s session for %s: taking the newest one on this machine", tool, visible(project))
	return all[0]
}

// ---- the preparation: everything the plan says ----------------------------------

// importCleanSeen, when a test sets it, keeps what the cleaning bar was told.
var importCleanSeen *[]int64

type importPrep struct {
	Tool         string
	Mode         string // conversation | brief
	Harness      string
	Info         transcriptInfo
	ConvID       string
	Decl         map[string]any
	Brief        string
	Out          string // the sanitized transcript (conversation mode)
	Bytes        int64
	SHA256       string
	Redactions   redactCounts
	Stripped     map[string]int
	Rewritten    int
	Files        *uploadPlan
	History      bool   // the repository's history (.git) is in Files
	GitNote      string // why it is not, when it was left out for its size
	Root         string
	Head         string
	Dirty        bool
	Turns        int
	SummaryChars int64 // what a written summary of it would be, in characters
	// Trimmed: the conversation is cut to what the harness sends the model, from the
	// last compaction on; UntrimmedBytes is what it was before. OverLimit: still
	// over what a session can resume (a person at a terminal is offered the choices).
	Trimmed        bool
	UntrimmedBytes int64
	TrimPrompts    int    // of the trim: the person's own messages in it
	TrimRecords    int    // and its records
	NoTrimWhy      string // a compaction is in the file but there is nothing to trim to, and why
	OverLimit      bool
	EstTokens      int64 // see import_estimate.go
	EstCompacted   bool
	Dest           string
	CursorNote     string
	cleanup        []func()
}

func (p *importPrep) Close() {
	c := p.cleanup
	p.cleanup = nil
	for i := len(c) - 1; i >= 0; i-- {
		c[i]()
	}
}

func gitOut(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err == nil
}

// importSettings reads the flags that say what the new session is.
type importSettings struct {
	harness string
	mode    string
	name    string
	model   string
	key     string
	spend   *int64
	files   bool
}

func importSettingsOf(inv *Invocation, tool string) importSettings {
	s := importSettings{model: inv.Str("model"), key: inv.Str("key"), files: importWantsFiles(inv)}
	s.harness = tool
	s.mode = "conversation"
	switch {
	case tool == "cursor":
		s.harness, s.mode = "claude-code", "brief"
	case inv.Set("harness") && inv.Str("harness") != tool:
		s.harness, s.mode = inv.Str("harness"), "brief"
	}
	if tool == "cursor" && inv.Set("harness") {
		s.harness = inv.Str("harness")
	}
	if inv.Bool("brief") {
		s.mode = "brief"
	}
	if s.harness != "claude-code" && s.harness != "codex" {
		importUsage(fmt.Sprintf("--harness is claude-code or codex; %q is not one. Nothing was sent", sanitize(s.harness)), "")
	}
	if inv.Set("name") {
		if err := newSessionNameRefusal(inv.Str("name"), "nothing was created"); err != nil {
			err.Message = "--name: " + err.Message
			fail(err)
		}
		s.name = inv.Str("name")
	}
	if inv.Set("budget") {
		if s.key != keepstateKeyArg {
			importUsage("--budget is the session's spend limit on KeepState's key, so it goes with --key keepstate. Nothing was sent", "ks import "+tool+" --latest --key keepstate --budget 20")
		}
		v, err := parseDollars("budget", inv.Str("budget"))
		if err != nil {
			fail(err)
		}
		s.spend = &v
	}
	return s
}

func prepareImport(inv *Invocation, tool string) *importPrep {
	return prepareImportOpts(inv, tool, false)
}

// prepareImportOpts: deferOver is for a person at a terminal, who is offered choices
// for a conversation that is still too big, instead of a refusal (importGuided).
func prepareImportOpts(inv *Invocation, tool string, deferOver bool) *importPrep {
	set := importSettingsOf(inv, tool)
	p := &importPrep{Tool: tool, Mode: set.mode, Harness: set.harness, Redactions: redactCounts{}, Stripped: map[string]int{}}
	tmp, err := os.MkdirTemp("", "ks-import-")
	if err != nil {
		die(err)
	}
	p.cleanup = append(p.cleanup, func() { os.RemoveAll(tmp) })
	onExit(p.Close) // a refusal exits the process: the private copy goes with it
	home, _ := os.UserHomeDir()

	var cwds []string
	var cursorChatRow *cursorChat
	var cs *cursorStore
	if tool == "cursor" {
		cs, cursorChatRow = chooseCursorChat(inv)
		p.cleanup = append(p.cleanup, cs.Close)
		p.Info = transcriptInfo{Tool: "cursor", ID: cursorChatRow.ID, Cwd: cursorChatRow.Folder, Model: cursorChatRow.Model}
		cwds = []string{cursorChatRow.Folder}
	} else {
		p.Info = chooseTranscript(tool, inv)
		cwds = p.Info.Cwds
	}
	p.ConvID = p.Info.ID

	// where the project's files are uploaded, and so what the old root becomes
	root, dest := p.Info.Cwd, importGuestWorkspace
	if set.files {
		p.Root, dest = importFilesRoot(inv, p, root)
		root = p.Root
	}
	p.Dest = dest
	rw := newPathRewriter(importRoots(root, cwds, home, dest))
	sb := newSummaryBuilder(rw.apply)

	switch {
	case tool == "cursor":
		turns, err := cs.turns(*cursorChatRow)
		if err != nil {
			fail(&cliError{Code: exitFailed, Kind: "cursor_unreadable", Message: sanitize(err.Error())})
		}
		if len(turns) == 0 {
			fail(&cliError{Code: exitFailed, Kind: "cursor_empty", Message: "that Cursor chat holds no text this client can read. Nothing was sent"})
		}
		for _, t := range turns {
			sb.add(t.Role, t.Text)
		}
		p.Turns = sb.Turns
		p.Info.Messages = len(turns)
		p.Info.Turns = sb.Turns
		p.Info.Model = cursorChatRow.Model
		p.Info.Version = ""
		if cursorChatRow.CreatedAt > 0 {
			p.Info.StartedAt = time.UnixMilli(cursorChatRow.CreatedAt).UTC().Format(time.RFC3339)
		}
		p.CursorNote = fmt.Sprintf("%d turns read; %d headers had no message and were skipped", len(turns), cs.Orphans)
	default:
		passes := int64(1)
		if tool == "claude-code" {
			passes = 2 // the first pass pairs tool calls with their results, the second writes
		}
		bar := newImportBar("Cleaning the conversation", p.Info.Size, 0, importCleanSeen)
		var read int64
		importReadProg = func(n int64) { read += n; bar.Set(min(read/passes, p.Info.Size)) }
		res, err := sanitizeTranscript(p.Info, rw, sb, filepath.Join(tmp, "transcript.jsonl"))
		importReadProg = nil
		if err == nil {
			bar.Set(p.Info.Size)
			bar.DoneWith("Cleaned the conversation", kitBytes(p.Info.Size)+" down to "+kitBytes(res.Bytes))
		}
		if err != nil {
			fail(&cliError{Code: exitFailed, Kind: "transcript_unreadable", Message: fmt.Sprintf("the %s session %s could not be read: %s. Nothing was sent", tool, p.Info.ID, sanitize(err.Error()))})
		}
		p.Turns = p.Info.Turns
		// over what a session can resume, with a compaction in it: keep only what
		// the harness sends the model (from the last compaction on). Before the size
		// check, the estimate and the digest.
		noTrimWhy := res.NoTrimWhy
		p.NoTrimWhy = noTrimWhy
		if set.mode == "conversation" && res.Bytes > importLimit() && res.Boundary >= 0 {
			tbar := newImportBar("Trimming to the last compaction", p.Info.Size, 0, importCleanSeen)
			var tread int64
			importReadProg = func(n int64) { tread += n; tbar.Set(min(tread/passes, p.Info.Size)) }
			tr, terr := trimTranscript(p.Info, rw, res, filepath.Join(tmp, "transcript-trimmed.jsonl"))
			importReadProg = nil
			if terr != nil {
				fail(&cliError{Code: exitFailed, Kind: "trim_failed", Message: sanitize(terr.Error()) + ". Nothing was sent. Run the same command again, or import a written summary with --brief",
					NextAction: importSessionCommand(inv, tool, p.Info.ID, "--brief")})
			}
			tbar.Set(p.Info.Size)
			tbar.DoneWith("Trimmed to the last compaction", kitBytes(res.Bytes)+" down to "+kitBytes(tr.Bytes))
			p.Trimmed, p.UntrimmedBytes = true, res.Bytes
			p.TrimPrompts, p.TrimRecords = tr.Prompts, tr.Records
			os.Remove(res.OutPath)
			res = tr
		}
		p.Bytes, p.SHA256, p.Stripped, p.Rewritten = res.Bytes, res.SHA256, res.Stripped, res.PathsRewritten
		p.Redactions.add(res.Redactions)
		p.Out = res.OutPath
		p.EstTokens, p.EstCompacted = res.EstTokens, res.EstCompacted
		p.SummaryChars = int64(len([]rune(sb.build(importToolLabel(tool), redactCounts{}))))
		if set.mode == "conversation" && p.Bytes == 0 {
			fail(&cliError{Code: exitFailed, Kind: "import_empty", Message: "this session holds no messages to carry once what must not travel is taken out. Nothing was sent. Pick another session",
				NextAction: "ks import list"})
		}
		if limit := importLimit(); set.mode == "conversation" && p.Bytes > limit {
			p.OverLimit = true
			if !deferOver { // a person at a terminal is offered the choices instead (importGuided)
				how := "once what must not travel is taken out"
				switch {
				case p.Trimmed:
					how = "once what must not travel is taken out and it is trimmed to its last compaction"
				case noTrimWhy != "":
					how += " (it holds a compaction, but " + noTrimWhy + ", so there is nothing to trim to)"
				case res.Boundary < 0:
					how += " (it holds no compaction to trim to)"
				}
				fail(&cliError{Code: exitConflict, Kind: "import_too_large",
					Message: fmt.Sprintf("this session is %s (%s bytes) %s, and a session can resume up to %s (%s bytes). Nothing was sent. Import a written summary of it instead, or compact it in %s first (run /compact in that session) and import again",
						bytesText(p.Bytes), commas(p.Bytes), how, bytesText(limit), commas(limit), importToolLabel(tool)),
					Title: "This conversation is too big for a session to resume",
					Why: []string{fmt.Sprintf("It is %s (%s bytes) %s, and a session can resume up to %s (%s bytes).", bytesText(p.Bytes), commas(p.Bytes), how, bytesText(limit), commas(limit)),
						"Nothing was sent. Import a written summary of it instead, or compact it in " + importToolLabel(tool) + " first (run /compact in that session) and import again."},
					NextAction: importSessionCommand(inv, tool, p.Info.ID, "--brief")})
			}
		}
	}
	if set.mode == "brief" {
		sumCounts := redactCounts{}
		p.Brief = sb.build(importToolLabel(tool), sumCounts)
		p.Redactions = sumCounts // only the summary travels
		p.Stripped = map[string]int{}
		sum := sha256Hex([]byte(p.Brief))
		p.SHA256, p.Bytes = sum, int64(len(p.Brief))
		p.Out = ""
		p.Rewritten = rw.Count
	}
	p.Decl = importDeclaration(p, set)
	return p
}

func importToolLabel(tool string) string {
	switch tool {
	case "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex CLI"
	}
	return "Cursor"
}

// importHashed keeps the upload plans already read, by folder, history and size.
var importHashed = map[string]*uploadPlan{}

// importFilesRoot plans the upload of the project's git repository and
// answers its root and the workspace path it will have.
func importFilesRoot(inv *Invocation, p *importPrep, cwd string) (string, string) {
	if inv.Set("project") {
		if abs, err := filepath.Abs(inv.Str("project")); err == nil {
			cwd = abs
		}
	}
	if cwd == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_project", Message: "this conversation names no project folder, so there are no files to bring. Nothing was sent", NextAction: importFilesNext(inv, p.Tool, p.Info.ID, "--no-files")})
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		fail(&cliError{Code: exitFailed, Kind: "no_project", Message: fmt.Sprintf("the project folder %s is not on this machine, so its files cannot be brought. Nothing was sent", visible(sanitize(cwd))),
			NextAction: importFilesNext(inv, p.Tool, p.Info.ID, "--no-files")})
	}
	top, ok := gitOut(cwd, "rev-parse", "--show-toplevel")
	if !ok {
		fail(&cliError{Code: exitFailed, Kind: "not_a_repository", Message: fmt.Sprintf("%s is not a git repository, and --with-files brings a repository. Nothing was sent", visible(sanitize(cwd))),
			NextAction: "ks file upload <folder> --session <id> (after the import) uploads any folder"})
	}
	top, _ = filepath.EvalSymlinks(top)
	p.Head, _ = gitOut(top, "rev-parse", "HEAD")
	st, _ := gitOut(top, "status", "--porcelain")
	p.Dirty = st != ""
	mode := importGitMode(inv)
	chosen := decideImportFiles(inv, p.Tool, p.Info.ID, probeImportFiles(top, mode != "without"), mode)
	// the plan is read once per run: a second preparation (the guided flow
	// prepares again after the files answer) keeps what was already hashed
	key := fmt.Sprintf("%s|%v|%d", top, chosen.History, chosen.Plan.Content)
	if h, ok := importHashed[key]; ok {
		chosen.Plan = h
	} else {
		hashUpload(chosen.Plan)
		importHashed[key] = chosen.Plan
	}
	p.Files, p.History, p.GitNote = chosen.Plan, chosen.History, chosen.Note
	return top, importGuestWorkspace + "/" + chosen.Plan.To
}

func importDeclaration(p *importPrep, set importSettings) map[string]any {
	src := map[string]any{"tool": p.Tool, "version": p.Info.Version, "session_id": p.Info.ID, "cwd": p.Info.Cwd, "model": p.Info.Model,
		"turns": p.Turns, "bytes_before": p.Info.Size, "bytes": p.Bytes, "sha256": p.SHA256,
		"redactions": map[string]int(p.Redactions), "stripped": map[string]int(p.Stripped), "paths_rewritten": p.Rewritten}
	if p.Tool == "codex" || p.Tool == "cursor" {
		src["started_at"] = utcStamp(p.Info.StartedAt)
	}
	if p.Trimmed {
		src["trimmed"] = true // bound into the digest: what travels is the trim, not the whole conversation
		src["untrimmed_bytes"] = p.UntrimmedBytes
	}
	if p.Mode == "conversation" && p.EstTokens > 0 {
		src["est_tokens"] = p.EstTokens // what the first reply reads, by the documented estimate
		if p.EstCompacted {
			src["est_compacted"] = true
		}
	}
	target := map[string]any{"harness": set.harness, "model": set.model, "key": set.key, "name": set.name, "budget_usd": 0.0}
	if set.spend != nil {
		target["budget_usd"] = float64(*set.spend) / 1e6
	}
	d := map[string]any{"mode": p.Mode, "source": src, "target": target}
	if p.Files != nil {
		d["workspace"] = map[string]any{"included": true, "folder": p.Files.To, "head": p.Head, "dirty": p.Dirty, "bytes": p.Files.Content, "history": p.History}
	} else {
		d["workspace"] = map[string]any{"included": false}
	}
	if p.Mode == "brief" {
		d["brief"] = p.Brief
	}
	return d
}

func utcStamp(s string) string {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

func chooseCursorChat(inv *Invocation) (*cursorStore, *cursorChat) {
	if inv.Bool("latest") == inv.Set("chat") {
		importUsage("name the chat to import: --latest (the newest) or --chat ID, and add --yes to import it without being asked. Nothing was sent", "ks import list --cursor")
	}
	store := firstNonEmptyStr(inv.Str("store"), cursorDefaultStore())
	cs, err := openCursorStore(store)
	if err != nil {
		fail(&cliError{Code: exitFailed, Kind: "cursor_unreadable", Message: sanitize(err.Error()) + ". Nothing was sent"})
	}
	chats, err := cs.chats()
	if err != nil {
		cs.Close()
		fail(&cliError{Code: exitFailed, Kind: "cursor_unreadable", Message: sanitize(err.Error()) + ". Nothing was sent"})
	}
	if len(chats) == 0 {
		cs.Close()
		fail(&cliError{Code: exitFailed, Kind: "cursor_empty", Message: "Cursor's store holds no chat with text. Nothing was sent"})
	}
	if inv.Bool("latest") {
		return cs, &chats[0]
	}
	id := inv.Str("chat")
	var hits []*cursorChat
	for i := range chats {
		if chats[i].ID == id {
			return cs, &chats[i]
		}
		if len(id) >= 4 && strings.HasPrefix(chats[i].ID, id) {
			hits = append(hits, &chats[i])
		}
	}
	cs.Close()
	if len(hits) == 1 {
		// the store is reopened for the one chat: its copy was closed with the list
		cs2, err := openCursorStore(store)
		if err != nil {
			fail(&cliError{Code: exitFailed, Kind: "cursor_unreadable", Message: sanitize(err.Error())})
		}
		return cs2, hits[0]
	}
	importUsage(fmt.Sprintf("no single Cursor chat starts with %q (%d match). Nothing was sent", sanitize(id), len(hits)), "ks import list --cursor")
	return nil, nil
}
