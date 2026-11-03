// file_upload.go: ks file upload, one file or a folder, up to 1 GB, into a
// session's workspace.
//
//	ks file upload <local path> [--to PATH] [--agent NAME | --session ID]
//	               [--replace] [--with-git] [--tell] [--tell-text TEXT] [--discard]
//
// The upload is declared first (what it carries: its kind, the workspace
// path, the byte count and sha256 of what will be sent, and for a folder the
// files in it), and the session's machine checks everything before any byte:
// the path stays inside the workspace, the target does not exist (unless
// --replace), no name is on the secret list, it is at most 1 GB and there is
// room. The bytes then go in chunks at the offset the machine holds, each
// with its own sha256, and the machine places the upload only when the whole
// sha256 matches: nothing half-written is ever at the target path.
//
// The upload's identity is what it carries, so running the same command again
// after an interruption declares the same upload and the machine answers what
// it already holds: the upload continues from there. Nothing is kept on this
// side but the files themselves.
//
// A folder is packed as a tar under the Cruise upload policy
// (ks-upload-policy/2: the repository's history, the client's job files,
// dependency and tool trees, credential-shaped names and what .gitignore
// lists are left out), plus the workspace's secret-file rule; links are left
// out. With --with-git a folder's .git is carried too (every file of it,
// links still never), and it counts toward the 1 GB limit like any file. The
// tar is generated twice, once to hash it and once to send it, never
// written to disk, and is the same bytes on every run over the same files.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// uploadLimit is the most an upload carries: 1 GB of file content (a
	// decimal gigabyte, the unit of the price book).
	uploadLimit    = 1_000_000_000
	uploadMaxFiles = 100000
	uploadRetries  = 5
)

// serviceSecretNames is the workspace's secret-file rule, the one file
// download refuses by: a file with such a name is never uploaded or
// downloaded. The service may add names of its own; it refuses those too.
var serviceSecretNames = []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ecdsa*", "id_ed25519*",
	".netrc", ".git-credentials", "credentials", "credentials.*", ".npmrc", ".pypirc"}

func secretNameRule(name string) string {
	for _, p := range serviceSecretNames {
		if ok, _ := path.Match(p, name); ok {
			return p
		}
	}
	return ""
}

// uploadEntry is one file a folder upload carries.
type uploadEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	mtime time.Time
	abs   string
}

type uploadSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type uploadPlan struct {
	Kind    string // file | folder
	Name    string // the local base name
	Local   string
	To      string
	Mode    string // a file's
	mtime   time.Time
	Entries []uploadEntry
	Content int64 // the files' bytes
	Size    int64 // the bytes sent
	SHA256  string
	Skipped []uploadSkip
	WithGit bool  // --with-git: the folder's .git is carried
	GitSize int64 // what the carried .git holds, for the size refusal
}

