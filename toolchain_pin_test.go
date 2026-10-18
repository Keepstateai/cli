// toolchain_pin_test.go: this client and the service build with one Go
// toolchain.
//
// A Cruise draft's workspace digest is SHA-256 over the gzip bytes of the
// workspace archive. `ks cruise init` writes that archive with this client's
// compress/gzip, and the service's console writes the same archive with its
// own, so a console draft and the terminal's init agree on the digests only
// while both are compiled by toolchains that write the same deflate stream.
// Go 1.27 writes another one. The client and the service are therefore locked
// to one toolchain: the toolchain line of each go.mod, exactly.
//
// These tests fail when a place in this repository that chooses a Go
// toolchain names another one (go.mod, GOTOOLCHAIN in any script, workflow
// or document, every actions/setup-go step, a Go release name outside the
// records), and when the service's pin, vendored in testdata/toolchain with
// its SOURCE.json, is not this one. The shared draft vectors
// (cruise_draft_vectors_test.go) are the measurement under the lock: under
// another gzip stream they fail on the archive bytes.
//
//	KS_GO_PIN_RUNTIME=1 go test -run '^TestGoToolchain' -v .
//
// also requires the toolchain that compiled the test to be the pin (CI sets
// it, so a runner that quietly used another Go fails here).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const toolchainVendored = "testdata/toolchain/go-toolchain.txt"

var (
	// a Go release name: go1.26.0, go1.27rc1, go1.26 (a bare line is never the pin)
	pinRelease = regexp.MustCompile(`\bgo1\.\d+(?:\.\d+)?(?:rc\d+|beta\d+)?\b`)
	// a GOTOOLCHAIN value: GOTOOLCHAIN=go1.26.0, GOTOOLCHAIN: go1.26.0,
	// ${GOTOOLCHAIN:-go1.26.0}, "GOTOOLCHAIN": "local"
	pinSets        = regexp.MustCompile(`GOTOOLCHAIN["']?\s*(?::-|:=|=|:)\s*["']?([A-Za-z0-9][A-Za-z0-9.+_-]*)`)
	pinGolangImage = regexp.MustCompile(`\bgolang:(\d+\.\d+(?:\.\d+)?)`)
	pinGoVersion   = regexp.MustCompile(`\bgo-version:\s*["']?([^"'\s,}]+)`)
	pinGoVerFile   = regexp.MustCompile(`\bgo-version-file:`)
	pinModLine     = regexp.MustCompile(`(?m)^toolchain\s+(\S+)\s*$`)
	pinModGo       = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	pinExact       = regexp.MustCompile(`^go\d+\.\d+\.\d+$`)
)

// toolchainRecords are files that record what was, not choose what is: the
// changelog, the v0.1.13 candidate's record, the vendored material and
// fixtures under testdata, and this file, whose planted cases name the very
// versions it must refuse.
func toolchainRecord(rel string) bool {
	switch rel {
	case "docs/changelog.md", "docs/release-candidate.md", "toolchain_pin_test.go":
		return true
	}
	return strings.HasPrefix(rel, "testdata/") || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "dist/")
}

type toolchainChoice struct {
	File  string
	Line  int
	Kind  string
	Value string
}

// toolchainChoices reads every toolchain choice in one file's text.
func toolchainChoices(rel, text string) []toolchainChoice {
	var out []toolchainChoice
	for i, line := range strings.Split(text, "\n") {
		var found []toolchainChoice
		add := func(kind, v string) { found = append(found, toolchainChoice{rel, i + 1, kind, v}) }
		for _, m := range pinSets.FindAllStringSubmatch(line, -1) {
			add("GOTOOLCHAIN", m[1])
		}
		for _, m := range pinGolangImage.FindAllStringSubmatch(line, -1) {
			add("golang image", "go"+m[1])
		}
		for _, m := range pinRelease.FindAllString(line, -1) {
			dup := false
			for _, f := range found {
				dup = dup || f.Value == m
			}
			if !dup {
				add("release name", m)
			}
		}
		if strings.HasPrefix(rel, ".github/workflows/") {
			for _, m := range pinGoVersion.FindAllStringSubmatch(line, -1) {
				add("setup-go go-version", m[1])
			}
			if pinGoVerFile.MatchString(line) {
				add("setup-go go-version-file", strings.Trim(strings.SplitN(line, "go-version-file:", 2)[1], " {}'\""))
			}
		}
		out = append(out, found...)
	}
	return out
}

