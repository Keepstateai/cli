// BACKLOG-627: ks agent files, an agent's reach into the files of the agent
// it is connected to (the server's BACKLOG-624) and the person's setting of
// it (BACKLOG-623). Keyless: a fake control plane, no model, no machine.
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
	"strconv"
	"strings"
	"sync"
	"testing"
)

type filesCtl struct {
	mu       sync.Mutex
	calls    []string
	puts     []map[string]any
	grants   []map[string]any
	badSum   bool
	refusal  string // a path the fake refuses, and with which code
	refCode  string
	refState int
}

const filesNotes = "hello from the adviser\n"

func (c *filesCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	grants := c.grants
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	sess := func(id, short, name, rec string) map[string]any {
		return map[string]any{"id": id, "short_id": short, "name": name, "runtime_state": "running", "record_id": rec,
			"agent_activity": "ready", "task_state": "none", "key_alias": "unbound", "observed_at": "x", "last_activity_at": "x",
			"created_at": "x", "image": "claude", "budget_tokens": 1, "execution_epoch": 1}
	}
	agent := func(id, sessRec string) map[string]any {
		return map[string]any{"id": id, "session_id": sessRec, "name": "main", "is_primary": true, "activity": "ready"}
	}
	q := r.URL.Query()
	base := "/api/v2/agents/agent_src/advisers/agent_rev/files"
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"agents.files","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions" && q.Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{
			sess("fleetsrc00000000000000000000aaaa", "fleetsrc0000", "checkout", "session_src"),
			sess("fleetrev00000000000000000000bbbb", "fleetrev0000", "reviewer", "session_rev"),
		}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		switch q.Get("session_id") {
		case "session_src":
			env(200, map[string]any{"items": []any{agent("agent_src", "session_src")}})
		case "session_rev":
			env(200, map[string]any{"items": []any{agent("agent_rev", "session_rev")}})
		default:
			env(200, map[string]any{"items": []any{}})
		}
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents/agent_rev":
		env(200, agent("agent_rev", "session_rev"))
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_rev":
		env(200, map[string]any{"id": "session_rev", "name": "reviewer", "project_id": "proj_1"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_src":
		env(200, map[string]any{"id": "session_src", "name": "checkout", "project_id": "proj_1"})
	case strings.HasPrefix(r.URL.Path, base) && c.refusal != "" && q.Get("path") == c.refusal:
		w.WriteHeader(c.refState)
		fmt.Fprintf(w, `{"schema_version":2,"error":{"code":%q,"type":%q,"message":"the service says no","work_started":"no"}}`, c.refCode, c.refCode)
	case r.URL.Path == base:
		env(200, map[string]any{"session_id": "session_rev", "root": "workspace", "path": q.Get("path"), "is_dir": true, "truncated": false, "total": 2,
			"entries": []map[string]any{{"name": "notes.txt", "type": "file", "size": len(filesNotes), "mode": "644", "mtime": 1}, {"name": "src", "type": "dir", "size": 0, "mode": "755", "mtime": 1}}})
	case r.URL.Path == base+"/content":
		b := []byte(filesNotes)
		sum := sha256.Sum256(b)
		env(200, map[string]any{"session_id": "session_rev", "path": q.Get("path"), "size": len(b), "returned": len(b), "truncated": false,
			"sha256_returned": hex.EncodeToString(sum[:]), "content_b64": base64.StdEncoding.EncodeToString(b)})
	case r.URL.Path == base+"/download":
		b := []byte(filesNotes)
		sum := sha256.Sum256(b)
		hexs := hex.EncodeToString(sum[:])
		if c.badSum {
			hexs = strings.Repeat("0", 64)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("X-KS-Size", strconv.Itoa(len(b)))
		w.Header().Set("X-KS-SHA256", hexs)
		_, _ = w.Write(b)
	case r.URL.Path == "/api/v2/agents/agent_src/file-fetches":
		env(200, map[string]any{"items": []map[string]any{
			{"id": "aff_1", "grant_id": "grant_0001", "asker_agent_id": "agent_src", "adviser_agent_id": "agent_rev", "op": "download", "path": "notes.txt", "bytes": len(filesNotes), "sha256": "ab", "outcome": "ok", "at": "2026-11-09T10:00:00Z", "asker_name": "main", "adviser_name": "main"},
			{"id": "aff_2", "grant_id": "grant_0001", "asker_agent_id": "agent_src", "adviser_agent_id": "agent_rev", "op": "content", "path": "secret.txt", "bytes": 0, "outcome": "refused", "code": "ks_file_credential", "at": "2026-11-09T10:01:00Z", "asker_name": "main", "adviser_name": "main"},
		}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/adviser-grants":
		env(200, map[string]any{"items": grants, "next_cursor": ""})
	case r.Method == "PUT" && r.URL.Path == "/api/v2/adviser-grants/grant_0001/files":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.puts = append(c.puts, body)
		c.mu.Unlock()
		env(200, map[string]any{"id": "grant_0001", "source_agent_id": "agent_src", "target_agent_id": "agent_rev", "files": body["files"], "live": true,
			"source_agent_name": "main", "source_session_name": "checkout", "target_agent_name": "main", "target_session_name": "reviewer"})
	default:
		env(404, nil)
	}
}

func (c *filesCtl) fileCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, x := range c.calls {
		if strings.Contains(x, "/advisers/agent_rev/files") {
			n++
		}
	}
	return n
}

func filesEnv(t *testing.T, c *filesCtl) (bin, cfg, dir string, env []string) {
	t.Helper()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg = buildAndAuth(t, srv)
	return bin, cfg, t.TempDir(), fastEnv(cfg)
}

func liveGrant() map[string]any {
	return map[string]any{"id": "grant_0001", "source_agent_id": "agent_src", "target_agent_id": "agent_rev", "scope": "advice", "live": true, "files": "read",
		"source_agent_name": "main", "source_session_name": "checkout", "target_agent_name": "main", "target_session_name": "reviewer"}
}

func TestAgentFilesListShowsTheAdvisersFolder(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "list", "reviewer/main", "--session", "fleetsrc0000")
	if code != 0 || !strings.Contains(out, "notes.txt") || !strings.Contains(out, "src/") {
		t.Fatalf("list: %d out=%q err=%q", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "agent", "files", "list", "reviewer/main", "src", "--session", "fleetsrc0000", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil || code != 0 || m["data"].(map[string]any)["path"] != "src" {
		t.Fatalf("list --json: %d %s", code, out)
	}
}

func TestAgentFilesGetLandsInSharedInAndIsChecked(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	_, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--session", "fleetsrc0000")
	if code != 0 {
		t.Fatalf("get: %d %s", code, errs)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".shared-in", "reviewer-main", "notes.txt"))
	if err != nil || string(got) != filesNotes {
		t.Fatalf("the file did not land in .shared-in/reviewer-main: %v %q", err, got)
	}
	// a second get never replaces what is there
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--session", "fleetsrc0000"); code != exitConflict || !strings.Contains(errs, "already exists") {
		t.Fatalf("a second get: %d %s", code, errs)
	}
	// --to names another place; the bytes are the ones described
	to := filepath.Join(dir, "mine.txt")
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--to", to, "--session", "fleetsrc0000"); code != 0 {
		t.Fatalf("get --to: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(to); string(b) != filesNotes {
		t.Fatalf("--to wrote %q", b)
	}
}

func TestAgentFilesGetNeverKeepsBytesThatAreNotTheDescribedOnes(t *testing.T) {
	c := &filesCtl{badSum: true}
	bin, cfg, dir, env := filesEnv(t, c)
	_, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--session", "fleetsrc0000")
	if code == 0 || !strings.Contains(errs, "nothing was written") {
		t.Fatalf("a wrong digest: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, ".shared-in", "reviewer-main", "notes.txt")); err == nil {
		t.Fatal("the file exists after a digest mismatch")
	}
	ents, _ := os.ReadDir(filepath.Join(dir, ".shared-in", "reviewer-main"))
	if len(ents) != 0 {
		t.Fatalf("a partial file was left: %v", ents)
	}
}

func TestAgentFilesRefusesAPathThatClimbsBeforeAskingAnything(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b"} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", p, "--session", "fleetsrc0000"); code != exitIntegrity || !strings.Contains(errs, "not a path inside the workspace") {
			t.Errorf("%q: %d %s", p, code, errs)
		}
	}
	if n := c.fileCalls(); n != 0 {
		t.Fatalf("%d requests reached the file routes for refused paths", n)
	}
}

func TestAgentFilesGetShowPrintsTextWithoutWritingAFile(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--show", "--session", "fleetsrc0000")
	if code != 0 || !strings.Contains(out, "hello from the adviser") {
		t.Fatalf("--show: %d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, ".shared-in")); err == nil {
		t.Fatal("--show wrote a file")
	}
}

func TestAgentFilesLogShowsEveryAsk(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "log", "--session", "fleetsrc0000")
	if code != 0 || !strings.Contains(out, "notes.txt") || !strings.Contains(out, "refused") || !strings.Contains(out, "ks_file_credential") {
		t.Fatalf("log: %d out=%q err=%q", code, out, errs)
	}
}

func TestAgentFilesAllowSetsTheConnectionsSetting(t *testing.T) {
	c := &filesCtl{grants: []map[string]any{liveGrant()}}
	bin, cfg, dir, env := filesEnv(t, c)
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "allow", "reviewer/main", "none", "--session", "fleetsrc0000"); code != 0 {
		t.Fatalf("allow none: %d %s", code, errs)
	}
	c.mu.Lock()
	puts := append([]map[string]any{}, c.puts...)
	c.mu.Unlock()
	if len(puts) != 1 || puts[0]["files"] != "none" {
		t.Fatalf("the service was sent %v", puts)
	}
	// a word that is no setting is refused before anything is sent
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "allow", "reviewer/main", "write", "--session", "fleetsrc0000"); code != exitUsage || !strings.Contains(errs, "read or none") {
		t.Fatalf("a bad setting: %d %s", code, errs)
	}
	// no live connection: the next step is named
	c2 := &filesCtl{}
	bin2, cfg2, dir2, env2 := filesEnv(t, c2)
	if _, errs, code := auditExec(t, bin2, cfg2, dir2, env2, "agent", "files", "allow", "reviewer/main", "read", "--session", "fleetsrc0000"); code == 0 || !strings.Contains(errs, "ks adviser connect") {
		t.Fatalf("no connection: %d %s", code, errs)
	}
}

