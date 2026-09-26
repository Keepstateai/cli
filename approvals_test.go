// KS-047 on the command line, against the approval as the control plane's
// route table serves it (action_kind, human_scope, arguments, decision):
// requests listed with their instruction, what they touch and what deciding
// costs; a decision bound to the revision and the exact action; a decision
// that lost says what was recorded, by whom and when; and the refusals.
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

type approvalCtl struct {
	mu        sync.Mutex
	mode      string // ok | lost | stale | forbid
	unavail   bool
	decisions []map[string]any
	calls     []string
}

func approvalFixtureRow(id, decision string) map[string]any {
	r := map[string]any{"id": id, "account_id": "acct_1", "revision": 3, "created_at": "x", "updated_at": "x", "session_id": "session_1",
		"requester": "agent", "human_scope": "write the release notes", "action_kind": "file_write",
		"arguments": map[string]any{"file_path": "docs/RELEASE.md"}, "exact_action_hash": "hash_" + id, "resource_revisions": map[string]any{},
		"branch_id": "", "expires_at": "2026-09-26T13:00:00Z", "actionable_until": "2026-09-26T12:00:45Z", "decision": decision,
		"decided_by": "", "decided_at": "", "epoch": 1, "requested_for_task": "tsk_1",
		"affects":          []string{"file_path: docs/RELEASE.md"},
		"cost_implication": "deciding costs nothing and the tool itself is not metered by this service; the model calls that follow an approval are admitted against this session's limits: 1200 of 500000 tokens are admitted or used"}
	if decision != "pending" {
		r["decided_by"], r["decided_at"] = "acct_other", "2026-09-26T12:00:10Z"
	}
	return r
}

