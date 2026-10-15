// cruise_drafts_test: the hosted draft verbs as the built binary, against a
// recording control plane that serves a draft taken from the shared vectors
// (the toy-repo case), so a consistent draft is one this client itself
// computed.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

const fakeDraftID = "crd_0123456789ab"

type draftFake struct {
	mu        sync.Mutex
	hits      []string
	posted    []byte
	put       []byte
	putType   string
	patched   []byte
	approved  []byte
	ready     map[string]any
	archive   []byte
	headerSHA string
}

func (f *draftFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func (f *draftFake) count(prefix string) int {
	n := 0
	for _, h := range f.seen() {
		if strings.HasPrefix(h, prefix) {
			n++
		}
	}
	return n
}

func (f *draftFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/capabilities":
		data(200, map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-10-15T00:00:00Z", "price_book": "v1.3",
			"capabilities": []any{map[string]any{"id": "cruise.drafts", "availability": "available", "summary": "drafts", "surface": "api", "since_protocol": 2}}, "limits": map[string]any{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/cruise-drafts":
		f.posted = body
		data(201, map[string]any{"id": fakeDraftID, "revision": 1, "state": "awaiting_upload",
			"upload": map[string]any{"method": "PUT", "path": route + "/upload", "max_bytes": 100 << 20}})
	case r.Method == "PUT" && r.URL.Path == route+"/upload":
		f.put, f.putType = body, r.Header.Get("Content-Type")
		data(200, f.ready)
	case r.Method == "GET" && r.URL.Path == route:
		data(200, f.ready)
	case r.Method == "GET" && r.URL.Path == "/api/v2/cruise-drafts":
		data(200, map[string]any{"items": []any{f.ready}, "next_cursor": "", "observed_at": "2026-10-15T00:00:00Z"})
	case r.Method == "PATCH" && r.URL.Path == route:
		f.patched = body
		data(200, f.ready)
	case r.Method == "POST" && r.URL.Path == route+"/approve":
		f.approved = body
		var req struct {
			ManifestSHA string `json:"manifest_sha"`
		}
		_ = json.Unmarshal(body, &req)
		data(201, map[string]any{"draft": f.ready, "job": map[string]any{"id": "job_0123456789ab", "state": "queued", "manifest_sha": req.ManifestSHA}})
	case r.Method == "DELETE" && r.URL.Path == route:
		v := map[string]any{}
		for k, x := range f.ready {
			v[k] = x
		}
		v["state"] = "discarded"
		data(200, v)
	case r.Method == "GET" && r.URL.Path == route+"/archive":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-KS-Sha256", f.headerSHA)
		w.WriteHeader(200)
		_, _ = w.Write(f.archive)
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no such route"}})
	}
}

// startDraftFake serves the vectors' toy-repo case as a ready draft.
func startDraftFake(t *testing.T) (*draftFake, string, string) {
	t.Helper()
	raw, err := os.ReadFile(cruiseDraftVectorsVendored)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name   string         `json:"name"`
			Expect map[string]any `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	e := file.Cases[0].Expect
	if !strings.HasPrefix(file.Cases[0].Name, "toy-repo") || e["manifest"] == nil {
		t.Fatalf("the vectors' first case is not the drafted toy-repo: %s", file.Cases[0].Name)
	}
	arch := e["archive"].(map[string]any)
	archive, _ := base64.StdEncoding.DecodeString(arch["b64"].(string))
	sel := e["selection"].(map[string]any)
	f := &draftFake{archive: archive, headerSHA: arch["sha256"].(string)}
	f.ready = map[string]any{
		"id": fakeDraftID, "revision": 3, "state": "ready",
		"source":    map[string]any{"kind": "upload", "format": "tar.gz", "bytes": 4096, "layout": "folder", "left_out": 1},
		"options":   map[string]any{"goal": "fix calc.py", "paths": []any{}, "ladder": []any{map[string]any{"family": "anthropic", "model": "claude-haiku-4-5-20251001"}}, "spend_microusd": 2000000, "routes": map[string]any{}},
		"files":     map[string]any{"policy_version": sel["policy_version"], "included": sel["files"], "bytes": sel["bytes"], "excluded": 1, "excluded_by_kind": map[string]any{"mandatory": 1}, "symlinks": []any{}, "selection_digest": sel["digest"]},
		"selection": sel,
		"check":     map[string]any{"status": "found", "runner": "pytest", "command": "python3 -m pytest -q", "source": "discovered", "with_tests": []any{"python"}, "pinned": 1},
		"manifest":  json.RawMessage(e["manifest"].(string)), "manifest_sha": e["manifest_sha"],
		"archive":     map[string]any{"sha256": arch["sha256"], "bytes": arch["bytes"], "path": "/api/v2/cruise-drafts/" + fakeDraftID + "/archive"},
		"readiness":   []any{map[string]any{"check": "key", "status": "ok"}},
		"spend_limit": map[string]any{"enforced": false, "note": "the rate book is not ratified, so the limit does not stop the job"},
		"warnings":    []any{}, "storage": "kept 24 hours, not billed", "job_id": "",
		"created_at": "2026-10-15T00:00:00Z", "updated_at": "2026-10-15T00:00:00Z", "expires_at": "2026-10-16T00:00:00Z",
	}
	syncFixture(t, &f.mu)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return f, bin, cfg
}

func draftTarNames(t *testing.T, tgz []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
	return out
}

func TestCruiseDraftCreateFromAFolderSendsTheSelection(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"x.py": "X = 1\n", "test_x.py": "from x import X\n\ndef test_x():\n    assert X == 1\n",
		".git/HEAD": "ref\n", "node_modules/a/i.js": "1\n", ".env": "TOKEN=x\n", "docs/n.md": "n\n",
	})
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "create", "--dir", dir, "--goal", "make test_x pass", "--json")
	if code != 0 {
		t.Fatalf("create exit %d\n%s%s", code, out, errs)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data["id"] != fakeDraftID || env.Data["state"] != "ready" {
		t.Fatalf("--json is not the draft: %v\n%s", err, out)
	}
	_, sel, err := scanSelected(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Source struct {
			Kind, Format, Layout string
			Bytes                int64
			LeftOut              []draftLeftOut `json:"left_out"`
		} `json:"source"`
		StorageConsent bool   `json:"storage_consent"`
		Goal           string `json:"goal"`
	}
	f.mu.Lock()
	posted, put, putType := f.posted, f.put, f.putType
	f.mu.Unlock()
	if err := json.Unmarshal(posted, &req); err != nil {
		t.Fatal(err)
	}
	if req.Source.Kind != "upload" || req.Source.Format != "tar.gz" || req.Source.Layout != "folder" || !req.StorageConsent || req.Goal != "make test_x pass" {
		t.Fatalf("create body: %s", posted)
	}
	if req.Source.Bytes != int64(len(put)) || putType != "application/octet-stream" {
		t.Fatalf("declared %d bytes, sent %d as %q", req.Source.Bytes, len(put), putType)
	}
	var want []string
	for _, x := range sel.Included {
		want = append(want, x.Path)
	}
	if got := draftTarNames(t, put); !reflect.DeepEqual(got, want) {
		t.Fatalf("the folder archive carries %v, the selection includes %v", got, want)
	}
	var left, wantLeft []string
	for _, l := range req.Source.LeftOut {
		left = append(left, l.Path+map[bool]string{true: "/", false: ""}[l.Dir])
	}
	for _, x := range sel.Excluded {
		fi, _ := os.Lstat(filepath.Join(dir, x.Path))
		wantLeft = append(wantLeft, x.Path+map[bool]string{true: "/", false: ""}[fi.IsDir()])
	}
	sort.Strings(left)
	sort.Strings(wantLeft)
	if !reflect.DeepEqual(left, wantLeft) || len(left) != 3 {
		t.Fatalf("left_out %v, the selection leaves out %v", left, wantLeft)
	}
	if !strings.Contains(errs, "deletes them when the draft is approved, discarded or expires") {
		t.Errorf("no storage sentence on stderr:\n%s", errs)
	}
}

func TestCruiseDraftCreateSendsNothingForALink(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"x.py": "X = 1\n", "test_x.py": "def test_x():\n    assert True\n"})
	if err := os.Symlink("x.py", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "create", "--dir", dir)
	if code == 0 || !strings.Contains(out+errs, "symlinks are refused") || f.count("POST") != 0 || f.count("PUT") != 0 {
		t.Fatalf("a folder with a link: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "create", "--archive", filepath.Join(dir, "x.py"))
	if code != exitUsage || f.count("POST") != 0 {
		t.Fatalf("an archive that is not one: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
}

func TestCruiseDraftApproveSendsTheDigestComputedHere(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	f.mu.Lock()
	sha := f.ready["manifest_sha"].(string)
	f.mu.Unlock()
	// a confirmation that is not the digest sends nothing
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "approve", fakeDraftID, "--confirm", strings.Repeat("0", 12)); code != exitUsage || f.count("POST /api/v2/cruise-drafts/"+fakeDraftID+"/approve") != 0 {
		t.Fatalf("a wrong --confirm: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
	// none at all, without a terminal: refused, --yes does not stand in
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "approve", fakeDraftID, "--yes"); code != exitUsage || f.count("POST /api/v2/cruise-drafts/"+fakeDraftID+"/approve") != 0 {
		t.Fatalf("no --confirm: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "approve", fakeDraftID, "--confirm", sha[:12])
	if code != 0 || !strings.Contains(out, "job_0123456789ab") {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	approved := f.approved
	f.mu.Unlock()
	var req map[string]any
	if err := json.Unmarshal(approved, &req); err != nil || req["manifest_sha"] != sha || len(req) != 1 {
		t.Fatalf("approve body %s", approved)
	}
	// a service whose stated digest is not its manifest's: nothing is sent
	f.mu.Lock()
	f.ready["manifest_sha"] = strings.Repeat("e", 64)
	f.approved = nil
	f.mu.Unlock()
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "approve", fakeDraftID, "--confirm", strings.Repeat("e", 64)); code != exitIntegrity || f.approved != nil {
		t.Fatalf("a mismatched service digest: exit %d, approved %s\n%s%s", code, f.approved, out, errs)
	}
}

func TestCruiseDraftShowVerifyRecomputesEveryDigest(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "show", fakeDraftID, "--verify")
	if code != 0 || !strings.Contains(out, "verified: draft "+fakeDraftID) || strings.Contains(out, "FAIL") {
		t.Fatalf("a consistent draft: exit %d\n%s%s", code, out, errs)
	}
	if n := strings.Count(out, "ok  "); n != 10 {
		t.Errorf("%d checks passed, want 10:\n%s", n, out)
	}
	// one archive byte
	f.mu.Lock()
	good := append([]byte(nil), f.archive...)
	f.archive[len(f.archive)/2] ^= 0x01
	f.mu.Unlock()
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "show", fakeDraftID, "--verify"); code != exitIntegrity || !strings.Contains(out, "FAIL") {
		t.Fatalf("a changed archive byte: exit %d\n%s%s", code, out, errs)
	}
	// one digest: a selected file the archive does not carry as recorded
	f.mu.Lock()
	f.archive = good
	inc := f.ready["selection"].(map[string]any)["included"].([]any)
	inc[0].(map[string]any)["sha256"] = strings.Repeat("a", 64)
	f.mu.Unlock()
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "show", fakeDraftID, "--verify"); code != exitIntegrity || !strings.Contains(out, "FAIL every selected file") {
		t.Fatalf("a changed selection digest: exit %d\n%s%s", code, out, errs)
	}
}

func TestCruiseDraftShowManifestIsTheCanonicalBytes(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "show", fakeDraftID, "--manifest")
	f.mu.Lock()
	want := string(f.ready["manifest"].(json.RawMessage))
	f.mu.Unlock()
	if code != 0 || out != want {
		t.Fatalf("--manifest: exit %d\n got  %q\n want %q\n%s", code, out, want, errs)
	}
	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "list")
	if code != 0 || !strings.Contains(out, fakeDraftID) {
		t.Fatalf("list: exit %d\n%s", code, out)
	}
	out, _, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "discard", fakeDraftID)
	if code != 0 || !strings.Contains(out, "discarded "+fakeDraftID) || f.count("DELETE /api/v2/cruise-drafts/"+fakeDraftID) != 1 {
		t.Fatalf("discard: exit %d\n%s", code, out)
	}
}

func TestCruiseDraftSetSendsTheRevisionAndOnlyWhatChanged(t *testing.T) {
	f, bin, cfg := startDraftFake(t)
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID); code != exitUsage || f.count("PATCH") != 0 {
		t.Fatalf("set with no change: exit %d, requests %v", code, f.seen())
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--paths", "src/**", "--any-file"); code != exitUsage || f.count("PATCH") != 0 {
		t.Fatalf("set with --paths and --any-file: exit %d", code)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--goal", "fix the mean", "--spend", "3.50")
	if code != 0 {
		t.Fatalf("set: exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	patched := f.patched
	f.mu.Unlock()
	var req map[string]any
	if err := json.Unmarshal(patched, &req); err != nil {
		t.Fatal(err)
	}
	if req["expected_revision"] != float64(3) || req["goal"] != "fix the mean" || req["spend_microusd"] != float64(3500000) || len(req) != 3 {
		t.Fatalf("PATCH body %s", patched)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--any-file"); code != 0 {
		t.Fatal("set --any-file failed")
	}
	f.mu.Lock()
	patched = f.patched
	f.mu.Unlock()
	if !strings.Contains(string(patched), `"paths":[]`) {
		t.Fatalf("--any-file did not send an empty path list: %s", patched)
	}
}
