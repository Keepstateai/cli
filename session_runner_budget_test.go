// ks session runner --token-budget: a session paused at its token budget is
// given a new one and, with --resume, continues, in one change the service
// carries out. The budget is checked by the service against what the session
// has used; a refusal says nothing changed and what to do.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// budgetCtl answers a token budget at or below what is used with the
// service's refusal, and anything else as the runner change does.
type budgetCtl struct{ *runnerCtl }

func (c budgetCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_park/runner-change" {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if n, _ := body["token_budget"].(float64); n > 0 && n <= 2034967 {
			c.rmu.Lock()
			c.sent = append(c.sent, body)
			c.rmu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": "ks_budget_below_used", "type": "ks_budget_below_used",
				"message": "this session has used 2,034,967 tokens of its budget, so a new budget must be more than that; 2,000,000 is not. Nothing was changed", "work_started": "no"}})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	c.runnerCtl.ServeHTTP(w, r)
}

func TestSessionRunnerRaisesTheTokenBudgetAndResumes(t *testing.T) {
	rc := &runnerCtl{modelCtl: modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}, ops: map[string]map[string]any{}, reads: map[string]int{},
		harness: "claude-code", key: map[string]any{"provider": "anthropic", "route": "own", "key": map[string]any{"id": "vlt_own", "label": "Your Anthropic key", "last4": "7Q2a"}}}
	syncFixture(t, &rc.mu)
	srv := httptest.NewServer(budgetCtl{rc})
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--token-budget", "4100000", "--resume")
	if code != 0 {
		t.Fatalf("raise and resume: %d\n%s%s", code, out, errs)
	}
	b := rc.sent[0]
	if b["token_budget"] != float64(4_100_000) || b["resume"] != true || b["expected_revision"] != float64(5) || b["harness"] != nil || b["spend_limit_microusd"] != nil {
		t.Fatalf("the change sent: %v", b)
	}
	// at or below what is used: the service's words, nothing changed, the way on
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--token-budget", "2000000", "--resume")
	if code == 0 || !strings.Contains(errs, "has used 2,034,967 tokens") || !strings.Contains(errs, "Nothing was changed") || !strings.Contains(errs, "--token-budget <more> --resume") {
		t.Fatalf("a budget below what is used: %d\n%s", code, errs)
	}
	// zero and words are refused before anything is sent
	n := len(rc.sent)
	for _, bad := range []string{"0", "-5", "lots"} {
		if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--token-budget", bad); code != exitUsage {
			t.Fatalf("--token-budget %s: %d\n%s", bad, code, errs)
		}
	}
	if len(rc.sent) != n {
		t.Fatal("a usage error sent a change")
	}
	// --resume goes with a token budget as with any other change
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--resume"); code != exitUsage || !strings.Contains(errs, "--token-budget") {
		t.Fatalf("--resume alone names --token-budget among the changes: %d\n%s", code, errs)
	}
}