// toolchainProblem says what is wrong with one choice, or "".
func toolchainProblem(c toolchainChoice, pin string) string {
	switch c.Kind {
	case "setup-go go-version-file":
		return "setup-go reads a version file's go line, not go.mod's toolchain line; name the pin with go-version"
	case "setup-go go-version":
		if c.Value != strings.TrimPrefix(pin, "go") {
			return fmt.Sprintf("setup-go installs %s, not the pinned %s", c.Value, strings.TrimPrefix(pin, "go"))
		}
		return ""
	}
	if c.Value != pin {
		return fmt.Sprintf("names %s, not the pinned %s", c.Value, pin)
	}
	return ""
}

// goModPin reads the toolchain a go.mod pins, and what is wrong with it.
func goModPin(text string) (string, []string) {
	pins := pinModLine.FindAllStringSubmatch(text, -1)
	if len(pins) == 0 {
		return "", []string{"go.mod names no toolchain (a `toolchain go1.N.N` line), so nothing is pinned"}
	}
	var problems []string
	if len(pins) > 1 {
		problems = append(problems, fmt.Sprintf("go.mod has %d toolchain lines", len(pins)))
	}
	pin := pins[0][1]
	if !pinExact.MatchString(pin) {
		problems = append(problems, fmt.Sprintf("go.mod's toolchain %q is not an exact release (go1.N.N)", pin))
	}
	if g := pinModGo.FindStringSubmatch(text); g != nil && versionAfter(g[1], strings.TrimPrefix(pin, "go")) {
		problems = append(problems, fmt.Sprintf("go.mod's go line %s is newer than its toolchain %s, which would refuse to build it", g[1], pin))
	}
	return pin, problems
}

