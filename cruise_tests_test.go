// cruise_tests_test: the generated-tests verbs as the built binary, against a
// recording control plane that serves a draft whose tests the service
// generates.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const genTestFile = "def test_add_two_positives():\n    from calc import add\n    assert add(2, 3) == 5\n"

type testsFake struct {
	mu        sync.Mutex
	hits      []string
	posted    []byte
	generated []byte
	reviewed  []byte
	ops       map[string][]byte // an accepted request's answer, by its idempotency key (as ctl's operation records keep it)
	content   string            // the file the tests view shows
	shaShown  string            // the sha256 shown beside it (default: the content's)
	setShown  string            // the set digest the service states (default: computed)
	state     string
	refusal   map[string]any // the view's refusal, when set (BACKLOG-488)
	patched   []byte         // the last PATCH of the draft
	run       map[string]any // the draft's check of its own tests (BACKLOG-493)
	checked   int            // POST .../check asked
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (f *testsFake) view() map[string]any {
	sha := f.shaShown
	if sha == "" {
		sha = sum(f.content)
	}
	set := f.setShown
	if set == "" {
		set = setDigest(map[string]string{"tests/keepstate/test_calc_add.py": sum(f.content)})
	}
	return map[string]any{"draft_id": fakeDraftID, "revision": 4, "source": "generated", "state": f.state,
		"aim":        map[string]any{"build": "add returns the sum", "acceptance": "the tests pass"},
		"model":      map[string]any{"family": "anthropic", "model": "claude-sonnet-5"},
		"key_route":  map[string]any{"source": "own", "provider": "anthropic", "key_id": "vlt_1"},
		"generation": map[string]any{"id": "gen_0123456789ab", "number": 1, "count": 1, "limit": 5, "session": map[string]any{"id": "s1", "state": "deleted", "deleted_at": "2026-10-18T00:00:00Z"}},
		"files":      []any{map[string]any{"path": "tests/keepstate/test_calc_add.py", "sha256": sha, "bytes": len(f.content), "content": f.content}},
		"tests": []any{map[string]any{"id": "tests.keepstate.test_calc_add::test_add_two_positives", "file": "tests/keepstate/test_calc_add.py", "name": "test_add_two_positives",
			"sentence": "Adding 2 and 3 gives 5.", "acceptance": "the sum", "red": []any{"failed", "failed"}, "status": "red", "witnessed": true}},
		"dropped": []any{}, "untested": []any{}, "set_digest": set, "usage": "unavailable", "cost": "unavailable", "refusal": f.refusal}
}

func (f *testsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	data := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "req_fake", "data": v})
	}
	route := "/api/v2/cruise-drafts/" + fakeDraftID
	draft := map[string]any{"id": fakeDraftID, "revision": 4, "state": "ready", "source": map[string]any{"kind": "upload", "format": "tar.gz", "layout": "folder"},
		"check": map[string]any{"status": "none"}, "manifest": nil, "manifest_sha": "", "tests": map[string]any{"source": "generated", "state": "awaiting_aim"},
		"upload": map[string]any{"method": "PUT", "path": route + "/upload", "max_bytes": 100 << 20}}
	if f.run != nil {
		draft["check"] = map[string]any{"status": "found", "runner": "pytest", "command": "python3 -m pytest -q", "source": "explicit", "pinned": 3, "run": f.run}
	}
	switch {
	case r.Method == "POST" && r.URL.Path == route+"/check":
		// as ctl's idempotency layer answers an accepted request: 202, its
		// answer kept under the operation id the client waits on
		f.checked++
		resp, _ := json.Marshal(map[string]any{"schema_version": 2, "request_id": "req_fake", "data": draft})
		if f.ops == nil {
			f.ops = map[string][]byte{}
		}
		f.ops[r.Header.Get("Idempotency-Key")] = resp
		w.WriteHeader(202)
		_, _ = w.Write(resp)
	case r.Method == "GET" && r.URL.Path == "/api/capabilities":
		caps := []any{}
		for _, id := range []string{"cruise.drafts", "cruise.generated_tests"} {
			caps = append(caps, map[string]any{"id": id, "availability": "available", "summary": id, "surface": "api", "since_protocol": 2})
		}
		data(200, map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-10-18T00:00:00Z", "price_book": "v1.3", "capabilities": caps, "limits": map[string]any{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/cruise-drafts":
		f.posted = body
		data(201, draft)
	case r.Method == "PUT" && r.URL.Path == route+"/upload", r.Method == "GET" && r.URL.Path == route:
		data(200, draft)
	case r.Method == "PATCH" && r.URL.Path == route:
		f.patched = body
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		lo := []any{}
		ps, _ := b["leave_out"].([]any)
		for _, p := range ps {
			lo = append(lo, map[string]any{"path": p, "why": "ModuleNotFoundError: No module named 'playwright'"})
		}
		draft["revision"] = 5
		draft["check"] = map[string]any{"status": "found", "runner": "pytest", "command": "python3 -m pytest -q", "source": "explicit", "pinned": 3, "left_out": lo}
		data(200, draft)
	case r.Method == "POST" && r.URL.Path == route+"/tests/estimate":
		data(200, map[string]any{"runner": "pytest", "model": map[string]any{"family": "anthropic", "model": "claude-sonnet-5"}, "key_route": map[string]any{"source": "own", "provider": "anthropic"},
			"tokens": map[string]any{"input_per_call": 5000, "output_per_call_at_most": 24000, "calls_at_most": 3, "budget": 400000}, "machine_minutes_at_most": 30, "dollars": "unavailable"})
	case r.Method == "POST" && r.URL.Path == route+"/tests/generate":
		f.generated = body
		v := f.view()
		v["state"] = "generating"
		resp, _ := json.Marshal(map[string]any{"schema_version": 2, "request_id": "req_fake", "data": v})
		if f.ops == nil {
			f.ops = map[string][]byte{}
		}
		f.ops[r.Header.Get("Idempotency-Key")] = resp
		w.WriteHeader(202)
		_, _ = w.Write(resp)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/operations/") && f.ops[strings.TrimPrefix(r.URL.Path, "/api/operations/")] != nil:
		key := strings.TrimPrefix(r.URL.Path, "/api/operations/")
		data(200, map[string]any{"operation_id": key, "state": "succeeded", "http_status": 202, "response": json.RawMessage(f.ops[key]),
			"method": "POST", "path": route + "/tests/generate"})
	case r.Method == "GET" && r.URL.Path == route+"/tests":
		data(200, f.view())
	case r.Method == "POST" && r.URL.Path == route+"/tests/review":
		f.reviewed = body
		v := f.view()
		v["state"] = "reviewed"
		data(200, v)
	case r.Method == "DELETE" && r.URL.Path == route+"/tests":
		v := f.view()
		v["state"] = "awaiting_aim"
		data(200, v)
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no such route"}})
	}
}

