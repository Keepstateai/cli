// cruise_auto_test: ks cruise auto as the built binary, against a recording
// control plane that serves a plan whose manifest this test builds, so a
// consistent plan is one this client itself computed. The digests the client
// shows and sends are its own, from the manifest, never the service's word.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fakePlanID = "ap_0123456789abcdef"

var autoSentences = []autoCheck{
	{ID: "t::two_negatives", Sentence: "Adding -2 and -3 gives -5.", CheckRef: "t::two_negatives"},
	{ID: "t::two_positives", Sentence: "Adding 2 and 3 gives 5.", CheckRef: "t::two_positives"},
}

type autoFake struct {
	mu       sync.Mutex
	hits     []string
	posted   map[string][]byte
	plan     map[string]any
	draft    map[string]any
	manifest map[string]any
}

func (f *autoFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func (f *autoFake) count(prefix string) int {
	n := 0
	for _, h := range f.seen() {
		if strings.HasPrefix(h, prefix) {
			n++
		}
	}
	return n
}

func (f *autoFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	f.posted[r.Method+" "+r.URL.Path] = body
	w.Header().Set("Content-Type", "application/json")
	data := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "req_fake", "data": v})
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/capabilities":
		data(200, map[string]any{"registry_version": "test", "build": "b", "fetched_at": "2026-11-12T00:00:00Z", "price_book": "v1.3",
			"capabilities": []any{map[string]any{"id": "cruise.autopilot", "availability": "available", "summary": "autopilot", "surface": "api", "since_protocol": 2}}, "limits": map[string]any{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/cruise-auto":
		v := map[string]any{}
		for k, x := range f.plan {
			v[k] = x
		}
		v["state"], v["state_label"], v["card"] = "questions", "Waiting for your answers", nil
		data(201, v)
	case r.Method == "GET" && r.URL.Path == "/api/v2/cruise-auto":
		data(200, map[string]any{"items": []any{f.plan}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/cruise-auto/"+fakePlanID:
		data(200, f.plan)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v2/cruise-auto/"+fakePlanID+"/answers"):
		data(200, f.plan)
	case r.Method == "POST" && r.URL.Path == "/api/v2/cruise-auto/"+fakePlanID+"/plan":
		data(202, f.plan)
	case r.Method == "POST" && r.URL.Path == "/api/v2/cruise-auto/"+fakePlanID+"/approve":
		v := map[string]any{}
		for k, x := range f.plan {
			v[k] = x
		}
		v["state"], v["state_label"], v["job_id"] = "approved", "Approved: the job is running on Cruise", "job_0123456789ab"
		data(201, v)
	case r.Method == "GET" && r.URL.Path == "/api/v2/cruise-auto/"+fakePlanID+"/receipt":
		line := func(n int, label, text string, holds bool) map[string]any {
			return map[string]any{"n": n, "label": label, "text": text, "holds": holds, "sources": []any{map[string]any{"kind": "job", "ref": "job_0123456789ab"}}}
		}
		data(200, map[string]any{"job_id": "job_0123456789ab", "state": "accepted", "manifest_sha": strings.Repeat("a", 64), "event_head": strings.Repeat("b", 64), "planning": "Planning used $0.012000 of the free $0.250000.",
			"lines": []any{line(1, "Checked independently:", "verified by a separate checker (claude-haiku-5-5), not by the model that did the work (claude-sonnet-5).", true),
				line(2, "Locked before work:", "what done means was approved on 2026-11-12T10:00:00Z, before any work, and did not change.", true),
				line(3, "Re-checkable:", "2 of 2 checks pass; KeepState can re-verify this receipt, and public verification is not yet available for it.", false),
				line(4, "Measured cost:", "$0.004000 measured from the provider's reported usage, under your $2.000000 cap.", true),
				line(5, "Not checked:", "Whether these checks capture what you meant.", true)}})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/cruise-auto/"+fakePlanID:
		v := map[string]any{}
		for k, x := range f.plan {
			v[k] = x
		}
		v["state"], v["state_label"] = "discarded", "Discarded"
		data(200, v)
	case r.Method == "GET" && r.URL.Path == "/api/v2/cruise-drafts/crd_0123456789ab":
		data(200, f.draft)
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no such route"}})
	}
}

// startAutoFake serves a plan whose checks are measured, and the draft whose
// manifest binds them (binding_version 2).
func startAutoFake(t *testing.T) (*autoFake, string, string) {
	t.Helper()
	plain := make([]any, 0)
	for _, c := range autoSentences {
		plain = append(plain, map[string]any{"id": c.ID, "sentence": c.Sentence, "check_ref": c.CheckRef})
	}
	digest := autoPlainChecksDigest(autoSentences)
	manifest := map[string]any{"family": "cruise", "version": json.Number("1"), "binding_version": json.Number("2"),
		"limits": map[string]any{"spend_microusd": json.Number("2000000")}, "ladder": []any{map[string]any{"family": "anthropic", "model": "claude-haiku-4-5-20251001"}},
		"autopilot": map[string]any{"lane": "code", "plain_checks": plain, "plain_checks_digest": digest,
			"not_checked": []any{"How it looks or feels: taste is not checked by a machine."}}}
	canon, err := canonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	msha := hex.EncodeToString(sum[:])
	f := &autoFake{posted: map[string][]byte{}, manifest: manifest}
	f.draft = map[string]any{"id": "crd_0123456789ab", "revision": 4, "state": "ready", "source": map[string]any{"kind": "upload", "layout": "folder"},
		"options": map[string]any{"goal": "g", "paths": []any{}, "ladder": []any{}, "spend_microusd": 2000000, "routes": map[string]any{}},
		"files":   map[string]any{"included": 3, "bytes": 100, "excluded_by_kind": map[string]any{}, "symlinks": []any{}},
		"check":   map[string]any{"status": "found", "runner": "pytest"}, "manifest": json.RawMessage(canon), "manifest_sha": msha, "expires_at": "2026-11-13T00:00:00Z"}
	checks := []any{}
	for _, c := range autoSentences {
		checks = append(checks, map[string]any{"id": c.ID, "sentence": c.Sentence, "check_ref": c.CheckRef, "red": []any{"failed", "failed"}})
	}
	f.plan = map[string]any{"id": fakePlanID, "state": "plan_ready", "state_label": "Plan ready: read the checks and approve", "lane": "code",
		"brief": "Add add(a, b) and sub(a, b) to calc.py.", "draft_id": "crd_0123456789ab", "hint": "",
		"questions": []any{map[string]any{"id": "language", "text": "Which language?", "options": []any{"python", "go"}, "default": "python"}},
		"answers":   map[string]any{"language": "python"}, "job_id": "",
		"planning": map[string]any{"line": "Planning used $0.012000 of the free $0.250000."},
		"card": map[string]any{"checks": checks, "already_true": []any{}, "today": map[string]any{"pass": 0, "of": 2, "line": "today 0 of 2 pass"},
			"not_checked": []any{"How it looks or feels: taste is not checked by a machine."},
			"cost":        map[string]any{"low_microusd": "unavailable", "high_microusd": 2000000, "cap_microusd": 2000000}, "ladder": []any{"claude-haiku-4-5-20251001"}, "routing": "starts on claude-haiku-4-5-20251001, the default: there are no settled jobs like this one yet",
			"plain_checks_digest": digest, "manifest_sha": msha, "held_out": "no checks are held back from the agent yet: it can read every check above",
			"checker": map[string]any{"model": "claude-haiku-5-5"}, "data_classification": "customer: your files are treated as private", "delivery": "a zip of the result and a preview link"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return f, bin, cfg
}

// A brief needs the person's consent and words; without either nothing is sent.
func TestCruiseAutoBriefSendsNothingWithoutConsent(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "brief", "crd_0123456789ab", "--text", "make add work"); code != exitUsage || f.count("POST ") != 0 {
		t.Fatalf("no --consent: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "brief", "crd_0123456789ab", "--consent"); code != exitUsage || f.count("POST ") != 0 {
		t.Fatalf("no --text: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "brief", "crd_0123456789ab", "--text", "make add work", "--consent")
	if code != 0 || !strings.Contains(out, "Which language? [python]") {
		t.Fatalf("brief: exit %d\n%s%s", code, out, errs)
	}
	f.mu.Lock()
	body := f.posted["POST /api/v2/cruise-auto"]
	f.mu.Unlock()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req["brief"] != "make add work" || req["draft_id"] != "crd_0123456789ab" || req["storage_consent"] != true || len(req) != 3 {
		t.Fatalf("brief body %s", body)
	}
}

// Every help path makes zero requests.
func TestCruiseAutoHelpMakesNoRequest(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	for _, v := range []string{"brief", "answer", "plan", "status", "approve", "receipt", "discard"} {
		if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", v, "--help"); code != 0 || !strings.Contains(out+errs, "ks cruise auto "+v) {
			t.Fatalf("help of %s: exit %d\n%s%s", v, code, out, errs)
		}
	}
	if out, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "--help"); code != 0 || !strings.Contains(out, "brief") {
		t.Fatalf("help of the group: %d %s", code, out)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("help made %d requests: %v", n, f.seen())
	}
}

func TestCruiseAutoAnswerSendsOnlyWhatWasSet(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "answer", fakePlanID, "--set", "nonsense"); code != exitUsage || f.count("POST ") != 0 {
		t.Fatalf("a malformed --set: exit %d\n%s%s", code, out, errs)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "answer", fakePlanID); code != 0 {
		t.Fatalf("answer with defaults: exit %d", code)
	}
	if body := string(f.posted["POST /api/v2/cruise-auto/"+fakePlanID+"/answers"]); !strings.Contains(body, `"answers":{}`) {
		t.Fatalf("every default is accepted by an empty object: %s", body)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "answer", fakePlanID, "--set", "language=go"); code != 0 {
		t.Fatalf("answer: exit %d", code)
	}
	if body := string(f.posted["POST /api/v2/cruise-auto/"+fakePlanID+"/answers"]); !strings.Contains(body, `"language":"go"`) {
		t.Fatalf("the answer was not sent: %s", body)
	}
}

// Approve shows the sentences the manifest holds, computes both digests here
// and sends exactly those; nothing is sent when the service's words differ.
func TestCruiseAutoApproveSendsTheDigestsComputedHere(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
	plain := f.plan["card"].(map[string]any)["plain_checks_digest"].(string)
	url := "POST /api/v2/cruise-auto/" + fakePlanID + "/approve"
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", strings.Repeat("0", 12)); code != exitUsage || f.count(url) != 0 {
		t.Fatalf("a wrong --confirm: exit %d, requests %v", code, f.seen())
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--yes"); code != exitUsage || f.count(url) != 0 {
		t.Fatalf("no --confirm: exit %d (--yes does not approve)", code)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12], "--keep-days", "31"); code != exitUsage || f.count(url) != 0 {
		t.Fatalf("31 days: exit %d", code)
	}
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12])
	if code != 0 || !strings.Contains(out, "job_0123456789ab") {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"Adding -2 and -3 gives -5.", "today 0 of 2 pass", "what cannot be checked", "most it can spend: $2.00", "taste is not checked by a machine"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the review does not say %q:\n%s", want, errs)
		}
	}
	f.mu.Lock()
	body := f.posted[url]
	f.mu.Unlock()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req["manifest_sha"] != msha || req["plain_checks_digest"] != plain || len(req) != 2 {
		t.Fatalf("approve body %s", body)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12], "--keep-days", "7"); code != 0 {
		t.Fatalf("approve with days: exit %d", code)
	}
	if body := string(f.posted[url]); !strings.Contains(body, `"retention_days":7`) {
		t.Fatalf("the days were not sent: %s", body)
	}
}

