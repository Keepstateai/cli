// cruise_result.go: an accepted Cruise job's result with its provenance and
// limits (KS-078, the client half). Once a job is accepted,
// GET /api/jobs/{id}/status carries result: the artifact (sha256 and size),
// a badge (verified, or accepted_without_provenance), the provenance chain
// (the passing attempt, the verified candidate and base tree digests, the
// receipt signature, the manifest sha, the verifier and its digests,
// isolation and the checks), and the limitations of the verification.
//
//	ks cruise result JOB             the badge EXACTLY as served, the chain,
//	                                 and every limitation line
//	ks cruise result JOB --download  also downloads the artifact, verified
//	ks cruise result JOB --wait      follows the job until nothing further
//	                                 happens, then prints the receipt once
//
// The receipt adds the changeset, the spend (a figure only where the service
// gives one, never $0 for a missing one) and the pull-request line: a served
// pull_request is shown only as a plain https link; without one the receipt
// says there is none yet and offers ks cruise apply.
//
// THE BADGE IS NEVER UPGRADED. "verified" is printed only when the service
// says verified; accepted_without_provenance, or any word this client does
// not know, reads as not verified.
//
// A download is checked twice before anything is written: its sha256 must
// equal the artifact's, and, for a verified result, the tree it contains
// must digest (the verifier's own tree-digest rule) to the candidate digest
// the signed receipt verified. The file is then linked into place, which
// never replaces an existing file.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type liveResult struct {
	ArtifactSHA     string         `json:"artifact_sha256"`
	ArtifactBytes   int64          `json:"artifact_bytes"`
	Download        string         `json:"download"`
	Badge           string         `json:"badge"`
	Provenance      map[string]any `json:"provenance"`
	ManifestSHA     string         `json:"manifest_sha"`
	ManifestVersion int64          `json:"manifest_version"`
	Limitations     []string       `json:"limitations"`
	// the per-file changeset against the approved workspace (QA-078-3)
	Changeset *liveChangeset `json:"changeset"`
	// the pull request the service opened for this result, once it opens
	// them; absent until then
	PullRequest *livePullRequest `json:"pull_request"`
}

type livePullRequest struct {
	URL    string `json:"url"`
	Number int64  `json:"number"`
	State  string `json:"state"`
}

const badgeVerified = "verified"

// badgeLine prints the badge as served, and what it means.
func badgeLine(badge string) string {
	switch badge {
	case badgeVerified:
		return "verified: the checks passed on exactly these bytes, with the provenance below"
	case "accepted_without_provenance":
		return "accepted_without_provenance: NOT verified; no provenance was recorded for these bytes"
	case "":
		return "not reported: NOT verified"
	}
	return sanitize(badge) + ": a badge this client does not know, so it is NOT read as verified"
}

func pStr(m map[string]any, k string) string {
	if m == nil {
		return "not recorded"
	}
	switch v := m[k].(type) {
	case nil:
		return "not recorded"
	case string:
		if v == "" {
			return "not recorded"
		}
		return sanitize(v)
	default:
		return sanitize(fmt.Sprint(v))
	}
}

func pMap(m map[string]any, k string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[k].(map[string]any)
	return v
}