func versionAfter(a, b string) bool {
	num := func(s string) []int {
		var n []int
		for _, p := range regexp.MustCompile(`\d+`).FindAllString(s, 3) {
			v, _ := strconv.Atoi(p)
			n = append(n, v)
		}
		for len(n) < 3 {
			n = append(n, 0)
		}
		return n
	}
	x, y := num(a), num(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// repoFiles lists the repository's files: git's index when there is one,
// else a walk of the tree.
func repoFiles(t *testing.T) []string {
	t.Helper()
	if out, err := exec.Command("git", "ls-files", "-z").Output(); err == nil && len(out) > 0 {
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		return files
	}
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == ".git" || p == "dist") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			files = append(files, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readVendoredPin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(toolchainVendored)
	if err != nil {
		t.Fatalf("the service's pin is not vendored: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// TestGoToolchainIsPinnedEverywhere: go.mod pins the service's toolchain, and
// every other place here that chooses a toolchain names the same one.
func TestGoToolchainIsPinnedEverywhere(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	modPin, problems := goModPin(string(mod))
	for _, p := range problems {
		t.Error(p)
	}
	pin := readVendoredPin(t)
	if modPin != pin {
		t.Errorf("go.mod pins %q; the service pins %s (vendored in %s): the client and the service build with one toolchain", modPin, pin, toolchainVendored)
	}
	var places []toolchainChoice
	setupGo := 0
	for _, rel := range repoFiles(t) {
		if toolchainRecord(rel) {
			continue
		}
		b, err := os.ReadFile(rel)
		if err != nil || !utf8Text(b) {
			continue
		}
		for _, c := range toolchainChoices(rel, string(b)) {
			places = append(places, c)
			if strings.HasPrefix(c.Kind, "setup-go") {
				setupGo++
			}
			if p := toolchainProblem(c, pin); p != "" {
				t.Errorf("%s:%d %s %s: %s", c.File, c.Line, c.Kind, c.Value, p)
			}
		}
	}
	if setupGo == 0 {
		t.Errorf("no actions/setup-go step names a Go version; the workflows would build with whatever the runner carries")
	}
	names := map[string]int{}
	for _, c := range places {
		names[c.Value]++
	}
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var summary []string
	for _, k := range keys {
		summary = append(summary, fmt.Sprintf("%s (%d)", k, names[k]))
	}
	t.Logf("go.mod pins %q; the service pins %s; places read: %d; names: %s", modPin, pin, len(places), strings.Join(summary, ", "))
	if os.Getenv("KS_GO_PIN_RUNTIME") == "1" && runtime.Version() != pin {
		t.Errorf("this test was compiled by %s, not the pinned %s", runtime.Version(), pin)
	}
}

func utf8Text(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// TestGoToolchainCheckCatchesAnotherVersion: the guard above can fail. Each
// planted place names another toolchain and must be reported; the pinned
// ones must not.
func TestGoToolchainCheckCatchesAnotherVersion(t *testing.T) {
	const pin = "go1.26.0"
	planted := map[string]string{
		".github/workflows/a.yml":     "      - uses: actions/setup-go@v5\n        with: { go-version: \"1.22.12\" }\n",
		".github/workflows/b.yml":     "      - uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n",
		".github/workflows/c.yml":     "    env:\n      GOTOOLCHAIN: go1.27.1\n",
		".github/workflows/d.yml":     "      - uses: actions/setup-go@v5\n        with: { go-version: '1.26.x' }\n",
		"RELEASING.md":                "Reproduce the CI bytes locally with `GOTOOLCHAIN=go1.22.12` and the same line.\n",
		"scripts/build.sh":            "export GOTOOLCHAIN=\"${GOTOOLCHAIN:-go1.26.8}\"\n",
		"scripts/local.sh":            "GOTOOLCHAIN=local go build .\n",
		"Dockerfile":                  "FROM golang:1.27\n",
		"install.sh":                  "curl -fsSLO https://go.dev/dl/go1.27.1.linux-amd64.tar.gz\n",
		"provenance.go":               "// built with go1.26 (a line, not a release)\n",
		".github/workflows/fine.yml":  "      - uses: actions/setup-go@v5\n        with: { go-version: '1.26.0' }\n    env: { GOTOOLCHAIN: go1.26.0 }\n",
		"scripts/fine.sh":             "GOTOOLCHAIN=go1.26.0 go build -trimpath .\n",
		"docs/notes.md":               "Use GOTOOLCHAIN=go1.26.0 to reproduce.\n",
		"main.go":                     "package main\n",
		".github/workflows/plain.yml": "      - uses: actions/checkout@v4\n",
	}
	want := map[string]bool{
		".github/workflows/a.yml": true, ".github/workflows/b.yml": true, ".github/workflows/c.yml": true,
		".github/workflows/d.yml": true, "RELEASING.md": true, "scripts/build.sh": true, "scripts/local.sh": true,
		"Dockerfile": true, "install.sh": true, "provenance.go": true,
	}
	for rel, text := range planted {
		caught := false
		for _, c := range toolchainChoices(rel, text) {
			if toolchainProblem(c, pin) != "" {
				caught = true
			}
		}
		if caught != want[rel] {
			t.Errorf("%s: reported=%v, want %v (%q)", rel, caught, want[rel], text)
		}
	}
	for name, mod := range map[string]string{
		"no toolchain line":     "module x\n\ngo 1.22\n",
		"a line, not a release": "module x\n\ngo 1.22\n\ntoolchain go1.26\n",
		"a go line too new":     "module x\n\ngo 1.27\n\ntoolchain go1.26.0\n",
		"two toolchain lines":   "module x\n\ngo 1.22\n\ntoolchain go1.26.0\ntoolchain go1.26.8\n",
	} {
		if _, problems := goModPin(mod); len(problems) == 0 {
			t.Errorf("go.mod with %s: not reported", name)
		}
	}
	if p, problems := goModPin("module x\n\ngo 1.22\n\ntoolchain go1.26.0\n"); p != pin || len(problems) != 0 {
		t.Errorf("a pinned go.mod: %q %v", p, problems)
	}
	for _, rel := range []string{"docs/changelog.md", "testdata/c12/x.json", "toolchain_pin_test.go"} {
		if !toolchainRecord(rel) {
			t.Errorf("%s should be a record", rel)
		}
	}
	if toolchainRecord("RELEASING.md") || toolchainRecord(".github/workflows/release.yml") {
		t.Errorf("a live instruction or workflow was taken for a record")
	}
}

// TestGoToolchainVendoredPinMatchesTheServer: the vendored pin is the
// service's as SOURCE.json records it, and, given a service checkout, still
// the toolchain line of the service's go.mod.
func TestGoToolchainVendoredPinMatchesTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From     string `json:"from"`
			SHA256   string `json:"sha256"`
			Vendored string `json:"vendored_sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/toolchain/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" || len(src.Files) != 1 {
		t.Fatalf("testdata/toolchain/SOURCE.json: %v", err)
	}
	f, ok := src.Files["go-toolchain.txt"]
	if !ok {
		t.Fatalf("testdata/toolchain/SOURCE.json records no go-toolchain.txt")
	}
	if got := fileSHA(t, toolchainVendored); got != f.Vendored || got != f.SHA256 {
		t.Errorf("%s is %s, SOURCE.json records %s (vendored) and %s (the service's)", toolchainVendored, got, f.Vendored, f.SHA256)
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in the service's CI")
	}
	mod, err := os.ReadFile(filepath.Join(server, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	pin, problems := goModPin(string(mod))
	if len(problems) > 0 {
		t.Fatalf("the service's go.mod: %v", problems)
	}
	sum := sha256.Sum256([]byte(pin + "\n"))
	if got := hex.EncodeToString(sum[:]); got != f.Vendored || readVendoredPin(t) != pin {
		t.Fatalf("DRIFT: the service now pins %s, but this client vendored %s from commit %s; move the client's go.mod, workflows and vendored pin together",
			pin, readVendoredPin(t), src.ServerCommit)
	}
}
