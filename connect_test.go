// The repository connection verbs against a fake control plane: what each
// sends (method, path, query, body), what each prints in text and in --json,
// a token that is never taken from a flag or an argument and never printed,
// the provider's page that is not set up, a choice between connections that
// lists every choice and picks none, and a placement followed to placed and
// to refused.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRepoToken = "glpat-NeverPrintThis0123456789"

type connCtl struct {
	mu          sync.Mutex
	sent        []string
	startPolls  int
	placePolls  int
	notSetUp    bool
	startRefuse bool
	draftPolls  int
}

func (c *connCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg, next string, extra map[string]any) {
		e := map[string]any{"code": typ, "type": typ, "message": msg, "retryable": code == 503, "work_started": "no", "next_action": next}
		for k, v := range extra {
			e[k] = v
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": e})
	}
	gh := map[string]any{"id": "conn_gh", "provider": "github", "kind": "app", "owner": "organization", "org_id": "org_a", "host": "github.com", "external_name": "acme",
		"scopes": []string{"contents:read"}, "status": "active", "status_words": "connected; reads the repositories it was given", "created_by": "acct_1", "created_at": "2026-11-08T10:00:00Z", "revision": 1}
	gl := map[string]any{"id": "conn_gl", "provider": "gitlab", "kind": "token", "owner": "personal", "host": "gitlab.example.com", "base_url": "https://gitlab.example.com", "external_name": "ann",
		"name": "work", "scopes": []string{"read_api", "read_repository"}, "status": "needs_reconnect", "status_words": "GitLab stopped accepting the token", "last4": "9f3a",
		"expires_at": "2026-12-01", "warning": "a personal token acts as its owner everywhere it reaches", "created_by": "acct_1", "created_at": "2026-11-07T10:00:00Z", "revision": 3}
	choices := []any{
		map[string]any{"connection_id": "conn_gh", "provider": "github", "kind": "app", "owner": "organization", "external_name": "acme", "words": "the organization's app, reaches acme/dup"},
		map[string]any{"connection_id": "conn_me", "provider": "github", "kind": "token", "owner": "personal", "external_name": "ann", "words": "your token, reaches acme/dup"},
	}
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	p := r.URL.Path
	switch {
	case p == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"repositories","availability":"available","summary":"s","surface":"api"},{"id":"cruise.drafts","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.Method == "GET" && p == "/api/v2/connections":
		items := []any{gh, gl}
		if r.URL.Query().Get("provider") == "github" {
			items = []any{gh}
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	case r.Method == "GET" && p == "/api/v2/connections/conn_gl":
		env(200, gl)
	case r.Method == "GET" && p == "/api/v2/connections/conn_gh":
		env(200, gh)
	case r.Method == "DELETE" && p == "/api/v2/connections/conn_gl":
		env(200, map[string]any{"id": "conn_gl", "removed": true, "note": "KeepState no longer holds the token; revoke it at GitLab too if nothing else uses it"})
	case r.Method == "POST" && strings.HasSuffix(p, "/token"):
		if body["token"] != testRepoToken {
			refuse(422, "ks_token_invalid", "GitLab did not accept the token", "", nil)
			return
		}
		if body["name"] == "narrow" {
			refuse(422, "ks_token_scopes", "the token lacks read_repository; give it read_api and read_repository", "ks connect gitlab --token", nil)
			return
		}
		env(201, gl)
	case r.Method == "POST" && strings.HasSuffix(p, "/start"):
		if c.notSetUp {
			refuse(503, "ks_provider_not_set_up", "GitHub connections through the app are not set up on this service yet; a token works", "ks connect github --token", nil)
			return
		}
		env(201, map[string]any{"state": "st_1", "url": "https://gitlab.com/oauth/authorize?state=st_1", "expires_at": "2099-01-01T00:00:00Z", "poll": "GET /api/v2/connection-starts/st_1",
			"note": "open the link in a browser signed in to KeepState as you, and approve KeepState there"})
	case r.Method == "GET" && p == "/api/v2/connection-starts/st_1":
		c.startPolls++
		switch {
		case c.startRefuse:
			env(200, map[string]any{"state": "refused", "message": "you declined at GitLab"})
		case c.startPolls < 2:
			env(200, map[string]any{"state": "waiting"})
		default:
			env(200, map[string]any{"state": "connected", "connection": gh})
		}
	case r.Method == "GET" && p == "/api/v2/connections/conn_gh/repositories":
		env(200, map[string]any{"items": []any{map[string]any{"provider_id": 42, "full_name": "acme/api", "default_branch": "main", "private": true, "web_url": "https://github.com/acme/api", "connection_id": "conn_gh"}}, "next_cursor": ""})
	case r.Method == "GET" && p == "/api/v2/connections/conn_gh/branches":
		env(200, map[string]any{"items": []any{map[string]any{"name": "main", "commit": "0123456789abcdef0123"}, map[string]any{"name": "release", "commit": "fedcba9876543210fedc"}}, "next_cursor": ""})
	case p == "/api/v2/orgs/org_a/connection-policy":
		only := true
		if r.Method == "PUT" {
			only = body["organization_only"] == true
		}
		env(200, map[string]any{"org_id": "org_a", "providers": []any{
			map[string]any{"provider": "github", "organization_only": only, "updated_by": "acct_1", "updated_at": "2026-11-08T11:00:00Z"},
			map[string]any{"provider": "gitlab", "organization_only": true}}})
	case p == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{
			map[string]any{"id": "fleetrepo00000000000000000000001", "short_id": "fleetrepo000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"},
			map[string]any{"id": "fleetpaus00000000000000000000002", "short_id": "fleetpaus000", "name": "parked", "runtime_state": "parked", "record_id": "session_2"}}, "next_cursor": ""})
	case r.Method == "POST" && p == "/api/v2/sessions/session_2/repository":
		refuse(409, "ks_session_not_running", "the session is paused; placing a repository does not wake it", "ks session resume fleetpaus000", nil)
	case r.Method == "POST" && p == "/api/v2/sessions/session_1/repository":
		switch body["repository"] {
		case "acme/dup":
			refuse(409, "ks_connection_choice", "Two connections can reach acme/dup: the organization's and yours. Say which", "ks session repo ... --connection CONNECTION", map[string]any{"choices": choices})
		case "github.com/acme/broken":
			env(202, map[string]any{"id": "rp_bad", "session_id": "session_1", "state": "fetching", "repository": "acme/broken"})
		default:
			env(202, map[string]any{"id": "rp_1", "session_id": "session_1", "state": "fetching", "provider": "github", "repository": "acme/api"})
		}
	case r.Method == "GET" && p == "/api/v2/sessions/session_1/repository/rp_1":
		c.placePolls++
		if c.placePolls < 2 {
			env(200, map[string]any{"id": "rp_1", "state": "fetching"})
			return
		}
		env(200, map[string]any{"id": "rp_1", "session_id": "session_1", "state": "placed", "provider": "github", "repository": "acme/api", "branch": "main", "commit": "0123456789abcdef0123",
			"connection_id": "conn_gh", "to": "repo", "files": 120, "bytes": 34567, "notes": []string{"submodule vendor/lib left as an empty folder", "2 Git LFS files are pointers only"},
			"message": "acme/api is in repo"})
	case r.Method == "GET" && p == "/api/v2/sessions/session_1/repository/rp_bad":
		env(200, map[string]any{"id": "rp_bad", "session_id": "session_1", "state": "refused", "repository": "acme/broken", "notes": []string{"1 link left out"},
			"refusal": map[string]any{"code": "ks_repository_too_large", "message": "acme/broken at that commit is 740 MB; the limit is 512 MB", "next_action": "ks file upload"}})
	case r.Method == "POST" && p == "/api/v2/cruise-drafts":
		src, _ := body["source"].(map[string]any)
		if src["repository"] == "acme/dup" {
			refuse(409, "ks_connection_choice", "Two connections can reach acme/dup. Say which", "", map[string]any{"choices": choices})
			return
		}
		env(201, map[string]any{"id": "crd_1", "state": "collecting", "revision": 1, "source": map[string]any{"kind": "repository", "repository": "acme/api", "provider": "github"}})
	case r.Method == "GET" && p == "/api/v2/cruise-drafts/crd_1":
		c.draftPolls++
		env(200, map[string]any{"id": "crd_1", "state": "ready", "revision": 2, "source": map[string]any{"kind": "repository", "provider": "github", "repository": "acme/api", "branch": "main",
			"commit": "0123456789abcdef0123", "connection_id": "conn_gh", "repository_notes": []string{"submodule vendor/lib was not fetched"}}})
	default:
		refuse(404, "ks_not_found", "no such resource", "", nil)
	}
}