func (c *approvalCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, e map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		e["work_started"] = "no"
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": e})
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/capabilities":
		a := "available"
		if c.unavail {
			a = "unavailable"
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}],"limits":{}}}`, a)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetapr0000000000000000000000001", "short_id": "fleetapr0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/approvals" && q.Get("session_id") == "session_1":
		items := []any{approvalFixtureRow("apr_1", "pending")}
		if q.Get("state") == "all" {
			items = append(items, approvalFixtureRow("apr_2", "approved"))
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	case r.URL.Path == "/api/v2/approvals/apr_1" && r.Method == "GET":
		env(200, approvalFixtureRow("apr_1", "pending"))
	case r.URL.Path == "/api/v2/approvals/apr_2" && r.Method == "GET":
		env(200, approvalFixtureRow("apr_2", "approved"))
	case strings.HasSuffix(r.URL.Path, "/decision") && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.decisions = append(c.decisions, body)
		switch c.mode {
		case "lost":
			refuse(409, map[string]any{"code": "ks_approval_not_pending", "type": "ks_approval_not_pending",
				"message": "this approval was already denied; your decision was not recorded", "next_action": "GET /api/v2/approvals/apr_1",
				"resolved": map[string]any{"decision": "denied", "decided_by": "acct_other", "decided_at": "2026-09-26T12:00:05Z", "revision": 4}})
		case "stale":
			refuse(409, map[string]any{"code": "ks_revision_conflict", "type": "ks_revision_conflict", "message": "the record changed since it was read; refresh before retrying"})
		case "forbid":
			refuse(403, map[string]any{"code": "ks_forbidden", "type": "ks_forbidden", "message": "deciding needs the operator role"})
		default:
			row := approvalFixtureRow("apr_1", body["decision"].(string)+"d")
			env(200, map[string]any{"approval": row, "decision": body["decision"], "decided_at": "2026-09-26T12:00:07Z"})
		}
	default:
		refuse(404, map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no such route in this fake: " + r.URL.Path})
	}
}

func (c *approvalCtl) set(f func(*approvalCtl)) {
	c.mu.Lock()
	f(c)
	c.mu.Unlock()
}

func (c *approvalCtl) decisionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.decisions)
}

func TestApprovalsAreListedWithTheirTaskWhatTheyTouchAndTheCost(t *testing.T) {
	c := &approvalCtl{mode: "ok"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "approval", "list", "--session", "fleetapr")
	if code != 0 {
		t.Fatalf("list: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"permission request apr_1  PENDING", "file_write — write the release notes", "docs/RELEASE.md",
		"for instruction tsk_1", "affects         file_path: docs/RELEASE.md", "cost            deciding costs nothing", "answerable till", "ks agent approve apr_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "approval", "list", "--session", "fleetapr", "--all")
	if code != 0 || !strings.Contains(out, "apr_2  APPROVED") || !strings.Contains(out, "approved by acct_other at 2026-09-26T12:00:10Z") {
		t.Fatalf("list --all: %d\n%s", code, out)
	}
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "approval", "show", "apr_1", "--session", "fleetapr", "--json")
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Data["requested_for_task"] != "tsk_1" || doc.Data["cost_implication"] == nil {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if c.decisionCount() != 0 {
		t.Fatal("a read decided something")
	}
}

func TestADecisionIsBoundToTheRevisionAndALostOneSaysWhatStands(t *testing.T) {
	c := &approvalCtl{mode: "ok"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// through the alias, with the revision and the exact action hash read
	out, errs, code := auditExec(t, bin, cfg, dir, env, "approval", "approve", "apr_1", "--session", "fleetapr")
	if code != 0 || !strings.Contains(out, "approved permission request apr_1") {
		t.Fatalf("approve: %d\n%s%s", code, out, errs)
	}
	if d := c.decisions[0]; d["expected_revision"] != float64(3) || d["action_hash"] != "hash_apr_1" || d["decision"] != "approve" {
		t.Fatalf("the decision sent: %v", d)
	}
	// lost to another decision: the recorded one, who and when; nothing retried
	c.set(func(c *approvalCtl) { c.mode, c.decisions = "lost", nil })
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "approve", "apr_1", "--session", "fleetapr")
	if code != exitConflict || !strings.Contains(errs, "already denied by acct_other at 2026-09-26T12:00:05Z") ||
		!strings.Contains(errs, "recorded decision  denied") || !strings.Contains(errs, "decided by         acct_other") || c.decisionCount() != 1 {
		t.Fatalf("lost: %d decisions=%d\n%s", code, c.decisionCount(), errs)
	}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "agent", "approve", "apr_1", "--session", "fleetapr", "--json")
	if !strings.Contains(out, `"decided_by":"acct_other"`) || !strings.Contains(out, `"decided_at":"2026-09-26T12:00:05Z"`) {
		t.Fatalf("lost --json: %s", out)
	}
	// already decided when read: refused with who and when, nothing sent
	c.set(func(c *approvalCtl) { c.mode, c.decisions = "ok", nil })
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "deny", "apr_2", "--session", "fleetapr"); code != exitConflict ||
		!strings.Contains(errs, "already approved by acct_other") || c.decisionCount() != 0 {
		t.Fatalf("already decided: %d\n%s", code, errs)
	}
	// a stale revision and the wrong role
	c.set(func(c *approvalCtl) { c.mode = "stale" })
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "deny", "apr_1", "--session", "fleetapr"); code != exitConflict || !strings.Contains(errs, "moved on") {
		t.Fatalf("stale: %d\n%s", code, errs)
	}
	c.set(func(c *approvalCtl) { c.mode = "forbid" })
	if out, _, code := auditExec(t, bin, cfg, dir, env, "agent", "deny", "apr_1", "--session", "fleetapr", "--json"); code != exitAuth || !strings.Contains(out, "ks_forbidden") {
		t.Fatalf("forbid: %d %s", code, out)
	}
	// unavailable: said so, nothing read or decided
	c.set(func(c *approvalCtl) { c.unavail, c.decisions, c.calls = true, nil, nil })
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "approval", "list", "--session", "fleetapr"); code != exitFailed || !strings.Contains(errs, "agent.workspace") {
		t.Fatalf("unavailable: %d\n%s", code, errs)
	}
	for _, call := range c.calls {
		if strings.Contains(call, "/approvals") {
			t.Fatalf("a disabled verb reached the service: %v", c.calls)
		}
	}
}
