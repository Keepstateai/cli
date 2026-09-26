// KS-057: a changeset applies only onto its recorded base (QA-057-1), a
// path outside the workspace refuses the whole of it (QA-057-2), and an
// interrupted apply is recovered to the pre-apply state (QA-057-3); paths
// are shown escaped.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileState(b []byte, mode string) *csFileState {
	return &csFileState{Kind: "file", SHA256: digestOf(b), Bytes: int64(len(b)), Mode: mode}
}

// a project with two files, and a changeset made against exactly them
func changesetFixture(t *testing.T) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, "a.txt"), "old a\n")
	_ = os.Chmod(filepath.Join(root, "a.txt"), 0o644)
	writeFileT(t, filepath.Join(root, "gone.txt"), "to delete\n")
	_ = os.Chmod(filepath.Join(root, "gone.txt"), 0o644)
	newA, added := []byte("new a\n"), []byte("brand new\n")
	cs := map[string]any{"kind": "keepstate.changeset", "version": 1, "task_id": "tsk_1", "attempt_id": "att_1",
		"base": map[string]any{"known": true, "digest": "sha256:" + strings.Repeat("a", 64)},
		"changes": []any{
			map[string]any{"path": "a.txt", "op": "modify", "before": fileState([]byte("old a\n"), "0644"), "after": fileState(newA, "0644"), "content_b64": base64.StdEncoding.EncodeToString(newA)},
			map[string]any{"path": "sub/new \x1b[31m.txt", "op": "add", "after": fileState(added, "0644"), "content_b64": base64.StdEncoding.EncodeToString(added)},
			map[string]any{"path": "gone.txt", "op": "delete", "before": fileState([]byte("to delete\n"), "0644")},
		}}
	raw, _ := json.Marshal(cs)
	return root, raw
}

func TestAChangesetAppliesOnlyOntoItsBaseAndRecovers(t *testing.T) {
	root, raw := changesetFixture(t)
	cs, err := parseChangeset(raw)
	if err != nil {
		t.Fatal(err)
	}
	// QA-057-1: a local edit since the base: conflict, nothing changed
	writeFileT(t, filepath.Join(root, "a.txt"), "my edit\n")
	refusals, conflicts := planApply(root, cs)
	if len(refusals) != 0 || len(conflicts) != 1 || !strings.Contains(conflicts[0], "a.txt") {
		t.Fatalf("conflict: %v %v", refusals, conflicts)
	}
	writeFileT(t, filepath.Join(root, "a.txt"), "old a\n")
	_ = os.Chmod(filepath.Join(root, "a.txt"), 0o644)
	if r, c := planApply(root, cs); len(r)+len(c) != 0 {
		t.Fatalf("clean base: %v %v", r, c)
	}

	// QA-057-3: interrupted after the first replacement, then recovered
	applyFault = func(done int) error {
		if done == 1 {
			return errors.New("power cut")
		}
		return nil
	}
	err = applyChangeset(root, "res_1", cs)
	applyFault = nil
	if err == nil {
		t.Fatal("the fault did not interrupt")
	}
	if readT(t, filepath.Join(root, "a.txt")) != "new a\n" {
		t.Fatal("the first step did not happen before the fault")
	}
	j, _, _ := readJournal(root)
	if j == nil || j.State != "applying" {
		t.Fatalf("journal: %+v", j)
	}
	if _, err := recoverApply(root); err != nil {
		t.Fatal(err)
	}
	if readT(t, filepath.Join(root, "a.txt")) != "old a\n" || readT(t, filepath.Join(root, "gone.txt")) != "to delete\n" {
		t.Fatal("recovery did not restore the pre-apply state")
	}
	if _, err := os.Lstat(filepath.Join(root, "sub", "new \x1b[31m.txt")); err == nil {
		t.Fatal("recovery left an added file")
	}

	// a whole apply, verified against the after-states
	if err := applyChangeset(root, "res_1", cs); err != nil {
		t.Fatal(err)
	}
	if readT(t, filepath.Join(root, "a.txt")) != "new a\n" || readT(t, filepath.Join(root, "sub", "new \x1b[31m.txt")) != "brand new\n" {
		t.Fatal("apply")
	}
	if _, err := os.Lstat(filepath.Join(root, "gone.txt")); err == nil {
		t.Fatal("the deletion did not happen")
	}
	if b, err := os.ReadFile(filepath.Join(applyDir(root), "backup", "0")); err != nil || string(b) != "old a\n" {
		t.Fatalf("backup: %q %v", b, err)
	}
}

