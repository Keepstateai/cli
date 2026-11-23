//go:build unix

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bigHistoryRepo is gitRepo with a history over the 1 GB limit: a sparse
// pack stands in for it, so nothing real is written and nothing is read.
func bigHistoryRepo(t *testing.T) string {
	t.Helper()
	repo := gitRepo(t)
	big := filepath.Join(repo, ".git", "objects", "pack", "pack-big.pack")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1_600_000_000); err != nil {
		t.Skipf("no sparse files here: %v", err)
	}
	f.Close()
	return repo
}

func planWorkspaceBytes(t *testing.T, e *importEnv, args ...string) (int64, string, string, int) {
	t.Helper()
	out, errs, code := e.run(append(args, "--plan", "--json")...)
	if code != 0 {
		return 0, out, errs, code
	}
	doc := decodeJSONOut(t, out)
	return jsonInt(asMap(e.c.plans[len(e.c.plans)-1]["workspace"])["bytes"]), flat(strings.Join(asStrings(doc["lines"]), " ")), errs, code
}

// The history is not carried unless it was asked for or it fits: a repository
// whose .git is over the limit goes as its files, and the plan says why.
func TestImportWithFilesLeavesOutAHistoryOverTheLimit(t *testing.T) {
	e := newImportEnv(t)
	repo := bigHistoryRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	n, lines, errs, code := planWorkspaceBytes(t, e, "import", "claude-code", "--latest", "--with-files", "--project", repo)
	if code != 0 || n <= 0 || n > 10_000_000 {
		t.Fatalf("--with-files on a repository with a 1.6 GB history: %d, %d bytes\n%s\n%s", code, n, lines, errs)
	}
	if !strings.Contains(lines, "history (.git) is not carried") || !strings.Contains(lines, "over the 1 GB limit") {
		t.Fatalf("the plan does not say the history was left out and why:\n%s", lines)
	}
	// by session id, the other way in
	n, _, errs, code = planWorkspaceBytes(t, e, "import", "claude-code", "--session", claudeSID, "--with-files", "--project", repo)
	if code != 0 || n > 10_000_000 {
		t.Fatalf("--session: %d, %d bytes\n%s", code, n, errs)
	}
}

// A history that was asked for and does not fit is refused with the flag
// that works, and that flag does work: no advice leads back to the error.
func TestImportWithGitOverTheLimitNamesAFlagThatWorks(t *testing.T) {
	e := newImportEnv(t)
	repo := bigHistoryRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	_, errs, code := e.run("import", "claude-code", "--session", claudeSID, "--with-git", "--project", repo, "--plan")
	if code == 0 || !strings.Contains(flat(errs), "history") || !strings.Contains(flat(errs), "1 GB") {
		t.Fatalf("--with-git over the limit: %d\n%s", code, errs)
	}
	if strings.Count(errs, "Nothing was") != 1 {
		t.Fatalf("the refusal says it twice or not at all:\n%s", errs)
	}
	nx := nextLines(errs)
	if len(nx) != 1 || !strings.Contains(nx[0], "--without-git") || strings.Contains(nx[0], "--with-git ") {
		t.Fatalf("next: %v\n%s", nx, errs)
	}
	if out, errs2, code := e.runNext(nx[0] + " --plan"); code != 0 {
		t.Fatalf("the advice does not work: %s\n%d\n%s\n%s", nx[0], code, out, errs2)
	}
}

func TestImportGitFlagsMeanWhatTheySayAndNeverClash(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	with, _, _, c1 := planWorkspaceBytes(t, e, "import", "claude-code", "--latest", "--with-git", "--project", repo)
	without, lines, _, c2 := planWorkspaceBytes(t, e, "import", "claude-code", "--latest", "--without-git", "--project", repo)
	if c1 != 0 || c2 != 0 || with <= without || without <= 0 {
		t.Fatalf("with history %d (%d), without %d (%d)", with, c1, without, c2)
	}
	if !strings.Contains(lines, "history (.git) is not carried") {
		t.Fatalf("the files-only plan does not say so:\n%s", lines)
	}
	// plain --with-files keeps carrying a history that fits
	n, _, _, _ := planWorkspaceBytes(t, e, "import", "claude-code", "--latest", "--with-files", "--project", repo)
	if n != with {
		t.Fatalf("--with-files carried %d, want the whole %d", n, with)
	}
	for _, args := range [][]string{
		{"--with-git", "--without-git"},
		{"--no-files", "--with-git"},
		{"--no-files", "--without-git"},
	} {
		_, errs, code := e.run(append([]string{"import", "claude-code", "--latest", "--project", repo, "--plan"}, args...)...)
		if code != exitUsage || !strings.Contains(errs, "Nothing was created") {
			t.Fatalf("%v: %d\n%s", args, code, errs)
		}
	}
	if len(e.c.creations) != 0 {
		t.Fatal("a session was created")
	}
}

