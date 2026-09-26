// KS-058 on the command line: file inspection and logs without a shell.
// QA-058-1 (a climbing, absolute or linked path reads nothing), QA-058-2
// (escape sequences and seeded tokens are shown safely), QA-058-3 (following
// the logs of a session that pauses ends, saying so, and never wakes it),
// VER-058-1's client half (the verbs send a path, a bound and a cursor,
// never a command).
package main

import (
	"crypto/sha256"
	"encoding/base64"
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
)

type inspectCtl struct {
	mu      sync.Mutex
	unavail bool
	stopped bool
	calls   []string
}

func (c *inspectCtl) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *inspectCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	unavail, stopped := c.unavail, c.stopped
	c.mu.Unlock()
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
	}
	content := func(path string, b []byte, sha string) {
		if sha == "" {
			s := sha256.Sum256(b)
			sha = hex.EncodeToString(s[:])
		}
		env(map[string]any{"session_id": "session_1", "path": path, "size": len(b), "returned": len(b), "sha256_returned": sha, "content_b64": base64.StdEncoding.EncodeToString(b)})
	}
	q := r.URL.Query()
	base := "/api/v2/sessions/session_1"
	switch {
	case r.URL.Path == "/api/capabilities":
		a := "available"
		if unavail {
			a = "unavailable"
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}],"limits":{}}}`, a)
	case r.URL.Path == "/api/v2/sessions" && q.Get("source") == "fleet":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetins0000000000000000000000001", "short_id": "fleetins0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case stopped && strings.HasPrefix(r.URL.Path, base+"/files"):
		refuse(409, "ks_session_not_running", "the session is not running, and it is not woken to be inspected; resume it first")
	case r.URL.Path == base+"/files":
		switch q.Get("path") {
		case "link":
			refuse(403, "ks_path_refused", "refused: a link out of the workspace")
		default:
			env(map[string]any{"session_id": "session_1", "root": "/workspace", "path": q.Get("path"), "is_dir": true, "total": 3, "entries": []any{
				map[string]any{"name": "notes.txt", "type": "file", "size": 12, "mode": "0644"},
				map[string]any{"name": ".env", "type": "file", "size": 40, "mode": "0600", "redacted": true},
				map[string]any{"name": "\x1b[31mred\x1b[0m", "type": "file", "size": 1, "mode": "0644"},
			}})
		}
	case r.URL.Path == base+"/files/content":
		switch q.Get("path") {
		case "notes.txt":
			content("notes.txt", []byte("hello \x1b]0;pwned\x07 \x1b[2J world\n"), "")
		case "bin":
			content("bin", []byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 0xff}, "")
		case "tampered":
			content("tampered", []byte("bytes"), strings.Repeat("0", 64))
		case ".env":
			refuse(403, "ks_path_redacted", ".env is a secret file")
		case "link":
			refuse(403, "ks_path_refused", "refused: a link out of the workspace")
		case "gone":
			refuse(404, "ks_path_absent", "gone does not exist")
		default:
			refuse(400, "ks_path_refused", "refused")
		}
	case r.URL.Path == base+"/logs":
		switch q.Get("cursor") {
		case "":
			env(map[string]any{"session_id": "session_1", "runtime_state": "running", "next_cursor": "c1",
				"entries": []any{
					map[string]any{"source": "setup", "time": "10:00:00", "kind": "provision", "text": "machine started \x1b[31mRED\x1b[0m"},
					map[string]any{"source": "agent", "time": "10:00:01", "level": "info", "kind": "runner", "text": "using key sk-ant-api03-abcdefghijklmnop"},
				},
				"agent_log": map[string]any{"status": "read"}, "gaps": []any{}, "follow": map[string]any{"ends": false}})
		case "c1":
			env(map[string]any{"session_id": "session_1", "runtime_state": "paused", "next_cursor": "c2",
				"entries":   []any{map[string]any{"source": "setup", "time": "10:05:00", "kind": "pause", "text": "saved and paused"}},
				"agent_log": map[string]any{"status": "unavailable", "detail": "the session is not running"}, "gaps": []any{"lines 40-52 of the agent log were not kept"},
				"follow": map[string]any{"ends": true, "why": "the session is paused; it is not woken to be followed"}})
		default:
			refuse(400, "ks_cursor_invalid", "that cursor is not one of this session's")
		}
	default:
		refuse(404, "ks_not_found", "no such route in this fake: "+r.URL.Path)
	}
}

func TestFileInspectionRefusesEscapesAndShowsSafely(t *testing.T) {
	c := &inspectCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	filesCalled := func() bool {
		for _, s := range c.seen() {
			if strings.Contains(s, "/files") {
				return true
			}
		}
		return false
	}

	// refused before anything is asked
	for _, p := range []string{"../../host-secret", "/etc/shadow", "a/../../b", `..\x`, "~/.ssh/id_rsa"} {
		if _, errs, code := run("file", "show", p, "--session", "fleetins"); code != exitIntegrity || !strings.Contains(errs, "not a path inside the workspace") {
			t.Errorf("%q: exit %d\n%s", p, code, errs)
		}
		if _, _, code := run("file", "list", p, "--session", "fleetins"); code != exitIntegrity {
			t.Errorf("list %q: exit %d", p, code)
		}
	}
	if filesCalled() {
		t.Fatalf("a refused path reached the service: %v", c.seen())
	}
	// refused by the service: a link, a secret, an absent path
	if _, errs, code := run("file", "show", "link", "--session", "fleetins"); code != exitIntegrity || !strings.Contains(errs, "link") {
		t.Errorf("link: %d\n%s", code, errs)
	}
	if _, errs, code := run("file", "show", ".env", "--session", "fleetins"); code != exitIntegrity || !strings.Contains(errs, "never returned") {
		t.Errorf("secret: %d\n%s", code, errs)
	}
	if _, _, code := run("file", "show", "gone", "--session", "fleetins"); code != exitFailed {
		t.Errorf("absent: %d", code)
	}
	// a listing: secrets marked, a hostile name shown as text
	out, errs, code := run("file", "list", "--session", "fleetins")
	if code != 0 || !strings.Contains(out, "[redacted") || strings.Contains(out, "\x1b") || !strings.Contains(out, `\x1b[31mred`) {
		t.Fatalf("list: %d %q\n%s", code, out, errs)
	}
	// content: escape sequences visible, never emitted
	out, _, code = run("file", "show", "notes.txt", "--session", "fleetins")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, `\x1b]0;pwned\x07`) {
		t.Fatalf("show: %d %q", code, out)
	}
	// not text: not printed; --out writes the exact bytes, never replacing
	out, _, code = run("file", "show", "bin", "--session", "fleetins")
	if code != 0 || !strings.Contains(out, "is not text") || strings.ContainsRune(out, 0) {
		t.Fatalf("binary: %d %q", code, out)
	}
	if _, errs, code := run("file", "show", "bin", "--session", "fleetins", "--out", "bin.out"); code != 0 {
		t.Fatalf("--out: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bin.out")); string(b) != string([]byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 0xff}) {
		t.Fatalf("--out bytes: %q", b)
	}
	if _, _, code := run("file", "show", "bin", "--session", "fleetins", "--out", "bin.out"); code != exitConflict {
		t.Fatalf("--out over an existing file: %d", code)
	}
	// bytes that do not match what the service stated: shown nowhere
	if out, errs, code := run("file", "show", "tampered", "--session", "fleetins"); code != exitIntegrity || strings.Contains(out, "bytes") {
		t.Fatalf("tampered: %d %s %s", code, out, errs)
	}
	// a session that is not running: refused, and nothing asked to wake it
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	if _, errs, code := run("file", "list", "--session", "fleetins"); code != exitConflict || !strings.Contains(errs, "never wakes") {
		t.Fatalf("stopped: %d\n%s", code, errs)
	}
	for _, s := range c.seen() {
		if strings.HasPrefix(s, "POST") || strings.Contains(s, "resume") || strings.Contains(s, "wake") {
			t.Fatalf("inspection asked for a change: %s", s)
		}
		if strings.Contains(s, "cmd") || strings.Contains(s, "exec") || strings.Contains(s, "command") {
			t.Fatalf("an inspection request carried a command: %s", s)
		}
	}
	// the capability is unavailable: said so, nothing read
	c.mu.Lock()
	c.unavail, c.stopped, c.calls = true, false, nil
	c.mu.Unlock()
	if _, errs, code := run("file", "list", "--session", "fleetins"); code != exitFailed || !strings.Contains(errs, "agent.workspace") || filesCalled() {
		t.Fatalf("unavailable: %d\n%s", code, errs)
	}
}

func TestLogsAreSafeAndFollowingEndsWithoutWaking(t *testing.T) {
	c := &inspectCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()

	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "logs", "--session", "fleetins")
	if code != 0 || strings.Contains(out, "\x1b") || strings.Contains(out, "abcdefghijklmnop") || !strings.Contains(out, "[redacted]") || !strings.Contains(out, "--cursor c1") {
		t.Fatalf("logs: %d %q\n%s", code, out, errs)
	}
	// following: the new lines, then the end, with why; nothing asked to wake
	out, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "logs", "--session", "fleetins", "--follow")
	if code != 0 || !strings.Contains(out, "saved and paused") || !strings.Contains(out, "following ended (session paused)") ||
		!strings.Contains(out, "not woken") || !strings.Contains(out, "agent log unavailable") || !strings.Contains(out, "gap: lines 40-52") {
		t.Fatalf("follow: %d %q\n%s", code, out, errs)
	}
	for _, s := range c.seen() {
		if !strings.HasPrefix(s, "GET") {
			t.Fatalf("following made a change: %s", s)
		}
	}
	// --follow --json: one object per line, the last one the end
	out, _, code = auditExec(t, bin, cfg, dir, env, "agent", "logs", "--session", "fleetins", "--follow", "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last map[string]any
	for _, l := range lines {
		if err := json.Unmarshal([]byte(l), &last); err != nil {
			t.Fatalf("not one object per line: %q", l)
		}
	}
	if code != 0 || last["type"] != "end" || last["runtime_state"] != "paused" {
		t.Fatalf("follow --json: %d %v", code, last)
	}
	// a cursor the service does not know
	if _, _, code := auditExec(t, bin, cfg, dir, env, "agent", "logs", "--session", "fleetins", "--cursor", "bogus"); code == 0 {
		t.Fatal("an unknown cursor succeeded")
	}
}
