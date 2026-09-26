// KS-078 client: an accepted job's result shown with its badge exactly as
// served, its provenance chain and every limitation; a download re-checked
// against the verified candidate tree before anything is written, and never
// written over an existing file; accepted_without_provenance never upgraded.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// treeFiles is a tree whose directory-first order differs from a plain
// path sort ("b" at the root comes before "a/x/f.txt"); the digest below was
// computed by the judge's own tree_digest (judge/manifest.py) over it.
var treeFiles = map[string]string{"b": "B", "a/x/f.txt": "AX", "a/z": "A", "a-b.txt": "AB", "a-b/k": "ABD", "README.md": "R"}

const judgeTreeDigest = "7da84eddd84672eb4a4aeb775e0f10d0713e7c045277a7c384965a1036e29fcd"

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(files[n]))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestTarTreeDigestIsTheVerifiersRule(t *testing.T) {
	got, err := tarTreeDigest(tarOf(t, treeFiles))
	if err != nil || got != judgeTreeDigest {
		t.Fatalf("tree digest %s (%v), the judge's is %s", got, err, judgeTreeDigest)
	}
	// a link cannot be digested as the verifier did: refused, not guessed
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "l", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	_ = gz.Close()
	if _, err := tarTreeDigest(buf.Bytes()); err == nil {
		t.Error("a symlink member was digested")
	}
}

type acceptedCtl struct {
	mu       sync.Mutex
	requests []string
	artifact []byte
	badge    string
	cand     string
	noStatus bool
}