func TestCruiseAutoApproveRefusesAPlanWhoseWordsDiffer(t *testing.T) {
	url := "POST /api/v2/cruise-auto/" + fakePlanID + "/approve"
	// the service states another digest for the sentences than the manifest's
	f, bin, cfg := startAutoFake(t)
	msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
	f.mu.Lock()
	f.plan["card"].(map[string]any)["plain_checks_digest"] = strings.Repeat("e", 64)
	f.mu.Unlock()
	if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12]); code != exitIntegrity || f.count(url) != 0 {
		t.Fatalf("a stated sentences digest that is not the manifest's: exit %d\n%s%s", code, out, errs)
	}
	// the manifest names a digest its sentences do not have (a sentence changed)
	f, bin, cfg = startAutoFake(t)
	f.mu.Lock()
	m := f.manifest
	m["autopilot"].(map[string]any)["plain_checks"].([]any)[0].(map[string]any)["sentence"] = "Adding -2 and -3 gives -6."
	canon, _ := canonicalJSON(m)
	sum := sha256.Sum256(canon)
	nsha := hex.EncodeToString(sum[:])
	f.draft["manifest"], f.draft["manifest_sha"] = json.RawMessage(canon), nsha
	f.plan["card"].(map[string]any)["manifest_sha"] = nsha
	f.mu.Unlock()
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", nsha[:12])
	if code != exitIntegrity || f.count(url) != 0 || !strings.Contains(out+errs, "plain_checks_digest_mismatch") && !strings.Contains(out+errs, "digest") {
		t.Fatalf("a changed sentence: exit %d\n%s%s", code, out, errs)
	}
	// the service states another manifest digest than the manifest hashes to
	f, bin, cfg = startAutoFake(t)
	f.mu.Lock()
	f.plan["card"].(map[string]any)["manifest_sha"] = strings.Repeat("d", 64)
	f.mu.Unlock()
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", strings.Repeat("d", 12)); code != exitIntegrity || f.count(url) != 0 {
		t.Fatalf("a stated manifest digest that is not the manifest's: exit %d", code)
	}
	// a plan that is not ready is not approved
	f, bin, cfg = startAutoFake(t)
	f.mu.Lock()
	f.plan["state"], f.plan["state_label"] = "planning", "Writing and measuring the checks"
	f.mu.Unlock()
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", strings.Repeat("a", 12)); code != exitConflict || f.count(url) != 0 {
		t.Fatalf("a plan still planning: exit %d", code)
	}
}