// QA-057-2: a path outside the workspace (or through a link here, or into
// the project's control files) refuses the whole changeset.
func TestAChangesetReachingOutsideIsRefusedWhole(t *testing.T) {
	for _, bad := range []string{"../outside.txt", "/etc/passwd", "a/../../x", `..\x`} {
		cs := map[string]any{"kind": "keepstate.changeset", "version": 1, "base": map[string]any{"known": true, "digest": "sha256:x"},
			"changes": []any{map[string]any{"path": "ok.txt", "op": "delete", "before": fileState([]byte("x"), "0644")}, map[string]any{"path": bad, "op": "delete", "before": fileState([]byte("x"), "0644")}}}
		raw, _ := json.Marshal(cs)
		if _, err := parseChangeset(raw); err == nil || !strings.Contains(err.Error(), "whole changeset is refused") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	cs := &changeset{Kind: "keepstate.changeset", Version: 1}
	cs.Base.Known = true
	cs.Changes = []csChange{{Path: "link/x.txt", Op: "add", After: fileState([]byte("x"), "0644"), ContentB64: base64.StdEncoding.EncodeToString([]byte("x"))},
		{Path: ".git/hooks/pre-commit", Op: "add", After: fileState([]byte("x"), "0755"), ContentB64: base64.StdEncoding.EncodeToString([]byte("x"))}}
	if refusals, _ := planApply(root, cs); len(refusals) != 2 {
		t.Fatalf("link and control paths: %v", refusals)
	}
}

// end to end: diff shows escaped paths; apply refuses a dirty tree and
// changes nothing; a confirmed apply lands; --yes never confirms
func TestResultDiffAndApplyEndToEnd(t *testing.T) {
	root, raw := changesetFixture(t)
	c := newResultCtl(raw)
	c.name = "ks-changeset.json"
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, root, env, "result", "diff", "res_1")
	if code != 0 || strings.Contains(out, "\x1b") || !strings.Contains(out, `new \x1b[31m.txt`) || !strings.Contains(out, "delete") {
		t.Fatalf("diff: %d\n%s%s", code, out, errs)
	}
	want := shaOf(raw)[:12]
	writeFileT(t, filepath.Join(root, "a.txt"), "my edit\n")
	_, errs, code = auditExec(t, bin, cfg, root, env, "result", "apply", "res_1", "--dir", root, "--confirm", want)
	if code != exitConflict || !strings.Contains(errs, "nothing was changed") || readT(t, filepath.Join(root, "a.txt")) != "my edit\n" {
		t.Fatalf("dirty: %d\n%s", code, errs)
	}
	writeFileT(t, filepath.Join(root, "a.txt"), "old a\n")
	if _, errs, code := auditExec(t, bin, cfg, root, env, "result", "apply", "res_1", "--dir", root, "--yes"); code != exitUsage || !strings.Contains(errs, "nothing was changed") {
		t.Fatalf("--yes: %d\n%s", code, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, root, env, "result", "apply", "res_1", "--dir", root, "--confirm", want)
	if code != 0 || readT(t, filepath.Join(root, "a.txt")) != "new a\n" || !strings.Contains(out, "nothing was committed") {
		t.Fatalf("apply: %d\n%s%s", code, out, errs)
	}
}
