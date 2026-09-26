// KS-091 C12 fixtures, client half: the service's deterministic F03
// hostile workspace and F04 archive/result attacks, vendored unchanged into
// internal/c12 with their SOURCE.json, run against the client's upload
// selection (KS-026), result extraction (KS-056), artifact tree digest
// (KS-078) and changeset apply (KS-057/078). Every row of each fixture's
// expectation is asserted here or named as a deliberate difference.
package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrygrtc/cli/internal/c12"
)

func TestC12VendoredFixturesMatchTheirSource(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From   string `json:"from"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/c12/SOURCE.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &src); err != nil {
		t.Fatal(err)
	}
	for local, f := range src.Files {
		if got := fileSHA(t, local); got != f.SHA256 {
			t.Errorf("%s is %s, SOURCE.json records %s: the vendored copy was edited", local, got, f.SHA256)
		}
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service's current fixtures runs in the drift workflow")
	}
	for local, f := range src.Files {
		if got := fileSHA(t, filepath.Join(server, f.From)); got != f.SHA256 {
			t.Errorf("DRIFT: the service's %s is now %s; this client vendored %s (%s) from %s; re-vendor and re-run the C12 tests",
				f.From, got, f.SHA256, local, src.ServerCommit)
		}
	}
}

// foldsNames asks this filesystem whether it treats two names as one.
func foldsNames(t *testing.T, a, b string) bool {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, a), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(d, b))
	return err == nil
}

func TestC12F04ArchivesAgainstTheExtractorAndTheTreeDigest(t *testing.T) {
	caseFolds := foldsNames(t, "README.md", "readme.md")
	nfcFolds := foldsNames(t, "caf\u00e9.txt", "cafe\u0301.txt")
	for _, ac := range c12.F04ArchiveCases(c12.DefaultSeed) {
		dir := t.TempDir()
		arch := filepath.Join(dir, ac.Name+".tar.gz")
		if err := os.WriteFile(arch, ac.Bytes, 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "out")
		n, xerr := extractArchive(arch, out)
		_, derr := tarTreeDigest(ac.Bytes)
		_, statErr := os.Stat(out)
		refusedWhole := xerr != nil && os.IsNotExist(statErr)
		switch ac.Name {
		case "clean", "digest-mismatch":
			// digest-mismatch is a manifest binding the server checks; the
			// client's check for a result is the sha256 of the download
			// (TestADownloadIsReverifiedAgainstTheCandidateAndNeverOverwrites)
			if xerr != nil || n != 2 || derr != nil {
				t.Errorf("%s: extract %v (%d files), digest %v", ac.Name, xerr, n, derr)
			}
		case "directory-entry":
			// DIFFERENCE, reported: an explicit directory entry is ordinary
			// in tar and zip results, so the extractor accepts it (every
			// file is still checked); the tree digest skips it, as the
			// verifier's tarball carries none
			if xerr != nil || derr != nil {
				t.Errorf("%s: extract %v, digest %v", ac.Name, xerr, derr)
			}
		case "case-collision", "unicode-normalization-pair":
			folds := caseFolds
			if ac.Name == "unicode-normalization-pair" {
				folds = nfcFolds
			}
			if folds {
				if !refusedWhole || !strings.Contains(xerr.Error(), "are the same file on this filesystem") {
					t.Errorf("%s on a folding filesystem: %v (dir left: %v)", ac.Name, xerr, statErr == nil)
				}
			} else if xerr != nil {
				t.Errorf("%s on a filesystem that keeps both names: %v", ac.Name, xerr)
			}
		default:
			if !refusedWhole {
				t.Errorf("%s: not refused whole (extract %v, dir left %v); the fixture expects: %s", ac.Name, xerr, statErr == nil, ac.Client)
			}
			// the artifact's own bound is 1 GiB (a result, not a workspace),
			// so 51 MiB of zeros digests; the extractor stops it at 50 MiB
			if derr == nil && ac.Name != "huge-expansion" {
				t.Errorf("%s: the artifact tree digest accepted it", ac.Name)
			}
		}
	}
}

// F04 apply-side scenarios against the changeset apply (partial-download and
// hash-mismatch are the download checks, asserted in cruise_result_test.go
// and results_test.go).
func TestC12F04ApplyScenarios(t *testing.T) {
	// dirty-local-base: the conflicting path is refused and the edit survives
	root, raw := changesetFixture(t)
	cs, err := parseChangeset(raw)
	if err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(root, "a.txt"), "local edit\n")
	if _, conflicts := planApply(root, cs); len(conflicts) != 1 {
		t.Fatalf("dirty base: %v", conflicts)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "local edit\n" {
		t.Fatal("the local edit did not survive")
	}

	// pre-existing-target-race: a file appears at an added path between the
	// preview and its replacement; it is not overwritten
	root, raw = changesetFixture(t)
	cs, _ = parseChangeset(raw)
	var added string
	for _, c := range cs.Changes {
		if c.Op == "add" {
			added = filepath.Join(root, filepath.FromSlash(c.Path))
		}
	}
	applyFault = func(done int) error {
		if done == 1 { // the add is the second change
			_ = os.MkdirAll(filepath.Dir(added), 0o755)
			return os.WriteFile(added, []byte("newcomer\n"), 0o644)
		}
		return nil
	}
	err = applyChangeset(root, "race", cs)
	applyFault = nil
	if err == nil || !strings.Contains(err.Error(), "after the preview") {
		t.Fatalf("the race was not refused: %v", err)
	}
	if b, _ := os.ReadFile(added); string(b) != "newcomer\n" {
		t.Fatalf("the newcomer was overwritten: %q", b)
	}
	if _, err := recoverApply(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "old a\n" {
		t.Errorf("recover did not restore the path the apply had replaced: %q", b)
	}

	// case / normalization collisions inside one changeset are named before
	// anything is written, on a filesystem that folds them
	for _, pair := range [][2]string{{"README.md", "readme.md"}, {"caf\u00e9.txt", "cafe\u0301.txt"}} {
		r := t.TempDir()
		mk := func(p, body string) map[string]any {
			return map[string]any{"path": p, "op": "add", "after": fileState([]byte(body), "0644"), "content_b64": base64.StdEncoding.EncodeToString([]byte(body))}
		}
		doc, _ := json.Marshal(map[string]any{"kind": "keepstate.changeset", "version": 1, "base": map[string]any{"known": true, "digest": "sha256:" + strings.Repeat("a", 64)},
			"changes": []any{mk(pair[0], "one\n"), mk(pair[1], "two\n")}})
		cs, err := parseChangeset(doc)
		if err != nil {
			t.Fatal(err)
		}
		got := collisionRefusals(r, cs)
		if foldsNames(t, pair[0], pair[1]) {
			if len(got) != 1 || !strings.Contains(got[0], "are the same file on this filesystem") {
				t.Errorf("%q/%q on a folding filesystem: %v", pair[0], pair[1], got)
			}
		} else if len(got) != 0 {
			t.Errorf("%q/%q on a filesystem that keeps both: %v", pair[0], pair[1], got)
		}
		if _, err := os.Stat(filepath.Join(r, ".keepstate")); !os.IsNotExist(err) {
			t.Errorf("the collision probe left .keepstate behind")
		}
	}
}