func (c *acceptedCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	art, badge, cand := append([]byte(nil), c.artifact...), c.badge, c.cand
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	sum := sha256.Sum256(art)
	sha := hex.EncodeToString(sum[:])
	if badge == "tampered-record" {
		sha = strings.Repeat("0", 64)
		badge = "verified"
	}
	switch r.URL.Path {
	case "/api/jobs/job_a":
		_ = enc.Encode(map[string]any{"id": "job_a", "state": "accepted", "verdict": "accepted", "artifact_sha": sha})
	case "/api/jobs/job_a/status":
		if c.noStatus {
			http.NotFound(w, r)
			return
		}
		res := map[string]any{"artifact_sha256": sha, "artifact_bytes": len(art), "download": "GET /api/jobs/job_a/artifact", "badge": badge,
			"manifest_sha": strings.Repeat("d", 64), "manifest_version": 2,
			"limitations": []string{
				"the verification covers these exact bytes (sha256 " + sha[:12] + "); a later edit, or any other archive, carries no verification",
				"the checks ran inside a test runner the candidate's own code could influence; a report forged from inside it cannot be ruled out",
			}}
		if badge == "verified" {
			res["provenance"] = map[string]any{"attempt_id": "att_2", "rung": 1, "model": "claude-sonnet-5",
				"candidate_digest": cand, "base_digest": strings.Repeat("b", 64), "receipt_sig": strings.Repeat("c", 64),
				"manifest_sha256": strings.Repeat("d", 64),
				"verifier": map[string]any{"implementation": "judge/verifier.py", "tests_digest": strings.Repeat("e", 64), "config_hash": strings.Repeat("f", 64),
					"pinned_inputs_digest": strings.Repeat("1", 64), "environment_digest": strings.Repeat("2", 64),
					"isolation": map[string]any{"enforced": true, "identity": "ks-verify"}},
				"checks": map[string]any{"outcome": "passed", "counts": map[string]any{"collected": 3, "passed": 3, "failed": 0, "skipped": 0}, "runs": 2}}
		} else {
			res["limitations"] = append(res["limitations"].([]string), "no provenance was recorded for this artifact, so it is not shown as verified")
		}
		_ = enc.Encode(map[string]any{"job_id": "job_a", "state": "accepted", "state_label": "Accepted: the checks passed", "result": res,
			"spend": map[string]any{}, "follow": map[string]any{"cursor": 9, "poll_after_s": 0}})
	case "/api/jobs/job_a/artifact":
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(art)
	default:
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func resultFixture(t *testing.T, c *acceptedCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func TestCruiseResultShowsTheBadgeTheChainAndEveryLimitation(t *testing.T) {
	c := &acceptedCtl{artifact: tarOf(t, treeFiles), badge: "verified", cand: judgeTreeDigest}
	bin, cfg := resultFixture(t, c)
	out := runOK(t, bin, cfg, "cruise", "result", "job_a")
	for _, want := range []string{
		"badge          verified: the checks passed on exactly these bytes",
		"attempt          att_2 (rung 1, claude-sonnet-5)",
		"candidate tree   " + judgeTreeDigest,
		"base tree        " + strings.Repeat("b", 64),
		"receipt sig      " + strings.Repeat("c", 64),
		"manifest sha256  " + strings.Repeat("d", 64),
		"verifier         judge/verifier.py",
		"isolation      enforced (identity ks-verify)",
		"checks           passed: collected 3, passed 3, failed 0, skipped 0; runs 2",
		"- the verification covers these exact bytes",
		"- the checks ran inside a test runner the candidate's own code could influence",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "downloaded") {
		t.Error("reading the result downloaded something")
	}
}

func TestADownloadIsReverifiedAgainstTheCandidateAndNeverOverwrites(t *testing.T) {
	c := &acceptedCtl{artifact: tarOf(t, treeFiles), badge: "verified", cand: judgeTreeDigest}
	bin, cfg := resultFixture(t, c)
	dir := t.TempDir()
	dest := filepath.Join(dir, "r.tar.gz")
	out := runOK(t, bin, cfg, "cruise", "result", "job_a", "--download", "--out", dest)
	if !strings.Contains(out, "its tree matches the verified candidate; verified") {
		t.Errorf("download line:\n%s", out)
	}
	if b, err := os.ReadFile(dest); err != nil || !bytes.Equal(b, c.artifact) {
		t.Fatalf("written bytes differ: %v", err)
	}
	// never over an existing file, by either verb
	if err := os.WriteFile(dest, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cruise", "result", "job_a", "--download", "--out", dest}, {"cruise", "artifact", "job_a", "--out", dest}} {
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != exitConflict || !strings.Contains(errs, "never written over") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
		if b, _ := os.ReadFile(dest); string(b) != "mine" {
			t.Fatalf("%v wrote over an existing file", args)
		}
	}
	// the bytes still match the recorded sha256 but hold a different tree
	// from the candidate the receipt verified: refused, nothing written
	c.mu.Lock()
	c.cand = strings.Repeat("9", 64)
	c.mu.Unlock()
	other := filepath.Join(dir, "other.tar.gz")
	for _, args := range [][]string{{"cruise", "result", "job_a", "--download", "--out", other}, {"cruise", "artifact", "job_a", "--out", other}} {
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != exitIntegrity || !strings.Contains(errs, "not the candidate") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
		if _, err := os.Stat(other); !os.IsNotExist(err) {
			t.Errorf("%v wrote a mismatched candidate", args)
		}
	}
	// the bytes do not match the recorded sha256: refused
	c.mu.Lock()
	c.cand, c.badge = judgeTreeDigest, "tampered-record"
	c.mu.Unlock()
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "result", "job_a", "--download", "--out", other)
	if code != exitIntegrity || !strings.Contains(errs, "does not match the job's record") {
		t.Errorf("sha mismatch: exit %d\n%s", code, errs)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ks-artifact-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// QA-078-2 in the client: accepted_without_provenance is never shown as
// verified, by the result or by a download.
func TestAcceptedWithoutProvenanceIsNeverUpgraded(t *testing.T) {
	c := &acceptedCtl{artifact: tarOf(t, treeFiles), badge: "accepted_without_provenance"}
	bin, cfg := resultFixture(t, c)
	dir := t.TempDir()
	out := runOK(t, bin, cfg, "cruise", "result", "job_a", "--download", "--out", filepath.Join(dir, "r.tgz"))
	if !strings.Contains(out, "badge          accepted_without_provenance: NOT verified") ||
		!strings.Contains(out, "provenance     none recorded") ||
		!strings.Contains(out, "- no provenance was recorded for this artifact") ||
		!strings.Contains(out, "; NOT verified (accepted_without_provenance") {
		t.Errorf("unverified result:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), "; verified") || strings.Contains(l, "badge          verified") {
			t.Errorf("upgraded to verified: %q", l)
		}
	}
	js := runOK(t, bin, cfg, "cruise", "result", "job_a", "--json")
	if !strings.Contains(js, `"verified":false`) || !strings.Contains(js, `"badge":"accepted_without_provenance"`) {
		t.Errorf("json: %s", js)
	}
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "artifact", "job_a", "--out", filepath.Join(dir, "a.tgz"))
	if code != 0 || !strings.Contains(errs, "badge: accepted_without_provenance: NOT verified") {
		t.Errorf("artifact: exit %d\n%s", code, errs)
	}
	// an unknown badge word is not read as verified either
	c.mu.Lock()
	c.badge = "verified_by_vibes"
	c.mu.Unlock()
	if out := runOK(t, bin, cfg, "cruise", "result", "job_a"); !strings.Contains(out, "a badge this client does not know, so it is NOT read as verified") {
		t.Errorf("unknown badge:\n%s", out)
	}
}

// Against the deployed control plane (no status route) the result verb
// says so; the artifact verb keeps its sha256 check and does not call the
// download verified.
func TestResultOnTheDeployedControlPlane(t *testing.T) {
	c := &acceptedCtl{artifact: tarOf(t, treeFiles), badge: "verified", cand: judgeTreeDigest, noStatus: true}
	bin, cfg := resultFixture(t, c)
	dir := t.TempDir()
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "result", "job_a")
	if code != exitFailed || !strings.Contains(errs, "does not serve a job's result") {
		t.Errorf("result: exit %d\n%s", code, errs)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "artifact", "job_a", "--out", filepath.Join(dir, "a.tgz"))
	if code != 0 || !strings.Contains(errs, "not shown as verified") || strings.Contains(errs, "badge: verified") {
		t.Errorf("artifact: exit %d\n%s", code, errs)
	}
}
