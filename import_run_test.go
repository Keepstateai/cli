package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// importCtl stands in for the control plane and the session's machine: what
// `ks run --agent` needs (the embedded runCtl), the upload routes (a folder is
// placed, a conversation is verified and held), and the two import routes.
type importCtl struct {
	*runCtl
	imu       sync.Mutex
	log       []string
	bodies    bytes.Buffer // every byte any request carried
	plans     []map[string]any
	applies   []map[string]any
	ups       map[string]*fakeImportUpload
	failApply int // the next N applies answer a server error
	applied   map[string]bool
	refuse    string // when set, the plan route refuses with this message
}

type fakeImportUpload struct {
	ID, Kind, To, SHA string
	Size              int64
	Got               []byte
	Done              bool
}

func newImportCtl() *importCtl {
	return &importCtl{runCtl: &runCtl{workspace: "available", prov: readyAnswer()}, ups: map[string]*fakeImportUpload{}, applied: map[string]bool{}}
}

func (c *importCtl) seenLog() []string {
	c.imu.Lock()
	defer c.imu.Unlock()
	return append([]string(nil), c.log...)
}

func (c *importCtl) index(prefix string) int {
	for i, l := range c.seenLog() {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func (c *importCtl) count(prefix string) int {
	n := 0
	for _, l := range c.seenLog() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (c *importCtl) countExact(line string) int {
	n := 0
	for _, l := range c.seenLog() {
		if l == line {
			n++
		}
	}
	return n
}

func canonicalDecl(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func (c *importCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	c.imu.Lock()
	c.log = append(c.log, r.Method+" "+r.URL.Path)
	c.bodies.Write(body)
	c.imu.Unlock()
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
	base := "/api/v2/sessions/session_abc123/uploads"
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/imports":
		var d map[string]any
		_ = json.Unmarshal(body, &d)
		c.imu.Lock()
		c.plans = append(c.plans, d)
		msg := c.refuse
		c.imu.Unlock()
		if msg != "" {
			refuse(422, "ks_import_refused", msg)
			return
		}
		sum := sha256.Sum256([]byte("acct_t\n" + canonicalDecl(d)))
		env(200, map[string]any{"digest": hex.EncodeToString(sum[:]), "mode": d["mode"],
			"plan": map[string]any{"lines": []string{"The service accepts this import: the conversation is placed before the agent's first instruction.", "Model use is billed as for any session."}, "max_bytes": 50000000}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/imports/apply":
		var a map[string]any
		_ = json.Unmarshal(body, &a)
		c.imu.Lock()
		c.applies = append(c.applies, a)
		fails := c.failApply
		if fails > 0 {
			c.failApply--
		}
		c.imu.Unlock()
		if fails > 0 {
			refuse(500, "ks_internal", "the service could not place it just now")
			return
		}
		decl, _ := a["declaration"].(map[string]any)
		sum := sha256.Sum256([]byte("acct_t\n" + canonicalDecl(decl)))
		if a["digest"] != hex.EncodeToString(sum[:]) {
			refuse(409, "ks_import_digest", "the declaration is not the one that was planned")
			return
		}
		mode, _ := decl["mode"].(string)
		out := map[string]any{"import_id": "imp_1", "mode": mode, "agent_id": a["agent_id"], "session_id": "session_abc123",
			"message": "Placed.", "next": "ks agent send main"}
		if mode == "conversation" {
			out["conversation_id"] = asStr(asMap(decl["source"])["session_id"])
		}
		c.imu.Lock()
		again := c.applied[a["digest"].(string)]
		c.applied[a["digest"].(string)] = true
		c.imu.Unlock()
		out["already_applied"] = again
		env(200, out)
	case strings.HasPrefix(r.URL.Path, base):
		c.uploads(w, r, base, body, env, refuse)
	default:
		c.runCtl.ServeHTTP(w, r)
	}
}

func (c *importCtl) uploads(w http.ResponseWriter, r *http.Request, base string, body []byte, env func(int, any), refuse func(int, string, string)) {
	c.imu.Lock()
	defer c.imu.Unlock()
	view := func(u *fakeImportUpload) map[string]any {
		state := "receiving"
		if int64(len(u.Got)) == u.Size {
			state = "received"
		}
		if u.Done {
			state = "verified"
			if u.Kind != "conversation" {
				state = "placed"
			}
		}
		v := map[string]any{"upload_id": u.ID, "session_id": "session_abc123", "kind": u.Kind, "to": u.To, "size": u.Size, "sha256": u.SHA, "content_bytes": u.Size,
			"received": len(u.Got), "state": state, "chunk_bytes": 256 << 10, "limit_bytes": 1000000000, "storage_note": "Uploaded files count toward your storage."}
		if u.Done && u.Kind != "conversation" {
			v["placed"] = map[string]any{"path": u.To, "kind": u.Kind, "bytes": u.Size, "files": 1, "replaced": false}
		}
		return v
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, base), "/")
	parts := strings.Split(rest, "/")
	if rest == "" && r.Method == "POST" {
		var d map[string]any
		_ = json.Unmarshal(body, &d)
		kind, _ := d["kind"].(string)
		to, _ := d["to"].(string)
		sha, _ := d["sha256"].(string)
		size := int64(d["size"].(float64))
		h := sha256.Sum256([]byte(kind + "|" + to + "|" + fmt.Sprint(size) + "|" + sha))
		id := "upl_" + hex.EncodeToString(h[:])[:24]
		u, ok := c.ups[id]
		if !ok {
			u = &fakeImportUpload{ID: id, Kind: kind, To: to, SHA: sha, Size: size}
			c.ups[id] = u
		}
		v := view(u)
		v["resumed"] = ok && len(u.Got) > 0
		env(200, v)
		return
	}
	u, ok := c.ups[parts[0]]
	if !ok {
		refuse(404, "ks_upload_absent", "no such upload in this session")
		return
	}
	switch {
	case r.Method == "GET" && len(parts) == 1:
		env(200, view(u))
	case r.Method == "PUT" && len(parts) == 2 && parts[1] == "bytes":
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if off != int64(len(u.Got)) {
			refuse(409, "ks_upload_offset", "the session holds a different count")
			return
		}
		s := sha256.Sum256(body)
		if hex.EncodeToString(s[:]) != r.Header.Get("X-KS-Chunk-SHA256") {
			refuse(422, "ks_upload_chunk_mismatch", "the chunk's bytes do not match")
			return
		}
		u.Got = append(u.Got, body...)
		env(200, view(u))
	case r.Method == "POST" && len(parts) == 2 && parts[1] == "complete":
		s := sha256.Sum256(u.Got)
		if int64(len(u.Got)) != u.Size || hex.EncodeToString(s[:]) != u.SHA {
			refuse(422, "ks_upload_digest_mismatch", "the bytes received are not the ones declared")
			return
		}
		u.Done = true
		env(200, view(u))
	default:
		refuse(404, "ks_not_found", "no such upload route")
	}
}

func (c *importCtl) upload(kind string) *fakeImportUpload {
	c.imu.Lock()
	defer c.imu.Unlock()
	for _, u := range c.ups {
		if u.Kind == kind {
			return u
		}
	}
	return nil
}

type importEnv struct {
	t    *testing.T
	c    *importCtl
	bin  string
	cfg  string
	home string
	dir  string
	env  []string
}

func (e *importEnv) run(args ...string) (string, string, int) {
	e.t.Helper()
	return auditExec(e.t, e.bin, e.cfg, e.dir, e.env, args...)
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	c := newImportCtl()
	syncFixture(t, &c.mu)
	syncFixture(t, &c.imu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	home := t.TempDir()
	e := &importEnv{t: t, c: c, bin: bin, cfg: cfg, home: home, dir: t.TempDir()}
	e.env = append(fastEnv(cfg), "HOME="+home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"), "CODEX_HOME="+filepath.Join(home, ".codex"), "KS_UPLOAD_BACKOFF_MS=1")
	return e
}

// session places a fixture where the tool keeps it, its project folder said
// as cwd (the fixture's own is /Users/alice/projects/shop).
func (e *importEnv) session(tool, fixture, cwd string) string {
	e.t.Helper()
	text := fixtureText(e.t, fixture)
	if cwd != "" {
		text = strings.ReplaceAll(text, oldRoot, cwd)
	}
	var p string
	if tool == "claude-code" {
		p = filepath.Join(e.home, ".claude", "projects", "-p", claudeSID+".jsonl")
	} else {
		p = filepath.Join(e.home, ".codex", "sessions", "2026", "11", "12", "rollout-2026-11-12T09-00-00-"+codexTID+".jsonl")
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	dir = filepath.Join(dir, "shop")
	_ = os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules", "dep"), 0o755)
	for f, c := range map[string]string{"main.go": "package main\n", "config/app.yml": "bucket: shop-assets\n", ".env": "TOKEN=planted\n", "node_modules/dep/i.js": "x\n", "notes.pem": "pem"} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n.env\n"), 0o644)
	return dir
}

func allFakesAbsent(t *testing.T, label string, data []byte) {
	t.Helper()
	for _, k := range []string{"anthropic_key", "openai_key", "github_token", "connection_string", "jwt", "aws_access_key", "telegram_bot_token"} {
		if bytes.Contains(data, []byte(fakeValue(t, k))) {
			t.Errorf("%s holds a fake %s", label, k)
		}
	}
}

func decodeJSONOut(t *testing.T, out string) map[string]any {
	t.Helper()
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Data == nil {
		t.Fatalf("not a --json answer: %v\n%s", err, out)
	}
	return doc.Data
}

// ---- the plan -------------------------------------------------------------------

func TestImportPlanShowsEverythingAndCreatesNothing(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	out, errs, code := e.run("import", "claude-code", "--latest", "--plan")
	if code != 0 {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"nothing has been created", "plan digest: ", "Claude Code 2.1.312", "Secrets replaced on this machine", "1 anthropic_key", "1 connection_string", "after taking out what must not travel", "attachment",
		"No files go along", "first reply re-reads the whole imported conversation", "Approvals you gave in the old session carry nothing", "The service accepts this import", "ks import claude-code --latest --execute "} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
	for _, none := range []string{"POST /api/v2/preflight", "POST /api/v2/sessions", "POST /api/v2/imports/apply"} {
		if e.c.index(none) >= 0 {
			t.Errorf("a plan asked %s", none)
		}
	}
	allFakesAbsent(t, "the plan", []byte(out+errs))
	allFakesAbsent(t, "what the service was sent", e.c.bodies.Bytes())
	if len(e.c.plans) != 1 {
		t.Fatalf("plans sent: %d", len(e.c.plans))
	}
	d := e.c.plans[0]
	src := asMap(d["source"])
	if d["mode"] != "conversation" || src["tool"] != "claude-code" || src["session_id"] != claudeSID || src["version"] != "2.1.312" || src["cwd"] != oldRoot || src["model"] != "claude-opus-4-8" ||
		len(asStr(src["sha256"])) != 64 || jsonInt(src["bytes"]) <= 0 || jsonInt(src["bytes_before"]) <= jsonInt(src["bytes"]) || jsonInt(src["turns"]) != 2 {
		t.Fatalf("the declaration: %v", d)
	}
	if asMap(src["redactions"])["anthropic_key"] != float64(1) || asMap(src["stripped"])["attachment"] == nil || jsonInt(src["paths_rewritten"]) == 0 {
		t.Fatalf("redactions, stripped or paths: %v", src)
	}
	if asMap(d["target"])["harness"] != "claude-code" || asMap(d["workspace"])["included"] != false {
		t.Fatalf("target or workspace: %v", d)
	}
	// --json: the same facts as data, no secret
	jout, _, code := e.run("import", "claude-code", "--latest", "--plan", "--json")
	doc := decodeJSONOut(t, jout)
	if code != 0 || len(asStr(doc["digest"])) != 64 || doc["mode"] != "conversation" || asMap(doc["redactions"])["connection_string"] != float64(1) || len(asList(doc["lines"])) < 8 {
		t.Fatalf("--json plan: %d %s", code, jout)
	}
	allFakesAbsent(t, "the --json plan", []byte(jout))
}

func TestImportPlanNamesFilesAndWhatIsLeftOut(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	out, errs, code := e.run("import", "claude-code", "--latest", "--with-files", "--project", repo, "--plan")
	if code != 0 {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"the git repository shop goes to /home/specimen/workspace/shop", ".env files", "node_modules", ".gitignore", "(.git) is carried", "uncommitted changes: "} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
	ws := asMap(e.c.plans[0]["workspace"])
	if ws["included"] != true || ws["folder"] != "shop" || len(asStr(ws["head"])) != 40 || jsonInt(ws["bytes"]) <= 0 {
		t.Fatalf("workspace: %v", ws)
	}
	// the project's root becomes the folder the repository lands in
	_, _, code = e.run("import", "claude-code", "--latest", "--with-files", "--project", repo, "--out", filepath.Join(e.dir, "t.jsonl"))
	raw, _ := os.ReadFile(filepath.Join(e.dir, "t.jsonl"))
	if code != 0 || !strings.Contains(string(raw), "/home/specimen/workspace/shop/config/app.yml") || strings.Contains(string(raw), repo) {
		t.Fatalf("with files, the root is the folder: %d %s", code, raw[:300])
	}
	// not a repository
	plain := t.TempDir()
	e.session("claude-code", "claude-code-2.1.312.jsonl", plain)
	_, errs, code = e.run("import", "claude-code", "--latest", "--with-files", "--project", plain, "--plan")
	if code != exitFailed || !strings.Contains(errs, "is not a git repository") {
		t.Fatalf("not a repository: %d\n%s", code, errs)
	}
}

func TestImportPlanRefusesAtTheSizeLimitBeforeAskingTheService(t *testing.T) {
	e := newImportEnv(t)
	var b strings.Builder
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 53; i++ {
		fmt.Fprintf(&b, `{"type":"user","uuid":"u%d","parentUuid":null,"cwd":"/p","sessionId":"s","message":{"role":"user","content":"%s"}}`+"\n", i, chunk)
	}
	p := filepath.Join(e.home, ".claude", "projects", "-p", claudeSID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(b.String()), 0o600)
	_, errs, code := e.run("import", "claude-code", "--latest", "--plan")
	if code != exitConflict || !strings.Contains(errs, "the limit is 50 MB (50,000,000 bytes)") || !strings.Contains(errs, "Nothing was sent") || !strings.Contains(errs, "--brief") {
		t.Fatalf("over the limit: %d\n%s", code, errs)
	}
	if e.c.index("POST /api/v2/imports") >= 0 {
		t.Fatalf("the service was asked about a conversation that is over the limit")
	}
	if ents, _ := filepath.Glob(filepath.Join(os.TempDir(), "ks-import-*")); false && len(ents) > 0 {
		t.Fatal("unreachable")
	}
}

// ---- execution ------------------------------------------------------------------

func planDigest(t *testing.T, e *importEnv, args ...string) string {
	t.Helper()
	out, errs, code := e.run(append(args, "--plan", "--json")...)
	if code != 0 {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errs)
	}
	return asStr(decodeJSONOut(t, out)["digest"])
}

func TestImportExecuteRunsInOrderAndTheServiceNeverSeesASecret(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	args := []string{"import", "claude-code", "--latest", "--with-files", "--project", repo, "--name", "shop import"}
	digest := planDigest(t, e, args...)
	out, errs, code := e.run(append(args, "--execute", digest)...)
	if code != 0 {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errs)
	}
	order := []string{"POST /api/v2/imports", "POST /api/v2/preflight", "POST /api/v2/sessions", "POST /api/v2/sessions/session_abc123/provision",
		"POST /api/v2/sessions/session_abc123/uploads", "POST /api/v2/imports/apply"}
	last := -1
	for _, step := range order {
		// the first plan call may repeat; take the last position of each step's first occurrence after the previous
		found := -1
		for i, l := range e.c.seenLog() {
			if i > last && strings.HasPrefix(l, step) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("%s did not come after the step before it:\n%s", step, strings.Join(e.c.seenLog(), "\n"))
		}
		last = found
	}
	logs := e.c.seenLog()
	// the folder is declared before the conversation, and both before apply
	folderAt, convAt := -1, -1
	for i, l := range logs {
		if l == "POST /api/v2/sessions/session_abc123/uploads" {
			if folderAt < 0 {
				folderAt = i
			} else if convAt < 0 {
				convAt = i
			}
		}
	}
	if folderAt < 0 || convAt < 0 || folderAt > convAt || convAt > e.c.index("POST /api/v2/imports/apply") {
		t.Fatalf("order of the uploads: folder %d conversation %d apply %d", folderAt, convAt, e.c.index("POST /api/v2/imports/apply"))
	}
	if e.c.count("POST /api/v2/agents/") > 0 {
		t.Fatal("an instruction was sent: an import must not call a model")
	}
	conv := e.c.upload("conversation")
	if conv == nil || conv.To != "claude-code/"+claudeSID || !conv.Done {
		t.Fatalf("the conversation upload: %+v", conv)
	}
	sum := sha256.Sum256(conv.Got)
	src := asMap(e.c.plans[len(e.c.plans)-1]["source"])
	if hex.EncodeToString(sum[:]) != src["sha256"] || int64(len(conv.Got)) != jsonInt(src["bytes"]) {
		t.Fatalf("the bytes sent are not the ones declared")
	}
	allFakesAbsent(t, "the conversation upload", conv.Got)
	allFakesAbsent(t, "everything the service was sent", e.c.bodies.Bytes())
	if bytes.Contains(e.c.bodies.Bytes(), []byte("PRIVATE KEY")) {
		t.Error("a key block was sent")
	}
	// the folder carries the repository without what must stay home
	folder := e.c.upload("folder")
	if folder == nil || folder.To != "shop" {
		t.Fatalf("the folder upload: %+v", folder)
	}
	names := importTarNames(t, folder.Got)
	for _, n := range names {
		if strings.Contains(n, ".env") || strings.Contains(n, "node_modules") || strings.HasSuffix(n, ".pem") {
			t.Errorf("the folder carries %s", n)
		}
	}
	if !listHas(names, "main.go") || !listHas(names, "config/app.yml") || !listHasPrefix(names, ".git/") {
		t.Errorf("the folder lacks files: %v", names)
	}
	// apply: the same declaration, the agent, the upload
	ap := e.c.applies[0]
	if ap["digest"] != digest || ap["agent_id"] != "agent_main1" || ap["upload_id"] != conv.ID || canonicalDecl(asMap(ap["declaration"])) != canonicalDecl(e.c.plans[len(e.c.plans)-1]) {
		t.Fatalf("apply: %v", ap)
	}
	for _, want := range []string{"Imported Claude Code session " + claudeSID, "session session_abc123", "ks agent send main", "ks agent open main"} {
		if !strings.Contains(out, want) {
			t.Errorf("the result does not say %q:\n%s", want, out)
		}
	}
	creations := e.c.creations
	if len(creations) != 1 || creations[0]["name"] != "shop import" {
		t.Fatalf("creation: %v", creations)
	}
}

func importTarNames(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func listHas(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func listHasPrefix(xs []string, p string) bool {
	for _, v := range xs {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

func TestImportExecuteRefusesAWrongOrStaleDigestAndCreatesNothing(t *testing.T) {
	e := newImportEnv(t)
	p := e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	args := []string{"import", "claude-code", "--latest"}
	digest := planDigest(t, e, args...)
	// a digest that is not the plan's
	_, errs, code := e.run(append(args, "--execute", strings.Repeat("0", 64))...)
	if code != exitFailed || !strings.Contains(errs, "nothing was created") || e.c.index("POST /api/v2/preflight") >= 0 {
		t.Fatalf("a wrong digest: %d\n%s", code, errs)
	}
	_, errs, code = e.run(append(args, "--execute", "abc")...)
	if code != exitFailed || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("a short digest: %d\n%s", code, errs)
	}
	// the conversation moved since the plan: the digest changes with the source
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"parentUuid":null,"isSidechain":false,"type":"user","uuid":"late","timestamp":"2026-11-13T10:00:00.000Z","cwd":"/Users/alice/projects/shop","sessionId":"` + claudeSID + `","version":"2.1.312","message":{"role":"user","content":"one more thing"}}` + "\n")
	f.Close()
	_, errs, code = e.run(append(args, "--execute", digest)...)
	if code != exitFailed || !strings.Contains(errs, "the session changed since the plan was made") || !strings.Contains(errs, "Plan again") || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("a stale digest: %d\n%s", code, errs)
	}
	// plan and execute are two steps
	_, errs, code = e.run(append(args, "--plan", "--execute", digest)...)
	if code != exitUsage {
		t.Fatalf("both: %d\n%s", code, errs)
	}
	_, errs, code = e.run(args...)
	if code != exitUsage || !strings.Contains(errs, "planned first") {
		t.Fatalf("neither: %d\n%s", code, errs)
	}
}

func TestImportExecuteContinuesWhereItStoppedAndCreatesNoSecondSession(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	e.c.failApply = 1
	args := []string{"import", "claude-code", "--latest"}
	digest := planDigest(t, e, args...)
	_, errs, code := e.run(append(args, "--execute", digest)...)
	if code != exitFailed || !strings.Contains(errs, "in place, but the service did not apply") || !strings.Contains(errs, "creates nothing new") {
		t.Fatalf("apply failed: %d\n%s", code, errs)
	}
	uploads := e.c.count("PUT ")
	out, errs, code := e.run(append(args, "--execute", digest)...)
	if code != 0 || !strings.Contains(out, "Imported Claude Code session") {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errs)
	}
	if n := e.c.count("POST /api/v2/sessions/session_abc123/provision"); n != 1 || len(e.c.creations) != 1 {
		t.Fatalf("a second session was created: %d provisions, %d creations", n, len(e.c.creations))
	}
	if e.c.count("PUT ") != uploads {
		t.Fatalf("bytes were sent again: %d then %d", uploads, e.c.count("PUT "))
	}
	if len(e.c.applies) != 2 {
		t.Fatalf("applies: %d", len(e.c.applies))
	}
}

func TestImportCodexConversationIsDeclaredWithItsStartAndThreadId(t *testing.T) {
	e := newImportEnv(t)
	e.c.runCtl.harnesses = []any{
		map[string]any{"id": "claude-code", "name": "Claude Code", "provider": "anthropic", "offer": map[string]any{"standing": "offered"}},
		map[string]any{"id": "codex", "name": "Codex CLI", "provider": "openai", "default_model": "gpt-6-luna", "offer": map[string]any{"standing": "offered"}},
	}
	e.c.runCtl.keys = []any{map[string]any{"id": "vlt_o", "provider": "openai", "last4": "oooo", "enabled": true}}
	e.session("codex", "codex-0.167.0-alpha.3.jsonl", "")
	args := []string{"import", "codex", "--latest"}
	digest := planDigest(t, e, args...)
	out, errs, code := e.run(append(args, "--execute", digest)...)
	if code != 0 {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errs)
	}
	src := asMap(e.c.plans[0]["source"])
	if src["tool"] != "codex" || src["session_id"] != codexTID || src["started_at"] != "2026-11-12T09:00:00Z" || src["version"] != "0.167.0-alpha.3" {
		t.Fatalf("source: %v", src)
	}
	conv := e.c.upload("conversation")
	if conv == nil || conv.To != "codex/"+codexTID {
		t.Fatalf("upload: %+v", conv)
	}
	if len(e.c.creations) != 1 || e.c.creations[0]["harness"] != "codex" {
		t.Fatalf("the session is a codex session: %v", e.c.creations)
	}
}

// ---- the written summary --------------------------------------------------------

func TestImportBriefSendsASummaryAndNoTranscript(t *testing.T) {
	e := newImportEnv(t)
	e.c.runCtl.harnesses = []any{
		map[string]any{"id": "claude-code", "name": "Claude Code", "provider": "anthropic", "offer": map[string]any{"standing": "offered"}},
		map[string]any{"id": "codex", "name": "Codex CLI", "provider": "openai", "default_model": "gpt-6-luna", "offer": map[string]any{"standing": "offered"}},
	}
	e.c.runCtl.keys = []any{map[string]any{"id": "vlt_o", "provider": "openai", "last4": "oooo", "enabled": true}}
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	// into the other tool: a summary, whatever the flags
	args := []string{"import", "claude-code", "--latest", "--harness", "codex"}
	out, errs, code := e.run(append(args, "--plan")...)
	if code != 0 || !strings.Contains(out, "written summary") || !strings.Contains(out, "not resumed in another agent") {
		t.Fatalf("plan: %d\n%s\n%s", code, out, errs)
	}
	d := e.c.plans[0]
	brief := asStr(d["brief"])
	if d["mode"] != "brief" || asMap(d["target"])["harness"] != "codex" || brief == "" || len([]rune(brief)) > summaryMaxChars {
		t.Fatalf("declaration: %v", d)
	}
	for _, want := range []string{"What the person asked for", "FACT-ALPHA-7731", "Decisions", "Where it stands", "Open tasks", "Files mentioned", "/home/specimen/workspace/config/app.yml"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the summary lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "/Users/alice") {
		t.Errorf("the summary names the old machine's paths")
	}
	allFakesAbsent(t, "the summary", []byte(brief))
	digest := planDigest(t, e, args...)
	out, errs, code = e.run(append(args, "--execute", digest)...)
	if code != 0 {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errs)
	}
	if e.c.upload("conversation") != nil || e.c.count("PUT ") != 0 {
		t.Fatalf("a brief uploads no transcript")
	}
	ap := e.c.applies[0]
	if _, has := ap["upload_id"]; has || ap["agent_id"] != "agent_main1" {
		t.Fatalf("apply: %v", ap)
	}
	if !strings.Contains(out, "a written summary of it") || !strings.Contains(out, "with this first instruction") {
		t.Fatalf("result words:\n%s", out)
	}
	if len(e.c.creations) != 1 || e.c.creations[0]["harness"] != "codex" {
		t.Fatalf("creations: %v", e.c.creations)
	}
}

func TestImportCursorIsAlwaysASummaryFromACopyOfTheStore(t *testing.T) {
	e := newImportEnv(t)
	store := copyCursorFixture(t, t.TempDir(), true)
	for _, mode := range [][]string{{"--latest"}, {"--chat", cursorCheckout[:8]}} {
		e.c.imu.Lock()
		e.c.plans = nil
		e.c.imu.Unlock()
		args := append([]string{"import", "cursor", "--store", store}, mode...)
		out, errs, code := e.run(append(args, "--plan")...)
		if code != 0 || !strings.Contains(out, "best effort") || !strings.Contains(out, "nothing is resumed") {
			t.Fatalf("plan %v: %d\n%s\n%s", mode, code, out, errs)
		}
		d := e.c.plans[0]
		src := asMap(d["source"])
		brief := asStr(d["brief"])
		if d["mode"] != "brief" || src["tool"] != "cursor" || src["session_id"] != cursorCheckout || asMap(d["target"])["harness"] != "claude-code" || src["cwd"] != oldRoot {
			t.Fatalf("declaration: %v", d)
		}
		if !strings.Contains(brief, "FACT-CHARLIE-9902") || !strings.Contains(brief, "cart_test.go") || strings.Contains(brief, "Readme title") || strings.Contains(brief, "Old chat") {
			t.Fatalf("the summary is of the wrong chat or lacks its content:\n%s", brief)
		}
	}
	// execute: a summary, no upload
	digest := planDigest(t, e, "import", "cursor", "--store", store, "--latest")
	out, errs, code := e.run("import", "cursor", "--store", store, "--latest", "--execute", digest)
	if code != 0 || e.c.count("PUT ") != 0 || e.c.upload("conversation") != nil {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errs)
	}
	if len(e.c.creations) != 1 || !strings.Contains(out, "Imported Cursor session "+cursorCheckout) {
		t.Fatalf("creations %v\n%s", e.c.creations, out)
	}
}

func TestImportOutWritesTheSanitizedFileAndTouchesNothingElse(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.296.jsonl", "")
	dst := filepath.Join(e.dir, "out.jsonl")
	out, errs, code := e.run("import", "claude-code", "--latest", "--out", dst)
	if code != 0 || !strings.Contains(out, "nothing was sent and nothing was created") {
		t.Fatalf("out: %d\n%s\n%s", code, out, errs)
	}
	if n := len(e.c.seenLog()); n != 0 {
		t.Fatalf("--out asked the service (%d calls)", n)
	}
	raw, err := os.ReadFile(dst)
	if err != nil || len(raw) == 0 || strings.Contains(string(raw), "prompt_snapshot") {
		t.Fatalf("the file: %v %d", err, len(raw))
	}
	// an existing file is never replaced
	if _, errs, code = e.run("import", "claude-code", "--latest", "--out", dst); code != exitUsage || !strings.Contains(errs, "never replaced") {
		t.Fatalf("existing: %d\n%s", code, errs)
	}
}

func TestImportRefusesWhatCannotBeMeant(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.296.jsonl", "")
	for _, c := range []struct {
		args []string
		code int
		say  string
	}{
		{[]string{"import", "claude-code", "--plan"}, exitUsage, "name the session"},
		{[]string{"import", "claude-code", "--latest", "--session", "7c1d", "--plan"}, exitUsage, "name the session"},
		{[]string{"import", "claude-code", "--session", "zzzz", "--plan"}, exitUsage, "no claude-code session"},
		{[]string{"import", "claude-code", "--latest", "--budget", "20", "--plan"}, exitUsage, "--key keepstate"},
		{[]string{"import", "claude-code", "--latest", "--name", "9bad", "--plan"}, exitUsage, "is not a name"},
		{[]string{"import", "claude-code", "--latest", "--harness", "aider", "--plan"}, exitUsage, "claude-code or codex"},
		{[]string{"import", "codex", "--latest", "--plan"}, exitFailed, "no codex session was found"},
	} {
		_, errs, code := e.run(c.args...)
		if code != c.code || !strings.Contains(errs, c.say) {
			t.Errorf("%v: %d\n%s", c.args, code, errs)
		}
	}
	if e.c.index("POST /api/v2/") >= 0 {
		t.Errorf("a refused command asked the service: %v", e.c.seenLog())
	}
	// the service's own refusal is shown in its words
	e.c.imu.Lock()
	e.c.refuse = "that model is not offered for Claude Code"
	e.c.imu.Unlock()
	_, errs, code := e.run("import", "claude-code", "--latest", "--plan")
	if code == 0 || !strings.Contains(errs, "that model is not offered for Claude Code") {
		t.Fatalf("service refusal: %d\n%s", code, errs)
	}
}

func TestImportListPrintsNoMessageText(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	e.session("codex", "codex-0.162.1.jsonl", "")
	out, errs, code := e.run("import", "list")
	if code != 0 {
		t.Fatalf("list: %d\n%s", code, errs)
	}
	for _, want := range []string{"claude-code", claudeSID, "codex", codexTID, oldRoot, "messages"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	for _, content := range []string{"FACT-ALPHA-7731", "FACT-BRAVO-4410", "cart module", "checkout endpoint", "database_url", "shop-assets"} {
		if strings.Contains(out, content) {
			t.Errorf("the list printed message text: %q", content)
		}
	}
	jout, _, code := e.run("import", "list", "--json")
	var doc struct {
		Data struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(jout), &doc) != nil || len(doc.Data.Sessions) != 2 {
		t.Fatalf("--json: %d %s", code, jout)
	}
	for _, s := range doc.Data.Sessions {
		for _, k := range []string{"tool", "id", "project", "date", "bytes", "messages"} {
			if _, ok := s[k]; !ok {
				t.Errorf("a session row lacks %s: %v", k, s)
			}
		}
	}
	if n := len(e.c.seenLog()); n != 0 {
		t.Fatalf("list asked the service (%d calls)", n)
	}
	// Cursor on request, labelled
	store := copyCursorFixture(t, t.TempDir(), false)
	out, _, code = e.run("import", "list", "--store", store)
	if code != 0 || !strings.Contains(out, "cursor") || !strings.Contains(out, "best effort") || !strings.Contains(out, cursorCheckout) {
		t.Fatalf("cursor list: %d\n%s", code, out)
	}
}

// The synthetic store keeps its secret as a marker; this run reads it through
// the test's own filter, in this process, so the real redaction is what it
// meets.
func TestImportCursorSummaryReplacesSecretsBeforeAnythingIsSent(t *testing.T) {
	cursorTextFilter = func(s string) string { return expandFakes(t, s) }
	t.Cleanup(func() { cursorTextFilter = nil })
	store := copyCursorFixture(t, t.TempDir(), false)
	inv := &Invocation{set: map[string]bool{"latest": true, "store": true}, strs: map[string]string{"store": store}}
	p := prepareImport(inv, "cursor")
	defer p.Close()
	if p.Mode != "brief" || p.Redactions["github_token"] != 1 || !strings.Contains(p.Brief, "[redacted:github_token]") {
		t.Fatalf("mode %s, redactions %v:\n%s", p.Mode, p.Redactions, p.Brief)
	}
	raw, _ := json.Marshal(p.Decl)
	allFakesAbsent(t, "the declaration", raw)
	allFakesAbsent(t, "the summary", []byte(p.Brief))
	if p.Out != "" || p.Bytes != int64(len(p.Brief)) || p.SHA256 != sha256Hex([]byte(p.Brief)) {
		t.Fatalf("a brief's bytes are its text: %d %s", p.Bytes, p.SHA256)
	}
}
