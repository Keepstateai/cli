// ks file download: one workspace file of any size, written to a new local
// file only when its bytes are the ones the service stated.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// downloadCtl answers the download route and hands everything else to the
// inspection fake.
type downloadCtl struct {
	*inspectCtl
	big []byte
}

func (c *downloadCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v2/sessions/session_1/files/download" {
		c.inspectCtl.ServeHTTP(w, r)
		return
	}
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	stopped := c.stopped
	c.mu.Unlock()
	refuse := func(code int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"schema_version":2,"error":{"code":%q,"type":%q,"message":%q,"work_started":"no"}}`, typ, typ, msg)
	}
	serve := func(stated []byte, sent []byte, sha string) {
		if sha == "" {
			s := sha256.Sum256(stated)
			sha = hex.EncodeToString(s[:])
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-KS-Size", fmt.Sprint(len(stated)))
		w.Header().Set("X-KS-SHA256", sha)
		w.Header().Set("Content-Length", fmt.Sprint(len(stated)))
		w.WriteHeader(200)
		_, _ = w.Write(sent)
	}
	if stopped {
		refuse(409, "ks_session_not_running", "the session is not running, and it is not woken to be inspected; resume it first")
		return
	}
	switch r.URL.Query().Get("path") {
	case "dist/big.bin":
		serve(c.big, c.big, "")
	case "short.bin":
		serve(c.big, c.big[:len(c.big)/2], "")
	case "changed.bin":
		serve(c.big, c.big, strings.Repeat("0", 64))
	case "unstated.bin":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("bytes with no stated size"))
	case ".env":
		refuse(403, "ks_path_redacted", ".env is a secret file")
	case "src":
		refuse(409, "ks_path_not_a_file", "src is a directory")
	default:
		refuse(404, "ks_path_absent", "that file does not exist")
	}
}

func TestFileDownloadWritesOnlyTheStatedBytes(t *testing.T) {
	big := make([]byte, 3<<20+17)
	for i := range big {
		big[i] = byte(i*7 + i/251)
	}
	c := &downloadCtl{inspectCtl: &inspectCtl{}, big: big}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	noPartials := func(when string) {
		t.Helper()
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".ks-partial") {
				t.Errorf("%s: a partial file was left: %s", when, e.Name())
			}
		}
	}

	// any size: 3 MiB, past file show's bound, to the file's own name here
	out, errs, code := run("file", "download", "dist/big.bin", "--session", "fleetins")
	if code != 0 || !strings.Contains(out, "wrote 3,145,745 bytes to big.bin") {
		t.Fatalf("download: %d %q\n%s", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big.bin")); string(b) != string(big) {
		t.Fatalf("the bytes written are not the file's (%d of %d bytes)", len(b), len(big))
	}
	// never replaces a file
	if _, errs, code := run("file", "download", "dist/big.bin", "--session", "fleetins"); code != exitConflict || !strings.Contains(errs, "already exists") {
		t.Fatalf("over an existing file: %d\n%s", code, errs)
	}
	if out, errs, code := run("file", "download", "dist/big.bin", "--session", "fleetins", "--out", "copy.bin", "--json"); code != 0 || !strings.Contains(out, `"sha256"`) || !strings.Contains(out, `"copy.bin"`) {
		t.Fatalf("--out --json: %d %q\n%s", code, out, errs)
	}
	// a body shorter than stated, a sha256 that does not match, or no stated
	// size: nothing written, no partial file left
	for _, p := range []string{"short.bin", "changed.bin", "unstated.bin"} {
		_, errs, code := run("file", "download", p, "--session", "fleetins", "--out", "x-"+p)
		if code == 0 {
			t.Errorf("%s: downloaded\n%s", p, errs)
		}
		if _, err := os.Stat(filepath.Join(dir, "x-"+p)); err == nil {
			t.Errorf("%s: a file was written", p)
		}
		if !strings.Contains(errs, "othing was written") {
			t.Errorf("%s: does not say nothing was written:\n%s", p, errs)
		}
		noPartials(p)
	}
	if _, _, code := run("file", "download", "short.bin", "--session", "fleetins", "--out", "s.bin"); code != exitTemporary {
		t.Errorf("an interrupted download: exit %d, want %d", code, exitTemporary)
	}
	if _, _, code := run("file", "download", "changed.bin", "--session", "fleetins", "--out", "c.bin"); code != exitIntegrity {
		t.Errorf("different bytes: exit %d, want %d", code, exitIntegrity)
	}
	// refusals: a secret, a directory, an absent file, an escape (before asking)
	if _, errs, code := run("file", "download", ".env", "--session", "fleetins", "--out", "env"); code != exitIntegrity || !strings.Contains(errs, "never returned") {
		t.Errorf("secret: %d\n%s", code, errs)
	}
	if _, errs, code := run("file", "download", "src", "--session", "fleetins", "--out", "src.out"); code != exitUsage || !strings.Contains(errs, "ks file list") {
		t.Errorf("directory: %d\n%s", code, errs)
	}
	if _, _, code := run("file", "download", "gone", "--session", "fleetins"); code != exitFailed {
		t.Errorf("absent: %d", code)
	}
	before := len(c.seen())
	if _, errs, code := run("file", "download", "../../etc/passwd", "--session", "fleetins"); code != exitIntegrity || !strings.Contains(errs, "not a path inside the workspace") {
		t.Errorf("escape: %d\n%s", code, errs)
	}
	for _, s := range c.seen()[before:] {
		if strings.Contains(s, "/files") {
			t.Fatalf("a refused path reached the service: %s", s)
		}
	}
	// a session that is not running: refused, never woken
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	if _, errs, code := run("file", "download", "dist/big.bin", "--session", "fleetins", "--out", "p.bin"); code != exitConflict || !strings.Contains(errs, "never wakes") {
		t.Fatalf("stopped: %d\n%s", code, errs)
	}
	for _, s := range c.seen() {
		if !strings.HasPrefix(s, "GET") {
			t.Fatalf("a download made a change: %s", s)
		}
	}
	noPartials("the end")
}
