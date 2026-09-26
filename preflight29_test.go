// KS-029 on the command line: preflight sends what only the client can
// measure (the workspace selection's bytes and files, the draft's
// acceptance check and ladder families, its protocol), prints each check's
// category and what was not checked, says it reserves nothing, and names a
// network failure as network. ks cruise run stops on a preflight blocker
// before any upload.
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

type pf29Ctl struct {
	mu     sync.Mutex
	bodies []map[string]any
	proto  []string
	block  bool
	drop   bool // the preflight connection is dropped with no answer
	jobs   int
}

func (c *pf29Ctl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"preflight","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/preflight" && c.drop:
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	case r.URL.Path == "/api/v2/preflight":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.bodies = append(c.bodies, b)
		c.proto = append(c.proto, r.Header.Get("X-KS-Protocol"))
		checks := []any{
			map[string]any{"check": "account", "status": "pass", "detail": "ok", "category": "authentication"},
			map[string]any{"check": "workspace", "status": "pass", "detail": "within the limits", "category": "setup"},
			map[string]any{"check": "key_route", "status": "warn", "detail": "one family has a disabled key", "category": "setup", "next_action": "ks key list"},
		}
		if c.block {
			checks = append(checks, map[string]any{"check": "credit", "status": "block", "detail": "no credit", "category": "quota", "next_action": "top up"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{
			"account_id": "acct_x", "cohort_state": "active", "credit_microusd": 5000000, "registry_version": "t", "checks": checks,
			"ready": !c.block, "not_checked": []string{"budget_tokens: not declared"},
			"reservation": "nothing is reserved by a preflight; admission happens when the job is submitted"}})
	case r.URL.Path == "/api/jobs":
		c.jobs++
		w.WriteHeader(500)
	default:
		w.WriteHeader(404)
	}
}

func TestPreflightSendsWhatOnlyTheClientMeasures(t *testing.T) {
	c := &pf29Ctl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, "main.go"), "package main\n")
	writeFileT(t, filepath.Join(repo, "main_test.go"), "package main\n")
	_ = os.MkdirAll(filepath.Join(repo, ".keepstate"), 0o755)
	writeFileT(t, filepath.Join(repo, ".keepstate", "cruise.json"), `{"verifier":{"command":"go test ./..."},"ladder":[{"family":"claude","model":"haiku"},{"family":"claude","model":"sonnet"},{"family":"gpt","model":"x"}]}`)
	out, errs, code := auditExec(t, bin, cfg, repo, fastEnv(cfg), "preflight")
	if code != 0 {
		t.Fatalf("preflight: %d\n%s%s", code, out, errs)
	}
	b := c.bodies[0]
	if b["workspace_files"] != float64(2) || b["workspace_bytes"] == nil || b["check_command"] != "go test ./..." {
		t.Fatalf("sent: %v", b)
	}
	if l, _ := json.Marshal(b["ladder"]); string(l) != `["claude","gpt"]` {
		t.Fatalf("ladder sent: %s", l)
	}
	if c.proto[0] != "2" {
		t.Fatalf("protocol not declared: %v", c.proto)
	}
	for _, want := range []string{"[setup]", "not checked: budget_tokens: not declared", "nothing is reserved by a preflight", "a passing preflight reserves nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// flags override the draft
	if _, _, code := auditExec(t, bin, cfg, repo, fastEnv(cfg), "preflight", "--check", "make test", "--ladder", "claude"); code != 0 || c.bodies[1]["check_command"] != "make test" {
		t.Fatalf("override: %d %v", code, c.bodies[1])
	}
}

func TestPreflightNamesANetworkFailure(t *testing.T) {
	c := &pf29Ctl{drop: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "preflight")
	if code != exitTemporary || !strings.Contains(errs, "category: network") || !strings.Contains(errs, "nothing was reserved") {
		t.Fatalf("network: %d\n%s", code, errs)
	}
}
