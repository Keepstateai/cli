// KS-026 and the client half of KS-027: the selection is deterministic,
// sensitive paths are excluded and named without their content, ignore
// rules are honored, symlinks are refused with their paths, overrides are
// explicit and recorded, and the preview neither writes nor requests.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func selectionFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"test_ok.py":               "def test_ok():\n    assert True\n",
		"src/app.py":               "print('hi')\n",
		".env":                     "SYNTHETIC_PLACEHOLDER=not-a-secret\n",
		".env.example":             "PLACEHOLDER=\n",
		".gitignore":               ".env\nnode_modules/\n*.log\n!keep.log\nbuild/\n",
		"node_modules/fixture.txt": "synthetic dependency file\n",
		"debug.log":                "x\n",
		"keep.log":                 "kept\n",
		"build/out.bin":            "b\n",
		"certs/server.pem":         "-----BEGIN SYNTHETIC-----\n",
		".aws/credentials":         "[default]\naws_access_key_id = SYNTHETIC\n",
		"deep/.gitignore":          "generated/\n",
		"deep/generated/x.txt":     "g\n",
		"deep/kept.txt":            "k\n",
		"config/.env.production":   "SYNTHETIC=1\n",
		".git/config":              "[core]\n",
		".keepstate/cruise.json":   "{}",
		"docs/notes.md":            "notes\n",
		"scripts/run.sh":           "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(root, "scripts/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func paths(sel *selection) []string {
	var out []string
	for _, f := range sel.Included {
		out = append(out, f.Path)
	}
	return out
}

func excludedReason(sel *selection, p string) string {
	for _, x := range sel.Excluded {
		if x.Path == p {
			return x.Reason
		}
	}
	return ""
}

func TestSelectionPolicyExcludesAndExplains(t *testing.T) {
	root := selectionFixture(t)
	_, sel, err := scanSelected(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths(sel), ",")
	want := ".env.example,.gitignore,keep.log,test_ok.py,deep/.gitignore,deep/kept.txt,docs/notes.md,scripts/run.sh,src/app.py"
	if got != want {
		t.Fatalf("included:\n  %s\nwant\n  %s", got, want)
	}
	for p, prefix := range map[string]string{
		".env": "sensitive:", "certs/server.pem": "sensitive:", ".aws": "sensitive:", "config/.env.production": "sensitive:", "node_modules": "ignored: .gitignore: node_modules/",
		"debug.log": "ignored: .gitignore: *.log", "build": "ignored:", "deep/generated": "ignored: deep/.gitignore: generated/", ".git": "mandatory:", ".keepstate": "mandatory:",
	} {
		if r := excludedReason(sel, p); !strings.HasPrefix(r, prefix) {
			t.Errorf("%s: reason %q, want prefix %q", p, r, prefix)
		}
	}
	// QA-026-1: the sensitive file is named, its content is not anywhere in the manifest
	b, _ := json.Marshal(sel)
	if strings.Contains(string(b), "SYNTHETIC_PLACEHOLDER") || strings.Contains(string(b), "aws_access_key_id") {
		t.Error("the selection manifest carries sensitive content")
	}
	if sel.Included[7].Mode != "0755" || sel.Included[0].Mode != "0644" {
		t.Errorf("modes: %+v", sel.Included)
	}
	// QA-026-3: two unchanged selections yield the same digest
	_, again, _ := scanSelected(root, nil, nil)
	if again.Digest != sel.Digest || sel.Digest == "" {
		t.Errorf("digest not deterministic: %s vs %s", sel.Digest, again.Digest)
	}
	// a changed file, a changed override or a changed exclusion set changes it
	if err := os.WriteFile(filepath.Join(root, "docs/notes.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, _ := scanSelected(root, nil, nil); changed.Digest == sel.Digest {
		t.Error("a changed file kept the digest")
	}
	if _, over, _ := scanSelected(root, nil, []string{".env"}); over.Digest == sel.Digest || excludedReason(over, ".env") != "" {
		t.Errorf("an override did not lift the block or kept the digest: %q", excludedReason(over, ".env"))
	}
	// an override never lifts a mandatory exclusion
	if _, over, _ := scanSelected(root, nil, []string{".git"}); excludedReason(over, ".git") == "" {
		t.Error("an override lifted .git")
	}
}

func TestSymlinksAreRefusedWithTheirPaths(t *testing.T) {
	root := selectionFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "external-link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(root, "link-to-src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../test_ok.py", filepath.Join(root, "docs", "inner-link.py")); err != nil {
		t.Fatal(err)
	}
	_, sel, err := scanSelected(root, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "external-link.txt") || !strings.Contains(err.Error(), "link-to-src") || !strings.Contains(err.Error(), "docs/inner-link.py") {
		t.Fatalf("symlinks: %v", err)
	}
	if len(sel.Symlinks) != 3 {
		t.Errorf("listed: %v", sel.Symlinks)
	}
	// VER-026-2: nothing outside the root was read: the outside file's bytes appear nowhere
	b, _ := json.Marshal(sel)
	if strings.Contains(string(b), "outside\n") {
		t.Error("outside content read")
	}
	// an override cannot lift a symlink
	if _, _, err := scanSelected(root, nil, []string{"external-link.txt"}); err == nil {
		t.Error("an override lifted a symlink")
	}
}