// At a terminal, a repository whose history does not fit is a question with
// the files only marked and the reason said, not an error before the question.
func TestGuidedAsksWithHistoryOrWithoutAndNeverFailsBeforeAsking(t *testing.T) {
	for _, how := range [][]string{{"--latest"}, {"--session", claudeSID}} {
		e := newImportEnv(t)
		repo := bigHistoryRepo(t)
		e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
		e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
		// "2" is the history, which cannot be taken: the key does nothing, and Enter takes what is marked
		got, code := e.ptyImport([]keyStep{{"Bring the project files too?", "2"}, {"Esc cancel", "\r"}, {"Review the plan", "\r"}}, append([]string{"import", "claude-code"}, how...)...)
		f := flat(got)
		if code != 0 || e.c.upload("folder") == nil || !strings.Contains(f, "Imported") {
			t.Fatalf("%v: the files were not brought: %d\n%s", how, code, got)
		}
		for _, want := range []string{"conversation only", "with the files and their history", "over the 1 GB limit", "with the files only, without the history", "cancel", "so they are marked"} {
			if !strings.Contains(f, want) {
				t.Errorf("%v: the question lacks %q:\n%s", how, want, got)
			}
		}
		if strings.Contains(f, "without --with-git") || strings.Contains(f, "Nothing was sent") {
			t.Errorf("%v: an error or old advice before the question:\n%s", how, got)
		}
		if up := e.c.upload("folder"); up != nil && int64(len(up.Got)) > 10_000_000 {
			t.Errorf("%v: the history went up: %d bytes", how, len(up.Got))
		}
	}
}

// When the history fits, the question offers both and takes the history on 2.
func TestGuidedHistoryThatFitsIsOneOfTheChoices(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	got, code := e.ptyImport([]keyStep{{"Bring the project files too?", "3\r"}, {"Review the plan", "\r"}}, "import", "claude-code", "--latest")
	f := flat(got)
	if code != 0 || e.c.upload("folder") == nil || !strings.Contains(f, "with the files only, without the history") || !strings.Contains(f, "(.git) is not carried") {
		t.Fatalf("files only: %d\n%s", code, got)
	}
}

// A refusal says that nothing was sent once, wraps its title on word
// boundaries and fits the terminal.
func TestRefusalSaysNothingWasSentOnceAtEveryWidth(t *testing.T) {
	ce := &cliError{Code: exitConflict, Kind: "import_history_too_large",
		Title:      "Can't bring the history of shop",
		Why:        []string{"Shop is 1.8 GB with its history and 214 MB without it (.git is 1.6 GB). One upload carries up to 1 GB (1,000,000,000 bytes).", "Nothing was sent."},
		NextAction: "ks import claude-code --session 3f2a1c --without-git"}
	plain := &cliError{Code: exitConflict, Kind: "x", Message: "shop is too big; one upload carries up to 1 GB. Nothing was sent."}
	for _, w := range []int{120, 80, 60} {
		for _, c := range []*cliError{ce, plain} {
			var b bytes.Buffer
			renderError(newUI(&b, appTheme{}, w, false), c)
			text := b.String()
			if n := strings.Count(text, "Nothing was"); n != 1 {
				t.Errorf("%d columns: %d 'Nothing was' lines:\n%s", w, n, text)
			}
			for _, l := range strings.Split(text, "\n") {
				if displayWidth(l) > w {
					t.Errorf("%d columns: %q is %d wide", w, l, displayWidth(l))
				}
			}
		}
	}
}

// The trim says what it did: the size before and after, never the size it
// read, which looked like a trim that changed nothing.
func TestTrimLineSaysFromAndTo(t *testing.T) {
	e := newImportEnv(t)
	e.placeLines(claudeLines(4, 5, nil))
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=9000")
	_, errs, code := e.run("import", "claude-code", "--latest", "--plan")
	f := flat(errs)
	if code != 0 || !strings.Contains(f, "Trimmed to the last compaction") || !strings.Contains(f, " down to ") {
		t.Fatalf("%d\n%s", code, errs)
	}
	i := strings.Index(f, "Trimmed to the last compaction")
	if !strings.Contains(f[i:min(len(f), i+120)], " down to ") {
		t.Fatalf("the trim line does not say from and to:\n%s", errs)
	}
}
