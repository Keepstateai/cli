// import_git.go: which parts of the project's repository an import brings.
//
// A repository has two parts a person may want or not: its files, and its
// history (.git), which can be many times the size of the files and counts
// toward the same 1 GB limit. The two are measured before anything is
// chosen, so a history that does not fit is a choice that is told, never a
// refusal that comes before the question.
//
//	--with-git     the files and the history
//	--without-git  the files only
//	--with-files   the files, and the history when it fits
//
// Both git flags imply --with-files. A person at a terminal is asked
// (importGuided); without a terminal nothing is asked and the part that fits
// goes, or the refusal names the exact flag that will work.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// importFilesProbe is what the repository holds, measured both ways.
type importFilesProbe struct {
	Top     string
	HasGit  bool        // .git is a folder: a history that can be brought
	GitWhy  string      // when .git is there but cannot be brought, why, in words
	Without *uploadPlan // the files only
	With    *uploadPlan // the files and the history; nil when there is none to bring, or it was not measured
}

// probeImportFiles measures the repository at top. Nothing is refused here
// for its size. The history is walked only when it may be asked for
// (needHistory): --without-git never touches it.
//
// A history is brought only when .git is a folder. A .git that is a file (a
// worktree's pointer to a gitdir elsewhere) or a link would upload a pointer
// to nothing, so it is not brought, and the probe says why.
func probeImportFiles(top string, needHistory bool) *importFilesProbe {
	pr := &importFilesProbe{Top: top}
	pr.Without = planUploadSoft(top, "", false)
	fi, err := os.Lstat(filepath.Join(top, ".git"))
	switch {
	case err != nil:
	case fi.IsDir():
		pr.HasGit = true
		if needHistory {
			pr.With = planUploadSoft(top, "", true)
		}
	case fi.Mode()&os.ModeSymlink != 0:
		pr.GitWhy = ".git here is a link, and a link is never uploaded, so its history cannot be brought"
	default:
		pr.GitWhy = ".git here is a file that points to a history kept elsewhere (a worktree), so the history cannot be brought"
	}
	return pr
}

// gitBytes is the history's size: what it adds to the files.
func (pr *importFilesProbe) gitBytes() int64 {
	if pr.With == nil {
		return 0
	}
	return pr.With.GitSize
}

// planFits says whether a plan is within what one upload carries, and if not,
// in words why not.
func planFits(p *uploadPlan) (bool, string) {
	switch {
	case p.Content > uploadLimit:
		return false, "over the 1 GB limit"
	case len(p.Entries) > uploadMaxFiles:
		return false, "over the " + commas(uploadMaxFiles) + " file limit"
	case len(p.Entries) == 0:
		return false, "no file to bring"
	}
	return true, ""
}

func (pr *importFilesProbe) withFits() bool {
	if pr.With == nil {
		return false
	}
	ok, _ := planFits(pr.With)
	return ok
}

// limitWords says, from planFits's why, which limit it is.
func limitWords(why string) string {
	switch {
	case strings.Contains(why, "file limit"):
		return "one upload carries up to " + commas(uploadMaxFiles) + " files"
	case why == "no file to bring":
		return "no file is left to bring once what the upload policy leaves out is taken away"
	}
	return "one upload carries up to 1 GB (1,000,000,000 bytes)"
}

func (pr *importFilesProbe) withoutFits() bool {
	ok, _ := planFits(pr.Without)
	return ok
}

// importGitMode is what the flags say about the history: "with", "without"
// or "" (not said). The two together, or either with --no-files, are one
// instruction too many.
func importGitMode(inv *Invocation) string {
	with, without := inv.Bool("with-git"), inv.Bool("without-git")
	switch {
	case with && without:
		importUsage("--with-git and --without-git say opposite things: one of them. Nothing was created", "")
	case (with || without) && inv.Bool("no-files"):
		importUsage("--no-files brings no files, so there is no history to choose: drop --no-files or the git flag. Nothing was created", "")
	case with:
		return "with"
	case without:
		return "without"
	}
	return ""
}

// importWantsFiles: the project's files are asked for by any of the flags.
func importWantsFiles(inv *Invocation) bool {
	return inv.Bool("with-files") || inv.Bool("with-git") || inv.Bool("without-git")
}

// importFilesChosen is the part of the repository that goes.
type importFilesChosen struct {
	Plan    *uploadPlan
	History bool   // the history (.git) is in the plan
	Note    string // why the history is not, when it was left out for its size
}

// repoName is the folder's name for a sentence.
func repoName(top string) string { return sanitize(visible(filepath.Base(top))) }