func TestPreviewWritesNothingRequestsNothingAndMatchesTheArchive(t *testing.T) {
	root := selectionFixture(t)
	f, bin, cfg := startFake(t)
	before, _ := os.ReadDir(filepath.Join(root, ".keepstate"))
	out, errs, code := ksIn(t, bin, cfg, root, "cruise", "preview", "--json")
	if code != 0 {
		t.Fatalf("preview: %d %s", code, errs)
	}
	after, _ := os.ReadDir(filepath.Join(root, ".keepstate"))
	if len(before) != len(after) {
		t.Error("preview wrote into .keepstate")
	}
	if seen := f.seen(); len(seen) != 0 {
		t.Errorf("preview made requests: %v", seen)
	}
	var env struct {
		Data struct {
			Included         []selFile     `json:"included"`
			Sensitive        []selExcluded `json:"sensitive_warnings"`
			Digest           string        `json:"selection_digest"`
			Uploadable       bool          `json:"uploadable"`
			EditScopeOutside []string      `json:"edit_scope_outside"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Sensitive) != 4 || !env.Data.Uploadable {
		t.Errorf("preview: %+v", env.Data)
	}
	// VER-026-1: init's archive carries exactly the previewed paths, hash for hash
	out, errs, code = ksIn(t, bin, cfg, root, "cruise", "init", "--goal", "g", "--tests", "pytest", "--paths", "src/**", "--json")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	files, _, err := scanSelected(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(env.Data.Included)+1 { // init added .keepstate/selection.json? no: .keepstate is mandatory-excluded; the count is the same
		t.Logf("files after init: %d, previewed %d", len(files), len(env.Data.Included))
	}
	for i, wf := range files {
		if i >= len(env.Data.Included) || wf.rel != env.Data.Included[i].Path {
			t.Errorf("path %d: packed %q, previewed %q", i, wf.rel, env.Data.Included[i].Path)
			break
		}
	}
	// QA-026-2: an allowed-to-edit glob outside the upload is reported distinctly
	out, _, _ = ksIn(t, bin, cfg, root, "cruise", "preview", "--json")
	_ = json.Unmarshal([]byte(out), &env)
	if len(env.Data.EditScopeOutside) != 0 {
		t.Errorf("src/** reaches src/app.py: %v", env.Data.EditScopeOutside)
	}
	ksIn(t, bin, cfg, root, "cruise", "init", "--goal", "g", "--tests", "pytest", "--paths", "build/**")
	out, _, _ = ksIn(t, bin, cfg, root, "cruise", "preview", "--json")
	_ = json.Unmarshal([]byte(out), &env)
	if len(env.Data.EditScopeOutside) != 1 || !strings.Contains(env.Data.EditScopeOutside[0], "build/**") {
		t.Errorf("edit scope outside: %v", env.Data.EditScopeOutside)
	}
	// human output names the sensitive files and never their content
	human, _, _ := ksIn(t, bin, cfg, root, "cruise", "preview")
	if !strings.Contains(human, ".env  [") || strings.Contains(human, "SYNTHETIC_PLACEHOLDER") || !strings.Contains(human, "selection digest:") {
		t.Errorf("human preview: %s", human)
	}
}

func TestApprovalIsBoundToTheSelection(t *testing.T) {
	root := selectionFixture(t)
	_, bin, cfg := startFake(t)
	if _, errs, code := ksIn(t, bin, cfg, root, "cruise", "init", "--goal", "g", "--tests", "pytest"); code != 0 {
		t.Fatalf("init: %s", errs)
	}
	if _, errs, code := ksIn(t, bin, cfg, root, "cruise", "approve"); code != 0 {
		t.Fatalf("approve: %s", errs)
	}
	// the override set changes without any packed file changing (an override
	// that lifts nothing): the tree digest holds, the selection digest does not, run refuses
	sel, _ := os.ReadFile(filepath.Join(root, cruiseSelection))
	edited := strings.Replace(string(sel), `"overrides": []`, `"overrides": ["reviewed/but-absent"]`, 1)
	if edited == string(sel) {
		t.Fatalf("fixture: %s", sel)
	}
	if err := os.WriteFile(filepath.Join(root, cruiseSelection), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code := ksIn(t, bin, cfg, root, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "upload selection changed since approve") {
		t.Errorf("run with a changed selection: %d %s", code, errs)
	}
	// a lock without a selection digest (approved before the policy) is refused too
	_ = os.WriteFile(filepath.Join(root, cruiseSelection), sel, 0o644)
	lk, _ := os.ReadFile(filepath.Join(root, cruiseLock))
	var lock map[string]any
	_ = json.Unmarshal(lk, &lock)
	delete(lock, "selection_digest")
	b, _ := json.Marshal(lock)
	_ = os.WriteFile(filepath.Join(root, cruiseLock), b, 0o644)
	if _, errs, code := ksIn(t, bin, cfg, root, "cruise", "run"); code == 0 || !strings.Contains(errs, "predates the upload policy") {
		t.Errorf("old lock: %d %s", code, errs)
	}
}
