// output_test: KS-007's guards. --json is one document on stdout whatever
// happens on stderr; a figure the control plane did not send is null or
// "unavailable", never zero; the exit code follows the table; a decision
// that needs a person fails under --no-input instead of guessing; and an
// automation fixture drives the client end to end parsing JSON only.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseEnvelope requires stdout to be exactly one JSON document with the
// schema fields; anything else on stdout is a defect.
func parseEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carries more than one document:\n%q", stdout)
	}
	if env["schema_version"] != float64(2) || env["request_id"] == nil {
		t.Fatalf("envelope lacks schema_version 2 or request_id: %v", env)
	}
	if _, ok := env["data"]; !ok {
		if _, ok := env["error"]; !ok {
			t.Fatalf("envelope has neither data nor error: %v", env)
		}
	}
	return env
}

// QA-007-1: JSON parses when warnings and retries happen.
func TestJSONIsOneDocumentDespiteRetries(t *testing.T) {
	ctl := newOpCtl()
	ctl.dropFirst = true
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	env := parseEnvelope(t, stdout)
	if env["data"].(map[string]any)["session_id"] != "sess-1" {
		t.Errorf("data: %v", env["data"])
	}
	if !strings.Contains(stderr, "retrying") {
		t.Errorf("the retry warning did not go to stderr:\n%s", stderr)
	}
	// --quiet silences the warnings and leaves the document
	stdout, stderr, _ = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "meter", "s1", "--json", "--quiet")
	parseEnvelope(t, stdout)
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("--quiet still wrote to stderr: %q", stderr)
	}
}

// QA-007-2: unknown spend prints unavailable/null; a genuine zero stays zero.
func TestMissingFiguresAreNullNeverZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/sessions/partial/meter":
			fmt.Fprint(w, `{"session":"partial","budget":500000,"billed_calls":0}`) // no spent, no key sources
		case "/api/sessions/full/meter":
			fmt.Fprint(w, `{"session":"full","spent":0,"budget":500000,"billed_calls":0,"key_sources":["byok"]}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"no such session"}`)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), nil, "meter", "partial")
	if code != 0 || !strings.Contains(out, "spent unavailable") || !strings.Contains(out, "billed calls 0") || !strings.Contains(out, "key source(s): unavailable") {
		t.Errorf("human partial meter:\n%s", out)
	}
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), nil, "meter", "partial", "--json")
	data := parseEnvelope(t, out)["data"].(map[string]any)
	if _, present := data["spent"]; present {
		t.Errorf("json invented a spent figure: %v", data)
	}
	if data["billed_calls"] != float64(0) {
		t.Errorf("a genuine zero was not kept: %v", data)
	}
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), nil, "meter", "full")
	if !strings.Contains(out, "spent 0 /") {
		t.Errorf("a genuine zero should print as 0:\n%s", out)
	}
}

// QA-007-3 and the exit table: noninteractive ambiguity fails; every
// failure class maps to its code, in both output modes.
func TestExitTableAndNoInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/s401/meter"):
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"unauthorized"}`)
		case strings.HasSuffix(r.URL.Path, "/s409/meter"):
			w.WriteHeader(409)
			fmt.Fprint(w, `{"error":{"type":"ks_tier_fence","message":"one session at a time"}}`)
		case strings.HasSuffix(r.URL.Path, "/s422/meter"):
			w.WriteHeader(422)
			fmt.Fprint(w, `{"error":{"type":"ks_workspace_digest_mismatch","message":"digest"}}`)
		case strings.HasSuffix(r.URL.Path, "/s503/meter"):
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"fleet unreachable"}`)
		case strings.HasSuffix(r.URL.Path, "/s404/meter"):
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"no such session ksk_secretsecretsecret \x1b[31mred\x1b[0m"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	want := map[string]int{"s401": exitAuth, "s409": exitConflict, "s422": exitIntegrity, "s503": exitTemporary, "s404": exitFailed}
	for sess, code := range want {
		_, errs, got := auditExec(t, bin, cfg, t.TempDir(), nil, "meter", sess)
		if got != code {
			t.Errorf("meter %s: exit %d, want %d\n%s", sess, got, code, errs)
		}
		if !strings.Contains(errs, "No remote work was started.") {
			t.Errorf("meter %s: the failure does not say whether work started:\n%s", sess, errs)
		}
		stdout, _, got := auditExec(t, bin, cfg, t.TempDir(), nil, "meter", sess, "--json")
		if got != code {
			t.Errorf("meter %s --json: exit %d, want %d", sess, got, code)
		}
		env := parseEnvelope(t, stdout)
		e := env["error"].(map[string]any)
		if e["code"] == nil || e["message"] == nil || e["work_started"] == nil {
			t.Errorf("meter %s --json: error shape %v", sess, e)
		}
		if sess == "s401" && e["next_action"] != "ks login" {
			t.Errorf("s401 next action: %v", e)
		}
	}
	// secrets and terminal control sequences never reach the terminal
	_, errs, _ := auditExec(t, bin, cfg, t.TempDir(), nil, "meter", "s404")
	if strings.Contains(errs, "ksk_secret") || strings.Contains(errs, "\x1b[") || !strings.Contains(errs, "[redacted]") {
		t.Errorf("error text not sanitised: %q", errs)
	}
	// a usage error is 2; an interactive verb under --no-input is 2 with the flag named
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), nil, "login", "--no-input")
	if code != exitUsage || !strings.Contains(errs, "--no-input") {
		t.Errorf("login --no-input: exit %d %s", code, errs)
	}
	_, _, code = auditExec(t, bin, cfg, t.TempDir(), nil, "attach", "s1", "--json")
	if code != exitUsage {
		t.Errorf("attach --json: exit %d, want %d", code, exitUsage)
	}
	// not signed in is a sign-in failure, 3
	os.Remove(filepath.Join(cfg, "keepstate", "token.json"))
	_, _, code = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "meter", "s1")
	if code != exitAuth {
		t.Errorf("signed out: exit %d, want %d", code, exitAuth)
	}
}