// importFilesNext is the same command again with the files said another way.
func importFilesNext(inv *Invocation, tool, id, flag string) string {
	skip := map[string]bool{"latest": true, "session": true, "chat": true, "with-files": true, "with-git": true, "without-git": true}
	parts := importFlagParts(inv, skip)
	if tool == "cursor" {
		parts = append(parts, "--chat", id)
	} else {
		parts = append(parts, "--session", id)
	}
	return strings.TrimSpace("ks import " + tool + " " + strings.Join(append(parts, flag), " "))
}

// decideImportFiles picks what goes without asking anybody: the history when
// it was asked for or when it fits, the files only when it does not, and a
// refusal that names the working flag when what was asked for cannot go.
func decideImportFiles(inv *Invocation, tool, id string, pr *importFilesProbe, mode string) importFilesChosen {
	name := repoName(pr.Top)
	sizes := func() string {
		if pr.With == nil {
			return fmt.Sprintf("%s is %s", name, bytesText(pr.Without.Content))
		}
		return fmt.Sprintf("%s is %s with its history and %s without it (.git is %s)", name, bytesText(pr.With.Content), bytesText(pr.Without.Content), bytesText(pr.gitBytes()))
	}
	switch {
	case mode == "with" && pr.GitWhy != "":
		fail(&cliError{Code: exitConflict, Kind: "import_history_not_bringable",
			Message:    fmt.Sprintf("the history of %s cannot be brought: %s. Nothing was sent", name, pr.GitWhy),
			Title:      "Can't bring the history of " + name,
			Why:        []string{capFirst(pr.GitWhy) + ".", "Nothing was sent."},
			NextAction: importFilesNext(inv, tool, id, "--without-git")})
	case mode == "with" && pr.HasGit && !pr.withFits():
		_, hwhy := planFits(pr.With)
		if pr.withoutFits() {
			fail(&cliError{Code: exitConflict, Kind: "import_history_too_large",
				Message: fmt.Sprintf("the history of %s does not fit: %s, and %s. Nothing was sent. The files without the history are %s and fit", name, sizes(), limitWords(hwhy), bytesText(pr.Without.Content)),
				Title:   "Can't bring the history of " + name,
				Why: []string{capFirst(sizes()) + "; " + limitWords(hwhy) + ".",
					fmt.Sprintf("The files without the history are %s and fit.", bytesText(pr.Without.Content)), "Nothing was sent."},
				NextAction: importFilesNext(inv, tool, id, "--without-git")})
		}
		fail(noFilesFit(inv, tool, id, name, sizes(), pr))
	case mode == "without" || (mode == "with" && !pr.HasGit):
		if !pr.withoutFits() {
			fail(noFilesFit(inv, tool, id, name, sizes(), pr))
		}
		return importFilesChosen{Plan: pr.Without}
	}
	// not said: the history when it fits, else the files only
	if pr.HasGit && pr.withFits() {
		return importFilesChosen{Plan: pr.With, History: true}
	}
	if !pr.withoutFits() {
		fail(noFilesFit(inv, tool, id, name, sizes(), pr))
	}
	note := pr.GitWhy
	if pr.HasGit {
		_, why := planFits(pr.With)
		note = fmt.Sprintf("it is %s, and with it %s is %s, %s", bytesText(pr.gitBytes()), name, bytesText(pr.With.Content), why)
	}
	return importFilesChosen{Plan: pr.Without, Note: note}
}

func noFilesFit(inv *Invocation, tool, id, name, sizes string, pr *importFilesProbe) *cliError {
	_, why := planFits(pr.Without)
	return &cliError{Code: exitConflict, Kind: "import_files_too_large",
		Message:    fmt.Sprintf("the project files of %s do not fit: %s, and %s. Nothing was sent", name, sizes, limitWords(why)),
		Title:      "Can't bring the project files of " + name,
		Why:        []string{capFirst(sizes) + "; " + limitWords(why) + ".", "Nothing was sent."},
		NextAction: importFilesNext(inv, tool, id, "--no-files")}
}

// historyWords says, in a plan, whether the repository's history goes.
func (p *importPrep) historyWords() string {
	switch {
	case p.History:
		return "The repository's own history (.git) is carried."
	case p.GitNote != "":
		return "The repository's own history (.git) is not carried: " + p.GitNote + "."
	}
	return "The repository's own history (.git) is not carried: files only."
}

// serviceLines are the service's plan lines as they are true: an older
// service says the .git folder is carried whatever was declared, and this
// run leaves it out, so that sentence is not shown.
func (p *importPrep) serviceLines(lines []string) []string {
	if p.Files == nil || p.History {
		return lines
	}
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "Also uploads the project folder") && strings.Contains(l, "the .git folder is carried") {
			l = strings.Replace(l, "; the .git folder is carried", "; the .git folder (the history) is not carried", 1)
		}
		out = append(out, l)
	}
	return out
}
