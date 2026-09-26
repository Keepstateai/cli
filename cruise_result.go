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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("the artifact could not be read: %v", err)
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "./")
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return "", fmt.Errorf("the artifact holds %q, which is not a plain file, so its tree cannot be digested as the verifier did", name)
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
	resp, err := hostedDo(c, "GET", "/api/jobs/"+id+"/artifact", "", nil)
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

// cruiseResult is ks cruise result JOB [--download] [--out FILE].
func cruiseResult(inv *Invocation) {
	c := mustCreds()
	id := strings.TrimSpace(inv.Arg(0))
	st, err := fetchJobStatus(c, id)
	if err != nil {
		if statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve a job's result with its provenance; nothing was read or written", NextAction: "ks cruise artifact " + id + " (sha256 checked; no provenance shown)"})
		}
		die(err)
	}
	if st.Result == nil {
		fail(&cliError{Code: exitConflict, Kind: "no_result", Message: fmt.Sprintf("job %s is %s (%s); only an accepted job has a result, so nothing was read or written", id, sanitize(st.StateLabel), sanitize(st.State)), NextAction: "ks cruise status " + id})
	}
	r := st.Result
	doc := map[string]any{"job_id": id, "result": r, "verified": r.Badge == badgeVerified}
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
	} else if inv.Set("out") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--out names where --download writes; without --download nothing is written", NextAction: "ks cruise result " + id + " --download --out FILE"})
	}
	emit(doc, func() {
		for _, l := range resultLines(id, r) {
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
