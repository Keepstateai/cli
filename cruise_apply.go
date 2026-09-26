// cruise_apply.go: applying an accepted Cruise result file by file onto
// this project (KS-078 QA-078-3). The control plane serves the result's
// per-file changeset (GET /api/jobs/{id}/changeset, owner-only) in the same
// ks-changeset.json shape an agent attempt's result carries (KS-057), and
// names its sha256, change count and route in status.result.changeset.
//
// ks cruise apply JOB reads the status, downloads the changeset, and checks
// its bytes BEFORE parsing them: the X-KS-Sha256 header and the sha256 of
// the bytes received must both equal status.result.changeset.sha256. Then
// the existing apply runs unchanged: each file checked against its
// recorded before-state, the whole apply refused on any local difference
// (conflicts are preserved: nothing of yours is overwritten), confirmed by
// the changeset's digest, applied transactionally with a backup and a
// journal (ks result apply --recover puts an interrupted one back).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type liveChangeset struct {
	SHA256   string `json:"sha256"`
	Changes  int    `json:"changes"`
	Download string `json:"download"`
}

// bareHex drops a "sha256:" prefix and lowercases.
func bareHex(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "sha256:"))
}

// fetchJobChangeset downloads the changeset and checks it against the digest
// the status names, before anything reads it.
func fetchJobChangeset(c hostedCreds, id, want string) ([]byte, error) {
	resp, err := hostedDo(c, "GET", "/api/jobs/"+id+"/changeset", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("the changeset download was interrupted: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("GET", "/api/jobs/"+id+"/changeset", resp, raw)
	}
	header := bareHex(resp.Header.Get("X-KS-Sha256"))
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	switch {
	case header == "":
		return nil, integrity("changeset_unsigned", "the changeset arrived without its X-KS-Sha256; it is not read, and nothing was changed")
	case header != want:
		return nil, integrity("changeset_mismatch", fmt.Sprintf("the changeset served names sha256 %s, and the job's result names %s; it is not read, and nothing was changed", short(header), short(want)))
	case got != want:
		return nil, integrity("changeset_mismatch", fmt.Sprintf("the changeset received hashes to %s, not the %s the job's result names; it is not read, and nothing was changed", short(got), short(want)))
	}
	return raw, nil
}

// cruiseApply is ks cruise apply JOB [--dir DIR] [--confirm DIGEST].
func cruiseApply(inv *Invocation) {
	c := mustCreds()
	id := strings.TrimSpace(inv.Arg(0))
	root := inv.Str("dir")
	if root == "" {
		var err error
		if root, err = projectRoot(); err != nil {
			die(err)
		}
	}
	root, _ = filepath.Abs(root)
	refuseIfInterrupted(root)
	st, err := fetchJobStatus(c, id)
	if err != nil {
		if statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve a job's result or its changeset; nothing was read or changed", NextAction: "ks cruise artifact " + id + " (the whole tree, sha256 checked)"})
		}
		die(err)
	}
	if st.Result == nil {
		fail(&cliError{Code: exitConflict, Kind: "no_result", Message: fmt.Sprintf("job %s is %s (%s); only an accepted job has a result to apply, so nothing was changed", id, sanitize(st.StateLabel), sanitize(st.State)), NextAction: "ks cruise status " + id})
	}
	if st.Result.Changeset == nil || st.Result.Changeset.SHA256 == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_changeset", Message: fmt.Sprintf("job %s's result was published without a per-file changeset, so it cannot be applied file by file; nothing was changed", id),
			NextAction: "ks cruise result " + id + " --download (the whole verified tree)"})
	}
	want := bareHex(st.Result.Changeset.SHA256)
	progress("job %s: result badge %s; changeset sha256 %s, %d change(s)", id, badgeLine(st.Result.Badge), short(want), st.Result.Changeset.Changes)
	raw, err := fetchJobChangeset(c, id, want)
	if err != nil {
		die(err)
	}
	cs, err := parseChangeset(raw)
	if err != nil {
		fail(integrity("changeset_refused", err.Error()))
	}
	if len(cs.Changes) != st.Result.Changeset.Changes {
		fail(integrity("changeset_refused", fmt.Sprintf("the changeset holds %d change(s), and the job's result names %d; nothing was changed", len(cs.Changes), st.Result.Changeset.Changes)))
	}
	confirmAndApply(root, id+"/changeset", want, cs, inv, "ks cruise apply "+id)
}
