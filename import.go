// import.go: ks import, a Claude Code, Codex CLI or Cursor conversation
// brought into a KeepState session.
//
//	ks import list [--cursor] [--store FILE] [--json]
//	ks import claude-code|codex [--latest | --session ID] [--project DIR]
//	      [--with-files] [--name NAME] [--model M] [--key own|keepstate|ALIAS]
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

// localTranscripts reads every transcript of the tools asked for, newest first.
func localTranscripts(tools ...string) []transcriptInfo {
	var all []transcriptInfo
	for _, tool := range tools {
		for _, p := range transcriptFiles(tool) {
			info, err := scanTranscript(tool, p)
			if err != nil || info.ID == "" {
				continue
			}
			all = append(all, info)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })
	return all
}

type importListEntry struct {
	Tool     string `json:"tool"`
	ID       string `json:"id"`
	Project  string `json:"project"`
	Date     string `json:"date"`
	Bytes    int64  `json:"bytes"`
	Messages int    `json:"messages"`
	Turns    int    `json:"turns"`
	Version  string `json:"version,omitempty"`
	Model    string `json:"model,omitempty"`
	Note     string `json:"note,omitempty"`
}

func importList(inv *Invocation) {
	var rows []importListEntry
	for _, t := range localTranscripts("claude-code", "codex") {
		rows = append(rows, importListEntry{Tool: t.Tool, ID: t.ID, Project: t.Cwd, Date: t.ModTime.UTC().Format(time.RFC3339), Bytes: t.Size, Messages: t.Messages, Turns: t.Turns, Version: t.Version, Model: t.Model})
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
	doc := map[string]any{"sessions": rows}
	if cursorNote != "" {
		doc["cursor_note"] = cursorNote
	}
	emit(doc, func() {
		if len(rows) == 0 {
			fmt.Println("no Claude Code or Codex session was found on this machine (Claude Code keeps them in " + visible(claudeProjectsDir()) + ", Codex in " + visible(codexSessionsDir()) + ")")
		}
		for _, r := range rows {
			note := ""
			if r.Note != "" {
				note = "  (" + r.Note + ")"
			}
			fmt.Printf("%-11s %s  %s  %9s  %5d messages  %s%s\n", r.Tool, r.ID, strings.TrimSuffix(r.Date, "Z")[:16], bytesText(r.Bytes), r.Messages, visible(sanitize(r.Project)), note)
		}
		if cursorNote != "" {
			fmt.Println(cursorNote)
		}
		if len(rows) > 0 {
			fmt.Println("import one with: ks import claude-code --latest   (or codex, or cursor; --session ID names one)")
		}
	})
}

// ---- choosing one --------------------------------------------------------------

func importUsage(msg, next string) {
	fail(&cliError{Code: exitUsage, Kind: "usage", Message: msg, NextAction: next})
}

// chooseTranscript answers the one session --latest or --session names.
func chooseTranscript(tool string, inv *Invocation) transcriptInfo {
	if inv.Bool("latest") == inv.Set("session") {
		importUsage("name the session to import: --latest (the newest for this project) or --session ID. Nothing was sent", "ks import list")
	}
	all := localTranscripts(tool)
	if inv.Set("session") {
		id := inv.Str("session")
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

type importPrep struct {
	Tool       string
	Mode       string // conversation | brief
	Harness    string
	Info       transcriptInfo
	ConvID     string
	Decl       map[string]any
	Brief      string
	Out        string // the sanitized transcript (conversation mode)
	Bytes      int64
	SHA256     string
	Redactions redactCounts
	Stripped   map[string]int
	Rewritten  int
	Files      *uploadPlan
	Root       string
	Head       string
	Dirty      bool
	Turns      int
	Dest       string
	CursorNote string
	cleanup    []func()
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
	s := importSettings{model: inv.Str("model"), key: inv.Str("key"), files: inv.Bool("with-files")}
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
		res, err := sanitizeTranscript(p.Info, rw, sb, filepath.Join(tmp, "transcript.jsonl"))
		if err != nil {
			fail(&cliError{Code: exitFailed, Kind: "transcript_unreadable", Message: fmt.Sprintf("the %s session %s could not be read: %s. Nothing was sent", tool, p.Info.ID, sanitize(err.Error()))})
		}
		p.Turns = p.Info.Turns
		p.Bytes, p.SHA256, p.Stripped, p.Rewritten = res.Bytes, res.SHA256, res.Stripped, res.PathsRewritten
		p.Redactions.add(res.Redactions)
		p.Out = res.OutPath
		if set.mode == "conversation" && p.Bytes > importMaxBytes {
			fail(&cliError{Code: exitConflict, Kind: "import_too_large",
				Message: fmt.Sprintf("this session is %s (%s bytes) once what must not travel is taken out; the limit is %s (%s bytes). Nothing was sent. Start a new session in %s from a shorter one, or import it as a written summary with --brief",
					bytesText(p.Bytes), commas(p.Bytes), bytesText(importMaxBytes), commas(importMaxBytes), tool),
				NextAction: "ks import " + tool + " --session " + p.Info.ID + " --brief"})
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

// importFilesRoot plans the upload of the project's git repository and
// answers its root and the workspace path it will have.
func importFilesRoot(inv *Invocation, p *importPrep, cwd string) (string, string) {
	if inv.Set("project") {
		if abs, err := filepath.Abs(inv.Str("project")); err == nil {
			cwd = abs
		}
	}
	if cwd == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_project", Message: "this conversation names no project folder, so there are no files to bring. Nothing was sent", NextAction: "run it again without --with-files, or name the folder with --project"})
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		fail(&cliError{Code: exitFailed, Kind: "no_project", Message: fmt.Sprintf("the project folder %s is not on this machine, so its files cannot be brought. Nothing was sent", visible(sanitize(cwd))),
			NextAction: "run it again without --with-files, or name where the folder is now with --project"})
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
	plan := planUpload(top, "", true)
	hashUpload(plan)
	p.Files = plan
	return top, importGuestWorkspace + "/" + plan.To
}

func importDeclaration(p *importPrep, set importSettings) map[string]any {
	src := map[string]any{"tool": p.Tool, "version": p.Info.Version, "session_id": p.Info.ID, "cwd": p.Info.Cwd, "model": p.Info.Model,
		"turns": p.Turns, "bytes_before": p.Info.Size, "bytes": p.Bytes, "sha256": p.SHA256,
		"redactions": map[string]int(p.Redactions), "stripped": map[string]int(p.Stripped), "paths_rewritten": p.Rewritten}
	if p.Tool == "codex" || p.Tool == "cursor" {
		src["started_at"] = utcStamp(p.Info.StartedAt)
	}
	target := map[string]any{"harness": set.harness, "model": set.model, "key": set.key, "name": set.name, "budget_usd": 0.0}
	if set.spend != nil {
		target["budget_usd"] = float64(*set.spend) / 1e6
	}
	d := map[string]any{"mode": p.Mode, "source": src, "target": target}
	if p.Files != nil {
		d["workspace"] = map[string]any{"included": true, "folder": p.Files.To, "head": p.Head, "dirty": p.Dirty, "bytes": p.Files.Content}
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
		importUsage("name the chat to import: --latest (the newest) or --chat ID. Nothing was sent", "ks import list --cursor")
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
