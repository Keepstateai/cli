// import_git_ask.go: the question a person at a terminal is asked about the
// repository's files and history, with the size of each and whether it fits.
package main

import (
	"fmt"
)

// igAskFiles asks which parts of the repository to bring. full: the first
// time, with "the conversation only" among the choices; not full: the person
// already said --with-files and only the history is asked. It sets the flags
// for the answer and says whether the conversation must be prepared again
// (its paths depend on whether files go). A cancel ends the command.
func igAskFiles(inv *Invocation, p *importPrep, pr *importFilesProbe, full bool) bool {
	if !pr.HasGit {
		return igAskFilesNoHistory(inv, p, pr, full)
	}

	var items []igItem
	var ids []string
	add := func(id, text string, off bool) {
		ids = append(ids, id)
		items = append(items, igItem{Text: text, Off: off})
	}
	files := func(pl *uploadPlan) string {
		return fmt.Sprintf("%s, %s", kitBytes(pl.Content), nounCount(len(pl.Entries), "file", "files"))
	}
	withOK, withy := planFits(pr.With)
	withoutOK, withouty := planFits(pr.Without)
	withText := fmt.Sprintf("with the files and their history (%s)", files(pr.With))
	if !withOK {
		withText = fmt.Sprintf("with the files and their history (%s, %s)", kitBytes(pr.With.Content), withy)
	}
	withoutText := fmt.Sprintf("with the files only, without the history (%s)", files(pr.Without))
	if !withoutOK {
		withoutText = fmt.Sprintf("with the files only, without the history (%s, %s)", kitBytes(pr.Without.Content), withouty)
	}
	if full {
		add("conv", fmt.Sprintf("conversation only %s", kitBytes(p.Bytes)), false)
	}
	add("with", withText, !withOK)
	add("without", withoutText, !withoutOK)
	add("cancel", "cancel", false)

	// what is marked first, and why when it is not the plain default
	def, note := 0, ""
	switch {
	case withOK:
		if !full {
			def = 0
		}
	case withoutOK:
		note = fmt.Sprintf("The history (.git) is %s, so with it the repository is over the 1 GB limit (1,000,000,000 bytes) and that choice cannot be taken. The files only fit, so they are marked.", kitBytes(pr.gitBytes()))
		def = indexOfID(ids, "without")
	default:
		note = fmt.Sprintf("The repository is over the 1 GB limit (1,000,000,000 bytes) even without its history (%s), so neither choice with files can be taken.", kitBytes(pr.Without.Content))
		if full {
			def = indexOfID(ids, "conv")
		} else {
			def = indexOfID(ids, "cancel")
		}
	}
	title := fmt.Sprintf("Bring the project files too? The git repository at %s.", igShortHome(pr.Top))
	if !full {
		title = fmt.Sprintf("Bring the repository's history too? The git repository at %s.", igShortHome(pr.Top))
	}

	i := igChooseItems(title, note, items, def)
	if i < 0 || ids[i] == "cancel" {
		igDeclined()
	}
	switch ids[i] {
	case "with":
		inv.set["with-files"], inv.set["with-git"] = true, true
		return full || !p.History
	case "without":
		inv.set["with-files"], inv.set["without-git"] = true, true
		return full || p.History
	}
	return false // the conversation only
}

// igAskFilesNoHistory is the plain question for a folder with no history of
// its own to choose (no .git at the top).
func igAskFilesNoHistory(inv *Invocation, p *importPrep, pr *importFilesProbe, full bool) bool {
	if !full {
		return false
	}
	ok, why := planFits(pr.Without)
	text := fmt.Sprintf("with the files (%s, %s)", kitBytes(pr.Without.Content), nounCount(len(pr.Without.Entries), "file", "files"))
	if !ok {
		text = fmt.Sprintf("with the files (%s, %s)", kitBytes(pr.Without.Content), why)
	}
	items := []igItem{{Text: fmt.Sprintf("conversation only %s", kitBytes(p.Bytes))}, {Text: text, Off: !ok}, {Text: "cancel"}}
	i := igChooseItems(fmt.Sprintf("Bring the project files too? The git repository at %s.", igShortHome(pr.Top)), "", items, 0)
	if i < 0 || i == 2 {
		igDeclined()
	}
	if i == 1 {
		inv.set["with-files"] = true
		return true
	}
	return false
}

func indexOfID(ids []string, id string) int {
	for i, s := range ids {
		if s == id {
			return i
		}
	}
	return 0
}
