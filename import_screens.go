// import_screens.go: what the import prints to a person, drawn with the
// shared kit (ui.go). The facts are the same ones the plan's JSON carries; here
// they are rows, not paragraphs.
package main

import (
	"fmt"
	"strings"
)

// estimateWords says the first-read estimate in one sentence. Every figure is
// labelled an estimate; the prices are the service's, never this client's.
func estimateWords(e *importEstimateAnswer) string {
	if e == nil || e.Tokens <= 0 {
		return ""
	}
	s := fmt.Sprintf("the first reply reads about %s tokens", commas(e.Tokens))
	for i, m := range e.Models {
		join := ", "
		if i == 0 {
			join = ": about "
		} else {
			join += "about "
		}
		s += fmt.Sprintf("%s$%s on %s (each later reply about $%s)", join, microsShown(m.FirstReadMicroUSD), m.Name, microsShown(m.LaterReplyMicroUSD))
	}
	return s + ". Estimates, not quotes"
}

// microsShown is micro-dollars as dollars with cents, and a decimal more
// below a dollar.
func microsShown(m int64) string {
	switch {
	case m >= 1_000_000:
		return fmt.Sprintf("%.2f", float64(m)/1e6)
	case m > 0:
		return fmt.Sprintf("%.3f", float64(m)/1e6)
	}
	return "0.00"
}

// importPlanFacts are the rows of the plan, in the order a person reads them.
func importPlanFacts(p *importPrep, set importSettings, svc importServicePlan) [][2]string {
	label := importToolLabel(p.Tool)
	name := firstNonEmptyStr(set.name, importSessionName(p))
	v := ""
	if p.Info.Version != "" {
		v = " " + p.Info.Version
	}
	rows := [][2]string{{"Source", sanitize(fmt.Sprintf("%s%s session %s from %s", label, v, p.Info.ID, firstNonEmptyStr(p.Info.Cwd, "an unknown folder")))}}
	rows = append(rows, [2]string{"New session", fmt.Sprintf("%q, agent main on %s", name, set.harness)})
	rows = append(rows, [2]string{"Model", firstNonEmptyStr(set.model, "the default for "+set.harness)})
	rows = append(rows, [2]string{"Key", firstNonEmptyStr(set.key, "your account's default key for the provider")})
	if set.spend != nil {
		rows = append(rows, [2]string{"Spend limit", fmt.Sprintf("$%.2f on KeepState's key", float64(*set.spend)/1e6)})
	}
	if p.Mode == "conversation" {
		rows = append(rows, [2]string{"Carries", fmt.Sprintf("the full conversation, %s of your messages and %s records", commas(int64(p.Turns)), commas(int64(p.Info.Messages)))})
		rows = append(rows, [2]string{"Size", fmt.Sprintf("%s to upload (%s read)", kitBytes(p.Bytes), kitBytes(p.Info.Size))})
		rows = append(rows, [2]string{"Taken out", sanitize(strippedWords(p.Stripped))})
	} else {
		rows = append(rows, [2]string{"Carries", fmt.Sprintf("a written summary of %s characters, not the conversation", commas(int64(len([]rune(p.Brief)))))})
		rows = append(rows, [2]string{"Written by", "fixed rules on this machine (no model wrote it)"})
	}
	rows = append(rows, [2]string{"Secrets", sanitize(p.Redactions.words()) + " replaced on this machine (never shown)"})
	if p.Rewritten > 0 {
		rows = append(rows, [2]string{"Paths", fmt.Sprintf("%s mentions of the old project folder become %s", commas(int64(p.Rewritten)), p.Dest)})
	}
	if p.Files != nil {
		dirty := "none"
		if p.Dirty {
			dirty = "yes"
		}
		rows = append(rows, [2]string{"Files", fmt.Sprintf("the git repository %s: %s in %s, uploaded as it is now (git HEAD %s, uncommitted changes: %s). Left out: .env and credential files, dependency folders such as node_modules, what .gitignore lists, links. The repository's own history (.git) is carried", sanitize(visible(p.Root)), kitBytes(p.Files.Content), nounCount(len(p.Files.Entries), "file", "files"), short64(p.Head), dirty)})
	} else if p.Tool != "cursor" || p.Info.Cwd != "" {
		rows = append(rows, [2]string{"Files", "none (add --with-files to bring the git repository)"})
	}
	if w := estimateWords(svc.Plan.Estimate); w != "" && p.Mode == "conversation" {
		rows = append(rows, [2]string{"Estimate", w})
	}
	return rows
}

// printImportPlan draws the plan on stdout. Nothing has been created.
func printImportPlan(p *importPrep, set importSettings, svc importServicePlan) {
	o, _ := cmdUI()
	label := importToolLabel(p.Tool)
	o.Title("", toneOK, "Import from "+label, "nothing has been created")
	if live := importLiveSentence(p); live != "" {
		o.Blank()
		o.Warn(live)
	}
	if p.Tool == "cursor" {
		o.Blank()
		o.Warn("Reading Cursor's chats is best effort", "Cursor does not document its chat store. Nothing is resumed: the chat goes across as a written summary.")
	}
	if p.Mode == "brief" && p.Tool != "cursor" {
		o.Blank()
		o.Warn("A "+label+" conversation is not resumed in another agent", "It goes across as a written summary.")
	}
	o.Blank()
	o.Facts(importPlanFacts(p, set, svc)...)
	if e := svc.Plan.Estimate; e != nil && p.Mode == "conversation" {
		for _, m := range e.Models {
			if m.OverContext && m.Note != "" {
				o.Blank()
				o.Warn("The estimate is over "+m.Name+"'s context window", m.Note)
				break
			}
		}
	}
	if e := svc.Plan.Estimate; e != nil && e.Notice && p.Mode == "conversation" {
		o.Blank()
		o.Warn("This is a big conversation: "+e.NoticeReason, "Its first reply re-reads all of it (about "+commas(e.Tokens)+" tokens, an estimate), so it is dear. Compacting it in "+label+" first, or importing a written summary with --brief, costs far less.")
	}
	var from []string
	for _, l := range svc.Plan.Lines {
		if !strings.HasPrefix(l, "Estimate (not a quote)") {
			from = append(from, l)
		}
	}
	if len(from) > 0 {
		o.Blank()
		o.Title("", toneOK, "From KeepState")
		for _, l := range from {
			o.Note(l)
		}
	}
	o.Blank()
	o.Note("Creating the session calls no model. The agent's first reply re-reads the whole conversation, so it costs more than a short message, and the imported bytes count toward your storage. Approvals you gave in the old session do not carry over.")
}

// importLiveSentence is the notice for a conversation written in the last
// two minutes, or "".
func importLiveSentence(p *importPrep) string {
	if p.Tool == "cursor" || p.Info.ModTime.IsZero() || timeSince(p.Info.ModTime) >= importLiveWindow {
		return ""
	}
	return fmt.Sprintf("This conversation is open in %s right now; what you say after this point stays behind.", importToolLabel(p.Tool))
}