// uploadView is the service's answer about one upload.
type uploadView struct {
	UploadID     string `json:"upload_id"`
	SessionID    string `json:"session_id"`
	Kind         string `json:"kind"`
	To           string `json:"to"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	ContentBytes int64  `json:"content_bytes"`
	Files        int    `json:"files"`
	Received     int64  `json:"received"`
	State        string `json:"state"`
	Resumed      bool   `json:"resumed"`
	ChunkBytes   int64  `json:"chunk_bytes"`
	LimitBytes   int64  `json:"limit_bytes"`
	FreeBytes    int64  `json:"free_bytes"`
	Replace      bool   `json:"replace"`
	StorageNote  string `json:"storage_note"`
	Placed       *struct {
		Path     string `json:"path"`
		Kind     string `json:"kind"`
		Bytes    int64  `json:"bytes"`
		Files    int    `json:"files"`
		Replaced bool   `json:"replaced"`
	} `json:"placed,omitempty"`
}

// fileUpload is what --json reports.
type fileUpload struct {
	SessionID   string       `json:"session_id"`
	UploadID    string       `json:"upload_id"`
	Kind        string       `json:"kind"`
	Path        string       `json:"path"`
	Bytes       int64        `json:"bytes"`
	Sent        int64        `json:"sent_bytes"`
	Files       int          `json:"files"`
	SHA256      string       `json:"sha256"`
	Replaced    bool         `json:"replaced"`
	Resumed     bool         `json:"resumed"`
	Skipped     []uploadSkip `json:"skipped"`
	Told        bool         `json:"told"`
	Tell        any          `json:"tell"`
	StorageNote string       `json:"storage_note"`
}

const uploadStorageNote = "Uploaded files count toward your storage."

// uploadTarget refuses, before anything is sent, what can never be a path
// in the workspace, and anything not already in its plain form.
func uploadTarget(p string) string {
	bad := p == "" || p == "." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.ContainsAny(p, "\\\x00") || (len(p) > 1 && p[1] == ':')
	for _, part := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if part == ".." || part == "." || part == "" {
			bad = true
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			bad = true
		}
	}
	if bad || path.Clean(p) != strings.TrimSuffix(p, "/") {
		fail(&cliError{Code: exitIntegrity, Kind: "path_refused",
			Message: fmt.Sprintf("%q is not a path inside the workspace: a path there is relative, in plain form, without . or .., and never a host path; nothing was sent", sanitize(p))})
	}
	return strings.TrimSuffix(p, "/")
}

func refuseSecret(name, rule string) {
	fail(&cliError{Code: exitIntegrity, Kind: "path_redacted",
		Message: fmt.Sprintf("%s is a secret file name (it matches %s): a secret file's contents are never uploaded or downloaded; nothing was sent", sanitize(name), rule)})
}

func refuseTooLarge(name string, n int64) { refuseTooLargeWith(name, n, 0) }

// refuseTooLargeWith says, when the folder's history was asked for, how much
// of the size it is: .git counts toward the limit like any file.
func refuseTooLargeWith(name string, n, git int64) {
	msg := fmt.Sprintf("%s is %s (%s bytes); the limit is 1 GB (1,000,000,000 bytes). Nothing was sent.", sanitize(name), bytesText(n), commas(n))
	next := ""
	if git > 0 {
		msg = fmt.Sprintf("%s is %s (%s bytes) with its history, and .git holds %s of it: .git counts toward the limit like any file, and the limit is 1 GB (1,000,000,000 bytes). Nothing was sent.", sanitize(name), bytesText(n), commas(n), bytesText(git))
		next = "run the same command without --with-git to upload the files without the history"
	}
	fail(&cliError{Code: exitConflict, Kind: "upload_too_large", Message: msg, NextAction: next})
}

func modeWord(m os.FileMode) string {
	if m&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

// planUpload reads what the upload will carry, without its bytes.
func planUpload(local, to string, withGit bool) *uploadPlan {
	abs, err := filepath.Abs(local)
	if err != nil {
		die(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s cannot be read (%v); nothing was sent", sanitize(local), err)})
	}
	p := &uploadPlan{Local: abs, Name: filepath.Base(abs), Skipped: []uploadSkip{}, WithGit: withGit}
	if to == "" {
		to = p.Name
	}
	p.To = uploadTarget(to)
	if rule := secretNameRule(path.Base(p.To)); rule != "" {
		refuseSecret(path.Base(p.To), rule)
	}
	switch {
	case fi.Mode().IsRegular():
		p.Kind, p.Mode, p.mtime = "file", modeWord(fi.Mode()), fi.ModTime()
		if rule := secretNameRule(p.Name); rule != "" {
			refuseSecret(p.Name, rule)
		}
		if fi.Size() > uploadLimit {
			refuseTooLarge(p.Name, fi.Size())
		}
		p.Content, p.Size = fi.Size(), fi.Size()
	case fi.IsDir():
		p.Kind = "folder"
		planFolder(p)
	default:
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s is not a file or a folder; nothing was sent", sanitize(local))})
	}
	return p
}

func planFolder(p *uploadPlan) {
	sel, err := buildSelectionUnder(p.Local, nil, selectionPolicyV2)
	if sel == nil {
		// the walk itself failed (an unreadable folder, a socket or a device)
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s cannot be packed: %s; nothing was sent", sanitize(p.Name), sanitize(err.Error()))})
	}
	var gitDirs []string
	for _, x := range sel.Excluded {
		if p.WithGit && path.Base(x.Path) == ".git" && strings.HasPrefix(x.Reason, "mandatory") {
			gitDirs = append(gitDirs, x.Path) // carried below, on request
			continue
		}
		p.Skipped = append(p.Skipped, uploadSkip{Path: x.Path, Reason: x.Reason})
	}
	for _, l := range sel.Symlinks {
		p.Skipped = append(p.Skipped, uploadSkip{Path: l, Reason: "a link: links are never uploaded"})
	}
	for _, f := range sel.Included {
		if rule := secretNameRule(path.Base(f.Path)); rule != "" {
			p.Skipped = append(p.Skipped, uploadSkip{Path: f.Path, Reason: "secret file name (matches " + rule + "): a secret file's contents are never uploaded or downloaded"})
			continue
		}
		abs := filepath.Join(p.Local, filepath.FromSlash(f.Path))
		fi, err := os.Lstat(abs)
		if err != nil || !fi.Mode().IsRegular() {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s changed while it was read; nothing was sent", sanitize(f.Path))})
		}
		p.Entries = append(p.Entries, uploadEntry{Path: f.Path, Size: fi.Size(), Mode: modeWord(fi.Mode()), mtime: fi.ModTime(), abs: abs})
		p.Content += fi.Size()
	}
	for _, g := range gitDirs {
		addGit(p, g)
	}
	sort.Slice(p.Skipped, func(i, j int) bool { return p.Skipped[i].Path < p.Skipped[j].Path })
	if p.Content > uploadLimit {
		refuseTooLargeWith(p.Name, p.Content, p.GitSize)
	}
	if len(p.Entries) > uploadMaxFiles {
		fail(&cliError{Code: exitConflict, Kind: "upload_too_many",
			Message: fmt.Sprintf("%s holds %s files to upload; the limit is %s. Nothing was sent.", sanitize(p.Name), commas(int64(len(p.Entries))), commas(uploadMaxFiles))})
	}
	if len(p.Entries) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s has no file to upload once %d paths are left out; nothing was sent", sanitize(p.Name), len(p.Skipped))})
	}
}

// addGit carries one .git of the folder (--with-git): every regular file in
// it, in path order, after the selection's files; a link is left out and
// said, as everywhere; a secret file name is still never carried. A .git
// that is a file (a worktree's pointer) is carried as that file.
func addGit(p *uploadPlan, rel string) {
	add := func(r string, fi os.FileInfo) {
		if rule := secretNameRule(path.Base(r)); rule != "" {
			p.Skipped = append(p.Skipped, uploadSkip{Path: r, Reason: "secret file name (matches " + rule + "): a secret file's contents are never uploaded or downloaded"})
			return
		}
		p.Entries = append(p.Entries, uploadEntry{Path: r, Size: fi.Size(), Mode: modeWord(fi.Mode()), mtime: fi.ModTime(), abs: filepath.Join(p.Local, filepath.FromSlash(r))})
		p.Content += fi.Size()
		p.GitSize += fi.Size()
	}
	var walk func(r string)
	walk = func(r string) {
		abs := filepath.Join(p.Local, filepath.FromSlash(r))
		fi, err := os.Lstat(abs)
		if err != nil {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s cannot be read (%v); nothing was sent", sanitize(r), err)})
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			p.Skipped = append(p.Skipped, uploadSkip{Path: r, Reason: "a link: links are never uploaded"})
		case fi.IsDir():
			ents, err := os.ReadDir(abs)
			if err != nil {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s cannot be read (%v); nothing was sent", sanitize(r), err)})
			}
			for _, e := range ents { // ReadDir answers in name order: the same tar every run
				walk(r + "/" + e.Name())
			}
		case fi.Mode().IsRegular():
			add(r, fi)
		default:
			p.Skipped = append(p.Skipped, uploadSkip{Path: r, Reason: "not a regular file: only files are uploaded"})
		}
	}
	walk(rel)
}

// writeUploadTar writes the folder's tar: the entries in order, regular
// files only, modes 0644 or 0755, the files' own modification times to the
// second, no owners, so the same files always make the same bytes.
func writeUploadTar(w io.Writer, p *uploadPlan) error {
	tw := tar.NewWriter(w)
	for _, e := range p.Entries {
		mode := int64(0o644)
		if e.Mode == "0755" {
			mode = 0o755
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: e.Path, Size: e.Size, Mode: mode, ModTime: e.mtime.Truncate(time.Second).UTC(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		f, err := os.Open(e.abs)
		if err != nil {
			return fmt.Errorf("%s: %v", e.Path, err)
		}
		n, err := io.CopyN(tw, f, e.Size)
		f.Close()
		if err != nil || n != e.Size {
			return fmt.Errorf("%s changed while it was read", e.Path)
		}
	}
	return tw.Close()
}

// openUpload answers the bytes the upload sends, from offset on.
func openUpload(p *uploadPlan, offset int64) (io.ReadCloser, error) {
	if p.Kind == "file" {
		f, err := os.Open(p.Local)
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeUploadTar(pw, p)) }()
	if _, err := io.CopyN(io.Discard, pr, offset); err != nil {
		pr.Close()
		return nil, err
	}
	return pr, nil
}

// hashUpload reads what will be sent once: its size and sha256.
func hashUpload(p *uploadPlan) {
	if p.Content > 64<<20 {
		progress("reading %s (%s) to compute its sha256", visible(p.Name), bytesText(p.Content))
	}
	src, err := openUpload(p, 0)
	if err != nil {
		die(err)
	}
	defer src.Close()
	h := sha256.New()
	n, err := io.Copy(h, src)
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s could not be read: %s; nothing was sent", sanitize(p.Name), sanitize(err.Error()))})
	}
	if p.Kind == "file" && n != p.Content {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%s changed while it was read; nothing was sent", sanitize(p.Name))})
	}
	p.Size, p.SHA256 = n, hex.EncodeToString(h.Sum(nil))
}

// ---- the requests ------------------------------------------------------------

func uploadsBase(sess inventoryRow) string { return inspectBase(sess) + "/uploads" }

// uploadDo sends one request with its own headers and answers the body read
// in full (it is small: every answer here is JSON).
func uploadDo(ctx context.Context, cr hostedCreds, client *http.Client, method, p string, headers map[string]string, body io.Reader, length int64) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cr.CTL, "/")+p, body)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.ContentLength = length
	}
	req.Header.Set("Authorization", "Bearer "+cr.Token)
	req.Header.Set(protocolHeader, clientProtocolText)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, transportErr{err: err, mutation: method != "GET"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp, raw, hostedError(method, p, resp, raw)
	}
	return resp, raw, nil
}

func decodeView(raw []byte) (uploadView, error) {
	var env struct {
		Data uploadView `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.UploadID == "" {
		return uploadView{}, fmt.Errorf("the service's answer about the upload was not understood")
	}
	return env.Data, nil
}

