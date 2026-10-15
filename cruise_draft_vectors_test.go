// cruise_draft_vectors_test.go: the shared vectors of a hosted Cruise draft.
//
// A draft the control plane builds from uploaded or copied files must carry
// exactly what `ks cruise init` and `ks cruise approve` compute on the same
// files: the upload selection, the tree digest, the pinned check inputs, the
// tests digest, the verifier configuration, the workspace archive and the
// whole bound manifest. The vectors are GENERATED here, by this client's own
// built binary running init and approve against a recording control plane,
// and the service keeps the generated file (docs/upgrade/vectors/
// cruise-draft-v1.json); the service's own port and the judge read the same
// file, and this client vendors it back (testdata/cruisedraft, with
// SOURCE.json).
//
//	KS_WRITE_CRUISE_DRAFT_VECTORS=/path/cruise-draft-v1.json go test -run '^TestCruiseDraftVectors$'
//
// writes the file from the case table below. Without the variable the test
// reads the vendored copy, runs every case again and fails on any difference,
// so a change to init, approve, the selection policy or the discovery port
// that moves a digest is caught here before it reaches a release.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const cruiseDraftVectorsVendored = "testdata/cruisedraft/cruise-draft-v1.json"

type draftVectorEntry struct {
	Path       string  `json:"path"`
	Kind       string  `json:"kind"`                  // file | dir | symlink
	Mode       string  `json:"mode,omitempty"`        // 0644 | 0755 (files)
	Content    *string `json:"content,omitempty"`     // UTF-8 text (files)
	ContentB64 string  `json:"content_b64,omitempty"` // bytes that are not text (files)
	Target     string  `json:"target,omitempty"`      // symlinks
}

