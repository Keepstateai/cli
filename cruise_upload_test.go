// KS-001 "workspace PUT" (served as PUT /api/jobs/{id}/workspace, KS-027's
// second upload path): ks cruise run --separate-upload creates the job
// without its workspace and sends the archive's raw bytes on the upload
// route; a failed upload leaves the archive kept, and ks cruise upload sends
// exactly those bytes again. Every refusal sends nothing it should not.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// approvedDemo is a demo repository with an approved draft.
func approvedDemo(t *testing.T, f *fakeCtl, bin, cfg string) string {
	t.Helper()
	repo := demoRepo(t)
	if out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--goal", "make the inventory tests pass"); code != 0 {
		t.Fatalf("init: exit %d\n%s%s", code, out, errs)
	}
	if out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "approve"); code != 0 {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	f.hits = nil
	f.mu.Unlock()
	return repo
}

func keptPath(cfg string) string {
	return filepath.Join(cfg, "keepstate", "cruise-uploads", "job_0123456789ab.tar.gz")
}

// queuedJobFrom is the job the fake serves after the separate POST: queued,
// no workspace stored, the manifest the client posted.
func queuedJobFrom(t *testing.T, posted []byte) map[string]any {
	t.Helper()
	var p struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal(posted, &p); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(p.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": "job_0123456789ab", "state": "queued", "manifest": m, "workspace_sha": "", "workspace_bytes": 0,
		"goal": "g", "spend_ceiling_microusd": 2000000, "created_at": "2026-09-26T10:00:00Z", "updated_at": "2026-09-26T10:00:00Z"}
}

func TestCruiseRunSeparateUploadSendsTheArchiveOnItsOwnRoute(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := approvedDemo(t, f, bin, cfg)
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run", "--separate-upload")
	if code != 0 {
		t.Fatalf("run --separate-upload: exit %d\n%s%s", code, out, errs)
	}
	got := f.seen()
	if len(got) != 5 || got[3] != "POST /api/jobs" || got[4] != "PUT /api/jobs/job_0123456789ab/workspace" {
		t.Fatalf("requests %v: want the reads, the preflight, one POST without the workspace, then one PUT", got)
	}
	if strings.Contains(string(f.posted), "workspace_b64") {
		t.Fatalf("the job request still carries the workspace: %.200s", f.posted)
	}
	m := queuedJobFrom(t, f.posted)["manifest"].(map[string]any)
	ws := m["workspace"].(map[string]any)
	sum := sha256.Sum256(f.put)
	if ws["sha256"] != hex.EncodeToString(sum[:]) || int(ws["bytes"].(float64)) != len(f.put) {
		t.Fatalf("the uploaded bytes (%d, %x) are not the manifest's workspace %v", len(f.put), sum, ws)
	}
	if _, err := os.Stat(keptPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("the kept archive was not removed after a stored upload: %v", err)
	}
	if strings.TrimSpace(out) != "job_0123456789ab" {
		t.Errorf("stdout %q", out)
	}
}

func TestAFailedUploadKeepsTheArchiveAndUploadSendsTheSameBytes(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := approvedDemo(t, f, bin, cfg)
	f.mu.Lock()
	f.putFault = "mismatch"
	f.mu.Unlock()
	_, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run", "--separate-upload")
	if code != exitConflict || !strings.Contains(errs, "queued WITHOUT its workspace") || !strings.Contains(errs, "ks cruise upload job_0123456789ab") {
		t.Fatalf("refused upload: exit %d\n%s", code, errs)
	}
	first := append([]byte(nil), f.put...)
	if _, err := os.Stat(keptPath(cfg)); err != nil {
		t.Fatalf("the archive was not kept after a refused upload: %v", err)
	}

	// the job reads queued without a workspace, and status says so
	f.mu.Lock()
	f.job, f.putFault = queuedJobFrom(t, f.posted), ""
	f.mu.Unlock()
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "status", "job_0123456789ab")
	if code != 0 || !strings.Contains(out, "workspace: NOT uploaded; the job cannot start until it is: ks cruise upload job_0123456789ab") {
		t.Fatalf("status: exit %d\n%s%s", code, out, errs)
	}

	// ks cruise upload: the kept archive, the same bytes, then forgotten
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab")
	if code != 0 || !strings.Contains(out, "workspace stored") {
		t.Fatalf("upload: exit %d\n%s%s", code, out, errs)
	}
	if string(f.put) != string(first) {
		t.Fatal("the retried upload sent different bytes")
	}
	if _, err := os.Stat(keptPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("the kept archive stays after a stored upload")
	}
}

func TestCruiseUploadRefusalsSendNothing(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := approvedDemo(t, f, bin, cfg)
	f.mu.Lock()
	f.putFault = "mismatch"
	f.mu.Unlock()
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run", "--separate-upload"); code == 0 {
		t.Fatalf("setup: the refused upload exited 0\n%s", errs)
	}
	base := queuedJobFrom(t, f.posted)
	reset := func(job map[string]any, fault string) {
		f.mu.Lock()
		f.job, f.putFault, f.puts, f.hits = job, fault, 0, nil
		f.mu.Unlock()
	}
	copyJob := func(kv ...any) map[string]any {
		j := map[string]any{}
		for k, v := range base {
			j[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			j[kv[i].(string)] = kv[i+1]
		}
		return j
	}

	// the job left queued: refused before any upload (conflict)
	reset(copyJob("state", "running"), "")
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab"); code != exitConflict || f.puts != 0 || !strings.Contains(errs, "only while the job is queued") {
		t.Errorf("not queued: exit %d, puts %d\n%s", code, f.puts, errs)
	}
	// already stored: left alone, nothing sent, success
	reset(copyJob("workspace_sha", strings.Repeat("a", 64)), "")
	if out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab"); code != 0 || f.puts != 0 || !strings.Contains(out, "nothing was sent") {
		t.Errorf("already stored: exit %d, puts %d\n%s%s", code, f.puts, out, errs)
	}
	// digest mismatch: another archive is refused locally (integrity)
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := os.WriteFile(other, []byte("not the approved archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	reset(copyJob(), "")
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab", "--from", other); code != exitIntegrity || f.puts != 0 || !strings.Contains(errs, "nothing was sent") {
		t.Errorf("mismatch: exit %d, puts %d\n%s", code, f.puts, errs)
	}
	// wrong role: the service's 403 is an authorization failure
	reset(copyJob(), "role")
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab"); code != exitAuth || !strings.Contains(errs, "cannot upload") {
		t.Errorf("role: exit %d\n%s", code, errs)
	}
	// the service moved the job out of queued during the upload (stale)
	reset(copyJob(), "state")
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab"); code != exitConflict || !strings.Contains(errs, "only while the job is queued") {
		t.Errorf("stale state: exit %d\n%s", code, errs)
	}
	// no kept archive and no --from: nothing is re-packed or sent
	if err := os.Remove(keptPath(cfg)); err != nil {
		t.Fatal(err)
	}
	reset(copyJob(), "")
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "upload", "job_0123456789ab"); code != exitFailed || f.puts != 0 || !strings.Contains(errs, "does not re-pack") {
		t.Errorf("missing archive: exit %d, puts %d\n%s", code, f.puts, errs)
	}
	// usage: the job is required
	if _, _, code := ksIn(t, bin, cfg, repo, "cruise", "upload"); code != exitUsage {
		t.Errorf("no job: exit %d", code)
	}
}
