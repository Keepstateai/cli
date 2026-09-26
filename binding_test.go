// KS-022 on the command line: which session a command means when it is not
// told, bindings kept per account, project root and control plane, a
// repository's own binding never trusted silently, renames by id against a
// revision, and the warning about starting a second session for work a
// bound one is already doing.
package main

import (
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

type bindSession struct {
	id, short, name, rec, state, account string
}

type bindCtl struct {
	mu        sync.Mutex
	sessions  []bindSession
	agentRev  map[string]int64 // agent id -> revision
	agentName map[string]string
	sessRev   map[string]int64
	sessName  map[string]string
	unavail   string // a capability this fake reports unavailable
	forbid    bool   // PATCH answered 403 ks_forbidden
	stale     bool   // PATCH answered 409 ks_revision_conflict
	calls     []string
	patches   []map[string]any
}

func newBindCtl() *bindCtl {
	return &bindCtl{
		sessions: []bindSession{
			{"fleetaaa1000000000000000000000001", "fleetaaa1000", "checkout", "session_a1", "running", "acct_a"},
			{"fleetaaa2000000000000000000000002", "fleetaaa2000", "billing", "session_a2", "parked", "acct_a"},
			{"fleetbbb1000000000000000000000003", "fleetbbb1000", "checkout", "session_b1", "running", "acct_b"},
		},
		agentRev:  map[string]int64{"agent_a1": 4, "agent_a2": 1, "agent_b1": 1},
		agentName: map[string]string{"agent_a1": "main", "agent_a2": "main", "agent_b1": "main"},
		sessRev:   map[string]int64{"session_a1": 7, "session_a2": 1, "session_b1": 1},
		sessName:  map[string]string{"session_a1": "checkout", "session_a2": "billing", "session_b1": "checkout"},
	}
}

func (c *bindCtl) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *bindCtl) reset() {
	c.mu.Lock()
	c.calls, c.patches = nil, nil
	c.mu.Unlock()
}