type draftVectorOptions struct {
	Goal   string   `json:"goal,omitempty"`
	Paths  []string `json:"paths,omitempty"`
	Check  string   `json:"check,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Ladder string   `json:"ladder,omitempty"`
	Spend  string   `json:"spend,omitempty"`
}

type draftVectorCase struct {
	Name    string             `json:"name"`
	Tree    []draftVectorEntry `json:"tree"`
	Options draftVectorOptions `json:"options"`
	Expect  map[string]any     `json:"expect"`
}

type draftVectorFile struct {
	Name    string            `json:"name"`
	Version int               `json:"version"`
	What    string            `json:"what"`
	Rules   []string          `json:"rules"`
	Keys    []map[string]any  `json:"keys"`
	Cases   []draftVectorCase `json:"cases"`
}

func txt(s string) *string { return &s }

func vf(p, content string) draftVectorEntry {
	return draftVectorEntry{Path: p, Kind: "file", Mode: "0644", Content: txt(content)}
}

func vx(p, content string) draftVectorEntry {
	return draftVectorEntry{Path: p, Kind: "file", Mode: "0755", Content: txt(content)}
}

// draftVectorCases is the generation table: trees and init options only.
// The expectations are whatever this client computes for them.
func draftVectorCases() []draftVectorCase {
	bin := make([]byte, 64)
	for i := range bin {
		bin[i] = byte(255 - i*3)
	}
	toyREADME := "# toy-repo\n\nDeterministic specimen fixture (dragon 9). `calc.py` contains three bugs;\n`python3 test_calc.py` fails until all are fixed. No network, no external\ndeps, no randomness — the same task every run. Gates assert infrastructure\naround the specimen, never the specimen's success on this task.\n"
	toyCalc := "\"\"\"A tiny calculator library. Some of it is wrong.\"\"\"\n\n\ndef add(a, b):\n    return a - b\n\n\ndef mul(a, b):\n    result = 0\n    for _ in range(abs(b)):\n        result += a\n    return result\n\n\ndef mean(xs):\n    return sum(xs) / (len(xs) - 1)\n"
	toyTest := "import unittest\n\nfrom calc import add, mean, mul\n\n\nclass TestCalc(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(add(2, 3), 5)\n        self.assertEqual(add(-1, 1), 0)\n\n    def test_mul(self):\n        self.assertEqual(mul(3, 4), 12)\n        self.assertEqual(mul(3, -4), -12)\n        self.assertEqual(mul(0, 5), 0)\n\n    def test_mean(self):\n        self.assertAlmostEqual(mean([1, 2, 3]), 2.0)\n        self.assertAlmostEqual(mean([10]), 10.0)\n\n\nif __name__ == \"__main__\":\n    unittest.main()\n"
	return []draftVectorCase{
		{Name: "toy-repo: the fixture the gate uploads (fixtures/toy-repo of the service, byte for byte)",
			Tree:    []draftVectorEntry{vf("README.md", toyREADME), vf("calc.py", toyCalc), vf("test_calc.py", toyTest)},
			Options: draftVectorOptions{Goal: "fix calc.py so the tests in test_calc.py pass"}},
		{Name: "python: nested tests with their inputs, and every rule of the upload policy",
			Tree: []draftVectorEntry{
				vf(".gitignore", "build/\n*.log\n!keep.log\ncoverage/\n"),
				vf("src/app/__init__.py", ""),
				vf("src/app/core.py", "def total(xs):\n    return sum(xs)\n"),
				vf("tests/conftest.py", "import sys\nsys.path.insert(0, 'src')\n"),
				vf("tests/unit/test_core.py", "from app.core import total\n\n\ndef test_total():\n    assert total([1, 2]) == 3\n"),
				vf("tests/fixtures/data.json", "{\"xs\": [1, 2]}\n"),
				vf("tests/fixtures/café.txt", "crème\n"),
				vf("pytest.ini", "[pytest]\ntestpaths = tests\n"),
				vf("requirements.txt", "pytest==8.4.2\n"),
				vx("scripts/run.sh", "#!/bin/sh\npython3 -m pytest -q\n"),
				vf(".env", "TOKEN=not-a-real-token\n"),
				vf(".env.example", "TOKEN=\n"),
				vf("config/server.pem", "-----BEGIN NOTHING-----\n"),
				vf(".aws/config", "[default]\n"),
				vf("node_modules/left/index.js", "module.exports = 1\n"),
				vf(".venv/lib/site.py", "x = 1\n"),
				vf("src/app/__pycache__/core.cpython-312.pyc", "pyc"),
				vf("build/out.txt", "built\n"),
				vf("app.log", "log\n"),
				vf("keep.log", "kept\n"),
				vf("coverage/index.html", "<html></html>\n"),
				vf(".git/HEAD", "ref: refs/heads/main\n"),
				{Path: "data/blob.bin", Kind: "file", Mode: "0644", ContentB64: base64.StdEncoding.EncodeToString(bin)},
				vf("empty.txt", ""),
				{Path: "emptydir", Kind: "dir"},
			},
			Options: draftVectorOptions{Goal: "make the core tests pass; keep <public> & \"stable\" names: café ✓"}},
		{Name: "go: a module with nested tests, testdata and a path boundary",
			Tree: []draftVectorEntry{
				vf("go.mod", "module example.com/calc\n\ngo 1.22\n"),
				vf("go.sum", ""),
				vf("main.go", "package main\n\nfunc main() {}\n"),
				vf("pkg/calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b }\n"),
				vf("pkg/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"),
				vf("pkg/calc/testdata/cases.txt", "2 3 5\n"),
				vf("docs/notes.md", "notes\n"),
			},
			Options: draftVectorOptions{Goal: "fix Add", Paths: []string{"pkg/**", "docs/*.md"}}},
		{Name: "node: test/ and __tests__, a lockfile, another ladder and spend limit",
			Tree: []draftVectorEntry{
				vf("package.json", "{\"name\": \"sum\", \"scripts\": {\"test\": \"node --test\"}}\n"),
				vf("package-lock.json", "{\"lockfileVersion\": 3}\n"),
				vf("src/sum.js", "exports.sum = (a, b) => a - b\n"),
				vf("test/sum.test.js", "const {sum} = require('../src/sum')\nconst test = require('node:test')\ntest('sum', () => { if (sum(2, 3) !== 5) throw new Error('sum') })\n"),
				vf("__tests__/helper.js", "module.exports = {}\n"),
			},
			Options: draftVectorOptions{Goal: "fix sum", Ladder: "anthropic:claude-sonnet-5", Spend: "5.25"}},
		{Name: "two ecosystems with the check named",
			Tree: []draftVectorEntry{
				vf("app.py", "def f():\n    return 1\n"),
				vf("test_app.py", "from app import f\n\n\ndef test_f():\n    assert f() == 1\n"),
				vf("go.mod", "module example.com/tool\n\ngo 1.22\n"),
				vf("tool/tool.go", "package tool\n"),
				vf("tool/tool_test.go", "package tool\n\nimport \"testing\"\n\nfunc TestTool(t *testing.T) {}\n"),
			},
			Options: draftVectorOptions{Goal: "keep the tool tests passing", Check: "go"}},
		{Name: "a sensitive file and a dependency tree included by explicit review",
			Tree: []draftVectorEntry{
				vf("x.py", "X = 1\n"),
				vf("test_x.py", "from x import X\n\n\ndef test_x():\n    assert X == 1\n"),
				vf(".env.local", "MODE=test\n"),
				vf("node_modules/pkg/a.js", "module.exports = 2\n"),
				vf("venv/bin/activate", "true\n"),
			},
			Options: draftVectorOptions{Goal: "make test_x pass", Allow: []string{".env.local", "node_modules"}}},
		{Name: "refused: a symbolic link",
			Tree: []draftVectorEntry{
				vf("a.py", "A = 1\n"),
				vf("test_a.py", "from a import A\n\n\ndef test_a():\n    assert A == 1\n"),
				{Path: "current", Kind: "symlink", Target: "a.py"},
			},
			Options: draftVectorOptions{Goal: "g"}},
		{Name: "refused: tests of two ecosystems and no check named",
			Tree: []draftVectorEntry{
				vf("test_app.py", "def test_f():\n    assert True\n"),
				vf("go.mod", "module example.com/tool\n\ngo 1.22\n"),
				vf("tool_test.go", "package tool\n\nimport \"testing\"\n\nfunc TestTool(t *testing.T) {}\n"),
			},
			Options: draftVectorOptions{Goal: "g"}},
		{Name: "refused: no tests",
			Tree:    []draftVectorEntry{vf("app.py", "print('hi')\n"), vf("README.md", "# app\n")},
			Options: draftVectorOptions{Goal: "g"}},
		{Name: "refused: a verifier input whose name starts with a dot (a Node .nvmrc at the root)",
			Tree: []draftVectorEntry{
				vf("package.json", "{\"name\": \"n\", \"scripts\": {\"test\": \"node --test\"}}\n"),
				vf(".nvmrc", "22\n"),
				vf("test/a.test.js", "require('node:test')('a', () => {})\n"),
			},
			Options: draftVectorOptions{Goal: "g"}},
		{Name: "refused: a verifier input inside a dot directory",
			Tree:    []draftVectorEntry{vf("x.py", "X = 1\n"), vf(".ci/test_x.py", "def test_x():\n    assert True\n")},
			Options: draftVectorOptions{Goal: "g"}},
	}
}

// writeVectorTree materializes a case's tree under root.
func writeVectorTree(t *testing.T, root string, tree []draftVectorEntry) {
	t.Helper()
	for _, e := range tree {
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case "dir":
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.Symlink(e.Target, p); err != nil {
				t.Fatal(err)
			}
		case "file":
			var b []byte
			if e.Content != nil {
				b = []byte(*e.Content)
			} else {
				var err error
				if b, err = base64.StdEncoding.DecodeString(e.ContentB64); err != nil {
					t.Fatal(err)
				}
			}
			mode := os.FileMode(0o644)
			if e.Mode == "0755" {
				mode = 0o755
			}
			if err := os.WriteFile(p, b, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("entry %s: unknown kind %q", e.Path, e.Kind)
		}
	}
}

func canonText(t *testing.T, m map[string]any) (string, string) {
	t.Helper()
	b, err := canonicalJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return string(b), hex.EncodeToString(s[:])
}

// computeDraftVector runs init and approve, as the built binary, on one case
// and returns what they computed.
func computeDraftVector(t *testing.T, bin, cfg string, c draftVectorCase) map[string]any {
	t.Helper()
	root := t.TempDir()
	writeVectorTree(t, root, c.Tree)
	// before init: the selection and the archive of the tree as given, built
	// by the code init runs (a refused tree has no archive)
	var arch bytes.Buffer
	_, initSel, initErr := scanSelected(root, &arch, c.Options.Allow)
	args := []string{"cruise", "init"}
	if c.Options.Goal != "" {
		args = append(args, "--goal", c.Options.Goal)
	}
	for _, p := range c.Options.Paths {
		args = append(args, "--paths", p)
	}
	for _, a := range c.Options.Allow {
		args = append(args, "--allow", a)
	}
	if c.Options.Check != "" {
		args = append(args, "--check", c.Options.Check)
	}
	if c.Options.Ladder != "" {
		args = append(args, "--ladder", c.Options.Ladder)
	}
	if c.Options.Spend != "" {
		args = append(args, "--spend", c.Options.Spend)
	}
	args = append(args, "--json")
	stdout, stderr, code := ksIn(t, bin, cfg, root, args...)
	if code != 0 {
		refused, said := "", stdout+stderr
		switch {
		case strings.Contains(said, "symlinks are refused"):
			refused = "ks_workspace_symlinks"
		case strings.Contains(said, "Nothing is chosen for you"):
			refused = "ks_check_ambiguous"
		case strings.Contains(said, "no check found"):
			refused = "ks_check_not_found"
		case strings.Contains(said, "inside a dot directory"):
			refused = "ks_check_unpinnable"
		default:
			t.Fatalf("%s: init exit %d with a refusal this file does not name:\n%s%s", c.Name, code, stdout, stderr)
		}
		out := map[string]any{"refused": refused, "init_exit": code}
		if refused == "ks_workspace_symlinks" {
			if initSel == nil {
				t.Fatalf("%s: no selection to read the links from", c.Name)
			}
			out["symlinks"] = initSel.Symlinks
		}
		return out
	}
	var initOut map[string]any
	if err := json.Unmarshal([]byte(stdout), &initOut); err != nil {
		t.Fatalf("%s: init --json: %v\n%s", c.Name, err, stdout)
	}
	if d, ok := initOut["data"].(map[string]any); ok {
		initOut = d
	}
	draft, err := readDraftNumbers(root)
	if err != nil {
		t.Fatal(err)
	}
	initText, initSHA := canonText(t, draft)
	if initSHA != initOut["manifest_sha"] {
		t.Fatalf("%s: the init draft digests to %s, init printed %v", c.Name, initSHA, initOut["manifest_sha"])
	}
	cfgBytes, err := os.ReadFile(filepath.Join(root, cruiseVerifier))
	if err != nil {
		t.Fatal(err)
	}
	cfgSum := sha256.Sum256(cfgBytes)
	if initErr != nil {
		t.Fatalf("%s: init drafted a tree the selection refuses: %v", c.Name, initErr)
	}
	archSum := sha256.Sum256(arch.Bytes())
	ws := draft["workspace"].(map[string]any)
	if fmt.Sprint(ws["sha256"]) != hex.EncodeToString(archSum[:]) || fmt.Sprint(ws["bytes"]) != fmt.Sprint(arch.Len()) {
		t.Fatalf("%s: the archive packed again (%x, %d bytes) is not the one init recorded (%v, %v)", c.Name, archSum, arch.Len(), ws["sha256"], ws["bytes"])
	}

	stdout, stderr, code = ksIn(t, bin, cfg, root, "cruise", "approve")
	if code != 0 {
		t.Fatalf("%s: approve exit %d\n%s%s", c.Name, code, stdout, stderr)
	}
	approved := strings.TrimSpace(strings.SplitN(stdout, "\n", 2)[0])
	bound, err := readDraftNumbers(root)
	if err != nil {
		t.Fatal(err)
	}
	boundText, boundSHA := canonText(t, bound)
	if boundSHA != approved {
		t.Fatalf("%s: the bound draft digests to %s, approve printed %s", c.Name, boundSHA, approved)
	}
	// the selection approve bound, built again by the same code approve runs
	files, sel, err := scanSelected(root, nil, readSelectionOverrides(root))
	if err != nil {
		t.Fatal(err)
	}
	if want := bound["workspace"].(map[string]any)["selection_digest"]; want != sel.Digest {
		t.Fatalf("%s: the selection built again digests to %s, approve bound %v", c.Name, sel.Digest, want)
	}
	ver := bound["verifier"].(map[string]any)
	inputsDigest, rows, err := bindingInputs(files, checkRunnerOf(ver))
	if err != nil {
		t.Fatal(err)
	}
	if inputsDigest != ver["inputs_digest"] {
		t.Fatalf("%s: inputs digest %s, approve bound %v", c.Name, inputsDigest, ver["inputs_digest"])
	}
	shas := map[string]string{}
	for _, f := range files {
		shas[f.rel] = hex.EncodeToString(f.sha[:])
	}
	disc := ks072Discover(shas, nil)
	return map[string]any{
		"init_selection_digest": initSel.Digest,
		"selection":             sel,
		"tree_digest":           treeDigest(files),
		"with_tests":            disc.WithTests,
		"check":                 map[string]any{"runner": initOut["check_kind"], "source": initOut["check_source"], "command": initOut["check"]},
		"pinned_inputs":         rows,
		"inputs_digest":         inputsDigest,
		"tests_digest":          ver["tests_digest"],
		"tests_pinned":          initOut["tests_pinned"],
		"verifier_config":       string(cfgBytes),
		"config_hash":           hex.EncodeToString(cfgSum[:]),
		"archive":               map[string]any{"sha256": hex.EncodeToString(archSum[:]), "bytes": arch.Len(), "b64": base64.StdEncoding.EncodeToString(arch.Bytes())},
		"manifest_init":         initText,
		"manifest_init_sha":     initSHA,
		"manifest":              boundText,
		"manifest_sha":          boundSHA,
	}
}

func readDraftNumbers(root string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(root, cruiseDraft))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

// normalizeJSON round-trips v through encoding/json so a computed value and
// one read from the file compare as the same shapes.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCruiseDraftVectors(t *testing.T) {
	_, bin, cfg := startFake(t)
	dest := os.Getenv("KS_WRITE_CRUISE_DRAFT_VECTORS")
	if dest != "" {
		file := draftVectorFile{
			Name:    "ks-cruise-draft",
			Version: 1,
			What: "a hosted Cruise draft carries exactly what `ks cruise init` and `ks cruise approve` compute on the same files. " +
				"Generated by the public client's own built binary running init (with the options shown) and approve against a recording control plane that lists the keys below; " +
				"every implementation that builds a draft (the control plane's port) or recomputes a digest from one (the judge) must reproduce every expectation.",
			Rules: []string{
				"the tree is written as given: files with their mode, empty directories, symbolic links with their target; nothing else exists",
				"init runs with the options shown and no other; approve runs right after it, so .keepstate (written by init) exists when approve builds the selection it binds, and is recorded there as a mandatory exclusion; init_selection_digest is the digest of the selection of the tree as given, before init wrote .keepstate",
				"approve binds one route per provider the ladder uses: the only enabled key the control plane lists for that provider",
				"selection is the bound selection as the client builds it (policy, included files with size, mode and sha256 in the judge's walk order, excluded paths with their reasons sorted by path, refused symlinks, overrides, digest)",
				"archive is the gzip tar init packs: fixed epoch times, uid and gid 0, mode 0644 or 0755, entries in the judge's walk order; b64 is its bytes",
				"manifest_init and manifest are the canonical bytes (the shared canonical rule) of the draft after init and of the bound draft after approve; their sha256 are manifest_init_sha and manifest_sha",
				"a refused case names the refusal: ks_workspace_symlinks (with the links), ks_check_ambiguous, ks_check_not_found, ks_check_unpinnable; nothing is drafted for it",
			},
			Keys: []map[string]any{{"id": "vlt_anthropic1", "provider": "anthropic", "enabled": true, "created_at": "2026-09-01T00:00:00Z"}},
		}
		for _, c := range draftVectorCases() {
			c.Expect = normalizeJSON(t, computeDraftVector(t, bin, cfg, c)).(map[string]any)
			file.Cases = append(file.Cases, c)
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(file); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d cases to %s", len(file.Cases), dest)
		return
	}
	raw, err := os.ReadFile(cruiseDraftVectorsVendored)
	if err != nil {
		t.Fatal(err)
	}
	var file draftVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Name != "ks-cruise-draft" || file.Version != 1 || len(file.Cases) < 10 {
		t.Fatalf("vectors: %s v%d, %d cases", file.Name, file.Version, len(file.Cases))
	}
	refused := 0
	for _, c := range file.Cases {
		got := normalizeJSON(t, computeDraftVector(t, bin, cfg, c))
		want := normalizeJSON(t, c.Expect)
		if !reflect.DeepEqual(got, want) {
			gb, _ := json.MarshalIndent(got, "", " ")
			t.Errorf("%s: this client now computes something else than the shared vectors record:\n%s", c.Name, gb)
		}
		if _, ok := c.Expect["refused"]; ok {
			refused++
		}
	}
	if refused < 4 || len(file.Cases)-refused < 6 {
		t.Fatalf("the vectors hold %d drafted and %d refused cases; too few to hold a port to", len(file.Cases)-refused, refused)
	}
}

// TestCruiseDraftVendoredVectorsMatchTheServer: the vendored copy is the
// service's file as SOURCE.json records it, and, given a service checkout,
// still the service's current file.
func TestCruiseDraftVendoredVectorsMatchTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From     string `json:"from"`
			SHA256   string `json:"sha256"`
			Vendored string `json:"vendored_sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/cruisedraft/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" || len(src.Files) == 0 {
		t.Fatalf("testdata/cruisedraft/SOURCE.json: %v", err)
	}
	for name, f := range src.Files {
		if got := fileSHA(t, filepath.Join("testdata/cruisedraft", name)); got != f.Vendored {
			t.Errorf("testdata/cruisedraft/%s is %s, SOURCE.json records %s", name, got, f.Vendored)
		}
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in the service's CI")
	}
	for name, f := range src.Files {
		raw, err := os.ReadFile(filepath.Join(server, f.From))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(publicScrub(raw))
		if got := hex.EncodeToString(sum[:]); got != f.Vendored {
			t.Fatalf("DRIFT: the service's %s is now %s, but this client vendored %s (%s) from commit %s; generate, commit and vendor them again together",
				f.From, got, f.Vendored, name, src.ServerCommit)
		}
	}
}
