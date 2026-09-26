// KS-056 on the command line: results listed and described, and a download
// whose named file appears only when its bytes are the recorded ones.
// QA-056-1 (checksum mismatch, partial download and a target race keep every
// pre-existing local file), QA-056-2 (a malicious archive fails before
// anything is extracted), VER-056-1 (the final name appears only when
// valid) and the refusals: an expired grant, the service's own digest
// refusal, the wrong role, an unavailable capability.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type resultCtl struct {
	mu         sync.Mutex
	content    []byte // what the service holds and serves
	recorded   []byte // what the result's record says (its size and sha256)
	name       string
	mode       string // ok | corrupt | cut | expired | digest500 | race | forbid
	unavail    bool
	raceTarget string
	grants     int
	tokens     map[string]bool
	ranges     []string // Range + " | " + If-Range of each content request
	calls      []string
}

func newResultCtl(content []byte) *resultCtl {
	return &resultCtl{content: content, recorded: content, name: "report.txt", mode: "ok", tokens: map[string]bool{}}
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (c *resultCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	mode, content, recorded := c.mode, c.content, c.recorded
	c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
	}
	meta := map[string]any{"id": "res_1", "session_id": "session_1", "agent_id": "agent_1", "task_id": "tsk_1", "attempt_id": "att_1",
		"name": c.name, "media_type": "text/plain", "bytes": len(recorded), "sha256": shaOf(recorded),
		"checks": []any{map[string]any{"name": "unit tests", "state": "passed"}}, "checks_declared_by": "producer",
		"task_verification_state": "not_requested", "state": "available", "revision": 1,
		"provenance": map[string]any{"worker_id": "w", "execution_epoch": 3, "attempt_index": 1, "runner_version": "2.1.251", "recorded_at": "x"}}
	switch {
	case r.URL.Path == "/api/capabilities":
		a := "available"
		if c.unavail {
			a = "unavailable"
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}],"limits":{}}}`, a)
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks/tsk_1/results":
		env(200, []any{meta})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetres0000000000000000000000001", "short_id": "fleetres0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		env(200, map[string]any{"items": []any{map[string]any{"id": "tsk_1", "agent_id": "agent_1", "state": "finished"}, map[string]any{"id": "tsk_gone", "agent_id": "agent_1", "state": "finished"}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/results/res_1":
		env(200, meta)
	case r.Method == "POST" && r.URL.Path == "/api/v2/results/res_1/grants":
		if mode == "forbid" {
			refuse(403, "ks_forbidden", "this needs the reader role on the session")
			return
		}
		c.mu.Lock()
		c.grants++
		tok := fmt.Sprintf("ksrg_test_%d", c.grants)
		c.tokens[tok] = true
		c.mu.Unlock()
		env(201, map[string]any{"grant_id": "grant_x", "result_id": "res_1", "token": tok, "expires_at": time.Now().Add(10 * time.Minute).Format(time.RFC3339), "header": "X-KS-Grant"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/results/res_1/content":
		c.mu.Lock()
		ok := c.tokens[r.Header.Get("X-KS-Grant")]
		c.ranges = append(c.ranges, r.Header.Get("Range")+" | "+r.Header.Get("If-Range"))
		c.mu.Unlock()
		switch {
		case !ok:
			refuse(403, "ks_grant_invalid", "this grant does not permit this download")
			return
		case mode == "expired":
			refuse(403, "ks_grant_expired", "this grant expired; ask for a new one")
			return
		case mode == "digest500":
			refuse(500, "ks_digest_mismatch", "the stored bytes no longer match their recorded digest, so they are not served")
			return
		case mode == "race":
			_ = os.WriteFile(c.raceTarget, []byte("precious"), 0o600)
		case mode == "corrupt":
			content = bytes.Repeat([]byte("X"), len(content))
		}
		w.Header().Set("X-KS-Sha256", shaOf(recorded))
		w.Header().Set("ETag", `"sha256:`+shaOf(content)+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		if mode == "cut" {
			w.Header().Set("Content-Length", fmt.Sprint(len(content)))
			w.WriteHeader(200)
			_, _ = w.Write(content[:len(content)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	default:
		refuse(404, "ks_not_found", "no such route in this fake: "+r.URL.Path)
	}
}

func (c *resultCtl) set(f func(c *resultCtl)) {
	c.mu.Lock()
	f(c)
	c.mu.Unlock()
}

func (c *resultCtl) grantCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grants
}

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func noPartials(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".ks-partial-") {
			t.Errorf("a partial file was left behind: %s", e.Name())
		}
	}
}

