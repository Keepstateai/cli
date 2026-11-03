// ks file upload: one file or a folder, up to 1 GB, to a session's workspace,
// in chunks that resume, verified end to end by sha256, never replacing a
// path unless --replace says so.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeUpload is one upload as the fake guest holds it.
type fakeUpload struct {
	ID, Kind, To, SHA string
	Size, Content     int64
	Files             []map[string]any
	Replace           bool
	Got               []byte
	Placed            bool
}

// uploadCtl stands in for the control plane and the session's guest: the
// upload routes, the agent list and the instruction route; everything else
// goes to the inspection fake.
type uploadCtl struct {
	*inspectCtl
	chunk        int64
	free         int64
	ups          map[string]*fakeUpload
	workspace    map[string][]byte // what is placed
	failAt       int64             // a PUT at this offset drops its connection (-1: none)
	badChunks    int               // the next N chunks are answered as not matching
	staleDeclare bool              // declare answers received 0 whatever is held
	digestFail   bool
	declared     []map[string]any
	puts         []int64
	tasks        []string
	deletes      int
	delay        time.Duration // each chunk is answered this much later (the snapshots)
}

func newUploadCtl() *uploadCtl {
	return &uploadCtl{inspectCtl: &inspectCtl{}, chunk: 256 << 10, free: 2_000_000_000, ups: map[string]*fakeUpload{}, workspace: map[string][]byte{}, failAt: -1}
}

func (c *uploadCtl) view(u *fakeUpload) map[string]any {
	state := "receiving"
	if int64(len(u.Got)) == u.Size {
		state = "received"
	}
	if u.Placed {
		state = "placed"
	}
	v := map[string]any{"upload_id": u.ID, "session_id": "session_1", "kind": u.Kind, "to": u.To, "size": u.Size, "sha256": u.SHA,
		"content_bytes": u.Content, "files": len(u.Files), "received": len(u.Got), "state": state, "chunk_bytes": c.chunk,
		"limit_bytes": 1000000000, "free_bytes": c.free, "replace": u.Replace, "storage_note": "Uploaded files count toward your storage."}
	if u.Placed {
		n := len(u.Files)
		if u.Kind == "file" {
			n = 1
		}
		v["placed"] = map[string]any{"path": u.To, "kind": u.Kind, "bytes": u.Content, "files": n, "replaced": u.Replace}
	}
	return v
}

