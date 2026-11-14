// import_run.go: the plan and the execution of ks import.
//
// A plan creates nothing. It reads the conversation, takes it apart, and shows
// the person what would go up; the service judges the same declaration and
// answers the plan's digest. --execute names that digest. The conversation is
// read again, the service is asked again, and a digest that is not the same
// means the conversation moved since the plan was made: nothing is created.
//
// Execution is one operation in a fixed order: the agent session is created
// (with no first instruction, so no model is called), the project's files go
// up when they were asked for, the conversation goes up through the same
// chunked upload every file uses, and only then does the service place it and
// make it the agent's conversation. What was done so far is kept in a small
// file under the digest, so the same command run again continues instead of
// creating a second session.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type importServicePlan struct {
	Digest string `json:"digest"`
	Mode   string `json:"mode"`
	Plan   struct {
		Lines    []string `json:"lines"`
		MaxBytes int64    `json:"max_bytes"`
	} `json:"plan"`
}

func (p *importPrep) localLines(set importSettings) []string {
	var l []string
	label := importToolLabel(p.Tool)
	add := func(f string, a ...any) { l = append(l, sanitize(fmt.Sprintf(f, a...))) }
	switch p.Tool {
	case "cursor":
		add("Cursor chat %s%s: %d turns of text. Cursor's storage is not documented, so reading it is best effort. Cursor is not an agent KeepState runs, so nothing is resumed: the chat goes across as a written summary.", p.Info.ID, nameWords(p.Info.Model, ", model "), p.Turns)
	default:
		v := ""
		if p.Info.Version != "" {
			v = " " + p.Info.Version
		}
		add("%s%s session %s from %s: %d of your messages, %d records in all%s.", label, v, p.Info.ID, firstNonEmptyStr(p.Info.Cwd, "an unknown folder"), p.Turns, p.Info.Messages, nameWords(p.Info.Model, ", last model "))
	}
	if p.Mode == "conversation" {
		add("It goes up as the conversation itself: %s after taking out what must not travel (%s were read). Taken out: %s.", bytesText(p.Bytes), bytesText(p.Info.Size), strippedWords(p.Stripped))
		add("%s agent %s continues exactly this conversation, with its own system prompt and tools: the old machine's prompt, tool list, account ids, permission modes, signed thinking and encrypted reasoning are not carried.", label, "in the new session")
	} else {
		add("It goes up as a written summary of %d characters, built on this machine from the conversation's text by fixed rules (no model wrote it), never as a transcript. The new agent reads it with your first instruction, as data, not as instructions.", len([]rune(p.Brief)))
		if p.Tool != "cursor" {
			add("A %s conversation is not resumed in another agent: it goes across as a summary.", label)
		}
	}
	add("Secrets replaced on this machine before anything was sent: %s. The secrets themselves are never shown.", p.Redactions.words())
	if p.Rewritten > 0 {
		add("Paths: %d mentions of the old project folder are said as %s.", p.Rewritten, p.Dest)
	}
	if p.Files != nil {
		add("Files: the git repository %s goes to %s in the workspace: %s in %s under the upload policy. Left out: .env files and other credential-named files, node_modules and other dependency folders, what .gitignore lists, and links. The repository's own history (.git) is carried.", filepath.Base(p.Root), p.Dest, bytesText(p.Files.Content), nounCount(len(p.Files.Entries), "file", "files"))
		dirty := "none"
		if p.Dirty {
			dirty = "yes"
		}
		add("The files are uploaded as they are now, not as they were at the conversation's last turn (git HEAD %s, uncommitted changes: %s).", short64(p.Head), dirty)
	} else if p.Tool != "cursor" || p.Info.Cwd != "" {
		add("No files go along: the agent will find the project missing unless you add --with-files (the git repository at %s) or upload it with ks file upload.", firstNonEmptyStr(p.Info.Cwd, "the project folder"))
	}
	key := firstNonEmptyStr(set.key, "your account's default key for the provider")
	model := firstNonEmptyStr(set.model, "the service's default")
	budget := "the service's default"
	if set.spend != nil {
		budget = fmt.Sprintf("$%.2f on KeepState's key", float64(*set.spend)/1e6)
	}
	add("New session: harness %s, model %s, key: %s, spend limit: %s.", set.harness, model, key, budget)
	add("Cost: the agent's first reply re-reads the whole imported conversation, so it costs more than a short message; the imported bytes count toward your storage.")
	add("Approvals you gave in the old session carry nothing: every action is asked again.")
	return l
}

