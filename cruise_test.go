// cruise_test: the client side of the Cruise contract, measured. The two
// algorithms the fleet recomputes (the tree digest and the canonical
// manifest bytes) are pinned to values produced by judge/manifest.py, so a
// drift here is a digest the fleet would refuse. The verbs run as the
// built binary against a recording control plane, the way gates CR-1 and
// CR-7 run them.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
)

// Reference values from judge/manifest.py (python3 3.9, 2026-09-16) over
// the fixture written by refTree.
const (
	refTreeDigest = "c1f32d4a4e3155952dd69766fb1a48cb137743ccaf4746f5911ca13450fb612e"
	refCanonical  = `{"A":0,"_":-5,"goal":"caf\u00e9 <b> & \"q\" \\ \b\f\n\r\t \u0001 \ud83d\ude00 \u2028 \u007f ~","n":2000000,"s":"plain","z":[1,{"a":null,"b":true},"x"]}`
	refCanonSHA   = "e03f567557b4ad122958e96e4b4a7205c8b8347c543cb77e6c9b5a49486dc6ec"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// refTree is the fixture the reference digest was computed over, plus the
// entries the client must ignore: .git and .keepstate at two depths and an
// empty directory. (Until the upload policy of KS-027 a symlink to a
// directory was part of this fixture and skipped; every symlink is now
// refused with its path, see TestSymlinksAreRefusedWithTheirPaths.)
func refTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bin := make([]byte, 256)
	for i := range bin {
		bin[i] = byte(i)
	}
	writeTree(t, root, map[string]string{
		"z.txt":       "zed\n",
		"a/f.txt":     "af\n",
		"a/b/g.txt":   "abg\n",
		"a-x/h.txt":   "a-x h\n",
		"m/n/o/p.bin": string(bin),
		"empty.txt":   "",
	})
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		".git/config":              "[core]\n",
		".keepstate/cruise.json":   "{}",
		"a/.git/HEAD":              "ref\n",
		"m/.keepstate/verifier.js": "x",
	})
	return root
}

func TestTreeDigestMatchesJudge(t *testing.T) {
	root := refTree(t)
	files, err := scanWorkspace(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range files {
		order = append(order, f.rel)
	}
	want := []string{"empty.txt", "z.txt", "a/f.txt", "a-x/h.txt", "a/b/g.txt", "m/n/o/p.bin"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("walk order %v, want os.walk's %v", order, want)
	}
	if got := treeDigest(files); got != refTreeDigest {
		t.Fatalf("tree digest %s, judge/manifest.py says %s", got, refTreeDigest)
	}
	if got := treeDigest(nil); got != hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }()) {
		t.Fatalf("empty tree digest %s", got)
	}
}