func TestCruiseAutoStatusReceiptAndDiscard(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "status", fakePlanID)
	if code != 0 || !strings.Contains(out, "what done means (today 0 of 2 pass)") || !strings.Contains(out, "next: ks cruise auto approve "+fakePlanID) || !strings.Contains(out, "Planning is free to you") {
		t.Fatalf("status: exit %d\n%s%s", code, out, errs)
	}
	if out, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "status"); code != 0 || !strings.Contains(out, fakePlanID) {
		t.Fatalf("status list: %d %s", code, out)
	}
	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "receipt", fakePlanID, "--sources")
	if code != 0 || !strings.Contains(out, "1. Checked independently:") || !strings.Contains(out, "! 3. Re-checkable:") || !strings.Contains(out, "from: job job_0123456789ab") || !strings.Contains(out, "planning: free.") {
		t.Fatalf("receipt: exit %d\n%s%s", code, out, errs)
	}
	if out, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "receipt", fakePlanID, "--json"); code != 0 || !strings.Contains(out, `"lines"`) {
		t.Fatalf("receipt --json: %d %s", code, out)
	}
	if out, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "discard", fakePlanID); code != 0 || !strings.Contains(out, "discarded "+fakePlanID) {
		t.Fatalf("discard: %d %s", code, out)
	}
	if f.count("POST ") != 0 {
		t.Fatalf("a read verb posted: %v", f.seen())
	}
}