func (c *connCtl) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = nil
}

func (c *connCtl) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if !strings.HasPrefix(s, "GET /api/capabilities") {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func (c *connCtl) has(t *testing.T, want string) {
	t.Helper()
	want = strings.TrimSpace(want)
	for _, s := range c.requests() {
		if s == want || strings.HasPrefix(s, want) {
			return
		}
	}
	t.Errorf("no request %q among\n%s", want, strings.Join(c.requests(), "\n"))
}

func (c *connCtl) writes() []string {
	var out []string
	for _, s := range c.requests() {
		if !strings.HasPrefix(s, "GET ") {
			out = append(out, s)
		}
	}
	return out
}

type connHarness struct {
	t        *testing.T
	c        *connCtl
	srv      *httptest.Server
	bin, cfg string
	env      []string
	opened   string
}

func newConnHarness(t *testing.T) *connHarness {
	c := &connCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	pathDir := t.TempDir()
	opened := fakeOpener(t, pathDir)
	env := append(fastEnv(cfg), "KS_WAIT_POLL_MS=30", "PATH="+pathDir+":"+os.Getenv("PATH"))
	return &connHarness{t: t, c: c, srv: srv, bin: bin, cfg: cfg, env: env, opened: opened}
}

func (h *connHarness) run(stdin string, args ...string) (string, string, int) {
	h.t.Helper()
	h.c.reset()
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = h.t.TempDir()
	cmd.Env = append(os.Environ(), h.env...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	fixtureBarrier()
	err := cmd.Run()
	fixtureBarrier()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("%v: %v", args, err)
	}
	return o.String(), e.String(), code
}

func (h *connHarness) useContext(org string) {
	b, _ := json.Marshal(map[string]string{h.srv.URL: org})
	if err := os.WriteFile(filepath.Join(h.cfg, "keepstate", "context.json"), b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func contains(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, text)
		}
	}
}

