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
	"regexp"
	"sort"
	"strings"
)

var (
	ks072SkipDirs    = verifierSkipDirs // the vendored judge/checks.py SKIP_DIRS
	ks072TestDirs    = map[string]bool{"tests": true, "test": true, "__tests__": true}
	ks072FixtureDirs = map[string]bool{"testdata": true, "test_data": true, "fixtures": true, "__fixtures__": true, "__snapshots__": true}

	ks072PyTest   = regexp.MustCompile(`^(test_.*|.*_test)\.py$`)
	ks072PyConfig = map[string]bool{"conftest.py": true, "pytest.ini": true, "pyproject.toml": true, "setup.cfg": true, "tox.ini": true, "setup.py": true}
	ks072PyDeps   = regexp.MustCompile(`^(requirements.*\.txt|constraints.*\.txt|Pipfile|Pipfile\.lock|poetry\.lock|uv\.lock|pdm\.lock)$`)

	ks072GoTest   = regexp.MustCompile(`^.*_test\.go$`)
	ks072GoConfig = map[string]bool{"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true}

	ks072NodeTest   = regexp.MustCompile(`^(.*[.\-_]test|.*\.spec|test-.*|test)\.(c|m)?[jt]sx?$`)
	ks072NodeConfig = regexp.MustCompile(`^(package\.json|\.mocharc\..*|jest\.config\..*|vitest\.config\..*|babel\.config\..*|\.babelrc|tsconfig\.json|\.nvmrc|\.node-version)$`)
	ks072NodeDeps   = map[string]bool{"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true}
	ks072NodeSrc    = regexp.MustCompile(`\.(c|m)?[jt]sx?$`)
)

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

// ks072Classify is judge/checks.py classify().
func ks072Classify(rel string) []ecoRole {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	if ks072Ignored(rel) {
		return nil
	}
	var out []ecoRole
	inFixture, inTestdir, hasTestDir, hasUnderTests := false, false, false, false
	for _, d := range dirs {
		if ks072FixtureDirs[d] {
			inFixture = true
		}
		if ks072TestDirs[d] {
			inTestdir = true
		}
		if d == "test" {
			hasTestDir = true
		}
		if d == "__tests__" {
			hasUnderTests = true
		}
	}
	has := func(eco string) bool {
		for _, o := range out {
			if o.eco == eco {
				return true
			}
		}
		return false
	}
	// python
	switch {
	case ks072PyTest.MatchString(name):
		out = append(out, ecoRole{"python", "test"})
	case name == "conftest.py" || (ks072PyConfig[name] && len(parts) == 1):
		out = append(out, ecoRole{"python", "config"})
	case ks072PyDeps.MatchString(name) && len(parts) == 1:
		out = append(out, ecoRole{"python", "deps"})
	case inTestdir && strings.HasSuffix(name, ".py"):
		out = append(out, ecoRole{"python", "test_support"})
	}
	// go
	switch {
	case ks072GoTest.MatchString(name):
		out = append(out, ecoRole{"go", "test"})
	case ks072GoConfig[name]:
		role := "deps"
		if strings.HasSuffix(name, ".mod") || name == "go.work" {
			role = "config"
		}
		out = append(out, ecoRole{"go", role})
	}
	// node
	switch {
	case ks072NodeTest.MatchString(name) || (hasUnderTests && ks072NodeSrc.MatchString(name)) || (inTestdir && ks072NodeSrc.MatchString(name) && hasTestDir):
		out = append(out, ecoRole{"node", "test"})
	case ks072NodeConfig.MatchString(name) && (len(parts) == 1 || name == "package.json"):
		out = append(out, ecoRole{"node", "config"})
	case ks072NodeDeps[name]:
		out = append(out, ecoRole{"node", "deps"})
	}
	if inFixture || (inTestdir && len(out) == 0) {
		for _, eco := range []string{"python", "go", "node"} {
			if !has(eco) {
				out = append(out, ecoRole{eco, "fixture"})
			}
		}
	}
	return out
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
	for _, e := range []string{"python", "go", "node"} {
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

var ks072RunnerEco = map[string]string{"pytest": "python", "go": "go", "node": "node"}
var ks072LegacyPytest = regexp.MustCompile(`(^|\s)(-m\s+)?pytest(\s|$)`)

type ks072Selection struct {
	Runner    string
	Ecosystem string
	Source    string
}

// ks072Select is select(): an explicit runner, a legacy pytest command, or
// the single ecosystem with tests; refused as ks_check_ambiguous or
// ks_check_not_found otherwise.
func ks072Select(d discovery, runner, legacyCommand string) (ks072Selection, string, error) {
	var sel ks072Selection
	switch {
	case runner != "":
		if _, ok := ks072RunnerEco[runner]; !ok {
			return sel, "ks_check_invalid", fmt.Errorf("--check is pytest, go or node (got %q)", runner)
		}
		sel = ks072Selection{Runner: runner, Source: "explicit"}
	case legacyCommand != "" && ks072LegacyPytest.MatchString(legacyCommand):
		sel = ks072Selection{Runner: "pytest", Source: "legacy_command"}
	default:
		switch len(d.WithTests) {
		case 0:
			return sel, "ks_check_not_found", fmt.Errorf("no Python, Node or Go tests were discovered in the workspace")
		case 1:
			for r, e := range ks072RunnerEco {
				if e == d.WithTests[0] {
					sel = ks072Selection{Runner: r, Source: "discovered"}
				}
			}
		default:
			var runners []string
			for _, r := range []string{"pytest", "go", "node"} {
				for _, e := range d.WithTests {
					if ks072RunnerEco[r] == e {
						runners = append(runners, r)
					}
				}
			}
			return sel, "ks_check_ambiguous", fmt.Errorf("tests for %s were discovered; choose the check with --check %s", strings.Join(d.WithTests, ", "), strings.Join(runners, "|"))
		}
	}
	sel.Ecosystem = ks072RunnerEco[sel.Runner]
	if d.Ecosystems[sel.Ecosystem] == nil {
		return sel, "ks_check_not_found", fmt.Errorf("the selected %s check has no %s tests in the workspace", sel.Runner, sel.Ecosystem)
	}
	return sel, "", nil
}

// ks072Command is the verifier.command a runner is recorded with.
func ks072Command(runner string) string {
	switch runner {
	case "pytest":
		return "python3 -m pytest -q"
	case "go":
		return "go test ./..."
	}
	return "node --test"
}