// ---- the shared vectors ------------------------------------------------------------------------

func TestCruiseAutoClientPassesTheSharedBindingVectors(t *testing.T) {
	var v struct {
		DigestCases []struct {
			Name        string      `json:"name"`
			PlainChecks []autoCheck `json:"plain_checks"`
			Digest      string      `json:"plain_checks_digest"`
		} `json:"digest_cases"`
		BlockCases []struct {
			Name      string         `json:"name"`
			Block     map[string]any `json:"block"`
			Canonical string         `json:"canonical"`
			SHA256    string         `json:"sha256"`
		} `json:"block_cases"`
	}
	b, err := os.ReadFile("testdata/autopilot/autopilot-binding-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || len(v.DigestCases) < 4 || len(v.BlockCases) < 3 {
		t.Fatalf("vectors: %v", err)
	}
	for _, c := range v.DigestCases {
		if got := autoPlainChecksDigest(c.PlainChecks); got != c.Digest {
			t.Errorf("%s: this client digests the sentences to %s, the vectors say %s", c.Name, got, c.Digest)
		}
	}
	for _, c := range v.BlockCases {
		canon, err := canonicalJSON(c.Block)
		sum := sha256.Sum256(canon)
		if err != nil || string(canon) != c.Canonical || hex.EncodeToString(sum[:]) != c.SHA256 {
			t.Errorf("%s: this client's canonical bytes of the block differ from the judge's", c.Name)
		}
	}
}

