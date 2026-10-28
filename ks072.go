// ks072.go: discovering and pinning a repository's tests the way the Cruise
// verifier does (KS-072).
//
// This is judge/checks.py's discover / inputs_of / inputs_digest / select,
// ported rule for rule, so the set of files ks cruise init pins and the
// digests it writes are the ones the worker and the verifier recompute. A
// golden cross-check (ks072_test.go) holds the port to the server's own
// recorded answers for the same layouts.
//
// Real repositories keep tests nested beside their conftest, package
// configuration, lockfiles and fixture trees; every one of those is a
// verifier input and is pinned. When tests of more than one ecosystem are
// found, nothing is chosen: the person names the check (--check).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

var ks072SkipDirs = verifierSkipDirs // the vendored judge/checks.py SKIP_DIRS

type ecoRole struct{ eco, role string }

func ks072Ignored(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, d := range parts[:len(parts)-1] {
		if ks072SkipDirs[d] {
			return true
		}
	}
	return false
}

// ks072Classify is judge/checks.py classify(), from the vendored runner list.
func ks072Classify(rel string) []ecoRole {
	return runners.classify(rel, ks072Ignored)
}

type pinnedInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ecoInputs struct {
	Tests       []pinnedInput `json:"tests"`
	Config      []pinnedInput `json:"config"`
	Deps        []pinnedInput `json:"deps"`
	Fixtures    []pinnedInput `json:"fixtures"`
	TestSupport []pinnedInput `json:"test_support"`
}

type discovery struct {
	Ecosystems map[string]*ecoInputs `json:"ecosystems"`
	WithTests  []string              `json:"with_tests"`
	Symlinks   []string              `json:"symlinks"`
}

// ks072Discover is discover() over a file set: rel path -> content sha256
// (hex). links lists the symlinked paths (reported, never pinned).
func ks072Discover(files map[string]string, links []string) discovery {
	eco := map[string]*ecoInputs{}
	for _, e := range runners.ecosystems() {
		eco[e] = &ecoInputs{Tests: []pinnedInput{}, Config: []pinnedInput{}, Deps: []pinnedInput{}, Fixtures: []pinnedInput{}, TestSupport: []pinnedInput{}}
	}
	rels := make([]string, 0, len(files))
	for r := range files {
		rels = append(rels, r)
	}
	sort.Strings(rels) // os.walk sorted dirs and files == sorted full paths per directory
	sort.SliceStable(rels, func(i, j int) bool { return walkLess(rels[i], rels[j]) })
	for _, rel := range rels {
		for _, er := range ks072Classify(rel) {
			row := pinnedInput{Path: rel, SHA256: files[rel]}
			e := eco[er.eco]
			switch er.role {
			case "test":
				e.Tests = append(e.Tests, row)
			case "config":
				e.Config = append(e.Config, row)
			case "deps":
				e.Deps = append(e.Deps, row)
			case "fixture":
				e.Fixtures = append(e.Fixtures, row)
			case "test_support":
				e.TestSupport = append(e.TestSupport, row)
			}
		}
	}
	d := discovery{Ecosystems: map[string]*ecoInputs{}, WithTests: []string{}, Symlinks: append([]string{}, links...)}
	for name, e := range eco {
		if len(e.Tests) > 0 {
			d.Ecosystems[name] = e
			d.WithTests = append(d.WithTests, name)
		}
	}
	sort.Strings(d.WithTests)
	sort.Strings(d.Symlinks)
	return d
}

