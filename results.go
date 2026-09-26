// results.go: retrieving what an agent's task produced (KS-056), without a
// shell and without risking a local file.
//
//	ks result list      the results of a task, or of a session's tasks
//	ks result show      one result's metadata: size, sha256, task, attempt,
//	                    the producer's own checks, the task's independent
//	                    verification state, provenance
//	ks result download  the bytes, verified, finalized without overwrite
//
// A download is a sequence with one rule: the named file appears only when
// its bytes are the recorded ones.
//
//  1. The metadata is read first; the target is checked before anything
//     else is asked of the service: an existing file is refused unless
//     --force, a directory always.
//  2. A download grant is requested (account-scoped, expiring). Its token
//     authorises bytes, so it is held in memory for the one request and is
//     never printed, emitted or journalled.
//  3. The bytes go to a partial file beside the target (never the target),
//     created by this client, never through a link. An interrupted transfer
//     leaves that partial file, named as incomplete, and the same command
//     resumes it with Range and If-Range bound to the recorded sha256: a
//     service that no longer holds those exact bytes answers the whole
//     result and the partial file is started again.
//  4. The whole file is hashed from disk and compared, with its size, to the
//     recorded values (and to the digest the service states on the
//     response). A mismatch removes the partial file; nothing is written at
//     the target, and every file that was there before is untouched.
//  5. The file is finalized atomically: without --force by a hard link that
//     fails if anything appeared at the target in the meantime (the race is
//     refused, the newcomer untouched), with --force by a rename over it.
//  6. Success is printed only after that.
//
// Nothing is extracted or run by default. --extract DIR unpacks a tar,
// tar.gz or zip result into a NEW directory under the C09 rules: every
// entry is checked before a single byte is written -- absolute paths,
// traversal, links of either kind, devices, pipes, duplicate paths, a file
// where a directory must be, and the expanded size and count bounds -- and
// an archive that fails any check extracts nothing.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type resultCheck struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type resultRow struct {
	ID               string        `json:"id"`
	SessionID        string        `json:"session_id"`
	AgentID          string        `json:"agent_id"`
	TaskID           string        `json:"task_id"`
	AttemptID        string        `json:"attempt_id"`
	Name             string        `json:"name"`
	MediaType        string        `json:"media_type"`
	Bytes            int64         `json:"bytes"`
	SHA256           string        `json:"sha256"`
	Checks           []resultCheck `json:"checks"`
	ChecksDeclaredBy string        `json:"checks_declared_by"`
	TaskVerification string        `json:"task_verification_state"`
	Provenance       struct {
		WorkerID       string `json:"worker_id"`
		ExecutionEpoch int64  `json:"execution_epoch"`
		AttemptIndex   int64  `json:"attempt_index"`
		RunnerVersion  string `json:"runner_version"`
		RecordedAt     string `json:"recorded_at"`
	} `json:"provenance"`
	State     string `json:"state"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
}

// C04/C09 bounds for an extraction.
const (
	extractMaxBytes = 50 << 20
	extractMaxFiles = 10000
)

var (
	sha256Hex64    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	resultFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ---- reading ----------------------------------------------------------------

func fetchResult(cr hostedCreds, id string) (resultRow, error) {
	var env struct {
		Data resultRow `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/results/"+url.PathEscape(id), nil, &env); err != nil {
		return resultRow{}, err
	}
	return env.Data, nil
}