func (c *uploadCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := "/api/v2/sessions/session_1/uploads"
	isAgents := r.URL.Path == "/api/v2/agents" || strings.HasPrefix(r.URL.Path, "/api/v2/agents/")
	if !strings.HasPrefix(r.URL.Path, base) && !isAgents {
		c.inspectCtl.ServeHTTP(w, r)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string, extra map[string]any) {
		e := map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}
		for k, v := range extra {
			e[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": e})
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
		return
	case r.Method == "POST" && r.URL.Path == "/api/v2/agents/agent_1/tasks":
		var b struct {
			Text string `json:"text"`
			Sub  string `json:"submission_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.tasks = append(c.tasks, b.Text)
		env(map[string]any{"id": "tsk_1", "agent_id": "agent_1", "submission_id": b.Sub, "state": "queued"})
		return
	case isAgents:
		refuse(404, "ks_not_found", "no such agent route in this fake", nil)
		return
	}
	if c.stopped {
		refuse(409, "ks_session_not_running", "the session is not running; uploading never wakes it", nil)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
	parts := strings.Split(rest, "/")
	if rest == "" && r.Method == "POST" {
		var d map[string]any
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			refuse(400, "ks_upload_invalid", "the declaration is not JSON", nil)
			return
		}
		c.declared = append(c.declared, d)
		kind, _ := d["kind"].(string)
		to, _ := d["to"].(string)
		sha, _ := d["sha256"].(string)
		size := int64(d["size"].(float64))
		content := int64(d["content_bytes"].(float64))
		replace, _ := d["replace"].(bool)
		if content > 1000000000 {
			refuse(413, "ks_upload_too_large", "more than 1 GB", map[string]any{"size": content, "limit_bytes": 1000000000})
			return
		}
		if _, ok := c.workspace[to]; ok && !replace {
			refuse(409, "ks_path_exists", to+" already exists in the workspace", map[string]any{"path": to})
			return
		}
		if size+64<<20 > c.free {
			refuse(507, "ks_no_room", "no room", map[string]any{"free_bytes": c.free, "needed_bytes": size, "reserve_bytes": 64 << 20})
			return
		}
		h := sha256.Sum256([]byte(kind + "|" + to + "|" + fmt.Sprint(size) + "|" + sha))
		id := "upl_" + hex.EncodeToString(h[:])[:24]
		u, ok := c.ups[id]
		resumed := ok && len(u.Got) > 0
		if !ok {
			u = &fakeUpload{ID: id, Kind: kind, To: to, SHA: sha, Size: size, Content: content}
			if fs, ok := d["files"].([]any); ok {
				for _, f := range fs {
					u.Files = append(u.Files, f.(map[string]any))
				}
			}
			c.ups[id] = u
		}
		u.Replace = replace
		v := c.view(u)
		v["resumed"] = resumed
		if c.staleDeclare {
			v["received"], v["resumed"] = 0, false
		}
		env(v)
		return
	}
	u, ok := c.ups[parts[0]]
	if !ok {
		refuse(404, "ks_upload_absent", "no such upload in this session", nil)
		return
	}
	switch {
	case r.Method == "GET" && len(parts) == 1:
		env(c.view(u))
	case r.Method == "DELETE" && len(parts) == 1:
		delete(c.ups, u.ID)
		c.deletes++
		env(map[string]any{"upload_id": u.ID, "discarded": true})
	case r.Method == "PUT" && len(parts) == 2 && parts[1] == "bytes":
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		c.puts = append(c.puts, off)
		if off == c.failAt {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		if off != int64(len(u.Got)) {
			refuse(409, "ks_upload_offset", "the guest holds a different count", map[string]any{"received": len(u.Got)})
			return
		}
		b, _ := io.ReadAll(r.Body)
		if int64(len(b)) > c.chunk || len(b) == 0 || int64(len(b)) != r.ContentLength {
			refuse(400, "ks_upload_invalid", "bad chunk length", nil)
			return
		}
		s := sha256.Sum256(b)
		if c.badChunks > 0 || hex.EncodeToString(s[:]) != r.Header.Get("X-KS-Chunk-SHA256") {
			if c.badChunks > 0 {
				c.badChunks--
			}
			refuse(422, "ks_upload_chunk_mismatch", "the chunk's bytes do not match its sha256; nothing of it was kept", map[string]any{"received": len(u.Got)})
			return
		}
		u.Got = append(u.Got, b...)
		if c.delay > 0 {
			c.mu.Unlock()
			time.Sleep(c.delay)
			c.mu.Lock()
		}
		env(c.view(u))
	case r.Method == "POST" && len(parts) == 2 && parts[1] == "complete":
		if u.Placed {
			env(c.view(u))
			return
		}
		if int64(len(u.Got)) != u.Size {
			refuse(409, "ks_upload_incomplete", "bytes are missing", map[string]any{"received": len(u.Got)})
			return
		}
		s := sha256.Sum256(u.Got)
		if c.digestFail || hex.EncodeToString(s[:]) != u.SHA {
			delete(c.ups, u.ID)
			refuse(422, "ks_upload_digest_mismatch", "the bytes received are not the ones declared; they were discarded", nil)
			return
		}
		u.Placed = true
		c.workspace[u.To] = u.Got
		env(c.view(u))
	default:
		refuse(404, "ks_not_found", "no such upload route", nil)
	}
}

func (c *uploadCtl) with(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f()
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func uploadHarness(t *testing.T) (*uploadCtl, string, func(args ...string) (string, string, int)) {
	t.Helper()
	c := newUploadCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	env := append(fastEnv(cfg), "KS_UPLOAD_BACKOFF_MS=1")
	dir := t.TempDir()
	return c, dir, func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
}

func uploadsAsked(c *uploadCtl) bool {
	for _, s := range c.seen() {
		if strings.Contains(s, "/uploads") {
			return true
		}
	}
	return false
}

func TestFileUploadOneFileVerifiedAndNeverReplacing(t *testing.T) {
	c, dir, run := uploadHarness(t)
	data := randomBytes(1<<20+17, 1)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, errs, code := run("file", "upload", "data.bin", "--session", "fleetins")
	if code != 0 || !strings.Contains(out, "Uploaded data.bin (1 MB) to data.bin") || !strings.Contains(out, "count toward your storage") {
		t.Fatalf("upload: %d %q\n%s", code, out, errs)
	}
	sum := sha256.Sum256(data)
	c.with(func() {
		if !bytes.Equal(c.workspace["data.bin"], data) {
			t.Fatalf("the bytes placed are not the file's (%d of %d)", len(c.workspace["data.bin"]), len(data))
		}
		if c.declared[0]["sha256"] != hex.EncodeToString(sum[:]) || c.declared[0]["kind"] != "file" || c.declared[0]["mode"] != "0644" {
			t.Fatalf("declared: %v", c.declared[0])
		}
		if len(c.puts) != 5 { // 1 MiB + 17 in 256 KiB chunks
			t.Fatalf("chunks sent at %v", c.puts)
		}
	})
	// never replaces a path: refused, and --replace says so
	if _, errs, code := run("file", "upload", "data.bin", "--session", "fleetins"); code != exitConflict || !strings.Contains(errs, "already exists in the workspace") || !strings.Contains(errs, "--replace") || !strings.Contains(errs, "nothing was replaced") {
		t.Fatalf("over an existing path: %d\n%s", code, errs)
	}
	if out, errs, code := run("file", "upload", "data.bin", "--session", "fleetins", "--replace"); code != 0 || !strings.Contains(out, "replaced the existing data.bin") {
		t.Fatalf("--replace: %d %q\n%s", code, out, errs)
	}
	// --to, and --json
	out, errs, code = run("file", "upload", "data.bin", "--session", "fleetins", "--to", "in/sub/d.bin", "--json")
	var doc struct {
		Data struct {
			Path     string `json:"path"`
			Kind     string `json:"kind"`
			SHA256   string `json:"sha256"`
			UploadID string `json:"upload_id"`
			Bytes    int64  `json:"bytes"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Data.Path != "in/sub/d.bin" || doc.Data.Bytes != int64(len(data)) || doc.Data.SHA256 != hex.EncodeToString(sum[:]) || doc.Data.Kind != "file" || doc.Data.UploadID == "" {
		t.Fatalf("--json: %d %q\n%s", code, out, errs)
	}
}

func TestFileUploadRefusesBeforeSending(t *testing.T) {
	c, dir, run := uploadHarness(t)
	_ = os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=x\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "server.pem"), []byte("pem"), 0o600)
	// a target outside the workspace
	for _, to := range []string{"../escape", "/etc/passwd", "a/../../b", `a\b`, "~/x", ".", "a//b"} {
		_, errs, code := run("file", "upload", "ok.txt", "--session", "fleetins", "--to", to)
		if code != exitIntegrity || !strings.Contains(errs, "not a path inside the workspace") || !strings.Contains(errs, "othing was sent") {
			t.Errorf("--to %q: %d\n%s", to, code, errs)
		}
	}
	// the secret rule, on the local name and on the target name
	for _, args := range [][]string{{".env"}, {"server.pem"}, {"ok.txt", "--to", "keys/id_rsa"}, {"ok.txt", "--to", "credentials"}} {
		_, errs, code := run(append([]string{"file", "upload"}, append(args, "--session", "fleetins")...)...)
		if code != exitIntegrity || !strings.Contains(errs, "never uploaded or downloaded") {
			t.Errorf("secret %v: %d\n%s", args, code, errs)
		}
	}
	// more than 1 GB, a file and a folder (sparse: nothing is read)
	big := filepath.Join(dir, "big.iso")
	f, _ := os.Create(big)
	_ = f.Truncate(1_400_000_000)
	f.Close()
	_, errs, code := run("file", "upload", "big.iso", "--session", "fleetins")
	if code != exitConflict || !strings.Contains(errs, "big.iso is 1.4 GB (1,400,000,000 bytes); the limit is 1 GB (1,000,000,000 bytes)") || !strings.Contains(errs, "Nothing was sent") {
		t.Fatalf("too large: %d\n%s", code, errs)
	}
	os.MkdirAll(filepath.Join(dir, "huge"), 0o755)
	for _, n := range []string{"a.bin", "b.bin"} {
		f, _ := os.Create(filepath.Join(dir, "huge", n))
		_ = f.Truncate(600_000_000)
		f.Close()
	}
	_, errs, code = run("file", "upload", "huge", "--session", "fleetins")
	if code != exitConflict || !strings.Contains(errs, "huge is 1.2 GB (1,200,000,000 bytes)") {
		t.Fatalf("folder too large: %d\n%s", code, errs)
	}
	if uploadsAsked(c) {
		t.Fatalf("a refused upload reached the service: %v", c.seen())
	}
	// the service's refusals in plain words with the next command
	c.with(func() { c.free = 1000 })
	if _, errs, code := run("file", "upload", "ok.txt", "--session", "fleetins"); code != exitConflict || !strings.Contains(errs, "the session's disk has 1 KB free") || !strings.Contains(errs, "keeps 67 MB free for the agent") || !strings.Contains(errs, "ks file list --session") {
		t.Errorf("no room: %d\n%s", code, errs)
	}
	c.with(func() { c.free = 2_000_000_000; c.stopped = true })
	if _, errs, code := run("file", "upload", "ok.txt", "--session", "fleetins"); code != exitConflict || !strings.Contains(errs, "ks agent resume main --session") || !strings.Contains(errs, "uploading never wakes a session") {
		t.Errorf("not running: %d\n%s", code, errs)
	}
}

func TestFileUploadResumesWhereItStopped(t *testing.T) {
	c, dir, run := uploadHarness(t)
	data := randomBytes(2<<20, 2)
	_ = os.WriteFile(filepath.Join(dir, "two.bin"), data, 0o644)
	c.with(func() { c.failAt = 1 << 20 })
	_, errs, code := run("file", "upload", "two.bin", "--session", "fleetins")
	if code != exitTemporary || !strings.Contains(errs, "run the same command again to continue") || !strings.Contains(errs, "kept for 24 hours") {
		t.Fatalf("interrupted: %d\n%s", code, errs)
	}
	var first int
	c.with(func() {
		for _, u := range c.ups {
			if len(u.Got) != 1<<20 {
				t.Fatalf("the guest holds %d bytes after the interruption, want %d", len(u.Got), 1<<20)
			}
		}
		c.failAt = -1
		first = len(c.puts)
	})
	out, errs, code := run("file", "upload", "two.bin", "--session", "fleetins")
	if code != 0 || !strings.Contains(errs, "continuing an earlier upload: 1 MB of 2 MB are already there") {
		t.Fatalf("resume: %d %q\n%s", code, out, errs)
	}
	c.with(func() {
		again := c.puts[first:]
		if len(again) == 0 || again[0] != 1<<20 {
			t.Fatalf("the resumed upload sent from %v, want from %d (never from 0)", again, 1<<20)
		}
		if !bytes.Equal(c.workspace["two.bin"], data) {
			t.Fatalf("the resumed bytes are not the file's")
		}
	})
	// a stale count (the declaration says 0, the guest holds more): the
	// client asks and continues from what the guest holds
	data3 := randomBytes(1<<20, 3)
	_ = os.WriteFile(filepath.Join(dir, "three.bin"), data3, 0o644)
	c.with(func() { c.failAt = 512 << 10 })
	if _, _, code := run("file", "upload", "three.bin", "--session", "fleetins"); code != exitTemporary {
		t.Fatalf("interrupted three: %d", code)
	}
	c.with(func() { c.failAt = -1; c.staleDeclare = true; c.badChunks = 1; first = len(c.puts) })
	if out, errs, code := run("file", "upload", "three.bin", "--session", "fleetins"); code != 0 || !strings.Contains(out, "Uploaded three.bin") {
		t.Fatalf("stale count and a chunk that did not match: %d %q\n%s", code, out, errs)
	}
	c.with(func() {
		if !bytes.Equal(c.workspace["three.bin"], data3) {
			t.Fatalf("the bytes are not the file's")
		}
		c.staleDeclare = false
	})
	// the whole sha256 does not match: nothing placed, words say so
	_ = os.WriteFile(filepath.Join(dir, "four.bin"), []byte("four"), 0o644)
	c.with(func() { c.digestFail = true })
	if _, errs, code := run("file", "upload", "four.bin", "--session", "fleetins"); code != exitIntegrity || !strings.Contains(errs, "discarded") || !strings.Contains(errs, "nothing was placed") {
		t.Fatalf("digest mismatch: %d\n%s", code, errs)
	}
	c.with(func() {
		c.digestFail = false
		if _, ok := c.workspace["four.bin"]; ok {
			t.Fatal("a file whose bytes did not match was placed")
		}
	})
	// --discard: an unfinished upload is thrown away, nothing is sent
	_ = os.WriteFile(filepath.Join(dir, "five.bin"), randomBytes(1<<20, 5), 0o644)
	c.with(func() { c.failAt = 256 << 10 })
	run("file", "upload", "five.bin", "--session", "fleetins")
	c.with(func() { c.failAt = -1; first = len(c.puts) })
	if out, errs, code := run("file", "upload", "five.bin", "--session", "fleetins", "--discard"); code != 0 || !strings.Contains(out, "discarded the unfinished upload of five.bin") || !strings.Contains(out, "262 KB of 1 MB had arrived") {
		t.Fatalf("--discard: %d %q\n%s", code, out, errs)
	}
	c.with(func() {
		if len(c.puts) != first || c.deletes != 1 {
			t.Fatalf("--discard sent bytes (%v) or did not discard (%d)", c.puts[first:], c.deletes)
		}
	})
}

func TestFileUploadFolderUnderTheUploadPolicy(t *testing.T) {
	c, dir, run := uploadHarness(t)
	root := filepath.Join(dir, "myproj")
	write := func(rel string, b string, mode os.FileMode) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(b), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# my project\n", 0o644)
	write("src/main.go", "package main\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write("node_modules/left/index.js", "x", 0o644)
	write(".git/HEAD", "ref: refs/heads/main\n", 0o644)
	write(".env", "TOKEN=x\n", 0o600)
	write(".env.example", "TOKEN=\n", 0o644)
	write(".gitignore", "*.log\n", 0o644)
	write("debug.log", "noise\n", 0o644)
	if err := os.Symlink("README.md", filepath.Join(root, "readme-link")); err != nil {
		t.Fatal(err)
	}
	out, errs, code := run("file", "upload", "myproj", "--session", "fleetins")
	if code != 0 || !strings.Contains(out, "Uploaded myproj (") || !strings.Contains(out, "4 files) to myproj") {
		t.Fatalf("folder: %d %q\n%s", code, out, errs)
	}
	for _, want := range []string{"left out", "node_modules", ".git", ".env", ".env.example", "debug.log", "readme-link"} {
		if !strings.Contains(out+errs, want) {
			t.Errorf("the left-out summary does not name %s:\n%s\n%s", want, out, errs)
		}
	}
	var placed []byte
	var decl map[string]any
	c.with(func() { placed, decl = c.workspace["myproj"], c.declared[0] })
	// the tar carries exactly the declared files, in order, and nothing else
	tr := tar.NewReader(bytes.NewReader(placed))
	var names []string
	var content int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Fatalf("%s: type %c", h.Name, h.Typeflag)
		}
		if h.Name == "bin/run.sh" && h.Mode != 0o755 || h.Name != "bin/run.sh" && h.Mode != 0o644 {
			t.Errorf("%s: mode %o", h.Name, h.Mode)
		}
		names = append(names, h.Name)
		content += h.Size
	}
	files := decl["files"].([]any)
	if len(files) != len(names) {
		t.Fatalf("tar %v, declared %v", names, files)
	}
	for i, f := range files {
		if f.(map[string]any)["path"] != names[i] {
			t.Fatalf("entry %d: tar %s, declared %v", i, names[i], f)
		}
	}
	if strings.Join(names, ",") != ".gitignore,README.md,bin/run.sh,src/main.go" {
		t.Fatalf("carried %v", names)
	}
	if int64(decl["content_bytes"].(float64)) != content || decl["kind"] != "folder" {
		t.Fatalf("declared %v, content %d", decl, content)
	}
	// deterministic: the same folder declares the same bytes again
	if _, errs, code := run("file", "upload", "myproj", "--session", "fleetins", "--to", "copy", "--json"); code != 0 {
		t.Fatalf("second: %d\n%s", code, errs)
	}
	c.with(func() {
		if c.declared[1]["sha256"] != decl["sha256"] || c.declared[1]["size"] != decl["size"] {
			t.Fatalf("two packs of one folder differ: %v / %v", c.declared[1]["sha256"], decl["sha256"])
		}
	})
}