func nameWords(v, lead string) string {
	if v == "" {
		return ""
	}
	return lead + v
}

func nounCount(n int, one, many string) string {
	return fmt.Sprintf("%s %s", commas(int64(n)), noun(n, one, many))
}

func serverImportPlan(cr hostedCreds, decl map[string]any) importServicePlan {
	var env struct {
		Data importServicePlan `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/imports", decl, &env); err != nil {
		die(err)
	}
	if len(env.Data.Digest) != 64 {
		fail(&cliError{Code: exitFailed, Kind: "bad_plan", Message: "the service's plan carries no digest, so nothing can be executed against it. Nothing was created"})
	}
	return env.Data
}

// ---- progress kept under the digest ----------------------------------------------

type importState struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	FilesDone bool   `json:"files_done"`
	ConvDone  bool   `json:"conversation_uploaded"`
	UploadID  string `json:"upload_id"`
}

var digestSafe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func importStatePath(digest string) string {
	return filepath.Join(configDir(), "imports", digest[:32]+".json")
}

func loadImportState(digest string) importState {
	var s importState
	if b, err := os.ReadFile(importStatePath(digest)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveImportState(digest string, s importState) {
	p := importStatePath(digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(s)
	_ = os.WriteFile(p, b, 0o600)
}

func sessionStillThere(cr hostedCreds, id string) bool {
	return id != "" && hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, nil) == nil
}

var sessionNameBad = regexp.MustCompile(`[^A-Za-z0-9 _-]+`)

func importSessionName(p *importPrep) string {
	base := filepath.Base(firstNonEmptyStr(p.Root, p.Info.Cwd))
	if base == "" || base == "." || base == "/" {
		base = "imported"
	}
	name := strings.TrimSpace(sessionNameBad.ReplaceAllString(base, " ")) + " import"
	name = strings.TrimLeft(name, " 0123456789_-")
	if len(name) > 60 {
		name = name[:60]
	}
	if !displayNameOK(name, sessionNameMax) {
		return "imported session"
	}
	return name
}

// ---- the verbs ---------------------------------------------------------------------

func importTool(tool string) func(*Invocation) {
	return func(inv *Invocation) {
		write := inv.Set("out")
		if !write && inv.Bool("plan") == (inv.Str("execute") != "") {
			importUsage("an import is planned first (--plan) and executed by naming that plan's digest (--execute DIGEST): one of the two. Nothing was created", "ks import "+tool+" --latest --plan")
		}
		if inv.Bool("plan") && inv.Set("execute") {
			importUsage("--plan and --execute are two steps: one of them. Nothing was created", "")
		}
		p := prepareImport(inv, tool)
		defer p.Close()
		set := importSettingsOf(inv, tool)
		if write {
			importWriteOut(p, inv.Str("out"))
			return
		}
		cr, ok := hostedToken()
		if !ok {
			fail(&cliError{Code: exitAuth, Kind: "not_signed_in", Message: "Not signed in. Run: ks login", NextAction: "ks login"})
		}
		requireCapability(cr, inv.Cmd)
		svc := serverImportPlan(cr, p.Decl)
		if inv.Bool("plan") {
			importShowPlan(p, set, svc, inv)
			return
		}
		want := strings.ToLower(strings.TrimSpace(inv.Str("execute")))
		if !digestSafe.MatchString(want) || want != svc.Digest {
			fail(&cliError{Code: exitFailed, Kind: "plan_changed", Message: "the session changed since the plan was made (or this is not that plan's digest), so nothing was created. Plan again and execute the new digest",
				NextAction: "ks import " + tool + " " + importReplayArgs(inv, "--plan")})
		}
		importExecute(cr, inv, p, set, svc)
	}
}

// importReplayArgs is the command line's flags again with one added.
func importReplayArgs(inv *Invocation, extra string) string {
	var parts []string
	for _, f := range []string{"latest", "with-files", "brief"} {
		if inv.Bool(f) {
			parts = append(parts, "--"+f)
		}
	}
	for _, f := range []string{"session", "chat", "project", "store", "name", "model", "key", "budget", "harness"} {
		if inv.Set(f) {
			parts = append(parts, "--"+f, shellWord(inv.Str(f)))
		}
	}
	return strings.Join(append(parts, extra), " ")
}

func importPlanDoc(p *importPrep, set importSettings, svc importServicePlan) map[string]any {
	lines := append(p.localLines(set), svc.Plan.Lines...)
	files := map[string]any{"included": false}
	if p.Files != nil {
		files = map[string]any{"included": true, "folder": p.Files.To, "files": len(p.Files.Entries), "bytes": p.Files.Content, "head": p.Head, "dirty": p.Dirty, "left_out": len(p.Files.Skipped)}
	}
	return map[string]any{"digest": svc.Digest, "mode": p.Mode, "tool": p.Tool, "session_id": p.Info.ID, "turns": p.Turns,
		"bytes_before": p.Info.Size, "bytes": p.Bytes, "redactions": map[string]int(p.Redactions), "stripped": map[string]int(p.Stripped),
		"paths_rewritten": p.Rewritten, "files": files, "best_effort": p.Tool == "cursor",
		"target": map[string]any{"harness": set.harness, "model": set.model, "key": set.key, "name": set.name},
		"lines":  lines}
}

func importShowPlan(p *importPrep, set importSettings, svc importServicePlan, inv *Invocation) {
	doc := importPlanDoc(p, set, svc)
	emit(doc, func() {
		fmt.Println("Import plan (nothing has been created):")
		for _, l := range doc["lines"].([]string) {
			fmt.Println("  " + l)
		}
		fmt.Printf("plan digest: %s\n", svc.Digest)
		fmt.Printf("to do it: ks import %s %s\n", p.Tool, importReplayArgs(inv, "--execute "+svc.Digest))
	})
}

func importWriteOut(p *importPrep, dst string) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s cannot be written (%v); an existing file is never replaced. Nothing else was done", visible(sanitize(dst)), sanitize(err.Error()))})
	}
	var data []byte
	if p.Mode == "brief" {
		data = []byte(p.Brief)
	} else {
		data, err = os.ReadFile(p.Out)
		if err != nil {
			f.Close()
			die(err)
		}
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		die(err)
	}
	if err := f.Close(); err != nil {
		die(err)
	}
	emit(map[string]any{"out": dst, "mode": p.Mode, "bytes": len(data), "sha256": p.SHA256, "redactions": map[string]int(p.Redactions), "stripped": map[string]int(p.Stripped), "paths_rewritten": p.Rewritten}, func() {
		fmt.Printf("wrote %s (%s, sha256 %s); nothing was sent and nothing was created. Secrets replaced: %s.\n", visible(sanitize(dst)), bytesText(int64(len(data))), short64(p.SHA256), p.Redactions.words())
	})
}

func importExecute(cr hostedCreds, inv *Invocation, p *importPrep, set importSettings, svc importServicePlan) {
	st := loadImportState(svc.Digest)
	if st.SessionID != "" && !sessionStillThere(cr, st.SessionID) {
		st = importState{}
	}
	if st.SessionID == "" {
		harness, err := chooseHarness(cr, set.harness)
		if err != nil {
			die(err)
		}
		name := firstNonEmptyStr(set.name, importSessionName(p))
		progress("creating a %s agent session %q; no instruction is sent, so no model is called yet", set.harness, name)
		rec, prov, _, err := startAgentSession(cr, name, "", set.key, nil, set.model, "", harness, set.spend)
		if err != nil {
			die(err)
		}
		if !prov.Ready && !prov.Supervised {
			if prov.CleanedUp || !phaseDone(prov.Phases, "start_agent") {
				fail(failedStart(cr, prov, rec.ID, "ks import (to try again), or ks doctor", func(string) string {
					return "ks import " + p.Tool + " " + importReplayArgs(inv, "--execute "+svc.Digest)
				}))
			}
			fail(&cliError{Code: exitTemporary, Kind: "agent_not_ready", Message: prov.Note, NextAction: "ks agent status main --session " + rec.ID})
		}
		st = importState{SessionID: rec.ID, AgentID: prov.AgentID}
		saveImportState(svc.Digest, st)
	}
	sess := inventoryRow{ID: st.SessionID, RecordID: st.SessionID, ShortID: short64(st.SessionID)}
	again := "ks import " + p.Tool + " " + importReplayArgs(inv, "--execute "+svc.Digest)
	quiet := &Invocation{set: map[string]bool{}, strs: map[string]string{}}

	if p.Files != nil && !st.FilesDone {
		files := *p.Files
		u := &uploader{ctx: context.Background(), cr: cr, inv: &Invocation{set: map[string]bool{"replace": true}, strs: map[string]string{}}, sess: sess, agent: "main", plan: &files, tty: stderrIsTerminal(), againCmd: again}
		progress("uploading the project's files: %s (%s) to %s", visible(files.Name), bytesText(files.Content), visible(p.Dest))
		u.declare(false)
		if u.view.State != "placed" {
			u.send()
			u.view = u.complete()
		}
		st.FilesDone = true
		saveImportState(svc.Digest, st)
	}
	if p.Mode == "conversation" && !st.ConvDone {
		plan := &uploadPlan{Kind: "file", Wire: "conversation", Name: "the conversation", Local: p.Out, To: p.Harness + "/" + p.ConvID, Mode: "0644",
			Content: p.Bytes, Size: p.Bytes, SHA256: p.SHA256, Skipped: []uploadSkip{}}
		u := &uploader{ctx: context.Background(), cr: cr, inv: quiet, sess: sess, agent: "main", plan: plan, tty: stderrIsTerminal(), againCmd: again}
		progress("uploading the conversation (%s)", bytesText(p.Bytes))
		u.declare(false)
		if u.view.State != "verified" && u.view.State != "placed" {
			u.send()
			u.view = u.complete()
		}
		if u.view.State != "verified" && u.view.State != "placed" {
			fail(&cliError{Code: exitFailed, Kind: "upload_not_verified", WorkStarted: workYes,
				Message: "the session's machine did not confirm the conversation's bytes (state " + sanitize(u.view.State) + "), so it was not placed", NextAction: again})
		}
		st.ConvDone, st.UploadID = true, u.view.UploadID
		saveImportState(svc.Digest, st)
	}
	body := map[string]any{"digest": svc.Digest, "declaration": p.Decl, "agent_id": st.AgentID}
	if p.Mode == "conversation" {
		body["upload_id"] = st.UploadID
	}
	var env struct {
		Data struct {
			ImportID       string `json:"import_id"`
			Mode           string `json:"mode"`
			AgentID        string `json:"agent_id"`
			SessionID      string `json:"session_id"`
			ConversationID string `json:"conversation_id"`
			Message        string `json:"message"`
			Next           string `json:"next"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/imports/apply", body, &env); err != nil {
		fail(&cliError{Code: exitFailed, Kind: "import_not_applied", WorkStarted: workYes,
			Message:    "the session and the uploads are in place, but the service did not apply the import: " + sanitize(errText(err)),
			NextAction: again + " (it continues from here and creates nothing new)"})
	}
	d := env.Data
	nextSend := fmt.Sprintf("ks agent send main \"...\" --session %s   (the agent continues from here; its first reply re-reads the whole conversation)", st.SessionID)
	if p.Mode == "brief" {
		nextSend = fmt.Sprintf("ks agent send main \"...\" --session %s   (the summary reaches the agent with this first instruction)", st.SessionID)
	}
	res := map[string]any{"import_id": d.ImportID, "mode": p.Mode, "session_id": firstNonEmptyStr(d.SessionID, st.SessionID), "agent_id": firstNonEmptyStr(d.AgentID, st.AgentID),
		"conversation_id": d.ConversationID, "message": d.Message, "redactions": map[string]int(p.Redactions), "files_uploaded": p.Files != nil}
	emit(res, func() {
		what := "its conversation"
		if p.Mode == "brief" {
			what = "a written summary of it"
		}
		fmt.Printf("Imported %s session %s into session %s (agent main): %s is in place. %s\n", importToolLabel(p.Tool), visible(sanitize(p.Info.ID)), st.SessionID, what, sanitize(d.Message))
		fmt.Printf("next: %s\n", nextSend)
		fmt.Printf("      ks agent open main --session %s\n", st.SessionID)
	})
	_ = os.Remove(importStatePath(svc.Digest))
}