// provenanceLines is the chain, field by field, then anything else the
// service recorded, so nothing it sent is dropped.
func provenanceLines(p map[string]any) []string {
	if p == nil {
		return []string{"  provenance     none recorded"}
	}
	out := []string{
		"  provenance",
		"    attempt          " + pStr(p, "attempt_id") + " (rung " + pStr(p, "rung") + ", " + pStr(p, "model") + ")",
		"    candidate tree   " + pStr(p, "candidate_digest"),
		"    base tree        " + pStr(p, "base_digest"),
		"    receipt sig      " + pStr(p, "receipt_sig"),
		"    manifest sha256  " + pStr(p, "manifest_sha256"),
	}
	v := pMap(p, "verifier")
	iso := pMap(v, "isolation")
	isoText := "not recorded"
	if iso != nil {
		if iso["enforced"] == true {
			isoText = "enforced (identity " + pStr(iso, "identity") + ")"
		} else {
			isoText = "NOT enforced"
		}
	}
	out = append(out,
		"    verifier         "+pStr(v, "implementation"),
		"      tests          "+pStr(v, "tests_digest"),
		"      config         "+pStr(v, "config_hash"),
		"      pinned inputs  "+pStr(v, "pinned_inputs_digest"),
		"      environment    "+pStr(v, "environment_digest"),
		"      isolation      "+isoText)
	ch := pMap(p, "checks")
	counts := pMap(ch, "counts")
	out = append(out, fmt.Sprintf("    checks           %s: collected %s, passed %s, failed %s, skipped %s; runs %s",
		pStr(ch, "outcome"), pStr(counts, "collected"), pStr(counts, "passed"), pStr(counts, "failed"), pStr(counts, "skipped"), pStr(ch, "runs")))
	known := map[string]bool{"attempt_id": true, "rung": true, "model": true, "candidate_digest": true, "base_digest": true,
		"receipt_sig": true, "manifest_sha256": true, "verifier": true, "checks": true}
	var rest []string
	for k := range p {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, fmt.Sprintf("    %-16s %s", sanitize(k), pStr(p, k)))
	}
	return out
}

func resultLines(jobID string, r *liveResult) []string {
	out := []string{
		"job " + jobID + " result",
		"  badge          " + badgeLine(r.Badge),
		fmt.Sprintf("  artifact       sha256 %s, %s bytes", notRecorded(r.ArtifactSHA), commas(r.ArtifactBytes)),
		fmt.Sprintf("  manifest       sha256 %s, revision %d", notRecorded(r.ManifestSHA), r.ManifestVersion),
	}
	out = append(out, provenanceLines(r.Provenance)...)
	out = append(out, "  limitations")
	if len(r.Limitations) == 0 {
		out = append(out, "    none were stated")
	}
	for _, l := range r.Limitations {
		out = append(out, "    - "+sanitize(l))
	}
	return out
}