func (f *testsFake) count(hit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.hits {
		if h == hit {
			n++
		}
	}
	return n
}

func startTestsFake(t *testing.T) (*testsFake, string, string) {
	t.Helper()
	f := &testsFake{content: genTestFile, state: "generated"}
	syncFixture(t, &f.mu)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return f, bin, cfg
}

func TestCruiseDraftCreateWithGeneratedTestsSendsTheAim(t *testing.T) {
	f, bin, cfg := startTestsFake(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"calc.py": "def add(a, b):\n    return a - b\n"})
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "create", "--dir", dir, "--generate-tests", "--aim", "add returns the sum",
		"--acceptance", "the tests pass", "--constraints", "standard library only", "--model", "anthropic:claude-sonnet-5", "--json")
	if code != 0 {
		t.Fatalf("create exit %d\n%s%s", code, out, errs)
	}
	var req struct {
		Tests struct {
			Source string `json:"source"`
			Aim    struct {
				Build, Acceptance, Constraints string
			} `json:"aim"`
			Model struct{ Family, Model string } `json:"model"`
		} `json:"tests"`
	}
	f.mu.Lock()
	posted := f.posted
	f.mu.Unlock()
	if err := json.Unmarshal(posted, &req); err != nil || req.Tests.Source != "generated" || req.Tests.Aim.Build != "add returns the sum" ||
		req.Tests.Aim.Acceptance != "the tests pass" || req.Tests.Aim.Constraints != "standard library only" || req.Tests.Model.Model != "claude-sonnet-5" {
		t.Fatalf("the create body does not carry the aim: %v %s", err, posted)
	}
	// an aim without --generate-tests, and --generate-tests without what done means, send nothing
	for _, args := range [][]string{
		{"cruise", "draft", "create", "--dir", dir, "--aim", "add returns the sum", "--acceptance", "the tests pass"},
		{"cruise", "draft", "create", "--dir", dir, "--generate-tests", "--aim", "add returns the sum"},
	} {
		before := f.count("POST /api/v2/cruise-drafts")
		if _, errs, code := ksIn(t, bin, cfg, t.TempDir(), args...); code != 2 || f.count("POST /api/v2/cruise-drafts") != before {
			t.Fatalf("%v: exit %d (%s), or a request was sent", args, code, errs)
		}
	}
}

