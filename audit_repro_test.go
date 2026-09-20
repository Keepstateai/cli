// audit_repro_test: KS-001 of the Agent Workspace program. The five
// findings of the 2026-09-19 executable audit (ks-cli-technical-findings)
// were made against the public v0.1.8 source with a local mock. This file
// re-runs them in disposable fixtures against the built binary and writes
// what it OBSERVED, per finding, as repro.json plus one transcript each.
//
// It is an observation harness, not a guard: a finding that no longer
// reproduces is recorded as such rather than failing the run, because the
// permanent regression guards for each defect land with the fix that closes
// it (KS-002 parsing, KS-003 help boundaries, KS-006 install, KS-027 upload
// integrity, KS-033 terminal), under the patch law: red before, green after,
// guard in the same commit. What fails here is the harness itself: a binary
// that will not build, a recorder that cannot see a request.
//
// Opt in with KS_AUDIT_REPRO=1; KS_AUDIT_REPRO_OUT names the directory the
// evidence is written to (default: a temp dir, printed). Nothing here uses a
// real key, a real account or a network address outside the loopback
// recorder.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type auditHit struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

type auditRecorder struct {
	mu   sync.Mutex
	hits []auditHit
}

func (a *auditRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	a.mu.Lock()
	a.hits = append(a.hits, auditHit{r.Method, r.URL.Path, string(b)})
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/sessions":
		fmt.Fprint(w, `{"id":"sess-audit","image":"base","state":"running"}`)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec"):
		fmt.Fprint(w, `{"output":"(recorded)\n"}`)
	case r.Method == "POST" && strings.Contains(r.URL.Path, "/fork"):
		fmt.Fprint(w, `[{"id":"child-1","parent":"sess-audit"}]`)
	case r.Method == "POST" && r.URL.Path == "/api/jobs":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"job_audit","state":"queued","spend_ceiling_microusd":2000000}`)
	default:
		fmt.Fprint(w, `{}`)
	}
}

func (a *auditRecorder) drain() []auditHit {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]auditHit(nil), a.hits...)
	a.hits = nil
	return out
}

// one observation of one invocation: what was run, what came back, what
// the recorder saw. Bodies are kept verbatim except the workspace upload,
// which is summarised as its entry list.
type auditRun struct {
	Args     []string   `json:"args"`
	Exit     int        `json:"exit"`
	Stdout   string     `json:"stdout"`
	Stderr   string     `json:"stderr"`
	Requests []auditHit `json:"requests"`
}

type auditFinding struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Status   string     `json:"status"` // reproduced | not-reproduced | not-tested
	Observed string     `json:"observed"`
	Runs     []auditRun `json:"runs"`
	Note     string     `json:"note,omitempty"`
}

func auditExec(t *testing.T, bin, cfg, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+cfg), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

func TestAuditReproductions(t *testing.T) {
	if os.Getenv("KS_AUDIT_REPRO") != "1" {
		t.Skip("KS-001 audit reproduction harness; opt in with KS_AUDIT_REPRO=1")
	}
	outDir := os.Getenv("KS_AUDIT_REPRO_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &auditRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	// the harness control: the recorder must see a request when one is made
	auditExec(t, bin, cfg, t.TempDir(), nil, "run")
	if len(rec.drain()) == 0 {
		t.Fatal("control: `ks run` made no request; the recorder cannot observe, so nothing below would mean anything")
	}

	var findings []auditFinding
	record := func(f auditFinding) {
		findings = append(findings, f)
		var tr strings.Builder
		fmt.Fprintf(&tr, "%s — %s\nstatus: %s\nobserved: %s\n\n", f.ID, f.Title, f.Status, f.Observed)
		for _, r := range f.Runs {
			fmt.Fprintf(&tr, "$ ks %s\nexit %d\n", strings.Join(r.Args, " "), r.Exit)
			if r.Stdout != "" {
				fmt.Fprintf(&tr, "stdout:\n%s\n", indent(r.Stdout))
			}
			if r.Stderr != "" {
				fmt.Fprintf(&tr, "stderr:\n%s\n", indent(r.Stderr))
			}
			if len(r.Requests) == 0 {
				fmt.Fprintln(&tr, "requests: none")
			}
			for _, h := range r.Requests {
				fmt.Fprintf(&tr, "request: %s %s %s\n", h.Method, h.Path, h.Body)
			}
			fmt.Fprintln(&tr)
		}
		if f.Note != "" {
			fmt.Fprintf(&tr, "note: %s\n", f.Note)
		}
		if err := os.WriteFile(filepath.Join(outDir, f.ID+".txt"), []byte(tr.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runOne := func(dir string, args ...string) auditRun {
		so, se, code := auditExec(t, bin, cfg, dir, nil, args...)
		return auditRun{Args: args, Exit: code, Stdout: so, Stderr: se, Requests: rec.drain()}
	}

	// ---- Finding 1: invalid budget input still starts the create request
	{
		f := auditFinding{ID: "F1", Title: "invalid budget input can still start the create-session request"}
		// three malformed forms must send nothing; the equals form is a
		// legacy spelling the audit found silently ignored (Budget 0 sent),
		// and must send exactly 1000 once supported
		malformedSent := 0
		for _, args := range [][]string{{"run", "--buget", "1000"}, {"run", "--budget", "abc"}, {"run", "--budget"}} {
			r := runOne(t.TempDir(), args...)
			f.Runs = append(f.Runs, r)
			for _, h := range r.Requests {
				if h.Method == "POST" && h.Path == "/api/sessions" {
					malformedSent++
				}
			}
		}
		equalsSent := "nothing"
		r := runOne(t.TempDir(), "run", "--budget=1000")
		f.Runs = append(f.Runs, r)
		for _, h := range r.Requests {
			if h.Method == "POST" && h.Path == "/api/sessions" {
				var body map[string]json.RawMessage
				_ = json.Unmarshal([]byte(h.Body), &body)
				equalsSent = string(body["Budget"])
				if equalsSent == "" {
					equalsSent = "absent"
				}
			}
		}
		// a valid space-separated value, for the contrast
		f.Runs = append(f.Runs, runOne(t.TempDir(), "run", "--budget", "1000"))
		f.Observed = fmt.Sprintf("%d of 3 malformed invocations sent POST /api/sessions; --budget=1000 sent Budget %s", malformedSent, equalsSent)
		if malformedSent == 0 && equalsSent == "1000" {
			f.Status = "not-reproduced"
		} else {
			f.Status = "reproduced"
		}
		record(f)
	}

	// ---- Finding 2: remote help intercepted; argument boundaries lost
	{
		f := auditFinding{ID: "F2", Title: "remote help is intercepted and exec argument boundaries are lost"}
		intercepted := 0
		for _, args := range [][]string{{"exec", "mock-session", "python", "--help"}, {"exec", "mock-session", "--", "python", "--help"}, {"exec", "mock-session", "echo", "help"}} {
			r := runOne(t.TempDir(), args...)
			f.Runs = append(f.Runs, r)
			if len(r.Requests) == 0 && strings.Contains(r.Stdout, "usage:") {
				intercepted++
			}
		}
		r := runOne(t.TempDir(), "exec", "mock-session", "python", "-c", `print("a b")`)
		f.Runs = append(f.Runs, r)
		joined := false
		for _, h := range r.Requests {
			if strings.HasSuffix(h.Path, "/exec") && strings.Contains(h.Body, `"Cmd":"python -c print(\"a b\")"`) {
				joined = true
			}
		}
		f.Observed = fmt.Sprintf("%d of 3 help-looking remote invocations printed KeepState's own usage with no request; argv joined with spaces: %v", intercepted, joined)
		if intercepted == 3 && joined {
			f.Status = "reproduced"
		} else if intercepted == 0 && !joined {
			f.Status = "not-reproduced"
		} else {
			f.Status = "reproduced"
			f.Note = "partially: see the per-run output"
		}
		record(f)
	}

	// ---- Finding 3: terminal helpers do not operate on the attached terminal
	{
		f := auditFinding{ID: "F3", Title: "terminal helpers do not operate on the attached terminal"}
		cols, rows := termSize()
		_, sttyErr := exec.Command("stty", "size").Output()
		f.Observed = fmt.Sprintf("in-process termSize() returned %dx%d; the stty subprocess it spawns has no Stdin (Go reads the null device) and failed here: %v", cols, rows, sttyErr != nil)
		f.Status = "reproduced"
		f.Note = "function-level reproduction: the helper cannot depend on the caller's terminal because its subprocess is not connected to it. The audit's pseudo-terminal run (40x120 configured, 80x24 returned, raw mode unchanged) is cited from the source note; a pseudo-terminal fixture (F11) is KS-033's deliverable, so the PTY leg is not-tested here."
		record(f)
	}

	// ---- Finding 4: workspace packaging includes ignored files and follows outside symlinks
	{
		f := auditFinding{ID: "F4", Title: "workspace packaging includes ignored files and follows file symlinks outside the workspace"}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside the workspace\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fixture := t.TempDir()
		writeTree(t, fixture, map[string]string{
			"test_ok.py":               "def test_ok():\n    assert True\n",
			".env":                     "SYNTHETIC_PLACEHOLDER=not-a-secret\n",
			".gitignore":               ".env\nnode_modules/\n",
			"node_modules/fixture.txt": "synthetic dependency file\n",
		})
		if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(fixture, "external-link.txt")); err != nil {
			t.Fatal(err)
		}
		f.Runs = append(f.Runs, runOne(fixture, "cruise", "init", "--goal", "audit fixture"))
		f.Runs = append(f.Runs, runOne(fixture, "cruise", "approve"))
		r := runOne(fixture, "cruise", "run")
		var entries []string
		for i, h := range r.Requests {
			if h.Method == "POST" && h.Path == "/api/jobs" {
				var posted struct {
					WorkspaceB64 string `json:"workspace_b64"`
				}
				if err := json.Unmarshal([]byte(h.Body), &posted); err == nil {
					if tgz, err := base64.StdEncoding.DecodeString(posted.WorkspaceB64); err == nil {
						entries = tarNames(t, tgz)
					}
				}
				r.Requests[i].Body = "(POST /api/jobs body: manifest + workspace_b64; archive entries listed in `observed`)"
			}
		}
		f.Runs = append(f.Runs, r)
		sort.Strings(entries)
		f.Observed = "archive entries: " + strings.Join(entries, ", ")
		has := func(n string) bool {
			for _, e := range entries {
				if e == n {
					return true
				}
			}
			return false
		}
		if has(".env") && has("node_modules/fixture.txt") && has("external-link.txt") {
			f.Status = "reproduced"
		} else if !has(".env") && !has("node_modules/fixture.txt") && !has("external-link.txt") {
			f.Status = "not-reproduced"
		} else {
			f.Status = "reproduced"
			f.Note = "partially: see the entry list"
		}
		record(f)
	}

	// ---- Finding 5: README installation command points to an HTML page
	{
		f := auditFinding{ID: "F5", Title: "README installation command points to an HTML page"}
		readme, err := os.ReadFile("README.md")
		if err != nil {
			t.Fatal(err)
		}
		line := ""
		for _, l := range strings.Split(string(readme), "\n") {
			if strings.Contains(l, "keepstate.ai/install") && strings.Contains(l, "| sh") {
				line = strings.TrimSpace(l)
				break
			}
		}
		f.Observed = "README install line: " + line
		switch {
		case strings.Contains(line, "/install.sh"):
			f.Status = "not-reproduced"
		case strings.Contains(line, "/install "):
			f.Status = "reproduced"
		default:
			f.Status = "not-tested"
		}
		f.Note = "the content types of /install (text/html) and /install.sh (application/x-sh) are recorded in baseline.json from a read-only fetch on the baseline date; this harness reads only the README"
		record(f)
	}

	report := map[string]any{
		"harness":     "audit_repro_test.go (KS-001)",
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
		"findings":    findings,
	}
	b, _ := json.MarshalIndent(report, "", " ")
	if err := os.WriteFile(filepath.Join(outDir, "repro.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Logf("%s %-16s %s", f.ID, f.Status, f.Observed)
	}
	t.Logf("evidence written to %s", outDir)
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func tarNames(t *testing.T, tgz []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	return names
}