func TestResultListAndShowLabelWhoSaysWhat(t *testing.T) {
	c := newResultCtl([]byte("the report\n"))
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "result", "list", "--task", "tsk_1")
	if code != 0 || !strings.Contains(out, "res_1") || !strings.Contains(out, "not_requested") {
		t.Fatalf("list: %d\n%s%s", code, out, errs)
	}
	// a session's results: every task's, and a task whose results could not
	// be read is named as unread and fails the command, never shown as empty
	out, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "result", "list", "--session", "fleetres", "--agent", "main")
	if code != exitFailed || !strings.Contains(out, "res_1") || !strings.Contains(out, "UNREAD  task tsk_gone") {
		t.Fatalf("list --session: %d\n%s%s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "result", "show", "res_1")
	if code != 0 || !strings.Contains(out, shaOf(c.content)) || !strings.Contains(out, "declared by the producer, not verified") || !strings.Contains(out, "att_1") {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if c.grantCount() != 0 {
		t.Fatal("reading metadata asked for a download grant")
	}
}

// QA-056-1 / VER-056-1, one step at a time against one directory that also
// holds a file the user had before.
func TestADownloadAppearsOnlyWhenVerifiedAndNeverOverwrites(t *testing.T) {
	content := []byte(strings.Repeat("result bytes, line by line\n", 4000))
	c := newResultCtl(content)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "mine.txt"), "the user's own file")
	dl := func(args ...string) (string, string, int) {
		return auditExec(t, bin, cfg, dir, env, append([]string{"result", "download", "res_1"}, args...)...)
	}

	// a clean download: the file is the recorded bytes, and nothing else is left
	out, errs, code := dl()
	if code != 0 || readT(t, filepath.Join(dir, "report.txt")) != string(content) || !strings.Contains(out, "verified") || !strings.Contains(out, "Nothing was run") {
		t.Fatalf("download: %d\n%s%s", code, out, errs)
	}
	noPartials(t, dir)

	// the same again: refused before a grant is even asked for
	before := c.grantCount()
	if _, errs, code := dl(); code != exitConflict || !strings.Contains(errs, "already exists") || c.grantCount() != before {
		t.Fatalf("existing target: %d\n%s", code, errs)
	}

	// a checksum mismatch, with and without --force: nothing replaced, nothing new
	writeFileT(t, filepath.Join(dir, "report.txt"), "an older report")
	c.set(func(c *resultCtl) { c.mode = "corrupt" })
	if _, errs, code := dl("--force"); code != exitIntegrity || !strings.Contains(errs, "not the recorded result") {
		t.Fatalf("corrupt --force: %d\n%s", code, errs)
	}
	if readT(t, filepath.Join(dir, "report.txt")) != "an older report" {
		t.Fatal("a corrupt download replaced the existing file")
	}
	if _, _, code := dl("--out", "fresh.txt"); code != exitIntegrity {
		t.Fatalf("corrupt fresh: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.txt")); err == nil {
		t.Fatal("a corrupt download created its target")
	}
	noPartials(t, dir)

	// a partial download: nothing at the target, the incomplete bytes kept
	// under a partial name; the same command resumes with Range + If-Range
	c.set(func(c *resultCtl) { c.mode = "cut" })
	if _, errs, code := dl("--out", "resumed.txt"); code != exitTemporary || !strings.Contains(errs, "run the same command again to resume") {
		t.Fatalf("cut: %d\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "resumed.txt")); err == nil {
		t.Fatal("an interrupted download created its target")
	}
	partial := partialPath(filepath.Join(dir, "resumed.txt"), "res_1")
	fi, err := os.Stat(partial)
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(content)) {
		t.Fatalf("the partial file: %v %v", fi, err)
	}
	c.set(func(c *resultCtl) { c.mode = "ok"; c.ranges = nil })
	out, errs, code = dl("--out", "resumed.txt", "--json")
	if code != 0 || readT(t, filepath.Join(dir, "resumed.txt")) != string(content) {
		t.Fatalf("resume: %d\n%s%s", code, out, errs)
	}
	var doc struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &doc)
	if doc.Data["resumed_from"] != float64(fi.Size()) || doc.Data["verified"] != true {
		t.Fatalf("resume receipt: %v", doc.Data)
	}
	if len(c.ranges) != 1 || c.ranges[0] != fmt.Sprintf("bytes=%d- | \"sha256:%s\"", fi.Size(), shaOf(content)) {
		t.Fatalf("the resume request: %v", c.ranges)
	}
	noPartials(t, dir)

	// a partial file whose bytes are NOT a prefix of the result: the remainder
	// is fetched, the whole fails verification, and it is removed; the next
	// attempt starts clean
	writeFileT(t, partialPath(filepath.Join(dir, "garbled.txt"), "res_1"), "not the start of it")
	if _, errs, code := dl("--out", "garbled.txt"); code != exitIntegrity || !strings.Contains(errs, "partial download was removed") {
		t.Fatalf("garbled partial: %d\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "garbled.txt")); err == nil {
		t.Fatal("a garbled partial became a target")
	}
	if _, _, code := dl("--out", "garbled.txt"); code != 0 || readT(t, filepath.Join(dir, "garbled.txt")) != string(content) {
		t.Fatalf("after a garbled partial: %d", code)
	}

	// the target appears between the check and the finalization: refused,
	// the newcomer untouched, nothing left behind
	c.set(func(c *resultCtl) { c.mode = "race"; c.raceTarget = filepath.Join(dir, "raced.txt") })
	if _, errs, code := dl("--out", "raced.txt"); code != exitConflict || !strings.Contains(errs, "appeared while the result was downloading") {
		t.Fatalf("race: %d\n%s", code, errs)
	}
	if readT(t, filepath.Join(dir, "raced.txt")) != "precious" {
		t.Fatal("the file that appeared during the download was replaced")
	}
	noPartials(t, dir)

	// a link at the partial path is never followed
	outside := filepath.Join(t.TempDir(), "victim")
	writeFileT(t, outside, "do not touch")
	c.set(func(c *resultCtl) { c.mode = "ok" })
	if err := os.Symlink(outside, partialPath(filepath.Join(dir, "linked.txt"), "res_1")); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := dl("--out", "linked.txt"); code != exitIntegrity || !strings.Contains(errs, "not a regular file") {
		t.Fatalf("linked partial: %d\n%s", code, errs)
	}
	if readT(t, outside) != "do not touch" {
		t.Fatal("a download wrote through a link")
	}
	os.Remove(partialPath(filepath.Join(dir, "linked.txt"), "res_1"))

	if readT(t, filepath.Join(dir, "mine.txt")) != "the user's own file" {
		t.Fatal("a pre-existing file changed")
	}
}