func (c *bindCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	acct := map[string]string{"Bearer tok-a": "acct_a", "Bearer tok-b": "acct_b"}[r.Header.Get("Authorization")]
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
	}
	agentOf := map[string]string{"session_a1": "agent_a1", "session_a2": "agent_a2", "session_b1": "agent_b1"}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/capabilities":
		var caps []string
		for _, id := range []string{"agent.workspace", "session.list", "api.v2.workspace"} {
			a := "available"
			if id == c.unavail {
				a = "unavailable"
			}
			caps = append(caps, fmt.Sprintf(`{"id":%q,"availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}`, id, a))
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[%s],"limits":{}}}`, strings.Join(caps, ","))
	case acct == "":
		refuse(401, "ks_unauthorized", "a bearer token is required")
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions" && q.Get("source") == "fleet":
		items := []map[string]any{}
		for _, s := range c.sessions {
			if s.account != acct {
				continue
			}
			items = append(items, map[string]any{"id": s.id, "short_id": s.short, "name": s.name, "runtime_state": s.state, "record_id": s.rec,
				"agent_activity": "working", "task_state": "running", "key_alias": "unbound", "observed_at": "x", "last_activity_at": "x", "created_at": "x"})
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		id := agentOf[q.Get("session_id")]
		env(200, map[string]any{"items": []any{map[string]any{"id": id, "session_id": q.Get("session_id"), "name": c.agentName[id], "is_primary": true, "activity": "ready", "revision": c.agentRev[id]}}})
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v2/agents/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/agents/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.patches = append(c.patches, body)
		switch {
		case c.forbid:
			refuse(403, "ks_forbidden", "this needs the operator role")
		case c.stale || int64(body["expected_revision"].(float64)) != c.agentRev[id]:
			refuse(409, "ks_revision_conflict", "the record changed since it was read; refresh before retrying")
		default:
			c.agentRev[id]++
			c.agentName[id] = body["name"].(string)
			env(200, map[string]any{"id": id, "name": c.agentName[id], "revision": c.agentRev[id]})
		}
	case strings.HasPrefix(r.URL.Path, "/api/v2/sessions/session_"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/sessions/")
		if r.Method == "PATCH" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.patches = append(c.patches, body)
			if c.stale || int64(body["expected_revision"].(float64)) != c.sessRev[id] {
				refuse(409, "ks_revision_conflict", "the record changed since it was read; refresh before retrying")
				return
			}
			c.sessRev[id]++
			c.sessName[id] = body["name"].(string)
		}
		env(200, map[string]any{"id": id, "name": c.sessName[id], "revision": c.sessRev[id]})
	case r.Method == "POST" && r.URL.Path == "/api/sessions":
		w.WriteHeader(201) // the legacy route answers the session itself, not an envelope
		fmt.Fprint(w, `{"id":"sess-new","image":"base","state":"running"}`)
	default:
		refuse(404, "ks_not_found", "no such route in this fake: "+r.URL.Path)
	}
}

// signIn writes the stored credential the way ks login does, with the
// account it belongs to.
func signIn(t *testing.T, cfg, ctl, token, account string) {
	t.Helper()
	dir := filepath.Join(cfg, "keepstate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"ctl": ctl, "token": token, "account_id": account})
	if err := os.WriteFile(filepath.Join(dir, "token.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// project makes a directory that is a project root (it holds .git).
func project(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func calledAgents(calls []string) bool {
	for _, c := range calls {
		if strings.Contains(c, "/api/v2/agents") {
			return true
		}
	}
	return false
}

// VER-022-1 / QA-022-1: the resolution matrix -- explicit, bound, from a
// subdirectory, from another repository, under another account, against
// another control plane, ambiguous, and a binding whose session is gone.
func TestTargetResolutionMatrix(t *testing.T) {
	c := newBindCtl()
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	env := fastEnv(cfg)
	repo1, repo2 := project(t), project(t)

	// nothing named, nothing bound, no terminal: refused, nothing chosen
	_, errs, code := auditExec(t, bin, cfg, repo1, env, "agent", "list")
	if code != exitUsage || !strings.Contains(errs, "no session was named") || !strings.Contains(errs, "--session") || calledAgents(c.seen()) {
		t.Fatalf("unbound, unnamed: exit %d\n%s", code, errs)
	}
	// explicit: used, and no target line (the person named it)
	out, errs, code := auditExec(t, bin, cfg, repo1, env, "agent", "list", "--session", "fleetaaa2")
	if code != 0 || !strings.Contains(out, "agent_a2") || strings.Contains(errs, "target:") {
		t.Fatalf("explicit: exit %d\n%s%s", code, out, errs)
	}
	// ambiguous binding request: nothing bound
	if _, errs, code := auditExec(t, bin, cfg, repo1, env, "session", "use", "fleetaaa"); code != exitUsage || !strings.Contains(errs, "matches 2 sessions") {
		t.Fatalf("ambiguous use: exit %d\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(cfg, "keepstate", "bindings.json")); err == nil {
		t.Fatal("an ambiguous ks session use wrote a binding")
	}
	// bind repo1 to checkout
	if out, errs, code := auditExec(t, bin, cfg, repo1, env, "session", "use", "fleetaaa1"); code != 0 || !strings.Contains(out, "bound:") {
		t.Fatalf("use: exit %d\n%s%s", code, out, errs)
	}
	// bound: used, and the target is shown before anything acts
	c.reset()
	out, errs, code = auditExec(t, bin, cfg, repo1, env, "agent", "list")
	if code != 0 || !strings.Contains(out, "agent_a1") || !strings.Contains(errs, "target: session fleetaaa1000") || !strings.Contains(errs, "from the binding for") {
		t.Fatalf("bound: exit %d\n%s%s", code, out, errs)
	}
	// from a subdirectory of the same project: the same canonical root
	sub := filepath.Join(repo1, "src", "deep")
	_ = os.MkdirAll(sub, 0o755)
	if out, errs, code := auditExec(t, bin, cfg, sub, env, "agent", "list"); code != 0 || !strings.Contains(out, "agent_a1") {
		t.Fatalf("subdirectory: exit %d\n%s%s", code, out, errs)
	}
	// --session always wins over the binding
	if out, errs, code := auditExec(t, bin, cfg, repo1, env, "agent", "list", "--session", "fleetaaa2"); code != 0 || !strings.Contains(out, "agent_a2") || strings.Contains(errs, "target:") {
		t.Fatalf("explicit over bound: exit %d\n%s%s", code, out, errs)
	}
	// another repository: nothing stale is selected
	c.reset()
	if _, errs, code := auditExec(t, bin, cfg, repo2, env, "agent", "list"); code != exitUsage || calledAgents(c.seen()) {
		t.Fatalf("another repository used a binding: exit %d\n%s", code, errs)
	}
	// another account, same repository: the other account's binding does
	// not apply, and account B's own same-named session is not chosen either
	signIn(t, cfg, srv.URL, "tok-b", "acct_b")
	c.reset()
	if _, errs, code := auditExec(t, bin, cfg, repo1, env, "agent", "list"); code != exitUsage || calledAgents(c.seen()) {
		t.Fatalf("another account used account A's binding: exit %d\n%s", code, errs)
	}
	// another control plane, same account and repository
	other := httptest.NewServer(c)
	defer other.Close()
	signIn(t, cfg, other.URL, "tok-a", "acct_a")
	c.reset()
	if _, errs, code := auditExec(t, bin, cfg, repo1, env, "agent", "list"); code != exitUsage || calledAgents(c.seen()) {
		t.Fatalf("another control plane used the binding: exit %d\n%s", code, errs)
	}
	// back on the original: the bound session disappears (deleted): refused,
	// nothing else chosen in its place
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	c.mu.Lock()
	c.sessions = c.sessions[1:]
	c.mu.Unlock()
	c.reset()
	out, errs, code = auditExec(t, bin, cfg, repo1, env, "agent", "list", "--json")
	if code != exitUsage || calledAgents(c.seen()) || !strings.Contains(out, "binding_stale") || !strings.Contains(out, "no longer has") {
		t.Fatalf("stale binding: exit %d\n%s%s", code, out, errs)
	}
	// --clear removes it
	if out, _, code := auditExec(t, bin, cfg, repo1, env, "session", "use", "--clear"); code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("clear: exit %d %s", code, out)
	}
}

// QA-022-2: a repository that ships a binding is not trusted: the file is
// never used to select a session, --yes does not trust it, a confirmation
// naming another session is refused, a linked file is not read, and the
// project's .keepstate directory never travels in an upload.
func TestARepositoryBindingIsNotTrusted(t *testing.T) {
	c := newBindCtl()
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	env := fastEnv(cfg)
	repo := project(t)
	_ = os.MkdirAll(filepath.Join(repo, ".keepstate"), 0o755)
	writeFileT(t, filepath.Join(repo, ".keepstate", "session.json"), `{"session":"fleetaaa2000"}`)

	_, errs, code := auditExec(t, bin, cfg, repo, env, "agent", "list", "--yes")
	if code != exitUsage || !strings.Contains(errs, "not trusted") || !strings.Contains(errs, "--trust-repo") || calledAgents(c.seen()) {
		t.Fatalf("the repository's binding was used: exit %d\n%s", code, errs)
	}
	for _, args := range [][]string{
		{"session", "use", "--trust-repo"},
		{"session", "use", "--trust-repo", "--yes"},
		{"session", "use", "--trust-repo", "--confirm", "fleetaaa1000"},
	} {
		if _, errs, code := auditExec(t, bin, cfg, repo, env, args...); code != exitUsage || !strings.Contains(errs, "nothing was bound") {
			t.Errorf("`ks %s`: exit %d\n%s", strings.Join(args, " "), code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg, "keepstate", "bindings.json")); err == nil {
		t.Fatal("an unconfirmed repository binding was recorded")
	}
	// the reading form shows it, untrusted, and changes nothing
	out, _, code := auditExec(t, bin, cfg, repo, env, "session", "use", "--json")
	if code != 0 || !strings.Contains(out, `"repository_suggestion":"fleetaaa2000"`) || !strings.Contains(out, `"repository_suggestion_trusted":false`) {
		t.Fatalf("reading form: %d %s", code, out)
	}
	// confirmed by its id: adopted as an ordinary local binding
	out, errs, code = auditExec(t, bin, cfg, repo, env, "session", "use", "--trust-repo", "--confirm", "fleetaaa2000", "--json")
	if code != 0 || !strings.Contains(out, "repository file, confirmed") {
		t.Fatalf("confirmed trust: exit %d\n%s%s", code, out, errs)
	}
	if out, _, code := auditExec(t, bin, cfg, repo, env, "agent", "list"); code != 0 || !strings.Contains(out, "agent_a2") {
		t.Fatalf("after trust: %d %s", code, out)
	}
	// a repository file reached through a link is never read
	repo2 := project(t)
	_ = os.MkdirAll(filepath.Join(repo2, ".keepstate"), 0o755)
	if err := os.Symlink(filepath.Join(repo, ".keepstate", "session.json"), filepath.Join(repo2, ".keepstate", "session.json")); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := auditExec(t, bin, cfg, repo2, env, "session", "use", "--json"); !strings.Contains(out, `"repository_suggestion":""`) {
		t.Fatalf("a linked repository binding was read: %s", out)
	}
	// and nothing under .keepstate is ever selected for an upload
	sel, err := buildSelection(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range sel.Included {
		if strings.HasPrefix(f.Path, ".keepstate") {
			t.Fatalf("the upload selection carries %s", f.Path)
		}
	}
}

// QA-022-3 (the client's half) and the rename refusals: the target and the
// revision are shown before the change, the change is bound to the revision
// read, a bad name is refused before anything is sent, and a stale revision,
// the wrong role and an unavailable capability each rename nothing.
func TestAgentRenameIsByIDAgainstTheRevisionRead(t *testing.T) {
	c := newBindCtl()
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	env := fastEnv(cfg)
	dir := t.TempDir()

	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "rename", "main", "lead", "--session", "fleetaaa1")
	if code != 0 || !strings.Contains(out, "renamed agent agent_a1") || !strings.Contains(errs, "against revision 4") || !strings.Contains(errs, "by id") {
		t.Fatalf("rename: exit %d\n%s%s", code, out, errs)
	}
	if len(c.patches) != 1 || c.patches[0]["expected_revision"] != float64(4) || c.patches[0]["name"] != "lead" {
		t.Fatalf("the change sent: %v", c.patches)
	}
	c.reset()
	for _, bad := range []string{"1lead", "has space", strings.Repeat("a", 49), "../x"} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "rename", "lead", bad, "--session", "fleetaaa1"); code != exitUsage || !strings.Contains(errs, "not a name") {
			t.Errorf("bad name %q: exit %d\n%s", bad, code, errs)
		}
	}
	if len(c.patches) != 0 || calledAgents(c.seen()) {
		t.Fatalf("a bad name reached the service: %v", c.seen())
	}
	c.stale = true
	out, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "rename", "lead", "reviewer", "--session", "fleetaaa1", "--json")
	if code != exitConflict || !strings.Contains(out, "ks_revision_conflict") || !strings.Contains(out, "nothing was renamed") {
		t.Fatalf("stale revision: exit %d\n%s%s", code, out, errs)
	}
	c.stale, c.forbid = false, true
	if out, _, code := auditExec(t, bin, cfg, dir, env, "agent", "rename", "lead", "reviewer", "--session", "fleetaaa1", "--json"); code != exitAuth || !strings.Contains(out, "ks_forbidden") {
		t.Fatalf("wrong role: exit %d %s", code, out)
	}
	c.forbid, c.unavail = false, "agent.workspace"
	c.reset()
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "rename", "lead", "reviewer", "--session", "fleetaaa1"); code != exitFailed || !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "unavailable") || calledAgents(c.seen()) {
		t.Fatalf("unavailable: exit %d\n%s", code, errs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agentName["agent_a1"] != "lead" {
		t.Fatalf("a refused rename changed the name: %s", c.agentName["agent_a1"])
	}
}

func TestSessionRenameAgainstTheRevisionRead(t *testing.T) {
	c := newBindCtl()
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	env := fastEnv(cfg)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "rename", "fleetaaa1", "checkout-v2")
	if code != 0 || !strings.Contains(out, `"checkout" -> "checkout-v2"`) || !strings.Contains(errs, "against revision 7") {
		t.Fatalf("session rename: exit %d\n%s%s", code, out, errs)
	}
	c.stale = true
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "session", "rename", "fleetaaa1", "other"); code != exitConflict || !strings.Contains(errs, "nothing was renamed") {
		t.Fatalf("stale: exit %d\n%s", code, errs)
	}
}

// The duplicate-work warning: starting a session in a project already bound
// to one that is working says so, starts the new one anyway (nothing is
// merged), and asks the service nothing extra where there is no binding.
func TestStartingASecondSessionForABoundProjectWarns(t *testing.T) {
	c := newBindCtl()
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "tok-a", "acct_a")
	env := fastEnv(cfg)
	repo, other := project(t), project(t)
	if _, errs, code := auditExec(t, bin, cfg, repo, env, "session", "use", "fleetaaa1"); code != 0 {
		t.Fatalf("use: %s", errs)
	}
	out, errs, code := auditExec(t, bin, cfg, repo, env, "run")
	if code != 0 || strings.TrimSpace(out) != "sess-new" || !strings.Contains(errs, "SEPARATE session") || !strings.Contains(errs, "fleetaaa1000") {
		t.Fatalf("warning: exit %d\n%s%s", code, out, errs)
	}
	c.reset()
	out, errs, code = auditExec(t, bin, cfg, other, env, "run")
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("unbound project warned: exit %d\n%s%s", code, out, errs)
	}
	for _, call := range c.seen() {
		if strings.Contains(call, "source=fleet") {
			t.Fatalf("an unbound run read the inventory: %v", c.seen())
		}
	}
}

func TestTheSelectorHasNoDefault(t *testing.T) {
	rows := []inventoryRow{{ID: "a", ShortID: "a"}, {ID: "b", ShortID: "b"}}
	for _, answer := range []string{"", "\n", "0", "3", "b", "-1"} {
		if _, err := chooseSession(rows, answer); err == nil {
			t.Errorf("answer %q chose a session", answer)
		}
	}
	if r, err := chooseSession(rows, "2\n"); err != nil || r.ID != "b" {
		t.Fatalf("a valid choice: %v %v", r, err)
	}
}