// transient: worth sending again, after a pause.
func transient(err error) bool {
	var te transportErr
	if errors.As(err, &te) {
		return true
	}
	var he *hostedErr
	if errors.As(err, &he) {
		return he.Status == http.StatusBadGateway || he.Status == http.StatusServiceUnavailable || he.Status == http.StatusGatewayTimeout || he.Status == http.StatusTooManyRequests
	}
	return false
}

func uploadBackoff(round int) time.Duration {
	unit := time.Second
	if v, err := strconv.Atoi(os.Getenv("KS_UPLOAD_BACKOFF_MS")); err == nil && v > 0 {
		unit = time.Duration(v) * time.Millisecond
	}
	return unit << uint(round)
}

// refusalField reads one number the service put beside its refusal.
func refusalField(err error, name string) (int64, bool) {
	var he *hostedErr
	if !errors.As(err, &he) {
		return 0, false
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(he.Raw, &env) != nil {
		return 0, false
	}
	f, ok := env.Error[name].(float64)
	return int64(f), ok
}

func refusalType(err error) string {
	var he *hostedErr
	if errors.As(err, &he) {
		return he.Type
	}
	return ""
}

// uploader carries one upload's state for its steps and its words.
type uploader struct {
	ctx    context.Context
	cr     hostedCreds
	inv    *Invocation
	sess   inventoryRow
	agent  string // the agent the resume words and --tell name
	plan   *uploadPlan
	view   uploadView
	shown  int64
	tty    bool
	lastAt time.Time
}

// again is the command that continues (or repeats) this upload.
func (u *uploader) again(extra ...string) string {
	parts := []string{"ks file upload", shellWord(u.inv.Arg(0))}
	if u.inv.Set("to") {
		parts = append(parts, "--to", shellWord(u.plan.To))
	}
	if u.inv.Set("agent") {
		parts = append(parts, "--agent", shellWord(u.inv.Str("agent")))
	}
	parts = append(parts, "--session", u.sess.ShortID)
	if u.inv.Bool("replace") {
		parts = append(parts, "--replace")
	}
	return strings.Join(append(parts, extra...), " ")
}

func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// refusal turns a service refusal into plain words and the next command.
func (u *uploader) refusal(err error, sent string) error {
	he := (*hostedErr)(nil)
	if !errors.As(err, &he) {
		if te := (transportErr{}); errors.As(err, &te) {
			return &cliError{Code: exitTemporary, Kind: "unreachable", WorkStarted: workUnknown,
				Message:    "the control plane did not answer: " + sanitize(te.err.Error()) + ". " + sent,
				NextAction: u.again()}
		}
		return err
	}
	ce := classify(err)
	short := u.sess.ShortID
	switch he.Type {
	case "ks_upload_too_large":
		ce.Code, ce.NextAction = exitConflict, ""
		ce.Message = fmt.Sprintf("%s is %s (%s bytes); the limit is 1 GB (1,000,000,000 bytes). Nothing was sent.", sanitize(u.plan.Name), bytesText(u.plan.Content), commas(u.plan.Content))
	case "ks_session_not_running":
		ce.Code = exitConflict
		ce.Message = fmt.Sprintf("session %s is not running, so nothing can be uploaded to it. %s", short, sent)
		ce.NextAction = fmt.Sprintf("ks agent resume %s --session %s (uploading never wakes a session)", u.agent, short)
	case "ks_path_refused":
		ce.Code, ce.NextAction = exitIntegrity, ""
		ce.Message = fmt.Sprintf("%s: %s. Nothing was sent.", sanitize(u.plan.To), sanitize(he.Message))
	case "ks_path_redacted":
		ce.Code, ce.NextAction = exitIntegrity, ""
		ce.Message = sanitize(he.Message) + " (a secret file's contents are never uploaded or downloaded). Nothing was sent."
	case "ks_path_exists":
		ce.Code = exitConflict
		ce.Message = fmt.Sprintf("%s already exists in the workspace; nothing was replaced", sanitize(u.plan.To))
		ce.NextAction = u.again("--replace") + " (replaces it)"
		if u.inv.Bool("replace") {
			ce.NextAction = u.again()
		}
	case "ks_no_room":
		free, _ := refusalField(err, "free_bytes")
		need, _ := refusalField(err, "needed_bytes")
		keep, _ := refusalField(err, "reserve_bytes")
		ce.Code = exitConflict
		ce.Message = fmt.Sprintf("the session's disk has %s free; this upload needs %s and keeps %s free for the agent. %s Free some space in the session, then run the same command again", bytesText(free), bytesText(need), bytesText(keep), sent)
		ce.NextAction = "ks file list --session " + short
	case "ks_upload_absent":
		ce.Code = exitFailed
		ce.Message = "the session no longer holds this upload (it was discarded, or it expired after 24 hours). " + sent
		ce.NextAction = u.again()
	case "ks_upload_busy":
		ce.Code = exitConflict
		ce.Message = "the same upload is being sent from somewhere else (another terminal or the console). " + sent
		ce.NextAction = u.again()
	case "ks_upload_digest_mismatch":
		ce.Code = exitIntegrity
		ce.Message = fmt.Sprintf("the bytes the session received are not the ones this client hashed (sha256 %s), for example because %s changed during the upload; they were discarded and nothing was placed", short64(u.plan.SHA256), sanitize(u.plan.Name))
		ce.NextAction = u.again()
	case "ks_upload_incomplete":
		ce.Code = exitTemporary
		ce.Message = "some bytes of the upload have not arrived yet; nothing was placed. " + sent
		ce.NextAction = u.again()
	case "ks_upload_invalid":
		ce.Code, ce.NextAction = exitIntegrity, ""
		ce.Message = "the session refused the upload: " + sanitize(he.Message) + ". Nothing was placed."
	case "ks_workspace_changed":
		ce.Code = exitConflict
		ce.Message = "the workspace changed under the upload: " + sanitize(he.Message) + ". Nothing was placed."
		ce.NextAction = u.again()
	case "ks_no_runtime":
		ce.Code, ce.NextAction = exitFailed, "ks session show "+short
	case "ks_route_not_offered", "ks_not_found", "":
		if he.Status == http.StatusNotFound {
			ce.Code, ce.NextAction = exitFailed, ""
			ce.Message = "this service does not take uploads yet; nothing was sent"
		}
	}
	return ce
}

func short64(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (u *uploader) declare(discard bool) {
	body := map[string]any{"kind": u.plan.Kind, "to": u.plan.To, "size": u.plan.Size, "sha256": u.plan.SHA256,
		"content_bytes": u.plan.Content, "replace": u.inv.Bool("replace") || discard}
	if u.plan.Kind == "file" {
		body["mode"] = u.plan.Mode
	} else {
		files := make([]map[string]any, 0, len(u.plan.Entries))
		for _, e := range u.plan.Entries {
			files = append(files, map[string]any{"path": e.Path, "size": e.Size, "mode": e.Mode})
		}
		body["files"] = files
	}
	raw, _ := json.Marshal(body)
	var err error
	for round := 0; ; round++ {
		var rb []byte
		_, rb, err = uploadDo(u.ctx, u.cr, ordinaryClient(), "POST", uploadsBase(u.sess), map[string]string{"Content-Type": "application/json"}, bytes.NewReader(raw), int64(len(raw)))
		if err == nil {
			if u.view, err = decodeView(rb); err != nil {
				die(err)
			}
			return
		}
		if !transient(err) || round >= uploadRetries || u.ctx.Err() != nil {
			break
		}
		time.Sleep(uploadBackoff(round))
	}
	die(u.refusal(err, "Nothing was sent."))
}

func (u *uploader) status() (uploadView, error) {
	// a short read of its own, so it is still asked after a Ctrl-C
	_, rb, err := uploadDo(context.Background(), u.cr, ordinaryClient(), "GET", uploadsBase(u.sess)+"/"+url.PathEscape(u.view.UploadID), nil, nil, 0)
	if err != nil {
		return uploadView{}, err
	}
	return decodeView(rb)
}

// keptWords: what is said when the upload stops before it ends.
func (u *uploader) keptWords() string {
	return fmt.Sprintf("%s of %s had arrived; what arrived is kept for 24 hours: run the same command again to continue", bytesText(u.view.Received), bytesText(u.plan.Size))
}

func (u *uploader) progressLine(done int64) {
	if !u.tty || out.quiet || out.json {
		return
	}
	if time.Since(u.lastAt) < 200*time.Millisecond && done < u.plan.Size {
		return
	}
	u.lastAt = time.Now()
	pct := int64(100)
	if u.plan.Size > 0 {
		pct = done * 100 / u.plan.Size
	}
	fmt.Fprintf(os.Stderr, "\r\x1b[Kuploading %s: %s of %s (%d%%)", visible(u.plan.Name), progressBytes(done), progressBytes(u.plan.Size), pct)
	u.shown = done
}

func (u *uploader) endProgress() {
	if u.tty && !out.quiet && !out.json && !u.lastAt.IsZero() {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
}

// progressBytes is bytesText with one decimal, so a moving count moves.
func progressBytes(b int64) string {
	switch {
	case b < 1e6:
		return bytesText(b)
	case b < 1e9:
		return fmt.Sprintf("%.1f MB", float64(b)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/1e9)
}

// countingReader reports progress as a chunk's body leaves.
type countingReader struct {
	r    io.Reader
	base int64
	n    int64
	on   func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.on(c.base + c.n)
	return n, err
}

// send sends every byte from what the session holds to the end.
func (u *uploader) send() {
	chunk := u.view.ChunkBytes
	if chunk <= 0 || chunk > 16<<20 {
		chunk = 16 << 20
	}
	buf := make([]byte, chunk)
	offset := u.view.Received
	var src io.ReadCloser
	reopen := func() {
		if src != nil {
			src.Close()
		}
		var err error
		if src, err = openUpload(u.plan, offset); err != nil {
			u.endProgress()
			die(&cliError{Code: exitFailed, Kind: "failed", Message: fmt.Sprintf("%s could not be read: %s. %s", sanitize(u.plan.Name), sanitize(err.Error()), u.keptWords()), NextAction: u.again()})
		}
	}
	reopen()
	defer func() { src.Close() }()
	stream := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute, // the machine writes the chunk before it answers
		Proxy:                 http.ProxyFromEnvironment,
	}}
	for offset < u.plan.Size {
		n := chunk
		if left := u.plan.Size - offset; left < n {
			n = left
		}
		if _, err := io.ReadFull(src, buf[:n]); err != nil {
			u.endProgress()
			die(&cliError{Code: exitFailed, Kind: "failed", Message: fmt.Sprintf("%s changed while it was uploaded (%s). %s", sanitize(u.plan.Name), sanitize(err.Error()), u.keptWords()), NextAction: u.again()})
		}
		sum := sha256.Sum256(buf[:n])
		route := uploadsBase(u.sess) + "/" + url.PathEscape(u.view.UploadID) + "/bytes?offset=" + strconv.FormatInt(offset, 10)
		var err error
		var rb []byte
		for round := 0; ; round++ {
			body := &countingReader{r: bytes.NewReader(buf[:n]), base: offset, on: u.progressLine}
			_, rb, err = uploadDo(u.ctx, u.cr, stream, "PUT", route, map[string]string{"Content-Type": "application/octet-stream", "X-KS-Chunk-SHA256": hex.EncodeToString(sum[:])}, body, n)
			if err == nil || u.ctx.Err() != nil {
				break
			}
			if refusalType(err) == "ks_upload_chunk_mismatch" && round < uploadRetries {
				continue // nothing of the chunk was kept: the same bytes again
			}
			if !transient(err) || round >= uploadRetries {
				break
			}
			time.Sleep(uploadBackoff(round))
		}
		if u.ctx.Err() != nil {
			u.endProgress()
			if v, serr := u.status(); serr == nil {
				u.view.Received = v.Received
			}
			fail(&cliError{Code: exitInterrupt, Kind: "interrupted", WorkStarted: workYes,
				Message: "the upload was stopped. " + u.keptWords(), NextAction: u.again()})
		}
		if refusalType(err) == "ks_upload_offset" {
			// the session holds another count than this side thought: ask,
			// and continue from what it holds
			v, serr := u.status()
			if serr != nil {
				err = serr
			} else {
				u.view.Received, offset = v.Received, v.Received
				reopen()
				continue
			}
		}
		if err != nil {
			u.endProgress()
			if r, ok := refusalField(err, "received"); ok {
				u.view.Received = r
			} else if v, serr := u.status(); serr == nil {
				u.view.Received = v.Received
			}
			if transient(err) {
				die(&cliError{Code: exitTemporary, Kind: "upload_interrupted", WorkStarted: workYes,
					Message:    fmt.Sprintf("the upload stopped (%s). %s", sanitize(err.Error()), u.keptWords()),
					NextAction: u.again()})
			}
			die(u.refusal(err, u.keptWords()+"."))
		}
		v, derr := decodeView(rb)
		if derr != nil {
			u.endProgress()
			die(derr)
		}
		u.view.Received = v.Received
		offset += n
		if v.Received != offset {
			offset = v.Received
			reopen()
		}
		u.progressLine(offset)
	}
	u.endProgress()
}

// complete asks the session to check and place the upload. It is idempotent
// on the service's side: an answer lost on the way back is asked again.
func (u *uploader) complete() uploadView {
	long := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 15 * time.Minute, // checking and unpacking 1 GB
		Proxy:                 http.ProxyFromEnvironment,
	}}
	route := uploadsBase(u.sess) + "/" + url.PathEscape(u.view.UploadID) + "/complete"
	var err error
	for round := 0; ; round++ {
		var rb []byte
		_, rb, err = uploadDo(u.ctx, u.cr, long, "POST", route, map[string]string{"Content-Type": "application/json"}, strings.NewReader("{}"), 2)
		if err == nil {
			v, derr := decodeView(rb)
			if derr != nil {
				die(derr)
			}
			return v
		}
		if !transient(err) || round >= uploadRetries || u.ctx.Err() != nil {
			break
		}
		if v, serr := u.status(); serr == nil && v.State == "placed" {
			return v
		}
		time.Sleep(uploadBackoff(round))
	}
	if transient(err) {
		die(&cliError{Code: exitTemporary, Kind: "upload_unconfirmed", WorkStarted: workUnknown,
			Message:    fmt.Sprintf("every byte arrived, but whether the session placed the upload is not known (%s)", sanitize(err.Error())),
			NextAction: u.again()})
	}
	die(u.refusal(err, "Every byte arrived and nothing was placed."))
	return uploadView{}
}