// walkLess is os.walk's order: a directory's files before its
// subdirectories, each group sorted by name.
func walkLess(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		aFile, bFile := i == len(pa)-1, i == len(pb)-1
		if aFile != bFile {
			return aFile // files of this directory come first
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

// ks072InputsOf is inputs_of(): the ecosystem's whole verifier input set,
// sorted by path.
func ks072InputsOf(d discovery, ecosystem string) []pinnedInput {
	e := d.Ecosystems[ecosystem]
	if e == nil {
		return []pinnedInput{}
	}
	rows := map[string]pinnedInput{}
	for _, list := range [][]pinnedInput{e.Tests, e.Config, e.Deps, e.Fixtures, e.TestSupport} {
		for _, r := range list {
			rows[r.Path] = r
		}
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]pinnedInput, 0, len(keys))
	for _, k := range keys {
		out = append(out, rows[k])
	}
	return out
}

// ks072InputsDigest is inputs_digest(): sha256 over path NUL sha256 LF.
func ks072InputsDigest(rows []pinnedInput) string {
	h := sha256.New()
	for _, r := range rows {
		h.Write([]byte(r.Path + "\x00" + r.SHA256 + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ks072RunnerEco: each offered runner's ecosystem, from the runner list.
var ks072RunnerEco = func() map[string]string {
	m := map[string]string{}
	for _, r := range runners.offered() {
		m[r] = runners.byRunner[r].Ecosystem
	}
	return m
}()
var ks072LegacyPytest = regexp.MustCompile(`(^|\s)(-m\s+)?pytest(\s|$)`)

type ks072Selection struct {
	Runner    string
	Ecosystem string
	Source    string
}

// ks072Select is select(): an explicit runner, a legacy pytest command, or
// the single ecosystem with tests (the first, legacy ecosystems chosen among
// themselves first, as the verifier does); refused as ks_check_ambiguous or
// ks_check_not_found otherwise.
func ks072Select(d discovery, runner, legacyCommand string) (ks072Selection, string, error) {
	var sel ks072Selection
	switch {
	case runner != "":
		if runners.offeredRunner(runner) == nil {
			return sel, "ks_check_invalid", fmt.Errorf("--check is %s (got %q)", runners.choices(), runner)
		}
		sel = ks072Selection{Runner: runner, Source: "explicit"}
	case legacyCommand != "" && ks072LegacyPytest.MatchString(legacyCommand):
		sel = ks072Selection{Runner: "pytest", Source: "legacy_command"}
	default:
		found := []string{}
		for _, e := range d.WithTests {
			if runners.legacy(e) {
				found = append(found, e)
			}
		}
		if len(found) == 0 {
			found = d.WithTests
		}
		switch len(found) {
		case 0:
			return sel, "ks_check_not_found", fmt.Errorf("no %s tests were discovered in the workspace", runners.languages())
		case 1:
			r, ok := runners.offeredEco(found[0])
			if !ok {
				return sel, "ks_check_not_found", fmt.Errorf("%s tests were discovered; their check is not offered (the check is %s)", found[0], runners.choices())
			}
			sel = ks072Selection{Runner: r, Source: "discovered"}
		default:
			var choices []string
			for _, r := range runners.offered() {
				for _, e := range found {
					if ks072RunnerEco[r] == e {
						choices = append(choices, r)
					}
				}
			}
			return sel, "ks_check_ambiguous", fmt.Errorf("tests for %s were discovered; choose the check with --check %s", strings.Join(found, ", "), strings.Join(choices, "|"))
		}
	}
	sel.Ecosystem = ks072RunnerEco[sel.Runner]
	if d.Ecosystems[sel.Ecosystem] == nil {
		return sel, "ks_check_not_found", fmt.Errorf("the selected %s check has no %s tests in the workspace", sel.Runner, sel.Ecosystem)
	}
	if need := runners.byRunner[sel.Runner].RequiresDeps; need != "" {
		ok := false
		for _, r := range d.Ecosystems[sel.Ecosystem].Deps {
			ok = ok || path.Base(r.Path) == need
		}
		if !ok {
			return sel, "ks_check_not_found", fmt.Errorf("the %s check needs the project's %s in the workspace", sel.Runner, need)
		}
	}
	return sel, "", nil
}

// ks072Command is the verifier.command a runner is recorded with.
func ks072Command(runner string) string {
	if d := runners.byRunner[runner]; d != nil {
		return d.Command
	}
	return ""
}