// VER-007-1: golden outputs for the states a script must recognise. The
// snapshots live in testdata/golden; KS_UPDATE_GOLDEN=1 rewrites them.
func TestGoldenOutputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/sessions":
			fmt.Fprint(w, `{"id":"sess-golden","image":"base","state":"running"}`)
		case r.URL.Path == "/api/sessions/empty/meter":
			fmt.Fprint(w, `{"session":"empty"}`)
		case r.URL.Path == "/api/sessions/denied/meter":
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"forbidden"}`)
		case r.URL.Path == "/api/jobs/job_review":
			fmt.Fprint(w, `{"id":"job_review","state":"review","verdict":"ladder-exhausted","goal":"g","manifest":{"ladder":[]},"attempts":[],"updated_at":"2026-09-20T00:00:00Z"}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"not found"}`)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	cases := map[string][]string{
		"run-success-json":    {"run", "--json"},
		"meter-empty-human":   {"meter", "empty"},
		"meter-empty-json":    {"meter", "empty", "--json"},
		"meter-denied-json":   {"meter", "denied", "--json"},
		"meter-missing-human": {"meter", "nope"},
		"cruise-review-human": {"cruise", "status", "job_review"},
		"usage-error-human":   {"run", "--buget", "1"},
		"version-json":        {"version", "--json"},
	}
	for name, args := range cases {
		stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, args...)
		got := fmt.Sprintf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s", code, stdout, stderr)
		// the request id varies by run; pin its shape instead
		got = strings.ReplaceAll(got, `"request_id":"`+requestIDIn(stdout)+`"`, `"request_id":"req_…"`)
		got = strings.ReplaceAll(got, srv.URL, "http://control-plane")
		p := filepath.Join("testdata", "golden", name+".txt")
		if os.Getenv("KS_UPDATE_GOLDEN") == "1" {
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: no golden file (KS_UPDATE_GOLDEN=1 to write): %v", name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from its golden output\n--- want ---\n%s--- got ---\n%s", name, want, got)
		}
	}
}

func requestIDIn(stdout string) string {
	var env struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(stdout), &env)
	return env.RequestID
}

// VER-007-2: an automation fixture creates, inspects, and cancels parsing
// JSON only, never human prose.
func TestAutomationFixtureParsesOnlyJSON(t *testing.T) {
	f, bin, cfg := startFake(t)
	f.job["state"], f.job["verdict"] = "running", nil
	repo := demoRepo(t)
	// create a job: init, approve, run, each read back as JSON
	stdout, _, code := auditExec(t, bin, cfg, repo, nil, "cruise", "init", "--goal", "automation", "--json")
	if code != 0 {
		t.Fatal("init")
	}
	initData := parseEnvelope(t, stdout)["data"].(map[string]any)
	if initData["manifest_sha"] == nil {
		t.Fatalf("init json: %v", initData)
	}
	stdout, _, _ = auditExec(t, bin, cfg, repo, nil, "cruise", "approve", "--json")
	if parseEnvelope(t, stdout)["data"].(map[string]any)["manifest_sha"] != initData["manifest_sha"] {
		t.Fatal("approve json digest differs")
	}
	stdout, _, code = auditExec(t, bin, cfg, repo, nil, "cruise", "run", "--json")
	if code != 0 {
		t.Fatal("run")
	}
	jobID := parseEnvelope(t, stdout)["data"].(map[string]any)["job_id"].(string)
	// inspect
	stdout, _, _ = auditExec(t, bin, cfg, repo, nil, "cruise", "status", jobID, "--json")
	if parseEnvelope(t, stdout)["data"].(map[string]any)["id"] != jobID {
		t.Fatal("status json")
	}
	// logs: one event per line
	stdout, _, _ = auditExec(t, bin, cfg, repo, nil, "cruise", "logs", jobID, "--json")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, l := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev["type"] == nil {
			t.Fatalf("logs line is not an event object: %q", l)
		}
	}
	// cancel
	stdout, _, code = auditExec(t, bin, cfg, repo, nil, "cruise", "cancel", jobID, "--json")
	if code != 0 || parseEnvelope(t, stdout)["data"].(map[string]any)["state"] != "cancelled" {
		t.Fatalf("cancel json: %s", stdout)
	}
}