func stderrIsTerminal() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// skippedLines: what a folder upload left out, a line per reason with up to
// five of its paths (every path is in --json).
func skippedLines(sk []uploadSkip) []string {
	if len(sk) == 0 {
		return nil
	}
	groups := map[string][]string{}
	var order []string
	for _, s := range sk {
		g := s.Reason
		switch {
		case strings.HasPrefix(g, "dependency or tool tree"):
			g = "dependency and tool folders (their contents are never uploaded)"
		case strings.HasPrefix(g, "ignored: "):
			g = "listed in .gitignore"
		case strings.HasPrefix(g, "sensitive"):
			g = "names that hold credentials"
		case strings.HasPrefix(g, "secret file name"):
			g = "secret file names (a secret file's contents are never uploaded or downloaded)"
		case strings.HasPrefix(g, "mandatory"):
			g = "the repository's history and the client's job files"
		case strings.HasPrefix(g, "a link"):
			g = "links (never uploaded)"
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], s.Path)
	}
	lines := []string{fmt.Sprintf("left out %d %s (the upload policy ks-upload-policy/2 and the workspace's secret-file rule):", len(sk), noun(len(sk), "path", "paths"))}
	for _, g := range order {
		ps := groups[g]
		shown := ps
		more := ""
		if len(ps) > 5 {
			shown, more = ps[:5], fmt.Sprintf(" and %d more", len(ps)-5)
		}
		for i := range shown {
			shown[i] = visible(sanitize(shown[i]))
		}
		lines = append(lines, fmt.Sprintf("  %s: %s%s", g, strings.Join(shown, ", "), more))
	}
	return lines
}