func TestCruiseAutoVendoredVectorsMatchTheirSourceAndTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		From         string `json:"from"`
		SHA256       string `json:"sha256"`
		Vendored     string `json:"vendored_sha256"`
	}
	b, err := os.ReadFile("testdata/autopilot/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" || src.From == "" {
		t.Fatalf("SOURCE.json: %v", err)
	}
	if got := fileSHA(t, "testdata/autopilot/autopilot-binding-v2.json"); got != src.Vendored {
		t.Errorf("testdata/autopilot/autopilot-binding-v2.json is %s, SOURCE.json records %s", got, src.Vendored)
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in CI")
	}
	raw, err := os.ReadFile(filepath.Join(server, src.From))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(publicScrub(raw))
	if got := hex.EncodeToString(sum[:]); got != src.Vendored {
		t.Fatalf("DRIFT: the service's %s, scrubbed for the public repository, is now %s, but this client vendored %s; re-vendor it (cp the file, update SOURCE.json) and re-run the vectors", src.From, got, src.Vendored)
	}
}

// ---- ks mcp --------------------------------------------------------------------------------------------

func TestCruiseAutoMCPToolsSpeakEveryVerb(t *testing.T) {
	want := map[string][]string{"ks_cruise_auto_brief": {"cruise", "auto", "brief"}, "ks_cruise_auto_answer": {"cruise", "auto", "answer"}, "ks_cruise_auto_plan": {"cruise", "auto", "plan"},
		"ks_cruise_auto_status": {"cruise", "auto", "status"}, "ks_cruise_auto_receipt": {"cruise", "auto", "receipt"}, "ks_cruise_auto_approve": {"cruise", "auto", "approve"}, "ks_cruise_auto_discard": {"cruise", "auto", "discard"}}
	for name, verb := range want {
		tl := findMCPTool(name)
		if tl == nil || strings.Join(tl.Verb, " ") != strings.Join(verb, " ") || tl.Title == "" {
			t.Fatalf("tool %s: %+v", name, tl)
		}
		if c, _, _ := lookup(registry, verb); c == nil || c.Group {
			t.Errorf("tool %s runs a verb the registry lacks: %v", name, verb)
		}
	}
	// approving spends: only a person's answer approves, and there is no confirm a model could pass
	ap := findMCPTool("ks_cruise_auto_approve")
	if !ap.Human || ap.DenyTool != "" || ap.Kind != "confirm" {
		t.Fatalf("the approve tool: %+v", ap)
	}
	if _, has := ap.schema()["properties"].(map[string]any)["confirm"]; has {
		t.Fatal("the approve tool takes a confirm value a model could pass")
	}
	if findMCPTool("ks_cruise_auto_discard").Human {
		t.Fatal("discard needs no person's answer beyond the confirm step")
	}
	// the digest is the verb's --confirm, the plan is its positional
	argv := strings.Join(ap.argv(map[string]any{"plan": fakePlanID, "digest": "0123456789ab"}, true), " ")
	if argv != "cruise auto approve --json --no-input --confirm=0123456789ab -- "+fakePlanID {
		t.Fatalf("the approve argv: %s", argv)
	}
	// consent to the file note comes from a person, never from the model
	brief := findMCPTool("ks_cruise_auto_brief")
	if !brief.Human || brief.Kind != "confirm" || !strings.Contains(strings.Join(brief.Fixed, " "), "--consent") {
		t.Fatalf("the brief tool lets a model give consent: %+v", brief)
	}
	if _, has := brief.schema()["properties"].(map[string]any)["consent"]; has {
		t.Fatal("the brief tool takes a consent a model could pass")
	}
	bargv := strings.Join(brief.argv(map[string]any{"draft": "crd_1", "text": "x"}, true), " ")
	if !strings.Contains(bargv, "--consent") {
		t.Fatalf("the confirmed brief does not pass consent: %s", bargv)
	}
}

