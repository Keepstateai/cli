// Upload policy ks-upload-policy/2 (coordinator ruling): the verifier's
// SKIP_DIRS -- vendored, with a drift guard -- are excluded from an upload;
// an explicit --allow still includes one; a v1 approval still verifies,
// because run rebuilds the selection under the policy its approval names.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestVerifierSkipDirsVendoredAndUnchanged(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From   string `json:"from"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/skipdirs/SOURCE.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &src); err != nil {
		t.Fatal(err)
	}
	if got := fileSHA(t, "verifier_skip_dirs.json"); got != src.Files["verifier_skip_dirs.json"].SHA256 {
		t.Fatalf("verifier_skip_dirs.json is %s, SOURCE.json records %s: the vendored list was edited", got, src.Files["verifier_skip_dirs.json"].SHA256)
	}
	// the discovery port and the upload policy read the same set
	for d := range verifierSkipDirs {
		if !ks072SkipDirs[d] {
			t.Errorf("%s is in the upload policy and not in the discovery port", d)
		}
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the verifier's current SKIP_DIRS runs in the drift workflow")
	}
	py, err := os.ReadFile(filepath.Join(server, "judge", "checks.py"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)SKIP_DIRS = \{([^}]*)\}`).FindSubmatch(py)
	if m == nil {
		t.Fatal("DRIFT: judge/checks.py no longer defines SKIP_DIRS as a set literal; re-vendor by hand")
	}
	var theirs []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		theirs = append(theirs, string(q[1]))
	}
	var ours []string
	for d := range verifierSkipDirs {
		ours = append(ours, d)
	}
	sort.Strings(theirs)
	sort.Strings(ours)
	if strings.Join(theirs, ",") != strings.Join(ours, ",") {
		t.Fatalf("DRIFT: the verifier's SKIP_DIRS is now %v; this client vendored %v from %s. Re-vendor it; a changed list is a new upload policy version", theirs, ours, src.ServerCommit)
	}
}

func depRepo(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/app.py":                    "print(1)\n",
		"test_app.py":                   "def test_a():\n    assert True\n",
		"node_modules/leftpad/index.js": "module.exports = 1;\n",
		".venv/lib/site.py":             "x = 1\n",
		"web/node_modules/x/y.js":       "1\n",
		"dist/bundle.js":                "1\n",
	})
	return root
}

func TestPolicyV2ExcludesTheVerifiersSkipDirsAndAllowOverrides(t *testing.T) {
	root := depRepo(t)
	v2, err := buildSelectionUnder(root, nil, selectionPolicyV2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(v2), ","); got != "test_app.py,src/app.py" {
		t.Errorf("v2 included %s", got)
	}
	for _, p := range []string{"node_modules", ".venv", "web/node_modules", "dist"} {
		if r := excludedReason(v2, p); !strings.HasPrefix(r, "dependency or tool tree") {
			t.Errorf("%s: %q", p, r)
		}
	}
	// v1 uploads them, exactly as it always did
	v1, err := buildSelectionUnder(root, nil, selectionPolicyV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.Included) != 6 || v1.PolicyVersion != selectionPolicyV1 {
		t.Errorf("v1 included %v", paths(v1))
	}
	_, s1, _ := scanSelectedUnder(root, nil, nil, selectionPolicyV1)
	_, s2, _ := scanSelectedUnder(root, nil, nil, selectionPolicyV2)
	if s1.Digest == "" || s1.Digest == s2.Digest {
		t.Error("v1 and v2 selections digest the same: the policy is not bound")
	}
	// an explicit --allow still includes a tree the user wants
	allowed, err := buildSelectionUnder(root, []string{"node_modules"}, selectionPolicyV2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(paths(allowed), ","), "node_modules/leftpad/index.js") {
		t.Errorf("--allow node_modules did not include it: %v", paths(allowed))
	}
	if _, err := buildSelectionUnder(root, nil, "ks-upload-policy/9"); err == nil || !strings.Contains(err.Error(), "not one this client knows") {
		t.Errorf("an unknown policy: %v", err)
	}
}

// A v1 approval still verifies at run: the selection is rebuilt under v1,
// its digest matches, and the job is sent. The same lock claiming v2 does
// not verify.
func TestAV1ApprovalStillVerifiesAtRun(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := approvedDemo(t, f, bin, cfg) // no dependency trees: v1 and v2 pack the same files
	m, err := readDraftAt(repo)
	if err != nil {
		t.Fatal(err)
	}
	_, v1, err := scanSelectedUnder(repo, nil, readSelectionOverrides(repo), selectionPolicyV1)
	if err != nil || v1.Digest == "" {
		t.Fatalf("v1 selection: %v", err)
	}
	// what the pre-v2 client wrote: the draft binding the v1 selection
	// digest, and a lock naming v1
	m["workspace"].(map[string]any)["selection_digest"] = v1.Digest
	if err := writeDraft(repo, m); err != nil {
		t.Fatal(err)
	}
	m, _ = readDraftAt(repo)
	sha, err := manifestSHA(m)
	if err != nil {
		t.Fatal(err)
	}
	writeLock := func(policy string) {
		b, _ := json.MarshalIndent(lockFile{SHA256: sha, ApprovedAt: "2026-09-20T00:00:00Z", Version: m["version"], SelectionDigest: v1.Digest, PolicyVersion: policy}, "", "  ")
		if err := os.WriteFile(filepath.Join(repo, cruiseLock), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLock(selectionPolicyV1)
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run")
	if code != 0 || strings.TrimSpace(out) != "job_0123456789ab" {
		t.Fatalf("a v1 approval did not verify: exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	f.hits, f.posted = nil, nil
	f.mu.Unlock()
	writeLock(selectionPolicyV2) // the v1 digest under a v2 claim
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "selection changed since approve") {
		t.Fatalf("a v1 digest passed as v2: exit %d\n%s", code, errs)
	}
	for _, h := range f.seen() {
		if h == "POST /api/jobs" {
			t.Fatal("a refused run sent the job")
		}
	}
}