func TestFileUploadTellsTheAgentOnlyWhenAsked(t *testing.T) {
	c, dir, run := uploadHarness(t)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o644)
	if _, errs, code := run("file", "upload", "notes.txt", "--session", "fleetins"); code != 0 {
		t.Fatalf("upload: %d\n%s", code, errs)
	}
	c.with(func() {
		if len(c.tasks) != 0 {
			t.Fatalf("the agent was told without --tell: %v", c.tasks)
		}
	})
	out, errs, code := run("file", "upload", "notes.txt", "--session", "fleetins", "--to", "n2.txt", "--tell")
	if code != 0 || !strings.Contains(out, "told main: I uploaded notes.txt (6 bytes) to n2.txt in your workspace.") {
		t.Fatalf("--tell: %d %q\n%s", code, out, errs)
	}
	if _, errs, code := run("file", "upload", "notes.txt", "--agent", "main", "--session", "fleetins", "--to", "n3.txt", "--tell-text", "read n3.txt first"); code != 0 {
		t.Fatalf("--tell-text: %d\n%s", code, errs)
	}
	c.with(func() {
		if len(c.tasks) != 2 || c.tasks[0] != "I uploaded notes.txt (6 bytes) to n2.txt in your workspace." || c.tasks[1] != "read n3.txt first" {
			t.Fatalf("instructions: %q", c.tasks)
		}
	})
}