func TestCruiseAutoMCPApprovePlanNamesTheSentences(t *testing.T) {
	f, _, _ := startAutoFake(t)
	doc, _ := json.Marshal(map[string]any{"data": f.plan})
	msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
	rev, plan, refusal := autoApproveInspect(nil, map[string]any{"plan": fakePlanID, "digest": msha[:12]}, doc)
	if refusal != "" || !strings.Contains(plan, "Adding 2 and 3 gives 5.") || !strings.Contains(plan, "taste is not checked by a machine") || !strings.Contains(plan, "$2.00") || !strings.Contains(plan, msha[:12]) || rev == "" {
		t.Fatalf("the plan a person approves: %q (%s)", plan, refusal)
	}
	if _, _, refusal := autoApproveInspect(nil, map[string]any{"plan": fakePlanID, "digest": strings.Repeat("0", 12)}, doc); !strings.Contains(refusal, "nothing was approved") {
		t.Fatalf("another digest: %q", refusal)
	}
	f.plan["state"], f.plan["state_label"] = "planning", "Writing and measuring the checks"
	doc, _ = json.Marshal(map[string]any{"data": f.plan})
	if _, _, refusal := autoApproveInspect(nil, map[string]any{"plan": fakePlanID, "digest": msha[:12]}, doc); !strings.Contains(refusal, "nothing to approve") {
		t.Fatalf("a plan not ready: %q", refusal)
	}
}

// A plan card whose cap or models are not the manifest's is refused, in approve
// and in status: what is shown is what the digest binds.
func TestCruiseAutoRefusesACardThatDisagreesWithTheManifest(t *testing.T) {
	url := "POST /api/v2/cruise-auto/" + fakePlanID + "/approve"
	for name, mut := range map[string]func(card map[string]any){
		"a cap of another size": func(card map[string]any) { card["cost"].(map[string]any)["cap_microusd"] = 5000000 },
		"another ladder":        func(card map[string]any) { card["ladder"] = []any{"claude-opus-5-5"} },
	} {
		f, bin, cfg := startAutoFake(t)
		msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
		f.mu.Lock()
		mut(f.plan["card"].(map[string]any))
		f.mu.Unlock()
		if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12]); code != exitIntegrity || f.count(url) != 0 {
			t.Fatalf("%s, approve: exit %d, requests %v\n%s%s", name, code, f.seen(), out, errs)
		}
		if out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "status", fakePlanID); code != exitIntegrity {
			t.Fatalf("%s, status: exit %d\n%s%s", name, code, out, errs)
		}
	}
	// the cap shown is the manifest's
	f, bin, cfg := startAutoFake(t)
	msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
	_, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12])
	if code != 0 || !strings.Contains(errs, "most it can spend: $2.00") || !strings.Contains(errs, "claude-haiku-4-5-20251001") {
		t.Fatalf("approve: exit %d\n%s", code, errs)
	}
}

