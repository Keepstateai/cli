// The organization statement verbs against a fake control plane: issuing
// only with --yes (it settles usage), a correction naming its statement, the
// statement read as issued, and verification that fails loudly.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationStatementVerbs(t *testing.T) {
	broken := false
	c := &orgBillingCtl{}
	c.extra = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		env := func(code int, data any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
		}
		stmt := func(id, corrects string) map[string]any {
			return map[string]any{"statement": map[string]any{"id": id, "seq": 1, "statement_total_micros": 110000, "corrects": corrects},
				"text": "KeepState usage statement\nIssued by: KeepState, Inc., San Francisco, CA\nBill to:   Acme Widgets GmbH\n", "corrected_by": []any{"inv_2"}}
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/statements":
			env(200, map[string]any{"items": []any{map[string]any{"id": "inv_1", "seq": 1, "statement_total_micros": 110000, "bill_to": "Acme Widgets GmbH"},
				map[string]any{"id": "inv_2", "seq": 2, "statement_total_micros": 110000, "bill_to": "Acme Holdings AG", "corrects": "inv_1"}}, "next_cursor": ""})
		case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/statements":
			var p map[string]any
			_ = json.Unmarshal(body, &p)
			env(201, stmt("inv_9", fmtAny(p["corrects"])))
		case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/statements/inv_1":
			env(200, stmt("inv_1", ""))
		case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/statements/inv_1/verify":
			env(200, map[string]any{"reconciled": !broken, "self_total_ok": true, "content_hash_ok": true, "chain_ok": true, "live_statement_ok": true, "billing_profile_ok": !broken})
		default:
			return false
		}
		return true
	}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	out, errs, code := run("organization", "statements", "org_a")
	if code != 0 || !strings.Contains(out, "inv_2") || !strings.Contains(out, "(corrects inv_1)") || !strings.Contains(out, "Acme Holdings AG") {
		t.Fatalf("statements: %d\n%s%s", code, out, errs)
	}
	// issuing settles usage: nothing is sent without --yes
	out, _, code = run("organization", "statement", "issue", "org_a", "--from", "2026-10-01T00:00:00Z")
	if code != 0 || !strings.Contains(out, "Nothing was issued") || !strings.Contains(out, "settles the period's usage") || c.last("POST /api/v2/orgs/org_a/statements") != "" {
		t.Fatalf("issue preview: %d %s", code, out)
	}
	out, errs, code = run("organization", "statement", "issue", "org_a", "--corrects", "inv_1", "--po-number", "PO-2", "--yes")
	sent := c.last("POST /api/v2/orgs/org_a/statements")
	if code != 0 || !strings.Contains(sent, `"corrects":"inv_1"`) || !strings.Contains(sent, `"po_number":"PO-2"`) || !strings.Contains(out, "Issued by: KeepState, Inc., San Francisco, CA") || !strings.Contains(out, "Issued inv_9") {
		t.Fatalf("issue a correction: %d %q\n%s%s", code, sent, out, errs)
	}
	out, _, code = run("organization", "statement", "show", "inv_1", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "Bill to:   Acme Widgets GmbH") || !strings.Contains(out, "Corrected by: inv_2") {
		t.Errorf("show: %d %s", code, out)
	}
	if out, _, code := run("organization", "statement", "verify", "inv_1", "--organization", "org_a"); code != 0 || !strings.Contains(out, "verifies") {
		t.Errorf("verify: %d %s", code, out)
	}
	c.mu.Lock()
	broken = true
	c.mu.Unlock()
	if out, _, code := run("organization", "statement", "verify", "inv_1", "--organization", "org_a"); code == 0 || !strings.Contains(out, "does NOT verify: billing_profile") {
		t.Errorf("a statement that does not verify: %d %s", code, out)
	}
}

func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