func TestCanonicalJSONMatchesJudge(t *testing.T) {
	obj := map[string]any{
		"goal": "caf\u00e9 <b> & \"q\" \\ \b\f\n\r\t \u0001 \U0001F600 \u2028 \u007f ~",
		"n":    2000000,
		"z":    []any{1, map[string]any{"b": true, "a": nil}, "x"},
		"s":    "plain",
		"A":    0,
		"_":    -5,
	}
	got, err := canonicalJSON(obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != refCanonical {
		t.Fatalf("canonical bytes differ from judge/manifest.py:\n got %s\nwant %s", got, refCanonical)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != refCanonSHA {
		t.Fatalf("digest %x, judge says %s", sum, refCanonSHA)
	}
	// the same object read back from a file, numbers verbatim, digests the same
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&b)
	dec.UseNumber()
	var back map[string]any
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if sha, _ := manifestSHA(back); sha != refCanonSHA {
		t.Fatalf("round-tripped manifest digests to %s, want %s", sha, refCanonSHA)
	}
	if _, err := canonicalJSON(map[string]any{"x": 1.5}); err == nil {
		t.Fatal("a fraction must be refused, not rounded")
	}
	if _, err := canonicalJSON(map[string]any{"x": "\xff"}); err == nil {
		t.Fatal("invalid UTF-8 must be refused")
	}
}

func unpack(t *testing.T, tgz []byte, dir string) []*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var hdrs []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		hdrs = append(hdrs, h)
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, os.FileMode(h.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	return hdrs
}

func TestWorkspaceTarballIsDeterministicAndRoundTrips(t *testing.T) {
	root := refTree(t)
	var a, b bytes.Buffer
	if _, err := scanWorkspace(root, &a); err != nil {
		t.Fatal(err)
	}
	if _, err := scanWorkspace(root, &b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two packs of an unchanged tree differ; the sha256 at init would not be the sha256 at run")
	}
	dir := t.TempDir()
	hdrs := unpack(t, a.Bytes(), dir)
	for _, h := range hdrs {
		if strings.Contains(h.Name, ".git") || strings.Contains(h.Name, ".keepstate") || strings.HasPrefix(h.Name, "link-to-a") {
			t.Errorf("tarball carries %s", h.Name)
		}
		if h.ModTime.Unix() != 0 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("entry %s carries host metadata: %v uid %d gid %d %q %q", h.Name, h.ModTime, h.Uid, h.Gid, h.Uname, h.Gname)
		}
	}
	files, err := scanWorkspace(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := treeDigest(files); got != refTreeDigest {
		t.Fatalf("unpacked tree digests to %s, want %s: the fleet would refuse the workspace", got, refTreeDigest)
	}
}

func TestWorkspaceOverLimitIsRefusedBeforePacking(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"test_a.py": "def test_a(): pass\n"})
	f, err := os.Create(filepath.Join(root, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(cruiseMaxBytes + 1); err != nil { // sparse: the check is on sizes
		t.Fatal(err)
	}
	f.Close()
	packed := 0
	_, err = scanWorkspace(root, writerFunc(func(p []byte) (int, error) { packed += len(p); return len(p), nil }))
	if err == nil || !strings.Contains(err.Error(), "50 MiB") {
		t.Fatalf("oversize workspace not refused with the limit named: %v", err)
	}
	if packed != 0 {
		t.Fatalf("%d bytes were packed before the refusal", packed)
	}
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestDollarsAndUSD(t *testing.T) {
	for micro, want := range map[int64]string{2000000: "$2.00", 12345: "$0.012345", 10000: "$0.01", 0: "$0.00", 120000: "$0.12", -500000: "-$0.50", 1: "$0.000001"} {
		if got := dollars(micro); got != want {
			t.Errorf("dollars(%d) = %s, want %s", micro, got, want)
		}
	}
	for in, want := range map[string]int64{"2": 2000000, "2.5": 2500000, "$0.25": 250000, "0.000001": 1, "10.": 10000000} {
		got, err := parseUSD(in)
		if err != nil || got != want {
			t.Errorf("parseUSD(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "abc", "1.2345678", "-1", "1e6"} {
		if _, err := parseUSD(bad); err == nil {
			t.Errorf("parseUSD(%q) accepted", bad)
		}
	}
}

func TestDetectChecks(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"pytest at root", map[string]string{"app.py": "", "test_app.py": "def test(): pass"}, "python3 -m pytest -q"},
		{"pytest nested", map[string]string{"pkg/tests/test_x.py": ""}, "python3 -m pytest -q"},
		{"npm real script", map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`}, "npm test"},
		{"npm placeholder", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, ""},
		{"go with tests", map[string]string{"go.mod": "module x\n", "x_test.go": "package x"}, "go test ./..."},
		{"go without tests", map[string]string{"go.mod": "module x\n", "x.go": "package x"}, ""},
		{"nothing", map[string]string{"app.py": "print(1)"}, ""},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeTree(t, root, c.files)
		files, err := scanWorkspace(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := detectChecks(root, files).command; got != c.want {
			t.Errorf("%s: detected %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseLadder(t *testing.T) {
	l, err := parseLadder([]string{"anthropic:claude-haiku-4-5-20251001", "claude-sonnet-5", "openai:openai/gpt-4o"})
	if err != nil || len(l) != 3 || l[1].Family != "anthropic" || l[2].Provider() != "openrouter" {
		t.Fatalf("parseLadder: %v %v", l, err)
	}
	if _, err := parseLadder([]string{"claude-sonnet-5", "anthropic:claude-sonnet-5"}); err == nil {
		t.Fatal("a repeated rung must be refused")
	}
	if _, err := parseLadder([]string{"no-such-model"}); err == nil {
		t.Fatal("an unknown bare model id must be refused")
	}
}

func (r rung) Provider() string {
	if p := embeddedModels.Families[r.Family].Provider; p != "" {
		return p
	}
	return r.Family
}

// ---------------------------------------------------------------------
// The verbs, as the built binary against a recording control plane
// ---------------------------------------------------------------------

type fakeCtl struct {
	mu       sync.Mutex
	hits     []string
	posted   []byte // the last POST /api/jobs body
	job      map[string]any
	artifact []byte
	pfBlock  bool   // the job preflight answers a blocker (KS-029)
	keyAt    string // the anthropic key's created_at (KS-074: a rotation changes it)
	dropped  string // a model the catalog no longer lists (KS-075)
	keyOff   bool   // the anthropic key is disabled
}

func (f *fakeCtl) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func (f *fakeCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	// the test goroutine edits job and artifact between requests; the
	// handler serves a snapshot taken under the lock, never the live map
	job := make(map[string]any, len(f.job))
	for k, v := range f.job {
		job[k] = v
	}
	artifact := append([]byte(nil), f.artifact...)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	writeJSON := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/models":
		writeJSON(embeddedModels)
	case r.Method == "GET" && r.URL.Path == "/api/v2/models":
		f.mu.Lock()
		dropped := f.dropped
		f.mu.Unlock()
		var models []any
		for fam, mf := range embeddedModels.Families {
			for i, id := range mf.Rungs {
				if id == dropped {
					continue
				}
				models = append(models, map[string]any{"id": id, "family": fam, "provider_route": mf.Provider, "rung": i + 1,
					"runner_compatibility": map[string]any{"standing": map[bool]string{true: "exercised", false: "not_verified"}[fam == "anthropic"]},
					"availability":         map[string]any{"standing": "listed", "catalog_version": "v2", "live": "not checked"},
					"price":                map[string]any{"standing": "priced"},
					"key_route":            map[string]any{"provider": mf.Provider, "standing": map[bool]string{true: "enabled", false: "missing"}[fam == "anthropic"], "key_id": map[bool]string{true: "vlt_anthropic1", false: ""}[fam == "anthropic"]}})
			}
		}
		writeJSON(map[string]any{"schema_version": 2, "data": map[string]any{"catalog_version": "v2", "ratified": "2026-09-20", "served_at": "2026-09-26T10:00:00Z",
			"default_ladder": embeddedModels.DefaultLadder, "models": models, "note": "catalog data, never a charge"}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys":
		f.mu.Lock()
		at, off := f.keyAt, f.keyOff
		f.mu.Unlock()
		if at == "" {
			at = "2026-09-01T00:00:00Z"
		}
		writeJSON(map[string]any{"schema_version": 2, "data": map[string]any{"items": []any{
			map[string]any{"id": "vlt_anthropic1", "provider": "anthropic", "alias": "dev", "last4": "abcd", "enabled": !off, "revision": 1, "created_at": at}}}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/preflight":
		f.mu.Lock()
		block := f.pfBlock
		f.mu.Unlock()
		checks := []any{map[string]any{"check": "workspace", "status": "pass", "detail": "ok", "category": "setup"}}
		if block {
			checks = append(checks, map[string]any{"check": "key_route", "status": "block", "detail": "no enabled key for the ladder's provider: anthropic", "category": "setup", "next_action": "ks key add --provider anthropic"})
		}
		writeJSON(map[string]any{"schema_version": 2, "data": map[string]any{"checks": checks, "ready": !block}})
	case r.Method == "POST" && r.URL.Path == "/api/jobs":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posted = b
		f.mu.Unlock()
		w.WriteHeader(201)
		writeJSON(map[string]any{"id": "job_0123456789ab", "state": "queued", "spend_ceiling_microusd": 2000000})
	case r.Method == "GET" && r.URL.Path == "/api/jobs":
		writeJSON([]any{job})
	case r.Method == "GET" && r.URL.Path == "/api/jobs/job_0123456789ab":
		writeJSON(job)
	case r.Method == "GET" && r.URL.Path == "/api/jobs/job_0123456789ab/events":
		writeJSON([]any{
			map[string]any{"seq": 1, "ts": "2026-09-16T00:00:00Z", "type": "job.queued", "attempt_id": nil, "detail": map[string]any{}},
			map[string]any{"seq": 2, "ts": "2026-09-16T00:01:00Z", "type": "attempt.ambiguous", "attempt_id": "att_000000000001", "detail": map[string]any{"after_s": 60}},
		})
	case r.Method == "GET" && r.URL.Path == "/api/jobs/job_0123456789ab/artifact":
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(artifact)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
		writeJSON(map[string]any{"id": "job_0123456789ab", "state": "cancelled"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/resume"):
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posted = b
		f.mu.Unlock()
		writeJSON(map[string]any{"id": "job_0123456789ab", "state": "queued"})
	default:
		w.WriteHeader(404)
		writeJSON(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

// gateJob is gate CR-7's fixture: a job in review after one reconciled,
// failed attempt on the cheap rung, with no artifact.
func gateJob() map[string]any {
	return map[string]any{
		"id": "job_0123456789ab", "account_id": "acct_x", "state": "review",
		"manifest": map[string]any{"version": 1, "ladder": []any{map[string]any{"family": "anthropic", "model": "claude-haiku-4-5-20251001"}}, "limits": map[string]any{"spend_microusd": 500000}},
		"goal":     "g", "ladder_pos": 0, "spend_ceiling_microusd": 500000, "spent_microusd": 120000,
		"artifact_sha": nil, "verdict": "ladder-exhausted",
		"created_at": "2026-09-16T00:00:00Z", "updated_at": "2026-09-16T00:01:00Z",
		"attempts": []any{map[string]any{
			"id": "att_000000000001", "rung": 0, "model": "claude-haiku-4-5-20251001", "family": "anthropic",
			"state": "reconciled", "session_id": "s1", "parent_session_id": "p1", "restore_point": "cp1",
			"save_points": []any{"cp2"}, "cost_microusd": 120000, "verdict": "fail",
		}},
	}
}

func startFake(t *testing.T) (*fakeCtl, string, string) {
	t.Helper()
	f := &fakeCtl{job: gateJob(), artifact: []byte("not-a-tarball-but-bytes")}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return f, bin, cfg
}

// ksIn runs the binary in dir and returns stdout, stderr and the exit code.
func ksIn(t *testing.T, bin, cfg, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func demoRepo(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"README.md":         "# demo\n",
		"inventory.py":      "def count(xs):\n    return len(xs)\n",
		"test_inventory.py": "from inventory import count\n\ndef test_count():\n    assert count([1, 2]) == 2\n",
		"docs/notes.txt":    "n\n",
		".git/HEAD":         "ref: refs/heads/main\n",
	})
	return root
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func TestCruiseInitApproveRun(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := demoRepo(t)

	// init: the digest on its own first line, the draft and the verifier
	// config written, no request made
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--goal", "make the inventory tests pass")
	if code != 0 {
		t.Fatalf("init exit %d\n%s%s", code, out, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	initSHA := lines[0]
	if !isHex64(initSHA) {
		t.Fatalf("init's first line is not the digest: %q", lines[0])
	}
	if len(f.seen()) != 0 {
		t.Fatalf("init made requests: %v", f.seen())
	}
	if !strings.Contains(out, "python3 -m pytest -q") || !strings.Contains(out, "make the inventory tests pass") {
		t.Errorf("init summary lacks the check or the goal:\n%s", out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, cruiseDraft))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(m); err != nil {
		t.Fatalf("the draft fails the judge's validation: %v", err)
	}
	if sha, _ := manifestSHA(m); sha != initSHA {
		t.Fatalf("the draft on disk digests to %s, init printed %s", sha, initSHA)
	}
	ver := m["verifier"].(map[string]any)
	wantTests := sha256.Sum256(append([]byte("test_inventory.py"), []byte("from inventory import count\n\ndef test_count():\n    assert count([1, 2]) == 2\n")...))
	if ver["tests_digest"] != hex.EncodeToString(wantTests[:]) {
		t.Errorf("tests_digest %v, want sha256(path bytes + content bytes) over test_*.py = %x", ver["tests_digest"], wantTests)
	}
	cfgRaw, err := os.ReadFile(filepath.Join(repo, cruiseVerifier))
	if err != nil {
		t.Fatal(err)
	}
	cfgSum := sha256.Sum256(cfgRaw)
	if ver["config_hash"] != hex.EncodeToString(cfgSum[:]) {
		t.Errorf("config_hash is not the sha256 of %s", cruiseVerifier)
	}
	if !strings.Contains(string(cfgRaw), `"prohibited_paths":[".keepstate/","test_inventory.py"]`) {
		t.Errorf("verifier config does not pin the tests: %s", cfgRaw)
	}
	for _, k := range []string{"goal", "ladder", "checkpoint", "workspace", "image"} {
		if _, ok := m[k]; !ok {
			t.Errorf("draft lacks v2 key %s", k)
		}
	}
	if m["family"] != "job" || m["image"] != "claude" || m["data_classification"] != "customer" {
		t.Errorf("draft constants wrong: family=%v image=%v class=%v", m["family"], m["image"], m["data_classification"])
	}

	// run before approve: refused, names approve, no request
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "approve") || len(f.seen()) != 0 {
		t.Fatalf("run without approve: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}

	// approve: the same digest, the lock written
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "approve")
	if code != 0 {
		t.Fatalf("approve exit %d\n%s%s", code, out, errs)
	}
	// KS-074: approve writes the binding into the draft before digesting
	// it, so the approved digest is the bound draft's, not init's
	approvedSHA := strings.SplitN(out, "\n", 2)[0]
	if approvedSHA == initSHA || len(approvedSHA) != 64 {
		t.Fatalf("approve printed %s (init printed %s)", approvedSHA, initSHA)
	}
	if m, err := readDraftAt(repo); err != nil || m["binding_version"] == nil || m["routes"] == nil {
		t.Fatalf("the draft carries no binding: %v", err)
	} else if sha, _ := manifestSHA(m); sha != approvedSHA {
		t.Fatalf("the approved digest %s is not the bound draft's %s", approvedSHA, sha)
	}
	initSHA = approvedSHA
	lkRaw, err := os.ReadFile(filepath.Join(repo, cruiseLock))
	if err != nil {
		t.Fatal(err)
	}
	var lk lockFile
	if err := json.Unmarshal(lkRaw, &lk); err != nil || lk.SHA256 != initSHA || lk.ApprovedAt == "" {
		t.Fatalf("lock %s: %v", lkRaw, err)
	}
	// KS-074: approve reads the provider keys it binds, and nothing else
	if got := f.seen(); len(got) != 2 || got[0] != "GET /api/v2/models" || got[1] != "GET /api/v2/keys" {
		t.Fatalf("approve made requests: %v", got)
	}
	f.mu.Lock()
	f.hits = nil
	f.mu.Unlock()

	// KS-029: a preflight blocker stops the run before any upload
	f.mu.Lock()
	f.pfBlock = true
	f.mu.Unlock()
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code != exitConflict || !strings.Contains(errs, "no enabled key") || !strings.Contains(errs, "nothing was uploaded") {
		t.Fatalf("blocked run: exit %d\n%s", code, errs)
	}
	for _, h := range f.seen() {
		if h == "POST /api/jobs" {
			t.Fatal("a preflight blocker still created a job")
		}
	}
	f.mu.Lock()
	f.pfBlock, f.hits = false, nil
	f.mu.Unlock()

	// run: one POST, the manifest on the wire hashing to the approved digest
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code != 0 {
		t.Fatalf("run exit %d\n%s%s", code, out, errs)
	}
	// KS-029: the job preflight (an observation) and then exactly one job
	if got := f.seen(); len(got) != 4 || got[0] != "GET /api/v2/keys" || got[1] != "GET /api/v2/models" || got[2] != "POST /api/v2/preflight" || got[3] != "POST /api/jobs" {
		t.Fatalf("run made %v, want the route-key read, the catalog read, the preflight, then exactly one POST /api/jobs", got)
	}
	if strings.TrimSpace(out) != "job_0123456789ab" || !strings.Contains(errs, "$2.00") {
		t.Errorf("run output: stdout %q stderr %q", out, errs)
	}
	var posted struct {
		Manifest     json.RawMessage `json:"manifest"`
		ManifestSHA  string          `json:"manifest_sha"`
		WorkspaceB64 string          `json:"workspace_b64"`
	}
	if err := json.Unmarshal(f.posted, &posted); err != nil {
		t.Fatal(err)
	}
	wire := sha256.Sum256(posted.Manifest)
	if hex.EncodeToString(wire[:]) != posted.ManifestSHA || posted.ManifestSHA != initSHA {
		t.Fatalf("wire manifest digests to %x, manifest_sha %s, approved %s", wire, posted.ManifestSHA, initSHA)
	}
	tgz, err := base64.StdEncoding.DecodeString(posted.WorkspaceB64)
	if err != nil {
		t.Fatal(err)
	}
	ws := m["workspace"].(map[string]any)
	tsum := sha256.Sum256(tgz)
	if ws["sha256"] != hex.EncodeToString(tsum[:]) || ws["bytes"].(json.Number).String() != fmt.Sprint(len(tgz)) || ws["root"] != "." {
		t.Fatalf("uploaded tarball (%d bytes, %x) is not the draft's workspace %v", len(tgz), tsum, ws)
	}
	dir := t.TempDir()
	hdrs := unpack(t, tgz, dir)
	for _, h := range hdrs {
		if strings.HasPrefix(h.Name, ".git") || strings.HasPrefix(h.Name, ".keepstate") {
			t.Errorf("uploaded tarball carries %s", h.Name)
		}
	}
	files, err := scanWorkspace(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := treeDigest(files); got != m["initial_state_digest"] {
		t.Fatalf("the unpacked upload digests to %s, the manifest says %v", got, m["initial_state_digest"])
	}

	// a second init keeps the goal, refreshes the draft and drops the lock
	out, _, code = ksIn(t, bin, cfg, repo, "cruise", "init")
	if code != 0 || !strings.Contains(out, "make the inventory tests pass") || !strings.Contains(out, "previous approval removed") {
		t.Errorf("re-init: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(repo, cruiseLock)); !os.IsNotExist(err) {
		t.Error("re-init left the old lock in place")
	}
}

func TestCruiseInitRefusesWithoutACheck(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{"app.py": "print('hi')\n"})
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--goal", "add a feature")
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s%s", code, out, errs)
	}
	if !strings.Contains(strings.ToLower(errs), "check") {
		t.Errorf("the refusal does not say a job needs a check: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(repo, cruiseDir)); !os.IsNotExist(err) {
		t.Error("init wrote files for a repository it refused")
	}
	if len(f.seen()) != 0 {
		t.Errorf("init made requests: %v", f.seen())
	}
	// KS-072: the verifier runs pytest, go test or node --test and nothing
	// else, so a named command outside them is refused here rather than at
	// intake; a named pytest command over a repository with no tests is
	// refused as the verifier would refuse it
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "init", "--tests", "make check")
	if code != 2 || !strings.Contains(errs, "--check pytest|go|node") {
		t.Errorf("--tests make check: exit %d\n%s%s", code, out, errs)
	}
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--tests", "python3 -m pytest -q"); code != 2 || !strings.Contains(errs, "no check found") {
		t.Errorf("--tests pytest, no tests: exit %d\n%s", code, errs)
	}
}

func TestCruiseRunRefusals(t *testing.T) {
	f, bin, cfg := startFake(t)

	// a test changed after approve: named as tests_digest, before any request
	repo := demoRepo(t)
	if _, _, code := ksIn(t, bin, cfg, repo, "cruise", "init"); code != 0 {
		t.Fatal("init")
	}
	if _, _, code := ksIn(t, bin, cfg, repo, "cruise", "approve"); code != 0 {
		t.Fatal("approve")
	}
	f.mu.Lock()
	f.hits = nil // approve reads the provider keys it binds (KS-074); the runs below must send nothing
	f.mu.Unlock()
	fp := filepath.Join(repo, "test_inventory.py")
	orig, _ := os.ReadFile(fp)
	if err := os.WriteFile(fp, append(orig, []byte("\ndef test_forged():\n    assert True\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "tests_digest") {
		t.Errorf("changed test: exit %d, %s", code, errs)
	}
	if err := os.WriteFile(fp, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	// the draft edited after approve
	draft := filepath.Join(repo, cruiseDraft)
	d, _ := os.ReadFile(draft)
	if err := os.WriteFile(draft, bytes.Replace(d, []byte(`"time_s": 1800`), []byte(`"time_s": 3600`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "changed since approve") {
		t.Errorf("changed draft: exit %d, %s", code, errs)
	}
	if err := os.WriteFile(draft, d, 0o644); err != nil {
		t.Fatal(err)
	}

	// a source file changed after approve
	if err := os.WriteFile(filepath.Join(repo, "inventory.py"), []byte("def count(xs):\n    return 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "workspace changed since approve") {
		t.Errorf("changed workspace: exit %d, %s", code, errs)
	}

	// gate CR-7f: a hand-written, approved draft over an oversize
	// workspace is refused on the size, before the lock digest is compared
	big := t.TempDir()
	writeTree(t, big, map[string]string{".git/HEAD": "x\n"})
	bf, err := os.Create(filepath.Join(big, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bf.Truncate(cruiseMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	bf.Close()
	zeros := strings.Repeat("0", 64)
	hand := map[string]any{"family": "job", "version": 1, "goal": "g", "task_boundary": "x", "initial_state_digest": zeros,
		"workspace": map[string]any{"sha256": "", "bytes": 0, "root": "."}, "image": "claude",
		"permitted":           map[string]any{"tools": []any{}, "providers": []any{"anthropic"}, "regions": []any{"westeurope"}, "external_effects": []any{}},
		"data_classification": "customer", "storage_consent": "x", "done_condition": "x",
		"verifier":   map[string]any{"implementation": "judge/verifier.py", "config_hash": zeros, "tests_digest": zeros, "command": "python3 -m pytest -q", "checks_origin": "repo"},
		"limits":     map[string]any{"time_s": 600, "spend_microusd": 100, "attempts": 1, "cheap_max_output_tokens": 10, "reserve_microusd": 1},
		"escalation": map[string]any{"human_review_on": []any{}, "on_strong_failure": "human-review"},
		"ladder":     []any{map[string]any{"family": "anthropic", "model": "claude-haiku-4-5-20251001"}},
		"checkpoint": map[string]any{"before_attempt": true, "every_s": 300, "after_pass": true, "on_call_boundary": false}}
	hb, _ := json.Marshal(hand)
	writeTree(t, big, map[string]string{cruiseDraft: string(hb), cruiseLock: `{"sha256":"` + zeros + `","approved_at":"2026-09-16T00:00:00Z"}`})
	_, errs, code = ksIn(t, bin, cfg, big, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "50 MiB") {
		t.Errorf("oversize: exit %d, %s", code, errs)
	}

	if len(f.seen()) != 0 {
		t.Fatalf("a refused run made requests: %v", f.seen())
	}
}

func TestCruiseStatusAndLogsWords(t *testing.T) {
	f, bin, cfg := startFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "status", "job_0123456789ab")
	if code != 0 {
		t.Fatalf("status exit %d\n%s%s", code, out, errs)
	}
	for _, word := range []string{"state review", "verdict: ladder-exhausted", "state reconciled", "rung 1 of 1 (claude-haiku-4-5-20251001)",
		"cost $0.12", "spent $0.12 of $0.50 ceiling", "artifact: unavailable", "save points: 1", "verdict fail"} {
		if !strings.Contains(out, word) {
			t.Errorf("status lacks %q:\n%s", word, out)
		}
	}
	if strings.Contains(out, "$0.00") || strings.Contains(out, " 0\n") {
		t.Errorf("status renders an absent value as zero:\n%s", out)
	}
	// the newest job when none is named: the list, then the job itself
	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "status")
	if code != 0 || !strings.Contains(out, "job job_0123456789ab  state review") {
		t.Errorf("status without a job: exit %d\n%s", code, out)
	}

	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "logs", "job_0123456789ab")
	if code != 0 {
		t.Fatalf("logs exit %d\n%s%s", code, out, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "1 2026-09-16T00:00:00Z job.queued" ||
		lines[1] != `2 2026-09-16T00:01:00Z attempt.ambiguous att_000000000001 {"after_s":60}` {
		t.Errorf("logs lines: %q", lines)
	}

	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "cancel", "job_0123456789ab")
	if code != 0 || strings.TrimSpace(out) != "job_0123456789ab" {
		t.Errorf("cancel: exit %d, %q", code, out)
	}
	_, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "resume", "job_0123456789ab", "--ladder", "anthropic:claude-sonnet-5,openai:openai/gpt-4o")
	if code != 0 || !strings.Contains(string(f.posted), `{"ladder":[{"family":"anthropic","model":"claude-sonnet-5"},{"family":"openai","model":"openai/gpt-4o"}]}`) {
		t.Errorf("resume: exit %d, posted %s", code, f.posted)
	}
	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "models")
	if code != 0 || !strings.Contains(out, "model catalog v2, served at 2026-09-26T10:00:00Z") || !strings.Contains(out, "claude-sonnet-5") {
		t.Errorf("models: exit %d\n%s", code, out)
	}
}

// KS-075: the catalog as the service states it -- exact ids, family, route,
// rung, exercised/not_verified, price standing and this account's key
// route; live availability said to be unchecked; a cached copy labelled as
// such; and a ladder naming an unlisted model refused before any upload,
// nothing substituted.
func TestCruiseModelsCatalogAndLadderValidation(t *testing.T) {
	f, bin, cfg := startFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "models")
	if code != 0 {
		t.Fatalf("models: %d\n%s", code, errs)
	}
	for _, want := range []string{"model catalog v2, served at 2026-09-26T10:00:00Z", "NOT checked", "claude-haiku-4-5-20251001", "anthropic", "exercised", "openai/gpt-4o-mini", "openrouter", "not_verified", "enabled (vlt_anthropic1)", "missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("models lacks %q:\n%s", want, out)
		}
	}
	before := len(f.seen())
	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "models", "--cached")
	if code != 0 || !strings.Contains(out, "CACHED model catalog v2") || !strings.Contains(out, "not a live answer") || len(f.seen()) != before {
		t.Fatalf("cached: %d\n%s", code, out)
	}
	// a model dropped from the catalog: approve refuses, nothing bound
	repo := demoRepo(t)
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init"); code != 0 {
		t.Fatalf("init: %s", errs)
	}
	f.mu.Lock()
	f.dropped = "claude-sonnet-5"
	f.mu.Unlock()
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "approve")
	if code != exitConflict || !strings.Contains(errs, `model "claude-sonnet-5" (family anthropic) is not listed in catalog v2`) || !strings.Contains(errs, "nothing is substituted") {
		t.Fatalf("approve with a dropped model: %d\n%s", code, errs)
	}
	// dropped after approval: run refuses before any upload
	f.mu.Lock()
	f.dropped = ""
	f.mu.Unlock()
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "approve"); code != 0 {
		t.Fatalf("approve: %s", errs)
	}
	f.mu.Lock()
	f.dropped, f.hits = "claude-haiku-4-5-20251001", nil
	f.mu.Unlock()
	_, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code == 0 || !strings.Contains(errs, "claude-haiku-4-5-20251001") {
		t.Fatalf("run with a dropped model: %d\n%s", code, errs)
	}
	for _, h := range f.seen() {
		if h == "POST /api/jobs" || h == "POST /api/v2/preflight" {
			t.Fatalf("a refused run sent %s", h)
		}
	}
}

func TestCruiseArtifactVerifiesBeforeWriting(t *testing.T) {
	f, bin, cfg := startFake(t)
	f.mu.Lock()
	good := sha256.Sum256(f.artifact)
	f.job["state"], f.job["verdict"], f.job["artifact_sha"] = "accepted", "accepted", hex.EncodeToString(good[:])
	f.mu.Unlock()
	dir := t.TempDir()

	// tampered bytes: refused, the digest named, the file absent
	f.mu.Lock()
	f.artifact = append(f.artifact, []byte("tampered")...)
	f.mu.Unlock()
	out := filepath.Join(dir, "bad.tgz")
	_, errs, code := ksIn(t, bin, cfg, dir, "cruise", "artifact", "job_0123456789ab", "--out", out)
	if code == 0 || !strings.Contains(errs, "sha256") {
		t.Errorf("tampered artifact: exit %d, %s", code, errs)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("tampered artifact was written to %s", out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ks-artifact-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}

	// the real bytes: written, and the path printed
	f.mu.Lock()
	f.artifact = f.artifact[:len(f.artifact)-len("tampered")]
	want := append([]byte(nil), f.artifact...)
	f.mu.Unlock()
	stdout, errs, code := ksIn(t, bin, cfg, dir, "cruise", "artifact", "job_0123456789ab", "--out", out)
	if code != 0 || strings.TrimSpace(stdout) != out {
		t.Fatalf("artifact: exit %d, stdout %q, %s", code, stdout, errs)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("written artifact differs: %v", err)
	}

	// no artifact yet: refused with the job's state and verdict
	f.mu.Lock()
	f.job["artifact_sha"] = nil
	f.mu.Unlock()
	_, errs, code = ksIn(t, bin, cfg, dir, "cruise", "artifact", "job_0123456789ab", "--out", filepath.Join(dir, "none.tgz"))
	if code == 0 || !strings.Contains(errs, "no artifact") {
		t.Errorf("absent artifact: exit %d, %s", code, errs)
	}
}

// TestJudgeCrossCheck runs the judge's own manifest.py, when a checkout is
// named by KS_JUDGE_DIR, over a client draft: the judge must lock it and
// compute the same canonical digest, and its digest-tree must equal the
// client's tree digest. Skipped elsewhere; the reference constants above
// pin the same facts.
func TestJudgeCrossCheck(t *testing.T) {
	judge := os.Getenv("KS_JUDGE_DIR")
	if judge == "" {
		t.Skip("KS_JUDGE_DIR not set")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	_, bin, cfg := startFake(t)
	repo := demoRepo(t)
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--goal", "caf\u00e9 <b> & \"q\" \U0001F600")
	if code != 0 {
		t.Fatalf("init: %s%s", out, errs)
	}
	clientSHA := strings.SplitN(out, "\n", 2)[0]

	key := filepath.Join(t.TempDir(), "judge.key")
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(t.TempDir(), "manifest.json")
	d, _ := os.ReadFile(filepath.Join(repo, cruiseDraft))
	if err := os.WriteFile(draft, d, 0o644); err != nil {
		t.Fatal(err)
	}
	lock := exec.Command("python3", filepath.Join(judge, "manifest.py"), "lock", draft)
	lock.Env = append(os.Environ(), "KS_JUDGE_KEY="+key)
	if lo, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("judge refuses the client's draft: %v\n%s", err, lo)
	}
	lkRaw, _ := os.ReadFile(strings.TrimSuffix(draft, ".json") + ".lock")
	var lk struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(lkRaw, &lk); err != nil || lk.SHA256 != clientSHA {
		t.Fatalf("judge locked digest %s, client printed %s", lk.SHA256, clientSHA)
	}

	ws := t.TempDir()
	writeTree(t, ws, map[string]string{"z.txt": "zed\n", "a/f.txt": "af\n", "a/b/g.txt": "abg\n", "a-x/h.txt": "a-x h\n"})
	dt, err := exec.Command("python3", filepath.Join(judge, "manifest.py"), "digest-tree", ws).Output()
	if err != nil {
		t.Fatal(err)
	}
	files, _ := scanWorkspace(ws, nil)
	if got := treeDigest(files); got != strings.TrimSpace(string(dt)) {
		t.Fatalf("client tree digest %s, judge digest-tree %s", got, dt)
	}
}