// tarTreeDigest digests the tree inside a gzip tar by the verifier's rule
// (judge/manifest.py tree_digest): directories in path order, the files of
// each in name order, each as its path, a NUL, and its content's sha256.
// A member that is not a plain file or a directory cannot be digested the
// same way, so it is refused.
func tarTreeDigest(data []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("the artifact is not a gzip archive: %v", err)
	}
	tr := tar.NewReader(gz)
	type entry struct {
		dir, name string
		sum       [32]byte
	}
	var files []entry
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("the artifact could not be read: %v", err)
		}
		clean, nerr := safeEntryName(h.Name)
		if nerr == nil {
			nerr = archiveNameProblem(h.Name, clean, h.Typeflag == tar.TypeDir)
		}
		if nerr != nil {
			return "", nerr
		}
		name := clean
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return "", fmt.Errorf("the artifact holds %q, which is not a plain file, so its tree cannot be digested as the verifier did", sanitize(name))
		}
		if seen[name] {
			return "", fmt.Errorf("the artifact holds %q twice", sanitize(name))
		}
		seen[name] = true
		total += h.Size
		if h.Size < 0 || total > 1<<30 {
			return "", fmt.Errorf("the artifact expands past 1 GiB")
		}
		hs := sha256.New()
		if _, err := io.Copy(hs, tr); err != nil {
			return "", err
		}
		var e entry
		e.dir, e.name = path.Dir(name), path.Base(name)
		if e.dir == "." {
			e.dir = ""
		}
		copy(e.sum[:], hs.Sum(nil))
		files = append(files, e)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].dir != files[j].dir {
			return files[i].dir < files[j].dir
		}
		return files[i].name < files[j].name
	})
	h := sha256.New()
	for _, f := range files {
		rel := f.name
		if f.dir != "" {
			rel = f.dir + "/" + f.name
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(f.sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safeDownload fetches the artifact, checks it, and writes it to dest
// without ever replacing an existing file. candidate is the tree digest a
// verified result must contain; empty when there is none to check.
func safeDownload(c hostedCreds, id, dest, wantSHA, candidate string) (int64, error) {
	if _, err := os.Lstat(dest); err == nil {
		return 0, &cliError{Code: exitConflict, Kind: "file_exists", Message: dest + " exists and is never written over; nothing was downloaded", NextAction: "choose another name with --out"}
	}
	resp, err := hostedDo(c, "GET", inContext(c, "/api/jobs/"+id+"/artifact"), "", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, hostedError("GET", "/api/jobs/"+id+"/artifact", resp, raw)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		return 0, fmt.Errorf("download interrupted: %w", err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantSHA) {
		return 0, &cliError{Code: exitIntegrity, Kind: "artifact_mismatch", Message: fmt.Sprintf("artifact refused: sha256 %s does not match the job's record %s; nothing written", short(got), short(wantSHA))}
	}
	if candidate != "" {
		tree, terr := tarTreeDigest(data)
		if terr != nil {
			return 0, &cliError{Code: exitIntegrity, Kind: "candidate_unverifiable", Message: "the verified candidate could not be re-checked (" + sanitize(terr.Error()) + "); nothing written"}
		}
		if !strings.EqualFold(tree, candidate) {
			return 0, &cliError{Code: exitIntegrity, Kind: "candidate_mismatch", Message: fmt.Sprintf("artifact refused: the tree it holds digests to %s, not the candidate %s the signed receipt verified; nothing written", short(tree), short(candidate))}
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ks-artifact-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// a link fails if dest appeared meanwhile: an existing file is never replaced
	if err := os.Link(tmp.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, &cliError{Code: exitConflict, Kind: "file_exists", Message: dest + " appeared during the download and is never written over; nothing was written", NextAction: "choose another name with --out"}
		}
		return 0, err
	}
	return int64(len(data)), nil
}

var prStateWord = regexp.MustCompile(`^[a-z_]{1,32}$`)

// safePullRequestURL is the served link when it is a plain https address:
// printable ASCII with no spaces, an https scheme, a host and no user part.
// Anything else (javascript:, http:, an escape sequence) is not shown.
func safePullRequestURL(raw string) (string, bool) {
	if raw == "" || len(raw) > 2048 {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7e {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	return raw, true
}

// pullRequestLine is the receipt's pull-request line.
func pullRequestLine(id string, pr *livePullRequest) string {
	if pr == nil {
		return "Pull request: none yet. KeepState opens pull requests once GitHub is connected. Get the change now: ks cruise apply " + id
	}
	head := "Pull request"
	if pr.Number > 0 {
		head = fmt.Sprintf("Pull request #%d", pr.Number)
	}
	link, ok := safePullRequestURL(pr.URL)
	if !ok {
		return head + ": the service named a link that is not a plain https address, so it is not shown"
	}
	state := "state not reported"
	if prStateWord.MatchString(pr.State) {
		state = pr.State
	}
	return fmt.Sprintf("%s: %s (%s)", head, link, state)
}

// pullRequestDoc is the pull request for --json: the served one when its
// link is safe, otherwise null.
func pullRequestDoc(pr *livePullRequest) any {
	if pr == nil {
		return nil
	}
	link, ok := safePullRequestURL(pr.URL)
	if !ok {
		return nil
	}
	state := ""
	if prStateWord.MatchString(pr.State) {
		state = pr.State
	}
	return map[string]any{"url": link, "number": pr.Number, "state": state}
}

// jobStatusWords is a job's terminal status (state, blocked kind, message)
// in the words a status report carries. An unknown job state reports
// nothing rather than a guess.
func jobStatusWords(st jobLiveStatus) (state, kind, msg string) {
	attempt := func(word string) string {
		n := st.Attempts
		if n < 1 {
			n = 1
		}
		if a := st.CurrentAttempt; a != nil && a.Model != "" {
			return fmt.Sprintf("Attempt %d (%s): %s.", n, sanitize(a.Model), word)
		}
		return fmt.Sprintf("Attempt %d: %s.", n, word)
	}
	switch st.State {
	case "queued":
		return "working", "", "Queued."
	case "running", "verifying", "escalating":
		return "working", "", attempt(st.State)
	case "review":
		return "blocked", "question", "Review the result."
	case "accepted":
		if st.Result != nil && st.Result.Badge == badgeVerified {
			return "done", "", "Accepted: verified."
		}
		return "done", "", "Accepted without provenance."
	case "failed":
		v := "The job failed."
		if st.Verdict != nil && strings.TrimSpace(*st.Verdict) != "" {
			v = sanitize(*st.Verdict)
		}
		return "error", "", v
	case "cancelled":
		return "idle", "", "Cancelled."
	}
	return "", "", ""
}

// jobEnded: nothing further happens to the job until someone decides.
func jobEnded(state string) bool {
	switch state {
	case "accepted", "review", "cancelled", "failed":
		return true
	}
	return false
}

// spendLines is the job's spend, each figure only where the service gave one.
func spendLines(st jobLiveStatus) []string {
	m := func(w watchMoney) string { return strings.ReplaceAll(moneyText(w), " \u2014 ", "; ") }
	return []string{
		"  model spend    " + m(st.Spend.Model),
		"  validation     " + m(st.Spend.Validation),
		"  runtime        " + m(st.Spend.Runtime),
		"  storage        " + m(st.Spend.Storage),
	}
}

func jobLabel(st jobLiveStatus) string {
	if st.StateLabel == "" {
		return "Status unavailable"
	}
	return sanitize(st.StateLabel)
}

// jobReceiptLines is an accepted job's receipt: the result, the changeset,
// the spend, the pull-request line and how to get the files.
func jobReceiptLines(id string, st jobLiveStatus, downloaded bool) []string {
	r := st.Result
	out := []string{"receipt for job " + id + ": " + jobLabel(st)}
	out = append(out, resultLines(id, r)[1:]...)
	if cs := r.Changeset; cs != nil {
		out = append(out, fmt.Sprintf("  changeset      %d file change(s), sha256 %s", cs.Changes, notRecorded(bareHex(cs.SHA256))))
	} else {
		out = append(out, "  changeset      not served for this result")
	}
	out = append(out, spendLines(st)...)
	out = append(out, pullRequestLine(id, r.PullRequest))
	if !downloaded {
		out = append(out, "Download the files: ks cruise result "+id+" --download")
	}
	return out
}

// endedCard is the card of a job that ended without a result, and the one
// refusal that goes with it.
func endedCard(id string, st jobLiveStatus) ([]string, *cliError) {
	_, _, words := jobStatusWords(st)
	switch st.State {
	case "failed":
		lines := []string{"job " + id + " failed: " + words}
		lines = append(lines, spendLines(st)...)
		lines = append(lines, "Read why: ks cruise logs "+id)
		return lines, &cliError{Code: exitFailed, Kind: "job_failed", Message: "job " + id + " failed, so it has no result: " + words, NextAction: "ks cruise logs " + id}
	case "review":
		lines := []string{"job " + id + " is waiting for your review: " + jobLabel(st)}
		lines = append(lines, spendLines(st)...)
		lines = append(lines, "Review it: ks cruise review "+id)
		return lines, &cliError{Code: exitConflict, Kind: "in_review", Message: "job " + id + " waits for your review; it has no result until you decide", NextAction: "ks cruise review " + id}
	case "cancelled":
		lines := []string{"job " + id + " was cancelled; it has no result"}
		lines = append(lines, spendLines(st)...)
		return lines, &cliError{Code: exitConflict, Kind: "cancelled", Message: "job " + id + " was cancelled, so it has no result", NextAction: "ks cruise status " + id}
	}
	return []string{"job " + id + " is " + jobLabel(st) + "; it has no result"}, &cliError{Code: exitConflict, Kind: "no_result", Message: fmt.Sprintf("job %s is %s (%s) and has no result, so nothing was read or written", id, jobLabel(st), sanitize(st.State)), NextAction: "ks cruise status " + id}
}

// waitForJobEnd reads the job's status every five of the service's seconds
// until nothing further will happen, saying each change once on stderr. A
// read that fails for a passing reason is retried with a doubling backoff;
// Ctrl-C ends only the wait, and nothing is sent.
func waitForJobEnd(c hostedCreds, id string, st jobLiveStatus) (jobLiveStatus, error) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	unit := watchSecond()
	every := 5 * unit
	said := ""
	backoff := time.Duration(0)
	for {
		if backoff == 0 {
			if _, _, words := jobStatusWords(st); words != "" && words != said && !jobEnded(st.State) {
				progress("job %s: %s", id, words)
				said = words
			}
			if jobEnded(st.State) {
				return st, nil
			}
		}
		wait := every
		if backoff > 0 {
			wait = backoff
		}
		select {
		case <-stop:
			return st, &cliError{Code: exitInterrupt, Kind: "interrupted", Message: "stopped waiting; nothing was sent, and job " + id + " keeps going", NextAction: "ks cruise result " + id + " --wait"}
		case <-time.After(wait):
		}
		fresh, err := fetchJobStatus(c, id)
		if err != nil {
			if statusUnsupported(err) || classify(err).Code != exitTemporary {
				return st, err
			}
			if backoff == 0 {
				backoff = every
			}
			backoff *= 2
			if backoff > 60*unit {
				backoff = 60 * unit
			}
			progress("job %s could not be read (%s); trying again in %s", id, sanitize(errText(err)), (backoff / unit * time.Second).String())
			continue
		}
		backoff = 0
		st = fresh
	}
}

// cruiseResult is ks cruise result JOB [--wait] [--download] [--out FILE].
func cruiseResult(inv *Invocation) {
	c := mustCreds()
	id := strings.TrimSpace(inv.Arg(0))
	if inv.Set("out") && !inv.Bool("download") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--out names where --download writes; without --download nothing is written", NextAction: "ks cruise result " + id + " --download --out FILE"})
	}
	st, err := fetchJobStatus(c, id)
	if err == nil && inv.Bool("wait") {
		st, err = waitForJobEnd(c, id, st)
	}
	if err != nil {
		if statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve a job's result with its provenance; nothing was read or written", NextAction: "ks cruise artifact " + id + " (sha256 checked; no provenance shown)"})
		}
		die(err)
	}
	if st.Result == nil {
		if !inv.Bool("wait") {
			next := "ks cruise result " + id + " --wait"
			if jobEnded(st.State) {
				next = "ks cruise status " + id
			}
			fail(&cliError{Code: exitConflict, Kind: "no_result", Message: fmt.Sprintf("job %s is %s (%s); only an accepted job has a result, so nothing was read or written", id, jobLabel(st), sanitize(st.State)), NextAction: next})
		}
		lines, ce := endedCard(id, st)
		if !out.json {
			for _, l := range lines {
				fmt.Println(l)
			}
		}
		fail(ce)
	}
	r := st.Result
	next := []string{"ks cruise apply " + id}
	if !inv.Bool("download") {
		next = append(next, "ks cruise result "+id+" --download")
	}
	doc := map[string]any{"job_id": id, "state": st.State, "result": r, "verified": r.Badge == badgeVerified,
		"pull_request": pullRequestDoc(r.PullRequest), "next": next}
	written, dest := int64(0), ""
	if inv.Bool("download") {
		dest = id + ".tar.gz"
		if inv.Set("out") {
			dest = inv.Str("out")
		}
		candidate := ""
		if r.Badge == badgeVerified {
			candidate, _ = r.Provenance["candidate_digest"].(string)
			if candidate == "" {
				fail(&cliError{Code: exitIntegrity, Kind: "candidate_missing", Message: "the result reads verified but names no candidate digest to re-check the bytes against; nothing written"})
			}
		}
		n, derr := safeDownload(c, id, dest, r.ArtifactSHA, candidate)
		if derr != nil {
			die(derr)
		}
		written = n
		doc["path"], doc["bytes_written"] = dest, n
	}
	emit(doc, func() {
		for _, l := range jobReceiptLines(id, st, dest != "") {
			fmt.Println(l)
		}
		if dest != "" {
			how := "sha256 checked"
			if r.Badge == badgeVerified {
				how += ", and its tree matches the verified candidate"
			}
			label := "verified"
			if r.Badge != badgeVerified {
				label = "NOT verified (" + badgeLine(r.Badge) + ")"
			}
			fmt.Printf("downloaded %s: %s bytes, %s; %s\n", dest, commas(written), how, label)
		}
	})
}