// The service's refusals: an expired grant, bytes the service itself found
// corrupted, the wrong role, and a capability that is not available.
func TestADownloadRefusalWritesNothing(t *testing.T) {
	c := newResultCtl([]byte("bytes"))
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	cases := []struct {
		mode string
		exit int
		want string
	}{
		{"expired", exitAuth, "ks_grant_expired"},
		{"digest500", exitIntegrity, "ks_digest_mismatch"},
		{"forbid", exitAuth, "ks_forbidden"},
	}
	for _, tc := range cases {
		c.set(func(c *resultCtl) { c.mode = tc.mode })
		out, _, code := auditExec(t, bin, cfg, dir, env, "result", "download", "res_1", "--json")
		if code != tc.exit || !strings.Contains(out, tc.want) {
			t.Errorf("%s: exit %d\n%s", tc.mode, code, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "report.txt")); err == nil {
			t.Fatalf("%s: a refused download wrote its target", tc.mode)
		}
		noPartials(t, dir)
	}
	c.set(func(c *resultCtl) { c.mode = "ok"; c.unavail = true; c.calls = nil })
	_, errs, code := auditExec(t, bin, cfg, dir, env, "result", "download", "res_1")
	if code != exitFailed || !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "unavailable") {
		t.Fatalf("unavailable: %d\n%s", code, errs)
	}
	for _, call := range c.calls {
		if strings.Contains(call, "/results") {
			t.Fatalf("a disabled verb reached the service: %v", c.calls)
		}
	}
}

// ---- archives -------------------------------------------------------------------

type tarEntry struct {
	name     string
	typ      byte
	body     string
	linkname string
	mode     int64
}

func makeTar(t *testing.T, gz bool, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var g *gzip.Writer
	if gz {
		g = gzip.NewWriter(&buf)
		w = tar.NewWriter(g)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.linkname, Format: tar.FormatPAX}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = w.Write([]byte(e.body))
		}
	}
	_ = w.Close()
	if g != nil {
		_ = g.Close()
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entries map[string]os.FileMode) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, mode := range entries {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("x"))
	}
	_ = w.Close()
	return buf.Bytes()
}