// A plan on no files is a new project: the brief sends no draft and no consent,
// the outline is read from the manifest and its digest computed here, and the
// approval carries that digest with the checks' and the manifest's.
func startNewProjectFake(t *testing.T) (*autoFake, string, string, string) {
	t.Helper()
	f, bin, cfg := startAutoFake(t)
	files := []autoOutlineFile{{Path: "shopping.py", SHA256: strings.Repeat("c", 64)}}
	digest := autoSetDigest(files)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifest["autopilot"].(map[string]any)["outline"] = map[string]any{"digest": digest, "files": []any{map[string]any{"path": "shopping.py", "sha256": strings.Repeat("c", 64)}}}
	canon, _ := canonicalJSON(f.manifest)
	sum := sha256.Sum256(canon)
	msha := hex.EncodeToString(sum[:])
	f.draft["manifest"], f.draft["manifest_sha"] = json.RawMessage(canon), msha
	card := f.plan["card"].(map[string]any)
	card["manifest_sha"] = msha
	card["outline"] = map[string]any{"digest": digest, "files": []any{map[string]any{"path": "shopping.py", "bytes": 64, "content": "x"}}}
	f.plan["new_project"], f.plan["draft_id"] = true, "crd_0123456789ab"
	return f, bin, cfg, digest
}

func TestCruiseAutoBriefWithNoDraftIsANewProjectAndNeedsNoConsent(t *testing.T) {
	f, bin, cfg := startAutoFake(t)
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "brief", "--text", "a function that totals prices with VAT")
	if code != 0 || f.count("POST /api/v2/cruise-auto") != 1 {
		t.Fatalf("a brief with no draft: exit %d, requests %v\n%s%s", code, f.seen(), out, errs)
	}
	var req map[string]any
	if err := json.Unmarshal(f.posted["POST /api/v2/cruise-auto"], &req); err != nil || req["brief"] != "a function that totals prices with VAT" || len(req) != 1 {
		t.Fatalf("the brief carries files or a consent it does not have: %s", f.posted["POST /api/v2/cruise-auto"])
	}
	// files still need the consent, and send it
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "brief", "crd_0123456789ab", "--text", "x"); code != exitUsage {
		t.Fatalf("files without --consent: exit %d", code)
	}
}

func TestCruiseAutoApproveOfANewProjectSendsTheOutlineDigestComputedHere(t *testing.T) {
	f, bin, cfg, digest := startNewProjectFake(t)
	msha := f.plan["card"].(map[string]any)["manifest_sha"].(string)
	url := "POST /api/v2/cruise-auto/" + fakePlanID + "/approve"
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", msha[:12])
	if code != 0 {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"a new project: you gave no files", "shopping.py (64 bytes)"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the review does not say %q:\n%s", want, errs)
		}
	}
	var req map[string]any
	if err := json.Unmarshal(f.posted[url], &req); err != nil || req["outline_digest"] != digest || req["manifest_sha"] != msha || len(req) != 3 {
		t.Fatalf("approve body %s", f.posted[url])
	}
	// a card that shows another outline than the manifest holds is refused before anything is sent
	f2, bin2, cfg2, _ := startNewProjectFake(t)
	f2.mu.Lock()
	f2.plan["card"].(map[string]any)["outline"].(map[string]any)["digest"] = strings.Repeat("9", 64)
	m2 := f2.plan["card"].(map[string]any)["manifest_sha"].(string)
	f2.mu.Unlock()
	if _, _, code := ksIn(t, bin2, cfg2, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", m2[:12]); code != exitIntegrity || f2.count(url) != 0 {
		t.Fatalf("a card that shows another outline: exit %d", code)
	}
	// a new project whose manifest names no outline is refused
	f3, bin3, cfg3, _ := startNewProjectFake(t)
	f3.mu.Lock()
	delete(f3.manifest["autopilot"].(map[string]any), "outline")
	canon, _ := canonicalJSON(f3.manifest)
	sum := sha256.Sum256(canon)
	n3 := hex.EncodeToString(sum[:])
	f3.draft["manifest"], f3.draft["manifest_sha"] = json.RawMessage(canon), n3
	f3.plan["card"].(map[string]any)["manifest_sha"] = n3
	f3.mu.Unlock()
	if _, _, code := ksIn(t, bin3, cfg3, t.TempDir(), "cruise", "auto", "approve", fakePlanID, "--confirm", n3[:12]); code != exitIntegrity || f3.count(url) != 0 {
		t.Fatalf("a new project with no outline in its manifest: exit %d", code)
	}
}