// noun is one or many by the count, without the count.
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func hostedFileUpload(cr hostedCreds, inv *Invocation) {
	tellText := inv.Str("tell-text")
	tell := inv.Bool("tell") || inv.Set("tell-text")
	if inv.Set("tell-text") && strings.TrimSpace(tellText) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--tell-text is empty; nothing was sent"})
	}
	if inv.Bool("discard") && (tell || inv.Bool("replace")) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--discard throws an unfinished upload away; it takes no --tell, --tell-text or --replace"})
	}
	// everything that can be refused here is refused before any request
	p := planUpload(inv.Arg(0), inv.Str("to"), inv.Bool("with-git"))
	if inv.Bool("with-git") && p.Kind != "folder" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--with-git carries a folder's .git; " + sanitize(p.Name) + " is a file. Nothing was sent."})
	}

	var sess inventoryRow
	agentName := "main"
	var agent *agentRow
	if inv.Set("agent") {
		s, a := childTarget(cr, inv, inv.Str("agent"))
		sess, agent, agentName = s, &a, firstNonEmpty(a.Name, inv.Str("agent"))
	} else {
		sess = agentSession(cr, inv)
		if tell {
			a, err := resolveAgent(cr, sess, "main")
			if err != nil {
				ce := classify(err)
				ce.Message = "--tell needs the agent to tell: " + ce.Message + "; nothing was sent"
				die(ce)
			}
			agent = &a
		}
	}
	hashUpload(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-stop; ok {
			cancel()
		}
	}()
	defer signal.Stop(stop)
	u := &uploader{ctx: ctx, cr: cr, inv: inv, sess: sess, agent: agentName, plan: p, tty: stderrIsTerminal()}
	if !out.json {
		for _, l := range skippedLines(p.Skipped) {
			progress("%s", l)
		}
		for _, s := range p.Skipped {
			if path.Base(s.Path) == ".git" && strings.HasPrefix(s.Reason, "mandatory") {
				progress("  to include the project's history, add --with-git (.git counts toward the 1 GB limit)")
				break
			}
		}
	}
	u.declare(inv.Bool("discard"))

	if inv.Bool("discard") {
		had := u.view.Received
		_, _, err := uploadDo(ctx, cr, ordinaryClient(), "DELETE", uploadsBase(sess)+"/"+url.PathEscape(u.view.UploadID), nil, nil, 0)
		if err != nil {
			die(u.refusal(err, "Nothing was discarded."))
		}
		emit(map[string]any{"session_id": agentSessionID(sess), "upload_id": u.view.UploadID, "discarded": true, "received": had, "size": p.Size}, func() {
			fmt.Printf("discarded the unfinished upload of %s to %s: %s of %s had arrived. Nothing in the workspace changed.\n", visible(p.Name), visible(p.To), bytesText(had), bytesText(p.Size))
		})
		return
	}
	switch {
	case u.view.State == "placed":
	case u.view.Resumed || u.view.Received > 0:
		progress("continuing an earlier upload: %s of %s are already there", bytesText(u.view.Received), bytesText(p.Size))
	default:
		what := bytesText(p.Content)
		if p.Kind == "folder" {
			what = fmt.Sprintf("%s in %s %s, %s packed", what, commas(int64(len(p.Entries))), noun(len(p.Entries), "file", "files"), bytesText(p.Size))
		}
		progress("uploading %s (%s) to %s in session %s", visible(p.Name), what, visible(p.To), sess.ShortID)
	}
	if u.view.State != "placed" {
		u.send()
		u.view = u.complete()
	}
	res := fileUpload{SessionID: agentSessionID(sess), UploadID: u.view.UploadID, Kind: p.Kind, Path: p.To, Bytes: p.Content, Sent: p.Size,
		Files: len(p.Entries), SHA256: p.SHA256, Resumed: u.view.Resumed, Skipped: p.Skipped, StorageNote: uploadStorageNote}
	if p.Kind == "file" {
		res.Files = 1
	}
	if u.view.Placed != nil {
		res.Replaced = u.view.Placed.Replaced
		if u.view.Placed.Path != "" {
			res.Path = u.view.Placed.Path
		}
	}
	size := bytesText(p.Content)
	if p.Kind == "folder" {
		size += fmt.Sprintf(", %s %s", commas(int64(res.Files)), noun(res.Files, "file", "files"))
	}
	var tellLine, tellFail string
	if tell && agent != nil {
		text := tellText
		if text == "" {
			text = fmt.Sprintf("I uploaded %s (%s) to %s in your workspace.", p.Name, size, res.Path)
		}
		sid, err := newSubmissionID()
		if err == nil {
			var t submittedTask
			t, _, err = sendInstruction(cr, agent.ID, sid, map[string]any{"submission_id": sid, "text": text})
			if err == nil {
				res.Told = true
				res.Tell = map[string]any{"agent": agent.Name, "agent_id": agent.ID, "task_id": t.ID, "submission_id": sid, "text": text}
				tellLine = fmt.Sprintf("told %s: %s", visible(sanitize(firstNonEmpty(agent.Name, agentName))), visible(sanitize(text)))
			}
		}
		if err != nil {
			res.Tell = map[string]any{"agent": agent.Name, "sent": false, "error": sanitize(err.Error())}
			tellFail = fmt.Sprintf("the upload is done, but %s was not told (%s); tell it with: ks agent send %s %s --session %s",
				firstNonEmpty(agent.Name, agentName), sanitize(err.Error()), firstNonEmpty(agent.Name, agentName), shellWord(text), sess.ShortID)
		}
	}
	emit(res, func() {
		if res.Replaced {
			fmt.Printf("replaced the existing %s\n", visible(res.Path))
		}
		fmt.Printf("Uploaded %s (%s) to %s (session %s, sha256 %s, checked). %s\n", visible(p.Name), size, visible(res.Path), sess.ShortID, short64(p.SHA256), uploadStorageNote)
		if tellLine != "" {
			fmt.Println(tellLine)
		}
	})
	if tellFail != "" {
		progress("%s", tellFail)
	}
}
