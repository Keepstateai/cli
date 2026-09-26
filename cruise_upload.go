// cruise_upload.go: the workspace as its own upload (KS-001's "workspace
// PUT", served as PUT /api/jobs/{id}/workspace; KS-027 reads the archive on
// this path exactly as on the inline one).
//
// ks cruise run sends the workspace inside the job request as base64 by
// default. With --separate-upload it creates the job WITHOUT the workspace
// and then sends the packed archive's raw bytes on the upload route. The
// job cannot start until its workspace is stored: the control plane claims
// only a queued job whose workspace is recorded.
//
// The archive is KEPT under the config directory from the moment the job
// exists until the upload is confirmed, because a re-pack need not produce
// the same bytes and the manifest the job was created with names this
// archive's sha256 and size. If the upload fails, ks cruise upload JOB sends
// the kept archive again; sending the same bytes again is safe while the
// job is queued. Before anything is sent the archive is checked against the
// job's manifest locally, and a mismatch sends nothing.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// keptArchivePath is where a job's packed workspace waits for its upload.
func keptArchivePath(jobID string) string {
	return filepath.Join(configDir(), "cruise-uploads", filepath.Base(jobID)+".tar.gz")
}

// keepArchive copies the packed archive to the kept path.
func keepArchive(src *os.File, jobID string) (string, error) {
	dst := keptArchivePath(jobID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return "", err
	}
	return dst, f.Close()
}

// manifestWorkspace answers the sha256 and size the job's manifest names.
func manifestWorkspace(job map[string]any) (string, int64, bool) {
	m, _ := job["manifest"].(map[string]any)
	ws, _ := m["workspace"].(map[string]any)
	sha, _ := ws["sha256"].(string)
	n, ok := jnum(ws, "bytes")
	return strings.ToLower(sha), n, sha != "" && ok
}

// checkArchive measures a local archive against the manifest's figures.
func checkArchive(path, wantSHA string, wantBytes int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if n != wantBytes || got != wantSHA {
		return &cliError{Code: exitIntegrity, Kind: "workspace_mismatch",
			Message: fmt.Sprintf("the archive at %s is %d bytes with sha256 %s, and the job's manifest names %d bytes with sha256 %s; nothing was sent",
				path, n, short(got), wantBytes, short(wantSHA)),
			NextAction: "pass the archive the job was created with (--from FILE), or cancel the job: ks cruise cancel <job>"}
	}
	return nil
}

// putWorkspace sends the archive's raw bytes on the upload route.
func putWorkspace(c hostedCreds, jobID, path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	resp, err := hostedDo(c, "PUT", "/api/jobs/"+jobID+"/workspace", "application/octet-stream", f)
	if err != nil {
		return nil, &cliError{Code: exitTemporary, Kind: "upload_uncertain",
			Message:    fmt.Sprintf("the workspace upload for job %s got no answer (%s); whether it was stored is not known. Sending the same bytes again is safe while the job is queued", jobID, sanitize(err.Error())),
			NextAction: "ks cruise upload " + jobID}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("PUT", "/api/jobs/"+jobID+"/workspace", resp, raw)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, &cliError{Code: exitTemporary, Kind: "upload_uncertain",
			Message:    fmt.Sprintf("the control plane answered %s to the workspace upload with a result this client could not read", resp.Status),
			NextAction: "ks cruise status " + jobID}
	}
	return job, nil
}

// uploadKept checks and sends a job's archive, and forgets the kept copy
// once the control plane has recorded it.
func uploadKept(c hostedCreds, job map[string]any, path string, kept bool) (map[string]any, error) {
	id := jstr(job, "id")
	wantSHA, wantBytes, ok := manifestWorkspace(job)
	if !ok {
		return nil, &cliError{Code: exitIntegrity, Kind: "manifest_workspace_missing",
			Message: "the job's manifest names no workspace sha256 and size, so no archive can be checked against it; nothing was sent"}
	}
	if err := checkArchive(path, wantSHA, wantBytes); err != nil {
		return nil, err
	}
	progress("uploading the workspace for job %s: %s bytes, sha256 %s", id, commas(wantBytes), short(wantSHA))
	after, err := putWorkspace(c, id, path)
	if err != nil {
		return nil, err
	}
	if got, _ := after["workspace_sha"].(string); !strings.EqualFold(got, wantSHA) {
		return after, &cliError{Code: exitIntegrity, Kind: "workspace_not_recorded",
			Message:    fmt.Sprintf("the upload was answered but job %s records workspace %q, not %s; the kept archive stays", id, got, short(wantSHA)),
			NextAction: "ks cruise status " + id}
	}
	if kept {
		_ = os.Remove(path)
	}
	return after, nil
}

// cruiseUpload is ks cruise upload JOB [--from FILE].
func cruiseUpload(inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the job whose workspace to upload is required", NextAction: "ks cruise status"})
	}
	c := mustCreds()
	job := fetchJob(c, id)
	state := jstr(job, "state")
	if sha, _ := job["workspace_sha"].(string); sha != "" {
		emit(map[string]any{"job_id": id, "uploaded": false, "already_recorded": true, "workspace_sha": sha}, func() {
			fmt.Printf("job %s already has its workspace (sha256 %s); nothing was sent\n", id, short(sha))
		})
		return
	}
	if state != "queued" {
		fail(&cliError{Code: exitConflict, Kind: "job_state",
			Message:    fmt.Sprintf("job %s is %s; a workspace can be uploaded only while the job is queued, so nothing was sent", id, state),
			NextAction: "ks cruise status " + id})
	}
	path, kept := keptArchivePath(id), true
	if inv.Set("from") {
		path, kept = inv.Str("from"), false
	}
	if _, err := os.Stat(path); err != nil {
		fail(&cliError{Code: exitFailed, Kind: "archive_missing",
			Message:    fmt.Sprintf("no archive at %s; this client does not re-pack, because a re-pack need not produce the bytes job %s was created with. Nothing was sent", path, id),
			NextAction: "ks cruise upload " + id + " --from FILE, or cancel it: ks cruise cancel " + id})
	}
	after, err := uploadKept(c, job, path, kept)
	if err != nil {
		die(err)
	}
	emit(map[string]any{"job_id": id, "uploaded": true, "state": after["state"], "workspace_sha": after["workspace_sha"], "workspace_bytes": after["workspace_bytes"]}, func() {
		fmt.Printf("job %s: workspace stored (sha256 %s); the job can now be claimed (state %s)\n", id, short(jstr(after, "workspace_sha")), jstr(after, "state"))
	})
}

// workspaceLine is printJob's line for the job's workspace.
func workspaceLine(job map[string]any) string {
	if sha, _ := job["workspace_sha"].(string); sha != "" {
		n, _ := jnum(job, "workspace_bytes")
		return fmt.Sprintf("workspace: stored (sha256 %s, %s bytes)", short(sha), commas(n))
	}
	if _, ok := job["workspace_sha"]; !ok {
		return "" // a control plane that does not report it: no line, as before
	}
	if jstr(job, "state") == "queued" {
		return "workspace: NOT uploaded; the job cannot start until it is: ks cruise upload " + jstr(job, "id")
	}
	return "workspace: not recorded"
}