// fetchTaskResults reads a task's results. The service answers the list as
// the data array itself; a paged {items} answer is read too.
func fetchTaskResults(cr hostedCreds, taskID string) ([]resultRow, error) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/tasks/"+url.PathEscape(taskID)+"/results", nil, &env); err != nil {
		return nil, err
	}
	var rows []resultRow
	if err := json.Unmarshal(env.Data, &rows); err == nil {
		return rows, nil
	}
	var paged struct {
		Items []resultRow `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &paged); err != nil {
		return nil, fmt.Errorf("the results of task %s could not be read: %v", taskID, err)
	}
	return paged.Items, nil
}

func checksLine(r resultRow) string {
	if len(r.Checks) == 0 {
		return "none declared"
	}
	var parts []string
	for _, c := range r.Checks {
		parts = append(parts, sanitize(c.Name)+" "+sanitize(c.State))
	}
	return strings.Join(parts, ", ") + " (declared by the producer, not verified)"
}

func verificationWord(r resultRow) string {
	if r.TaskVerification == "" {
		return "not recorded"
	}
	return r.TaskVerification
}

func resultLine(r resultRow) string {
	return fmt.Sprintf("%-20s %-28s %12s  sha256 %s  task %s  verification %s", r.ID, clip(sanitize(r.Name), 28), commas(r.Bytes)+" B", short(r.SHA256), r.TaskID, verificationWord(r))
}

func hostedResultList(cr hostedCreds, inv *Invocation) {
	var rows []resultRow
	var unreadable []string
	scope := ""
	if t := inv.Str("task"); t != "" {
		if inv.Set("session") || inv.Set("agent") {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--task names one instruction; it goes without --session and --agent"})
		}
		rs, err := fetchTaskResults(cr, t)
		if err != nil {
			die(err)
		}
		rows, scope = rs, "task "+t
	} else {
		sess := agentSession(cr, inv)
		agents, err := fetchAgents(cr, agentSessionID(sess))
		if err != nil {
			die(err)
		}
		if n := inv.Str("agent"); n != "" {
			a, err := pickAgent(sess, agents, n)
			if err != nil {
				die(err)
			}
			agents = []agentRow{a}
		}
		scope = "session " + sess.ShortID
		for _, a := range agents {
			tasks, err := fetchTasks(cr, a.ID)
			if err != nil {
				unreadable = append(unreadable, "agent "+a.Name+": "+errText(err))
				continue
			}
			for _, t := range tasks {
				rs, err := fetchTaskResults(cr, t.ID)
				if err != nil {
					unreadable = append(unreadable, "task "+t.ID+": "+errText(err))
					continue
				}
				rows = append(rows, rs...)
			}
		}
	}
	emit(map[string]any{"scope": scope, "results": rows, "count": len(rows), "unreadable": unreadable}, func() {
		if len(rows) == 0 && len(unreadable) == 0 {
			fmt.Printf("no results recorded for %s\n", scope)
			return
		}
		for _, r := range rows {
			fmt.Println(resultLine(r))
		}
		for _, u := range unreadable {
			fmt.Printf("UNREAD  %s (not an empty list: it could not be read)\n", u)
		}
		fmt.Printf("%d result(s) for %s; download one: ks result download <result>\n", len(rows), scope)
	})
	if len(unreadable) > 0 {
		os.Exit(exitFailed)
	}
}

func hostedResultShow(cr hostedCreds, inv *Invocation) {
	r, err := fetchResult(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	emit(r, func() {
		fmt.Printf("result %s\n", r.ID)
		fmt.Printf("  name          %s (%s)\n", sanitize(r.Name), sanitize(r.MediaType))
		fmt.Printf("  size          %s bytes\n", commas(r.Bytes))
		fmt.Printf("  sha256        %s\n", r.SHA256)
		fmt.Printf("  task          %s, attempt %s (agent %s, session %s)\n", r.TaskID, r.AttemptID, r.AgentID, r.SessionID)
		fmt.Printf("  checks        %s\n", checksLine(r))
		fmt.Printf("  verification  %s (the task's independent verification; a result is never verified by being downloaded)\n", verificationWord(r))
		fmt.Printf("  provenance    attempt %d, epoch %d, runner %s, recorded %s\n", r.Provenance.AttemptIndex, r.Provenance.ExecutionEpoch, notRecorded(r.Provenance.RunnerVersion), r.Provenance.RecordedAt)
		fmt.Printf("  download      ks result download %s\n", r.ID)
	})
}

// ---- downloading ------------------------------------------------------------

type downloadOutcome struct {
	Bytes       int64
	ResumedFrom int64
}

func partialPath(dest, resultID string) string {
	return filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".ks-partial-"+resultID)
}

// checkTarget refuses a target that must not be written: a directory always,
// an existing file unless force, a missing parent directory.
func checkTarget(dest string, force bool) error {
	fi, err := os.Lstat(dest)
	switch {
	case err == nil && fi.IsDir():
		return &cliError{Code: exitUsage, Kind: "target_is_directory", Message: fmt.Sprintf("%s is a directory; name a file with --out. Nothing was downloaded", dest)}
	case err == nil && !force:
		return &cliError{Code: exitConflict, Kind: "target_exists", Message: fmt.Sprintf("%s already exists and is left untouched; nothing was downloaded", dest),
			NextAction: "choose another name with --out, or replace it with --force"}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	if pfi, err := os.Stat(filepath.Dir(dest)); err != nil || !pfi.IsDir() {
		return &cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("the directory %s does not exist; nothing was downloaded", filepath.Dir(dest))}
	}
	return nil
}

// openPartial opens (or creates) the partial file for appending. It is
// never reached through a link, and a non-regular file there is refused.
func openPartial(p string) (*os.File, int64, error) {
	fi, err := os.Lstat(p)
	if err == nil && !fi.Mode().IsRegular() {
		return nil, 0, &cliError{Code: exitIntegrity, Kind: "partial_not_regular",
			Message: fmt.Sprintf("%s is not a regular file (a link or a device); it is left untouched and nothing was downloaded; remove it yourself", p)}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, &cliError{Code: exitIntegrity, Kind: "partial_not_regular", Message: p + " is not a regular file; nothing was downloaded"}
	}
	return f, st.Size(), nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func requestGrant(cr hostedCreds, resultID string, ttl time.Duration) (string, error) {
	body := map[string]any{}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl / time.Second)
	}
	var env struct {
		Data struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/results/"+url.PathEscape(resultID)+"/grants", body, &env); err != nil {
		return "", err
	}
	if env.Data.Token == "" {
		return "", fmt.Errorf("the control plane answered a download grant with no token")
	}
	return env.Data.Token, nil
}

// integrity is the error for bytes that are not the recorded ones.
func integrity(kind, msg string) error {
	return &cliError{Code: exitIntegrity, Kind: kind, Message: msg}
}

// downloadVerified brings the result's bytes to partial and verifies them.
// On return with a nil error, partial holds exactly the recorded bytes.
func downloadVerified(cr hostedCreds, r resultRow, partial string, ttl time.Duration) (downloadOutcome, error) {
	var o downloadOutcome
	f, offset, err := openPartial(partial)
	if err != nil {
		return o, err
	}
	closed := false
	closeF := func() {
		if !closed {
			_ = f.Sync()
			_ = f.Close()
			closed = true
		}
	}
	defer closeF()
	if offset > r.Bytes {
		// longer than the result can be: not a prefix of it
		if err := f.Truncate(0); err != nil {
			return o, err
		}
		offset = 0
	}
	if offset < r.Bytes {
		token, err := requestGrant(cr, r.ID, ttl)
		if err != nil {
			return o, err
		}
		p := "/api/v2/results/" + url.PathEscape(r.ID) + "/content"
		req, err := http.NewRequest("GET", strings.TrimRight(cr.CTL, "/")+p, nil)
		if err != nil {
			return o, err
		}
		req.Header.Set("Authorization", "Bearer "+cr.Token)
		req.Header.Set("X-KS-Grant", token)
		if offset > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
			// the partial bytes are a prefix of THIS digest's bytes or of
			// nothing: a service holding anything else answers it whole
			req.Header.Set("If-Range", `"sha256:`+r.SHA256+`"`)
		}
		resp, err := streamClient().Do(req)
		if err != nil {
			return o, transportErr{err: err}
		}
		defer resp.Body.Close()
		if got := strings.ToLower(resp.Header.Get("X-KS-Sha256")); got != "" && got != r.SHA256 && resp.StatusCode/100 == 2 {
			closeF()
			os.Remove(partial)
			return o, integrity("digest_mismatch", fmt.Sprintf("the service states sha256 %s for these bytes, not the recorded %s; nothing was written and the partial download was removed", short(got), short(r.SHA256)))
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if offset > 0 {
				progress("the service answered the whole result rather than the rest of it; starting again from byte 0")
			}
			if err := f.Truncate(0); err != nil {
				return o, err
			}
			offset = 0
		case http.StatusPartialContent:
			start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
			if !ok || start != offset || total != r.Bytes {
				closeF()
				os.Remove(partial)
				return o, integrity("range_mismatch", fmt.Sprintf("the service resumed at %q, not at byte %d of %d; the partial download was removed, run the command again", resp.Header.Get("Content-Range"), offset, r.Bytes))
			}
			o.ResumedFrom = offset
		case http.StatusRequestedRangeNotSatisfiable:
			closeF()
			os.Remove(partial)
			return o, integrity("range_mismatch", "the partial download does not fit this result; it was removed, run the command again")
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			err := hostedError("GET", p, resp, raw)
			var he *hostedErr
			if errors.As(err, &he) {
				switch he.Type {
				case "ks_digest_mismatch":
					return o, integrity("ks_digest_mismatch", sanitize(he.Message)+"; nothing was written")
				case "ks_grant_expired", "ks_grant_revoked", "ks_grant_invalid", "ks_grant_required":
					ce := classify(err)
					ce.Message += "; no bytes were served and nothing was written"
					ce.NextAction = "run the same command again: each download asks for a new grant"
					return o, ce
				}
			}
			return o, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return o, err
		}
		want := r.Bytes - offset
		n, err := io.Copy(f, io.LimitReader(resp.Body, want+1))
		switch {
		case n > want:
			closeF()
			os.Remove(partial)
			return o, integrity("size_mismatch", fmt.Sprintf("the service sent more than the recorded %d bytes; nothing was written and the partial download was removed", r.Bytes))
		case err != nil || n < want:
			closeF()
			have := offset + n
			cause := "the connection ended early"
			if err != nil {
				cause = sanitize(err.Error())
			}
			return o, &cliError{Code: exitTemporary, Kind: "download_interrupted",
				Message:    fmt.Sprintf("the download stopped after %s of %s bytes (%s). Nothing was written at the target; the incomplete bytes are kept at %s", commas(have), commas(r.Bytes), cause, partial),
				NextAction: "run the same command again to resume, or delete " + partial}
		}
	}
	closeF()
	got, size, err := hashFile(partial)
	if err != nil {
		return o, err
	}
	if size != r.Bytes || got != r.SHA256 {
		os.Remove(partial)
		return o, integrity("digest_mismatch", fmt.Sprintf("the downloaded bytes (%s bytes, sha256 %s) are not the recorded result (%s bytes, sha256 %s); nothing was written and the partial download was removed",
			commas(size), short(got), commas(r.Bytes), short(r.SHA256)))
	}
	o.Bytes = size
	return o, nil
}

func parseContentRange(v string) (start, total int64, ok bool) {
	// bytes START-END/TOTAL
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, false
	}
	rng, tot, found := strings.Cut(strings.TrimPrefix(v, "bytes "), "/")
	if !found {
		return 0, 0, false
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(s, 10, 64)
	b, err2 := strconv.ParseInt(tot, 10, 64)
	return a, b, err1 == nil && err2 == nil
}

// finalize moves the verified partial file to dest. Without force it is a
// hard link, which fails when anything is at dest by then: a target that
// appeared during the download is refused and left as it is.
func finalize(partial, dest string, force bool) error {
	if force {
		if fi, err := os.Lstat(dest); err == nil && fi.IsDir() {
			os.Remove(partial)
			return &cliError{Code: exitUsage, Kind: "target_is_directory", Message: dest + " became a directory; nothing was written"}
		}
		return os.Rename(partial, dest)
	}
	if err := os.Link(partial, dest); err != nil {
		os.Remove(partial)
		if errors.Is(err, os.ErrExist) {
			return &cliError{Code: exitConflict, Kind: "target_appeared",
				Message:    fmt.Sprintf("%s appeared while the result was downloading; it is left untouched and the verified download was discarded", dest),
				NextAction: "choose another name with --out, or replace it with --force"}
		}
		return fmt.Errorf("the verified download could not be placed at %s without replacing anything (%v); nothing was written", dest, err)
	}
	return os.Remove(partial)
}

func hostedResultDownload(cr hostedCreds, inv *Invocation) {
	var ttl time.Duration
	if inv.Set("grant-ttl") {
		d, err := time.ParseDuration(inv.Str("grant-ttl"))
		if err != nil || d < time.Second || d > time.Hour {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--grant-ttl %q is not a duration between 1s and 1h", inv.Str("grant-ttl"))})
		}
		ttl = d
	}
	extract := inv.Str("extract")
	if extract != "" {
		if _, err := os.Lstat(extract); err == nil {
			fail(&cliError{Code: exitConflict, Kind: "target_exists", Message: fmt.Sprintf("%s already exists; an extraction goes into a NEW directory. Nothing was downloaded", extract)})
		}
	}
	r, err := fetchResult(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if !sha256Hex64.MatchString(r.SHA256) || r.Bytes <= 0 {
		fail(integrity("metadata_invalid", fmt.Sprintf("result %s carries no usable digest or size; nothing was downloaded", r.ID)))
	}
	if t := inv.Str("task"); t != "" && t != r.TaskID {
		fail(&cliError{Code: exitUsage, Kind: "wrong_task", Message: fmt.Sprintf("result %s belongs to task %s, not %s; nothing was downloaded", r.ID, r.TaskID, t)})
	}
	keep := extract == "" || inv.Set("out")
	dest := inv.Str("out")
	if dest == "" {
		if !resultFileName.MatchString(r.Name) {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("result %s's name is not a plain file name; choose one with --out. Nothing was downloaded", r.ID)})
		}
		dest = r.Name
		if !keep {
			dest = filepath.Join(os.TempDir(), "ks-result-"+r.ID)
		}
	}
	if keep {
		if err := checkTarget(dest, inv.Bool("force")); err != nil {
			die(err)
		}
	}
	partial := partialPath(dest, r.ID)
	progress("downloading %s (%s bytes, sha256 %s) to %s", r.ID, commas(r.Bytes), short(r.SHA256), partial)
	o, err := downloadVerified(cr, r, partial, ttl)
	if err != nil {
		// a partial file that holds nothing is not worth keeping
		if fi, serr := os.Lstat(partial); serr == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
			os.Remove(partial)
		}
		die(err)
	}
	source := partial
	if keep {
		if err := finalize(partial, dest, inv.Bool("force")); err != nil {
			die(err)
		}
		source = dest
	}
	var files int
	if extract != "" {
		files, err = extractArchive(source, extract)
		if !keep {
			os.Remove(partial)
		}
		if err != nil {
			die(err)
		}
	}
	data := map[string]any{"result_id": r.ID, "task_id": r.TaskID, "attempt_id": r.AttemptID, "bytes": o.Bytes, "sha256": r.SHA256,
		"verified": true, "resumed_from": o.ResumedFrom, "task_verification_state": r.TaskVerification}
	if keep {
		data["path"] = dest
	}
	if extract != "" {
		data["extracted_to"], data["files"] = extract, files
	}
	emit(data, func() {
		if keep {
			fmt.Printf("downloaded %s: %s bytes, sha256 %s verified (result %s of task %s, attempt %s)\n", dest, commas(o.Bytes), r.SHA256, r.ID, r.TaskID, r.AttemptID)
		}
		if extract != "" {
			fmt.Printf("extracted %d file(s) into %s after every entry passed the safety checks\n", files, extract)
		}
		fmt.Printf("verification of the task: %s. Nothing was run.\n", verificationWord(r))
	})
}

// ---- safe extraction (C09) ------------------------------------------------------

type archiveEntry struct {
	name  string // cleaned, slash-separated, relative
	dir   bool
	exec  bool
	size  int64
	check func(io.Reader) error
}

// safeEntryName validates one archive path. It never rewrites a name into
// a different identity: anything that would need rewriting is refused.
func safeEntryName(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.Contains(raw, `\`) {
		return "", fmt.Errorf("entry %q has an unusable name", sanitize(raw))
	}
	if strings.HasPrefix(raw, "/") || (len(raw) > 1 && raw[1] == ':') {
		return "", fmt.Errorf("entry %q is an absolute path", sanitize(raw))
	}
	for _, part := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		if part == ".." {
			return "", fmt.Errorf("entry %q climbs out of the directory", sanitize(raw))
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("entry %q names the directory itself", sanitize(raw))
	}
	return clean, nil
}

// archiveKind reads the leading bytes: gzip (a tar inside), zip, or tar.
func archiveKind(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "tar.gz", nil
	case n >= 4 && string(head[:4]) == "PK\x03\x04", n >= 4 && string(head[:4]) == "PK\x05\x06":
		return "zip", nil
	case n >= 262 && string(head[257:262]) == "ustar":
		return "tar", nil
	}
	return "", &cliError{Code: exitUsage, Kind: "not_an_archive", Message: "the result is not a tar, tar.gz or zip archive, so it is not extracted; nothing was extracted"}
}

// walkArchive visits every entry in order, applying the C09 rules as it
// goes; visit receives each entry with a reader of exactly its bytes.
func walkArchive(p string, visit func(e archiveEntry, rd io.Reader) error) error {
	kind, err := archiveKind(p)
	if err != nil {
		return err
	}
	seen := map[string]bool{} // path -> is a directory
	var total int64
	count := 0
	accept := func(raw string, dir bool, mode os.FileMode, size int64, rd io.Reader) error {
		name, err := safeEntryName(raw)
		if err != nil {
			return err
		}
		if isDir, ok := seen[name]; ok && !(isDir && dir) {
			return fmt.Errorf("entry %q appears twice", name)
		}
		// no entry may sit below a file, and no directory may be a file
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			if isDir, ok := seen[d]; ok && !isDir {
				return fmt.Errorf("entry %q sits below the file %q", name, d)
			}
			seen[d] = true
		}
		seen[name] = dir
		if !dir {
			count++
			if count > extractMaxFiles {
				return fmt.Errorf("the archive holds more than %d files", extractMaxFiles)
			}
			total += size
			if size < 0 || total > extractMaxBytes {
				return fmt.Errorf("the archive expands past %d MiB", extractMaxBytes>>20)
			}
		}
		// a reader that refuses to deliver more than the declared size, and
		// reports a short entry
		lr := &exactReader{r: io.LimitReader(rd, size+1), want: size}
		return visit(archiveEntry{name: name, dir: dir, exec: mode&0o111 != 0, size: size}, lr)
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	switch kind {
	case "zip":
		st, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return fmt.Errorf("the zip archive is unreadable: %v", err)
		}
		for _, zf := range zr.File {
			m := zf.Mode()
			switch {
			case m&os.ModeSymlink != 0:
				return fmt.Errorf("entry %q is a symbolic link", sanitize(zf.Name))
			case m.IsDir():
				if err := accept(zf.Name, true, m, 0, strings.NewReader("")); err != nil {
					return err
				}
			case m.IsRegular():
				if zf.UncompressedSize64 > extractMaxBytes {
					return fmt.Errorf("entry %q expands past %d MiB", sanitize(zf.Name), extractMaxBytes>>20)
				}
				rc, err := zf.Open()
				if err != nil {
					return fmt.Errorf("entry %q is unreadable: %v", sanitize(zf.Name), err)
				}
				err = accept(zf.Name, false, m, int64(zf.UncompressedSize64), rc)
				rc.Close()
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("entry %q is not a regular file or a directory", sanitize(zf.Name))
			}
		}
		return nil
	default:
		var rd io.Reader = bufio.NewReader(f)
		if kind == "tar.gz" {
			gz, err := gzip.NewReader(rd)
			if err != nil {
				return fmt.Errorf("the gzip stream is unreadable: %v", err)
			}
			defer gz.Close()
			rd = gz
		}
		tr := tar.NewReader(rd)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("the tar archive is unreadable: %v", err)
			}
			switch h.Typeflag {
			case tar.TypeXGlobalHeader:
				continue
			case tar.TypeDir:
				err = accept(h.Name, true, os.FileMode(h.Mode), 0, strings.NewReader(""))
			case tar.TypeReg, tar.TypeRegA:
				err = accept(h.Name, false, os.FileMode(h.Mode), h.Size, tr)
			case tar.TypeSymlink:
				err = fmt.Errorf("entry %q is a symbolic link", sanitize(h.Name))
			case tar.TypeLink:
				err = fmt.Errorf("entry %q is a hard link", sanitize(h.Name))
			default:
				err = fmt.Errorf("entry %q is not a regular file or a directory (type %q)", sanitize(h.Name), string(h.Typeflag))
			}
			if err != nil {
				return err
			}
		}
	}
}

// exactReader delivers exactly want bytes or fails.
type exactReader struct {
	r    io.Reader
	want int64
	got  int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.got += int64(n)
	if e.got > e.want {
		return n, fmt.Errorf("an entry holds more bytes than it declares")
	}
	if err == io.EOF && e.got < e.want {
		return n, fmt.Errorf("an entry holds fewer bytes than it declares")
	}
	return n, err
}

// extractArchive checks the WHOLE archive first and writes nothing unless
// every entry passes; then it creates dir (which must not exist) and writes
// into it. A failure part-way removes what this call created.
func extractArchive(archive, dir string) (int, error) {
	if err := walkArchive(archive, func(e archiveEntry, rd io.Reader) error {
		_, err := io.Copy(io.Discard, rd)
		return err
	}); err != nil {
		return 0, integrity("archive_refused", "the archive was refused before anything was extracted: "+err.Error())
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, &cliError{Code: exitConflict, Kind: "target_appeared", Message: dir + " appeared before the extraction; it is left untouched and nothing was extracted"}
		}
		return 0, err
	}
	files := 0
	err := walkArchive(archive, func(e archiveEntry, rd io.Reader) error {
		target := filepath.Join(dir, filepath.FromSlash(e.name))
		if e.dir {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if e.exec {
			mode = 0o755
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, rd); err != nil {
			f.Close()
			return err
		}
		files++
		return f.Close()
	})
	if err != nil {
		os.RemoveAll(dir)
		return 0, integrity("archive_refused", "the extraction stopped and "+dir+" was removed: "+err.Error())
	}
	return files, nil
}
