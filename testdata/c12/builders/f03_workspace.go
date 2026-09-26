//go:build unix

package c12

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// F03 kinds: what each planted path is, and what the client's selection must
// do with it. None of the planted content is a real secret: the dummy .env
// says so, and the private-key-shaped file carries the PEM armour around a
// marker, never key material.
const (
	KindOrdinary     = "ordinary"      // selected and uploaded
	KindNestedTest   = "nested_test"   // selected: tests live where the repository keeps them
	KindSensitive    = "sensitive"     // blocked unless explicitly overridden
	KindIgnoredDep   = "ignored_dep"   // excluded (dependency tree or .gitignore)
	KindExcluded     = "excluded"      // excluded by policy (.git, .keepstate)
	KindOutsideLink  = "outside_link"  // a symlink leaving the root: refused
	KindInsideLink   = "inside_link"   // a symlink inside the root: refused (links never upload)
	KindSpecial      = "special"       // a FIFO: refused
	KindOversized    = "oversized"     // larger than the upload bound: refused
	KindRaceTarget   = "race_target"   // selected; SwapToOutsideLink replaces it after preview (J09)
	KindUnicodeName  = "unicode_name"  // selected; the name survives byte-exact
	KindOutsideEntry = "outside_entry" // lives OUTSIDE the root; must never be read
)

// F03Entry is one planted path, relative to the workspace root (or, for
// KindOutsideEntry, to its parent directory).
type F03Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Expect string `json:"expect"`
}

// OversizedBytes is one byte past the control plane's expanded-archive bound
// (ctl archiveMaxExpanded, 50 MiB). The file is sparse, so building it costs
// no disk to speak of.
const OversizedBytes = int64(50<<20) + 1

const pemBegin = "-----BEGIN " + "OPENSSH PRIVATE KEY-----"
const pemEnd = "-----END " + "OPENSSH PRIVATE KEY-----"

// BuildF03 plants the hostile workspace under dir/workspace (and one file in
// dir/outside that the workspace links to), deterministically for a seed.
// It returns the root and every planted path with its expectation.
func BuildF03(dir string, seed int64) (string, []F03Entry, error) {
	r := rand.New(rand.NewSource(seed))
	root := filepath.Join(dir, "workspace")
	outside := filepath.Join(dir, "outside")
	var entries []F03Entry
	write := func(rel, kind, expect string, body []byte) error {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			return err
		}
		entries = append(entries, F03Entry{rel, kind, expect})
		return nil
	}
	files := []struct {
		rel, kind, expect string
		body              []byte
	}{
		{"README.md", KindOrdinary, "upload", append([]byte("# F03 hostile workspace (synthetic)\n"), filler(r, 80)...)},
		{"src/app.py", KindOrdinary, "upload", append([]byte("def main():\n    return 42\n# "), filler(r, 40)...)},
		{"src/swap-me.py", KindRaceTarget, "upload at preview; after SwapToOutsideLink the changed selection needs reapproval", []byte("VALUE = 1\n")},
		{"pkg/sub/tests/test_nested.py", KindNestedTest, "upload", []byte("def test_nested():\n    assert True\n")},
		{"web/__tests__/a.test.js", KindNestedTest, "upload", []byte("test('a', () => {});\n")},
		{"docs/çalışma notları.md", KindUnicodeName, "upload; name byte-exact", []byte("Türkçe içerik\n")},
		{".env", KindSensitive, "block unless explicitly overridden", []byte("KS_FIXTURE_DUMMY=this-is-not-a-secret\n")},
		{"config/.env.production", KindSensitive, "block unless explicitly overridden", []byte("KS_FIXTURE_DUMMY=this-is-not-a-secret\n")},
		{"keys/id_ed25519", KindSensitive, "block unless explicitly overridden", []byte(pemBegin + "\nF03-FIXTURE-MARKER-NOT-KEY-MATERIAL\n" + pemEnd + "\n")},
		{"node_modules/leftpad/index.js", KindIgnoredDep, "exclude", []byte("module.exports = 1;\n")},
		{".venv/lib/site.py", KindIgnoredDep, "exclude", []byte("x = 1\n")},
		{".gitignore", KindOrdinary, "upload", []byte("build/\n*.log\n")},
		{"build/out.bin", KindIgnoredDep, "exclude (.gitignore)", filler(r, 32)},
		{"debug.log", KindIgnoredDep, "exclude (.gitignore)", filler(r, 32)},
		{".git/config", KindExcluded, "exclude", []byte("[core]\n")},
		{".keepstate/draft.json", KindExcluded, "exclude", []byte("{}\n")},
	}
	for _, f := range files {
		if err := write(f.rel, f.kind, f.expect, f.body); err != nil {
			return "", nil, err
		}
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("F03 OUTSIDE THE ROOT: never read\n"), 0o644); err != nil {
		return "", nil, err
	}
	entries = append(entries, F03Entry{"../outside/secret.txt", KindOutsideEntry, "never read, never uploaded"})
	if err := os.Symlink(filepath.Join("..", "outside", "secret.txt"), filepath.Join(root, "link-outside")); err != nil {
		return "", nil, err
	}
	entries = append(entries, F03Entry{"link-outside", KindOutsideLink, "refuse; name the link"})
	if err := os.Symlink(filepath.Join("src", "app.py"), filepath.Join(root, "link-inside")); err != nil {
		return "", nil, err
	}
	entries = append(entries, F03Entry{"link-inside", KindInsideLink, "refuse; links never upload"})
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		return "", nil, fmt.Errorf("mkfifo: %w", err)
	}
	entries = append(entries, F03Entry{"pipe", KindSpecial, "refuse; never open it (opening a FIFO blocks)"})
	big, err := os.Create(filepath.Join(root, "data", "huge.bin"))
	if os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Join(root, "data"), 0o755)
		big, err = os.Create(filepath.Join(root, "data", "huge.bin"))
	}
	if err != nil {
		return "", nil, err
	}
	if err := big.Truncate(OversizedBytes); err != nil {
		big.Close()
		return "", nil, err
	}
	big.Close()
	entries = append(entries, F03Entry{"data/huge.bin", KindOversized, "refuse; over the upload bound"})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return root, entries, nil
}

// SwapToOutsideLink is the race of J09: after a preview approved the
// selection, the selected file is replaced by a symlink leaving the root.
// Old consent must not authorize the new bytes.
func SwapToOutsideLink(root string) error {
	p := filepath.Join(root, "src", "swap-me.py")
	if err := os.Remove(p); err != nil {
		return err
	}
	return os.Symlink(filepath.Join("..", "..", "outside", "secret.txt"), p)
}