func TestAgentFilesRefusalsNameTheNextStep(t *testing.T) {
	c := &filesCtl{refusal: "deny.txt", refCode: "ks_files_not_allowed", refState: 403}
	bin, cfg, dir, env := filesEnv(t, c)
	_, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "deny.txt", "--show", "--session", "fleetsrc0000")
	if code == 0 || !strings.Contains(errs, "ks agent files allow") {
		t.Fatalf("files not allowed: %d %s", code, errs)
	}
	c.mu.Lock()
	c.refCode, c.refState = "ks_no_connection", 404
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "deny.txt", "--show", "--session", "fleetsrc0000")
	if code == 0 || !strings.Contains(errs, "ks adviser connect") {
		t.Fatalf("no connection: %d %s", code, errs)
	}
	c.mu.Lock()
	c.refCode, c.refState = "ks_file_too_big", 413
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "deny.txt", "--show", "--session", "fleetsrc0000")
	if code == 0 || !strings.Contains(errs, "250 MiB") {
		t.Fatalf("too big: %d %s", code, errs)
	}
}

func TestAgentFilesGetNeverWritesThroughALink(t *testing.T) {
	c := &filesCtl{}
	bin, cfg, dir, env := filesEnv(t, c)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, ".shared-in")); err != nil {
		t.Fatal(err)
	}
	_, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "files", "get", "reviewer/main", "notes.txt", "--session", "fleetsrc0000")
	if code != exitIntegrity || !strings.Contains(errs, "is a link") {
		t.Fatalf("a link at .shared-in: %d %s", code, errs)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatalf("something was written through the link: %v", ents)
	}
}