func TestConnectListShowAndContext(t *testing.T) {
	h := newConnHarness(t)

	out, errs, code := h.run("", "connect", "list")
	if code != 0 {
		t.Fatalf("list: %d\n%s%s", code, out, errs)
	}
	h.c.has(t, "GET /api/v2/connections ")
	contains(t, "list", out, "your personal account", "conn_gh  GitHub on github.com", "acts as   acme, through the KeepState app", "owner     the organization org_a",
		"status    active: connected; reads the repositories it was given", "conn_gl  GitLab on gitlab.example.com (work)", "token     ending 9f3a, expires 2026-12-01",
		"needs_reconnect: GitLab stopped accepting the token", "warning   a personal token acts as its owner everywhere it reaches")

	out, _, code = h.run("", "connect", "list", "--organization", "org_a", "--provider", "github", "--json")
	if code != 0 {
		t.Fatalf("list --json: %d %s", code, out)
	}
	h.c.has(t, "GET /api/v2/connections?provider=github&context=org_a ")
	var doc struct {
		Data struct {
			Context     string           `json:"context"`
			Connections []repoConnection `json:"connections"`
			Count       int              `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Data.Context != "org_a" || doc.Data.Count != 1 || doc.Data.Connections[0].ID != "conn_gh" {
		t.Errorf("list --json: %v %+v\n%s", err, doc, out)
	}

	// the context this client works in is the organization when no flag names one
	h.useContext("org_a")
	if _, _, code = h.run("", "connect", "list"); code != 0 {
		t.Fatalf("list in context: %d", code)
	}
	h.c.has(t, "GET /api/v2/connections?context=org_a ")
	h.useContext("")

	if _, _, code := h.run("", "connect", "list", "--provider", "bitbucket"); code != exitUsage || len(h.c.requests()) != 0 {
		t.Errorf("an unknown provider: exit %d, requests %v", code, h.c.requests())
	}

	out, _, code = h.run("", "connect", "show", "conn_gl")
	if code != 0 {
		t.Fatalf("show: %d", code)
	}
	h.c.has(t, "GET /api/v2/connections/conn_gl ")
	contains(t, "show", out, "address   https://gitlab.example.com", "scopes    read_api, read_repository", "made      2026-11-07T10:00:00Z by acct_1 (revision 3)")
	out, _, _ = h.run("", "connect", "show", "conn_gl", "--json")
	var one struct {
		Data repoConnection `json:"data"`
	}
	if json.Unmarshal([]byte(out), &one) != nil || one.Data.Last4 != "9f3a" {
		t.Errorf("show --json: %s", out)
	}
}

func TestConnectTokenIsReadFromStandardInputNeverAFlagAndNeverPrinted(t *testing.T) {
	h := newConnHarness(t)
	never := func(what, text string) {
		t.Helper()
		if strings.Contains(text, testRepoToken) || strings.Contains(text, testRepoToken[6:]) {
			t.Errorf("%s printed the token:\n%s", what, text)
		}
	}

	// piped: one POST, the token in the body exactly, never in the output
	out, errs, code := h.run(testRepoToken+"\n", "connect", "gitlab", "--token", "--url", "https://gitlab.example.com", "--name", "work", "--organization", "org_a")
	if code != 0 {
		t.Fatalf("gitlab --token: %d\n%s%s", code, out, errs)
	}
	want := `POST /api/v2/connections/gitlab/token?context=org_a {"base_url":"https://gitlab.example.com","name":"work","token":"` + testRepoToken + `"}`
	if w := h.c.writes(); len(w) != 1 || w[0] != want {
		t.Errorf("sent\n%v\nwant\n%s", w, want)
	}
	contains(t, "gitlab --token", out, "connected GitLab", "token     ending 9f3a", "ks connect repos conn_gl")
	never("gitlab --token", out+errs)

	out, errs, code = h.run(testRepoToken+"\n", "connect", "github", "--token", "--json")
	if code != 0 {
		t.Fatalf("github --token --json: %d %s%s", code, out, errs)
	}
	h.c.has(t, `POST /api/v2/connections/github/token {"token":"`+testRepoToken+`"}`)
	var doc struct {
		Data repoConnection `json:"data"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil || doc.Data.ID != "conn_gl" {
		t.Errorf("--json: %s", out)
	}
	never("github --token --json", out+errs)

	// the service's refusal in its words; the token is not printed with it
	out, errs, code = h.run(testRepoToken+"\n", "connect", "gitlab", "--token", "--name", "narrow")
	if code == 0 {
		t.Fatalf("a refused token exited 0")
	}
	contains(t, "refused token", errs, "the token lacks read_repository", "nothing was connected", "Next: ks connect gitlab --token")
	never("refused token", out+errs)

	// never from an argument or a flag's value: refused before any request, and not repeated back
	for _, args := range [][]string{
		{"connect", "github", "--token", testRepoToken},
		{"connect", "gitlab", "--token=" + testRepoToken},
		{"connect", "github", testRepoToken},
		{"connect", "gitlab", "--token", "--url", "https://gitlab.example.com", testRepoToken},
	} {
		out, errs, code := h.run("", args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args[:2], code, exitUsage)
		}
		if len(h.c.requests()) != 0 {
			t.Errorf("%v made requests: %v", args[:2], h.c.requests())
		}
		never(strings.Join(args[:3], " ")+" ...", out+errs)
	}

	// nothing on standard input: nothing is sent
	if _, errs, code := h.run("", "connect", "github", "--token"); code != exitUsage || len(h.c.writes()) != 0 || !strings.Contains(errs, "no token was read") {
		t.Errorf("empty stdin: %d %v %s", code, h.c.writes(), errs)
	}
	// --url is a self-managed instance, which connects with a token only
	if _, errs, code := h.run("", "connect", "gitlab", "--url", "https://gitlab.example.com"); code != exitUsage || len(h.c.requests()) != 0 || !strings.Contains(errs, "with a token only") {
		t.Errorf("--url without --token: %d %v %s", code, h.c.requests(), errs)
	}
	if _, _, code := h.run("", "connect", "github", "--name", "x"); code != exitUsage || len(h.c.requests()) != 0 {
		t.Errorf("--name without --token: %d %v", code, h.c.requests())
	}
}

func TestConnectThroughTheProvidersPage(t *testing.T) {
	h := newConnHarness(t)

	out, errs, code := h.run("", "connect", "gitlab")
	if code != 0 {
		t.Fatalf("connect gitlab: %d\n%s%s", code, out, errs)
	}
	h.c.has(t, "POST /api/v2/connections/gitlab/start {}")
	h.c.has(t, "GET /api/v2/connection-starts/st_1 ")
	contains(t, "connect gitlab stderr", errs, "https://gitlab.com/oauth/authorize?state=st_1", "approve KeepState there", "Opened it in your browser", "Waiting for the approval")
	contains(t, "connect gitlab", out, "connected GitLab", "conn_gh  GitHub on github.com")
	if strings.Count(out, "connected GitLab") != 1 {
		t.Errorf("the outcome is printed once:\n%s", out)
	}
	// the opener runs in the background: give it a moment to write
	var opened []byte
	for i := 0; i < 100 && len(opened) == 0; i++ {
		if opened, _ = os.ReadFile(h.opened); len(opened) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if strings.TrimSpace(string(opened)) != "https://gitlab.com/oauth/authorize?state=st_1" {
		t.Errorf("opened %q", opened)
	}

	// --no-input never opens a browser; --json is the outcome alone on stdout
	_ = os.Remove(h.opened)
	h.c.mu.Lock()
	h.c.startPolls = 0
	h.c.mu.Unlock()
	out, _, code = h.run("", "connect", "github", "--organization", "org_a", "--no-input", "--json")
	if code != 0 {
		t.Fatalf("--no-input: %d", code)
	}
	h.c.has(t, "POST /api/v2/connections/github/start?context=org_a {}")
	if _, err := os.Stat(h.opened); err == nil {
		t.Errorf("--no-input opened a browser")
	}
	var doc struct {
		Data connectStartPoll `json:"data"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil || doc.Data.State != "connected" || doc.Data.Connection == nil {
		t.Errorf("--json: %s", out)
	}

	// refused at the provider: said once, not connected
	h.c.mu.Lock()
	h.c.startRefuse = true
	h.c.mu.Unlock()
	_, errs, code = h.run("", "connect", "gitlab", "--no-input")
	if code == 0 || !strings.Contains(errs, "you declined at GitLab") || !strings.Contains(errs, "nothing was connected") {
		t.Errorf("refused: %d %s", code, errs)
	}

	// the app not set up: its message and its next action, the token path
	h.c.mu.Lock()
	h.c.notSetUp = true
	h.c.mu.Unlock()
	_, errs, code = h.run("", "connect", "github", "--no-input")
	if code == 0 {
		t.Fatalf("not set up exited 0")
	}
	contains(t, "not set up", errs, "not set up on this service yet; a token works", "Next: ks connect github --token")
	out, _, _ = h.run("", "connect", "github", "--no-input", "--json")
	var e struct {
		Error cliError `json:"error"`
	}
	if json.Unmarshal([]byte(out), &e) != nil || e.Error.Kind != "ks_provider_not_set_up" || e.Error.NextAction != "ks connect github --token" {
		t.Errorf("not set up --json: %s", out)
	}
}

func TestConnectRemoveIsConfirmedByItsID(t *testing.T) {
	h := newConnHarness(t)
	_, errs, code := h.run("", "connect", "remove", "conn_gl")
	if code != exitUsage || len(h.c.writes()) != 0 || !strings.Contains(errs, "--confirm conn_gl") {
		t.Errorf("unconfirmed: %d %v %s", code, h.c.writes(), errs)
	}
	if _, _, code := h.run("", "connect", "remove", "conn_gl", "--confirm", "conn_gh"); code != exitUsage || len(h.c.writes()) != 0 {
		t.Errorf("a wrong id confirmed: %d %v", code, h.c.writes())
	}
	if _, _, code := h.run("", "connect", "remove", "conn_gl", "--yes"); code != exitUsage || len(h.c.writes()) != 0 {
		t.Errorf("--yes confirmed a removal: %d %v", code, h.c.writes())
	}
	out, errs, code := h.run("", "connect", "remove", "conn_gl", "--confirm", "conn_gl")
	if code != 0 {
		t.Fatalf("remove: %d %s%s", code, out, errs)
	}
	if w := h.c.writes(); len(w) != 1 || w[0] != "DELETE /api/v2/connections/conn_gl" {
		t.Errorf("sent %v", w)
	}
	contains(t, "remove stderr", errs, "remove this connection", "conn_gl  GitLab on gitlab.example.com, acting as ann, yours")
	contains(t, "remove", out, "removed conn_gl", "revoke it at GitLab too")
	out, _, _ = h.run("", "connect", "remove", "conn_gl", "--confirm", "conn_gl", "--json")
	if !strings.Contains(out, `"removed":true`) || !strings.Contains(out, `"note":"KeepState no longer holds the token`) {
		t.Errorf("remove --json: %s", out)
	}
}

func TestConnectReposAndBranches(t *testing.T) {
	h := newConnHarness(t)
	out, _, code := h.run("", "connect", "repos", "conn_gh", "--search", "api")
	if code != 0 {
		t.Fatalf("repos: %d", code)
	}
	h.c.has(t, "GET /api/v2/connections/conn_gh/repositories?q=api ")
	contains(t, "repos", out, "acme/api  default branch main, private", "ks connect branches conn_gh REPOSITORY")
	out, _, _ = h.run("", "connect", "repos", "conn_gh", "--json")
	if !strings.Contains(out, `"full_name":"acme/api"`) || !strings.Contains(out, `"count":1`) {
		t.Errorf("repos --json: %s", out)
	}

	out, _, code = h.run("", "connect", "branches", "conn_gh", "acme/api", "--search", "rel")
	if code != 0 {
		t.Fatalf("branches: %d", code)
	}
	h.c.has(t, "GET /api/v2/connections/conn_gh/branches?q=rel&repository=acme%2Fapi ")
	contains(t, "branches", out, "branches of acme/api (the default branch first)", "  main  0123456789ab", "  release  fedcba987654")
	out, _, _ = h.run("", "connect", "branches", "conn_gh", "acme/api", "--json")
	if !strings.Contains(out, `"name":"main"`) || !strings.Contains(out, `"repository":"acme/api"`) {
		t.Errorf("branches --json: %s", out)
	}
	// a refusal is the service's words
	_, errs, code := h.run("", "connect", "repos", "conn_nope")
	if code == 0 || !strings.Contains(errs, "no such resource") {
		t.Errorf("unknown connection: %d %s", code, errs)
	}
}

func TestOrganizationConnectionsPolicy(t *testing.T) {
	h := newConnHarness(t)
	_, errs, code := h.run("", "organization", "connections")
	if code != exitUsage || len(h.c.requests()) != 0 || !strings.Contains(errs, "--organization") {
		t.Errorf("no organization: %d %v %s", code, h.c.requests(), errs)
	}
	out, _, code := h.run("", "organization", "connections", "--organization", "org_a")
	if code != 0 {
		t.Fatalf("read: %d", code)
	}
	h.c.has(t, "GET /api/v2/orgs/org_a/connection-policy ")
	contains(t, "policy", out, "GitHub  organization only on", "GitLab  organization only on: its work uses only the organization's own connections")

	h.useContext("org_a")
	out, _, code = h.run("", "organization", "connections", "--provider", "github", "--organization-only", "off")
	if code != 0 {
		t.Fatalf("set: %d", code)
	}
	if w := h.c.writes(); len(w) != 1 || w[0] != `PUT /api/v2/orgs/org_a/connection-policy {"organization_only":false,"provider":"github"}` {
		t.Errorf("sent %v", w)
	}
	contains(t, "set", out, "GitHub  organization only off: its work may also use a member's own connection", "(set 2026-11-08T11:00:00Z by acct_1)")
	out, _, _ = h.run("", "organization", "connections", "--json")
	if !strings.Contains(out, `"org_id":"org_a"`) || !strings.Contains(out, `"organization_only":true`) {
		t.Errorf("--json: %s", out)
	}
	for _, args := range [][]string{
		{"organization", "connections", "--provider", "github"},
		{"organization", "connections", "--organization-only", "on"},
		{"organization", "connections", "--provider", "github", "--organization-only", "maybe"},
		{"organization", "connections", "--provider", "svn", "--organization-only", "on"},
	} {
		if _, _, code := h.run("", args...); code != exitUsage || len(h.c.writes()) != 0 {
			t.Errorf("%v: %d %v", args[2:], code, h.c.writes())
		}
	}
}

func TestSessionRepoFollowsThePlacement(t *testing.T) {
	h := newConnHarness(t)
	out, errs, code := h.run("", "session", "repo", "fleetrepo000", "github.com/acme/api", "--branch", "main", "--connection", "conn_gh", "--to", "src", "--replace")
	if code != 0 {
		t.Fatalf("repo: %d\n%s%s", code, out, errs)
	}
	h.c.has(t, `POST /api/v2/sessions/session_1/repository {"branch":"main","connection_id":"conn_gh","replace":true,"repository":"github.com/acme/api","to":"src"}`)
	h.c.has(t, "GET /api/v2/sessions/session_1/repository/rp_1 ")
	contains(t, "placed", out, "placed acme/api at main (0123456789ab) into repo: 120 files, 34,567 bytes", "acme/api is in repo",
		"note: submodule vendor/lib left as an empty folder", "note: 2 Git LFS files are pointers only", "through the connection conn_gh")

	// --json: the placement as placed
	out, _, code = h.run("", "session", "repo", "fleetrepo000", "github.com/acme/api", "--json")
	var doc struct {
		Data repoPlacement `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Data.State != "placed" || len(doc.Data.Notes) != 2 {
		t.Errorf("--json: %d %s", code, out)
	}
	h.c.has(t, `POST /api/v2/sessions/session_1/repository {"repository":"github.com/acme/api"}`)

	// --no-wait: the fetch has started; nothing is followed
	out, _, code = h.run("", "session", "repo", "fleetrepo000", "github.com/acme/api", "--no-wait")
	if code != 0 || !strings.Contains(out, "placement rp_1 is fetching") {
		t.Errorf("--no-wait: %d %s", code, out)
	}
	for _, r := range h.c.requests() {
		if strings.Contains(r, "/repository/rp_1") {
			t.Errorf("--no-wait followed the placement: %s", r)
		}
	}

	// refused: its message, its notes and its next action
	out, errs, code = h.run("", "session", "repo", "fleetrepo000", "github.com/acme/broken")
	if code == 0 {
		t.Fatalf("refused exited 0")
	}
	contains(t, "refused", out+errs, "note: 1 link left out", "acme/broken at that commit is 740 MB; the limit is 512 MB", "Next: ks file upload")
	out, _, _ = h.run("", "session", "repo", "fleetrepo000", "github.com/acme/broken", "--json")
	var e struct {
		Error cliError `json:"error"`
	}
	if json.Unmarshal([]byte(out), &e) != nil || e.Error.Kind != "ks_repository_too_large" {
		t.Errorf("refused --json: %s", out)
	}

	// a paused session is refused and not woken
	_, errs, code = h.run("", "session", "repo", "fleetpaus000", "github.com/acme/api")
	if code != exitConflict || !strings.Contains(errs, "does not wake it") || !strings.Contains(errs, "Next: ks session resume fleetpaus000") {
		t.Errorf("paused: %d %s", code, errs)
	}
}

func TestAChoiceBetweenConnectionsListsEveryChoiceAndPicksNone(t *testing.T) {
	h := newConnHarness(t)
	_, errs, code := h.run("", "session", "repo", "fleetrepo000", "acme/dup")
	if code != exitConflict {
		t.Errorf("choice: exit %d", code)
	}
	if w := h.c.writes(); len(w) != 1 {
		t.Errorf("a choice was retried or picked: %v", w)
	}
	contains(t, "choice", errs, "2 connections can reach this repository", "conn_gh  GitHub, the KeepState app, the organization's, acting as acme",
		"conn_me  GitHub, a token, yours, acting as ann: your token, reaches acme/dup", "ks session repo fleetrepo000 acme/dup --connection CONNECTION")
	out, _, _ := h.run("", "session", "repo", "fleetrepo000", "acme/dup", "--json")
	var e struct {
		Error struct {
			Code   string       `json:"code"`
			Detail choiceDetail `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(out), &e) != nil || e.Error.Code != "ks_connection_choice" || len(e.Error.Detail.Choices) != 2 || e.Error.Detail.Choices[1].ConnectionID != "conn_me" {
		t.Errorf("choice --json: %s", out)
	}

	// the same from a Cruise draft
	_, errs, code = h.run("", "cruise", "draft", "create", "--repo", "acme/dup")
	if code != exitConflict {
		t.Errorf("draft choice: exit %d %s", code, errs)
	}
	contains(t, "draft choice", errs, "conn_gh", "conn_me", "ks cruise draft create --repo acme/dup --connection CONNECTION")
}

func TestCruiseDraftFromARepository(t *testing.T) {
	h := newConnHarness(t)
	out, errs, code := h.run("", "cruise", "draft", "create", "--repo", "github.com/acme/api", "--branch", "main", "--connection", "conn_gh", "--goal", "make the tests pass")
	if code != 0 {
		t.Fatalf("draft: %d\n%s%s", code, out, errs)
	}
	var sent map[string]any
	for _, w := range h.c.writes() {
		if strings.HasPrefix(w, "POST /api/v2/cruise-drafts ") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(w, "POST /api/v2/cruise-drafts ")), &sent)
		} else {
			t.Errorf("a draft from a repository sent more than the draft: %s", w)
		}
	}
	src, _ := sent["source"].(map[string]any)
	if src["kind"] != "repository" || src["repository"] != "github.com/acme/api" || src["branch"] != "main" || src["connection_id"] != "conn_gh" || sent["storage_consent"] != true || sent["goal"] != "make the tests pass" {
		t.Errorf("sent %v", sent)
	}
	h.c.has(t, "GET /api/v2/cruise-drafts/crd_1 ")
	contains(t, "draft", out, "draft crd_1  READY", "source       repository acme/api on GitHub, branch main at 0123456789ab, through conn_gh", "not brought  submodule vendor/lib was not fetched")
	contains(t, "draft stderr", errs, "fetches the repository's files")

	// in an organization's context, the draft is the organization's
	h.useContext("org_a")
	if _, _, code := h.run("", "cruise", "draft", "create", "--repo", "github.com/acme/api"); code != 0 {
		t.Errorf("draft in context: %d", code)
	}
	h.c.has(t, "POST /api/v2/cruise-drafts?context=org_a ")
	h.useContext("")

	for _, args := range [][]string{
		{"cruise", "draft", "create", "--branch", "main", "--dir", "."},
		{"cruise", "draft", "create", "--connection", "conn_gh", "--session", "fleetrepo000"},
		{"cruise", "draft", "create", "--repo", "acme/api", "--dir", "."},
	} {
		if _, _, code := h.run("", args...); code != exitUsage || len(h.c.writes()) != 0 {
			t.Errorf("%v: %d %v", args[3:], code, h.c.writes())
		}
	}
}
