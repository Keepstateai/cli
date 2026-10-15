// ks session brief: reads the handoff brief of the session's latest harness
// switch (GET /api/v2/sessions/{id}/handoff-brief), as the new agent received
// it: who wrote its narrative on whose key, its size, its checks, and the
// brief itself. A session that never switched has none, said with the next
// action. Authored before the verb (rule 12).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type briefCtl struct{ harnessCtl }

func (c *briefCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/handoff-brief") {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "session_park") {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{
				"handoff_id": "hof_1", "session_id": "session_park", "state": "delivered", "from": "claude-code", "to": "codex",
				"from_label": "Claude Code", "to_label": "Codex CLI", "created_at": "2026-10-16T10:00:00Z", "written_at": "2026-10-16T10:01:00Z",
				"delivered_at": "2026-10-16T10:02:00Z", "delivered_task_id": "tsk_7", "narrative_model": "claude-haiku-5-5", "provider": "anthropic",
				"fallback": false, "tokens": 812, "sha256": "ab12", "brief": "KEEPSTATE HANDOFF RECORD.\n1 The person's instructions\nEND OF KEEPSTATE HANDOFF RECORD.",
				"checks": map[string]any{"lines": 5, "kept": 3, "dropped_citation": 1, "dropped_injection": 1, "redactions": 2, "injection_seqs": []int{5, 6}},
				"cost":   "billed by Anthropic on the session's own key at its rates; KeepState does not price it"}})
			return
		}
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": "ks_not_found", "type": "ks_not_found",
			"message": "this session has not switched its harness, so it has no handoff brief", "work_started": "no"}})
		return
	}
	c.harnessCtl.ServeHTTP(w, r)
}

func TestSessionBriefReadsTheLatestSwitchsBrief(t *testing.T) {
	c := &briefCtl{harnessCtl{modelCtl{model: "gpt-6-luna", rev: 5}}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "brief", "fleetpark")
	for _, want := range []string{"handoff brief of session fleetpark000: Claude Code to Codex CLI (hof_1)", "delivered with instruction tsk_7",
		"its narrative by claude-haiku-5-5 on the session's own key", "812 tokens", "KeepState does not price it",
		"kept 3 of 5 model lines; dropped 1 without a valid citation, 1 addressed to the agent that no person wrote; 2 secrets redacted",
		"KEEPSTATE HANDOFF RECORD.\n1 The person's instructions"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("the brief lacks %q: %d\n%s%s", want, code, out, errs)
		}
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "brief", "fleetpark", "--json")
	if code != 0 || !strings.Contains(out, `"handoff_id": "hof_1"`) && !strings.Contains(out, `"handoff_id":"hof_1"`) {
		t.Fatalf("--json: %d\n%s", code, out)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "brief", "fleetrun")
	if code == 0 || !strings.Contains(errs, "has not switched its harness") || !strings.Contains(errs, "ks session harness fleetrun0000") {
		t.Fatalf("a session that never switched: %d\n%s", code, errs)
	}
}