// TestFileUploadSnapshots drives the command in a pseudo-terminal and keeps
// the raw screens (rendered with pyte by the evidence script); opt in with
// KS_UPLOAD_SNAPSHOTS=<dir>.
func TestFileUploadSnapshots(t *testing.T) {
	outDir := os.Getenv("KS_UPLOAD_SNAPSHOTS")
	if outDir == "" {
		t.Skip("terminal snapshots of ks file upload; opt in with KS_UPLOAD_SNAPSHOTS=<dir>")
	}
	c := newUploadCtl()
	c.chunk, c.delay = 1<<20, 150*time.Millisecond
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	data := randomBytes(24<<20, 7)
	_ = os.WriteFile(filepath.Join(dir, "dataset.bin"), data, 0o644)
	root := filepath.Join(dir, "myproj")
	for rel, b := range map[string]string{"README.md": "# p\n", "src/app.js": "x\n", "node_modules/dep/index.js": "y", ".git/HEAD": "ref\n", ".env": "K=v\n", ".gitignore": "*.log\n", "run.log": "z"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(b), 0o644)
	}
	big := filepath.Join(dir, "disk.img")
	f, _ := os.Create(big)
	_ = f.Truncate(1_500_000_000)
	f.Close()
	env := map[string]string{"XDG_CONFIG_HOME": cfg, "TERM": "xterm-256color", "PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")}
	drive := func(name string, argv []string, steps [][]string) {
		spec := map[string]any{"argv": append([]string{bin}, argv...), "env": env, "rows": 12, "cols": 110, "timeout": 30, "steps": steps}
		b, _ := json.Marshal(spec)
		sp := filepath.Join(t.TempDir(), name+".json")
		_ = os.WriteFile(sp, b, 0o644)
		od := filepath.Join(outDir, name)
		os.MkdirAll(od, 0o755)
		cmd := exec.Command("python3", "testdata/ptydrive.py", sp, od)
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, o)
		}
	}
	drive("file-progress-and-result", []string{"file", "upload", filepath.Join(dir, "dataset.bin"), "--session", "fleetins"},
		[][]string{{"MB of 25.2 MB", "", "snap:progress"}, {"count toward your storage", "", "snap:result"}})
	drive("folder-left-out", []string{"file", "upload", root, "--session", "fleetins", "--tell"},
		[][]string{{"told main", "", "snap:result"}})
	drive("refusal-too-large", []string{"file", "upload", big, "--session", "fleetins"},
		[][]string{{"Nothing was sent", "", "snap:refusal"}})
	drive("refusal-exists", []string{"file", "upload", filepath.Join(dir, "dataset.bin"), "--session", "fleetins"},
		[][]string{{"Next:", "", "snap:refusal"}})
	c.with(func() { c.stopped = true })
	drive("refusal-not-running", []string{"file", "upload", root, "--session", "fleetins", "--to", "other"},
		[][]string{{"Next:", "", "snap:refusal"}})
}