func TestCruiseTestsGenerateIsConfirmedAndNamesTheRevision(t *testing.T) {
	f, bin, cfg := startTestsFake(t)
	if _, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "generate", fakeDraftID); code != 2 || f.count("POST /api/v2/cruise-drafts/"+fakeDraftID+"/tests/generate") != 0 {
		t.Fatalf("an unconfirmed generation without a terminal: exit %d (%s)", code, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "generate", fakeDraftID, "--yes", "--json")
	if code != 0 {
		t.Fatalf("generate exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	gen := string(f.generated)
	f.mu.Unlock()
	if !strings.Contains(gen, `"expected_revision":4`) {
		t.Fatalf("the generation does not name the revision read: %s", gen)
	}
	if !strings.Contains(errs, "at most 3 calls") || !strings.Contains(errs, "your own anthropic key") {
		t.Fatalf("the estimate was not shown before asking: %s", errs)
	}
}

func TestCruiseTestsReviewConfirmsTheSetComputedHere(t *testing.T) {
	f, bin, cfg := startTestsFake(t)
	set := setDigest(map[string]string{"tests/keepstate/test_calc_add.py": sum(genTestFile)})
	review := "POST /api/v2/cruise-drafts/" + fakeDraftID + "/tests/review"
	// a digest that is not the files shown, and no digest without a terminal, review nothing
	for _, args := range [][]string{{"--confirm", strings.Repeat("0", 64)}, {}} {
		if _, errs, code := ksIn(t, bin, cfg, t.TempDir(), append([]string{"cruise", "tests", "review", fakeDraftID}, args...)...); code != 2 || f.count(review) != 0 {
			t.Fatalf("review %v: exit %d (%s), or the review was sent", args, code, errs)
		}
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "review", fakeDraftID, "--confirm", set[:12], "--json")
	if code != 0 {
		t.Fatalf("review exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	sent := string(f.reviewed)
	f.mu.Unlock()
	if sent != `{"set_digest":"`+set+`"}` {
		t.Fatalf("the review does not send the set digest computed here: %s", sent)
	}
	if !strings.Contains(errs, genTestFile) || !strings.Contains(errs, set) {
		t.Fatalf("the review did not show every file in full and the digest computed here: %s", errs)
	}
	// a file whose bytes are not the sha256 shown, or a service that states another set, is refused
	for name, mut := range map[string]func(*testsFake){
		"bytes not their sha256": func(f *testsFake) { f.shaShown = sum("other bytes") },
		"another set stated":     func(f *testsFake) { f.setShown = strings.Repeat("e", 64) },
	} {
		f.mu.Lock()
		f.shaShown, f.setShown = "", ""
		mut(f)
		f.mu.Unlock()
		before := f.count(review)
		if _, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "review", fakeDraftID, "--confirm", set); code == 0 || f.count(review) != before {
			t.Fatalf("%s: exit %d (%s), or the review was sent", name, code, errs)
		}
	}
}

func TestCruiseTestsShowAndDiscard(t *testing.T) {
	_, bin, cfg := startTestsFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "show", fakeDraftID, "--files")
	if code != 0 || !strings.Contains(out, "fails now, twice; a sample solution passes it") || !strings.Contains(out, genTestFile) ||
		!strings.Contains(out, "next: ks cruise tests review "+fakeDraftID) {
		t.Fatalf("show: exit %d\n%s%s", code, out, errs)
	}
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "discard", fakeDraftID); code != 0 || !strings.Contains(out, "discarded") {
		t.Fatalf("discard: exit %d\n%s%s", code, out, errs)
	}
}

// --verify's checks of a generated set, against the manifest's own names.
func TestVerifyChecksTheGeneratedSet(t *testing.T) {
	sha := sum(genTestFile)
	ver := map[string]any{"prohibited_paths": []any{".keepstate/", "tests/keepstate/test_calc_add.py"},
		"generated": map[string]any{"files": []any{map[string]any{"path": "tests/keepstate/test_calc_add.py", "sha256": sha}},
			"set_digest": setDigest(map[string]string{"tests/keepstate/test_calc_add.py": sha})}}
	run := func(ver map[string]any, bySHA map[string]string) []string {
		var failed []string
		add := func(name, got, want string) {
			if got != want || want == "" {
				failed = append(failed, name)
			}
		}
		generatedSetChecks(ver, bySHA, add)
		return failed
	}
	good := map[string]string{"calc.py": sum("x"), "tests/keepstate/test_calc_add.py": sha}
	if f := run(ver, good); len(f) != 0 {
		t.Fatalf("a consistent set failed: %v", f)
	}
	if f := run(ver, map[string]string{"calc.py": sum("x"), "tests/keepstate/test_calc_add.py": sum("changed")}); len(f) != 2 {
		t.Fatalf("a changed file is not caught by its sha256 and the set digest: %v", f)
	}
	if f := run(ver, map[string]string{"calc.py": sum("x"), "tests/keepstate/test_calc_add.py": sha, "tests/keepstate/conftest.py": sum("skip")}); len(f) != 1 {
		t.Fatalf("an extra file in the generated folder is not caught: %v", f)
	}
	loose := map[string]any{"prohibited_paths": []any{".keepstate/"}, "generated": ver["generated"]}
	if f := run(loose, good); len(f) != 1 {
		t.Fatalf("an unpinned generated file is not caught: %v", f)
	}
	if n := generatedSetChecks(map[string]any{}, good, func(string, string, string) {}); n != 0 {
		t.Fatalf("a draft without generated tests counts %d generated files", n)
	}
}
