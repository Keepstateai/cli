// KS-059 on the command line: a check is defined with no shell and runs
// nothing; a run stops at approval_required until a person trusts the exact
// action by its hash (--yes never does); a changed action is refused; the
// result reads as an ordinary check with its output shown safely.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type checkCtl struct {
	mu       sync.Mutex
	defs     []map[string]any
	trusts   []map[string]any
	runs     int
	runState string
	stale    bool
}

const checkActionHash = "sha256:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"

func (c *checkCtl) run() map[string]any {
	exit := 1
	r := map[string]any{"id": "crun_1", "check_id": "chk_1", "agent_id": "agent_1", "role": "ordinary_check", "state": c.runState,
		"requested_by": "acct_1", "requested_at": "x", "observed_inputs_digest": "sha256:inputs", "action_hash": checkActionHash,
		"exact_action": "run `npm test` in the workspace root over package.json, test/run.sh", "output_bytes": 0, "artifacts": []any{}}
	if c.runState == "failed" {
		r["exit_code"], r["output"], r["output_bytes"], r["process_group_clear"] = exit, "1 failing \x1b[31mtest\x1b[0m\n", 20, true
		r["artifacts"] = []any{map[string]any{"path": "coverage.json", "present": true, "sha256": "abc", "bytes": 12}}
	}
	return r
}

func (c *checkCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetchk0000000000000000000000001", "short_id": "fleetchk0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.URL.Path == "/api/v2/agents/agent_1/checks" && r.Method == "POST":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.defs = append(c.defs, b)
		env(201, map[string]any{"id": "chk_1", "name": b["name"], "definition_hash": "sha256:def", "exact_action": "run `npm test`",
			"definition": map[string]any{"argv": b["argv"], "profile": "ks-check-v1: argv without a shell"}, "trusted": []any{}})
	case r.URL.Path == "/api/v2/checks/chk_1/runs" && r.Method == "POST":
		c.runs++
		c.runState = "approval_required"
		env(200, c.run())
	case r.URL.Path == "/api/v2/check-runs/crun_1" && r.Method == "GET":
		env(200, c.run())
	case r.URL.Path == "/api/v2/check-runs/crun_1/trust":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.trusts = append(c.trusts, b)
		if c.stale || b["expected_action_hash"] != checkActionHash {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_action_hash_mismatch","type":"ks_action_hash_mismatch","message":"the action changed"}}`)
			return
		}
		c.runState = "requested"
		env(200, c.run())
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`)
	}
}

func TestACheckRunsOnlyOverAnActionAPersonTrusted(t *testing.T) {
	c := &checkCtl{runState: "approval_required"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	// no inputs, a climbing input: refused before anything is sent
	if _, _, code := auditExec(t, bin, cfg, dir, env, "check", "define", "--session", "fleetchk", "unit", "--", "npm", "test"); code != exitUsage {
		t.Fatalf("no inputs: %d", code)
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "check", "define", "--session", "fleetchk", "--input", "../etc/passwd", "unit", "--", "npm", "test"); code != exitIntegrity {
		t.Fatalf("climbing input: %d\n%s", code, errs)
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "check", "define", "--session", "fleetchk", "--input", "package.json", "--input", "test/run.sh", "unit", "--", "npm", "test", "--", "--ci")
	if code != 0 || !strings.Contains(out, "nothing ran") || len(c.defs) != 1 {
		t.Fatalf("define: %d\n%s%s", code, out, errs)
	}
	if argv, _ := json.Marshal(c.defs[0]["argv"]); string(argv) != `["npm","test","--","--ci"]` {
		t.Fatalf("argv sent: %s", argv)
	}
	if _, _, code := auditExec(t, bin, cfg, dir, env, "check", "run", "chk_1"); code != 0 || c.runs != 1 {
		t.Fatalf("run: %d", code)
	}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "check", "show", "crun_1")
	if !strings.Contains(out, "NOT RUN") || !strings.Contains(out, "--confirm 9f8e7d6c5b4a") {
		t.Fatalf("show approval_required:\n%s", out)
	}
	// --yes, --no-input and a wrong hash trust nothing
	for _, args := range [][]string{{"--yes"}, {"--no-input"}, {"--confirm", "000000000000"}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, append([]string{"check", "trust", "crun_1"}, args...)...); code != exitUsage || !strings.Contains(errs, "nothing was trusted") {
			t.Errorf("%v: %d\n%s", args, code, errs)
		}
	}
	if len(c.trusts) != 0 {
		t.Fatal("an unconfirmed trust was sent")
	}
	// the action changed between the read and the trust: refused
	c.stale = true
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "check", "trust", "crun_1", "--confirm", "9f8e7d6c5b4a"); code != exitConflict || !strings.Contains(errs, "changed after it was shown") {
		t.Fatalf("stale trust: %d\n%s", code, errs)
	}
	c.stale = false
	out, errs, code = auditExec(t, bin, cfg, dir, env, "check", "trust", "crun_1", "--confirm", "9f8e7d6c5b4a")
	if code != 0 || !strings.Contains(out, "trusted") || !strings.Contains(errs, "npm test") || c.trusts[len(c.trusts)-1]["expected_action_hash"] != checkActionHash {
		t.Fatalf("trust: %d\n%s%s", code, out, errs)
	}
	// a finished run: an ordinary check, exit status, output made safe
	c.runState = "failed"
	out, _, _ = auditExec(t, bin, cfg, dir, env, "check", "show", "crun_1")
	if !strings.Contains(out, "never a verification") || !strings.Contains(out, "exit status   1") || strings.Contains(out, "\x1b") || !strings.Contains(out, "coverage.json") {
		t.Fatalf("finished:\n%s", out)
	}
}