// QA-056-2: every unsafe entry is refused before anything is written: the
// destination directory never exists afterwards and nothing appears beside
// it.
func TestAMaliciousArchiveExtractsNothing(t *testing.T) {
	bad := map[string][]byte{
		"traversal":      makeTar(t, true, tarEntry{name: "ok.txt", typ: tar.TypeReg, body: "fine"}, tarEntry{name: "../evil.txt", typ: tar.TypeReg, body: "evil"}),
		"deep traversal": makeTar(t, false, tarEntry{name: "a/../../evil.txt", typ: tar.TypeReg, body: "evil"}),
		"absolute":       makeTar(t, false, tarEntry{name: "/tmp/evil.txt", typ: tar.TypeReg, body: "evil"}),
		"symlink":        makeTar(t, true, tarEntry{name: "ok.txt", typ: tar.TypeReg, body: "fine"}, tarEntry{name: "link", typ: tar.TypeSymlink, linkname: "/etc/passwd"}),
		"hard link":      makeTar(t, false, tarEntry{name: "ok.txt", typ: tar.TypeReg, body: "fine"}, tarEntry{name: "hl", typ: tar.TypeLink, linkname: "ok.txt"}),
		"device":         makeTar(t, false, tarEntry{name: "dev", typ: tar.TypeChar}),
		"fifo":           makeTar(t, false, tarEntry{name: "pipe", typ: tar.TypeFifo}),
		"duplicate":      makeTar(t, false, tarEntry{name: "a.txt", typ: tar.TypeReg, body: "1"}, tarEntry{name: "./a.txt", typ: tar.TypeReg, body: "2"}),
		"below a file":   makeTar(t, false, tarEntry{name: "a", typ: tar.TypeReg, body: "1"}, tarEntry{name: "a/b", typ: tar.TypeReg, body: "2"}),
		"backslash":      makeTar(t, false, tarEntry{name: `..\evil.txt`, typ: tar.TypeReg, body: "evil"}),
		"zip traversal":  makeZip(t, map[string]os.FileMode{"../evil.txt": 0o644}),
		"zip symlink":    makeZip(t, map[string]os.FileMode{"link": os.ModeSymlink | 0o777}),
		"not an archive": []byte("just some text, not an archive at all"),
	}
	for name, archive := range bad {
		parent := t.TempDir()
		src := filepath.Join(t.TempDir(), "a")
		if err := os.WriteFile(src, archive, 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(parent, "out")
		if _, err := extractArchive(src, dest); err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Lstat(dest); err == nil {
			t.Errorf("%s: the destination was created", name)
		}
		if ents, _ := os.ReadDir(parent); len(ents) != 0 {
			t.Errorf("%s: something appeared beside it: %v", name, ents)
		}
	}
	// the size bound, measured by bytes, not by a count
	big := makeTar(t, true, tarEntry{name: "big", typ: tar.TypeReg, body: strings.Repeat("0", extractMaxBytes+1)})
	src := filepath.Join(t.TempDir(), "big")
	_ = os.WriteFile(src, big, 0o600)
	if _, err := extractArchive(src, filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Errorf("oversize: %v", err)
	}
}

func TestASafeArchiveExtractsIntoANewDirectory(t *testing.T) {
	archive := makeTar(t, true,
		tarEntry{name: "pkg/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "pkg/run.sh", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		tarEntry{name: "pkg/doc/readme.md", typ: tar.TypeReg, body: "hello"})
	src := filepath.Join(t.TempDir(), "a")
	_ = os.WriteFile(src, archive, 0o600)
	dest := filepath.Join(t.TempDir(), "out")
	n, err := extractArchive(src, dest)
	if err != nil || n != 2 {
		t.Fatalf("extract: %d %v", n, err)
	}
	if readT(t, filepath.Join(dest, "pkg", "doc", "readme.md")) != "hello" {
		t.Fatal("content")
	}
	fi, _ := os.Stat(filepath.Join(dest, "pkg", "run.sh"))
	if fi.Mode().Perm() != 0o755 || fi.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("mode %v: only the executable bit is kept", fi.Mode())
	}
	// an existing directory is never extracted into
	if _, err := extractArchive(src, dest); err == nil {
		t.Fatal("extracted into an existing directory")
	}
}

// --extract end to end: a verified archive result is unpacked into a new
// directory, and a malicious one is refused with nothing extracted and no
// archive kept (no --out was given).
func TestDownloadExtractEndToEnd(t *testing.T) {
	good := makeTar(t, true, tarEntry{name: "out.txt", typ: tar.TypeReg, body: "ok"})
	c := newResultCtl(good)
	c.name = "result.tar.gz"
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, env, "result", "download", "res_1", "--extract", "unpacked")
	if code != 0 || readT(t, filepath.Join(dir, "unpacked", "out.txt")) != "ok" || !strings.Contains(out, "extracted 1 file") {
		t.Fatalf("extract: %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.tar.gz")); err == nil {
		t.Fatal("the archive was kept although --out was not given")
	}
	evil := makeTar(t, true, tarEntry{name: "../escape.txt", typ: tar.TypeReg, body: "evil"})
	c.set(func(c *resultCtl) { c.content, c.recorded = evil, evil })
	_, errs, code = auditExec(t, bin, cfg, dir, env, "result", "download", "res_1", "--extract", "unpacked2")
	if code != exitIntegrity || !strings.Contains(errs, "refused before anything was extracted") {
		t.Fatalf("malicious: %d\n%s", code, errs)
	}
	for _, p := range []string{filepath.Join(dir, "unpacked2"), filepath.Join(dir, "escape.txt"), filepath.Join(filepath.Dir(dir), "escape.txt")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists after a refused extraction", p)
		}
	}
}
