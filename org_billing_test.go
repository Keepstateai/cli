// The organization billing verbs against a fake control plane: what each
// sends (a change carries the whole profile and the revision read), what
// each prints, and the refusals made before anything is sent.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgBillingCtl struct {
	mu      sync.Mutex
	sent    []string
	profile map[string]any
	extra   func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

func (c *orgBillingCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	view := func() map[string]any {
		missing := []any{"legal_name", "address_line1", "city", "postal_code", "country"}
		var prof any
		if c.profile != nil {
			prof, missing = c.profile, []any{}
		}
		return map[string]any{"org_id": "org_a", "profile": prof, "missing": missing, "issuer": map[string]any{"name": "KeepState, Inc.", "address": "San Francisco, CA"},
			"contacts": []any{map[string]any{"account_id": "acct_bill", "kind": "both", "role": "billing", "receives": true}, map[string]any{"account_id": "acct_gone", "kind": "alerts", "receives": false}},
			"note":     "Changes apply to the next statement and the next purchase; past statements never change.", "your_role": "owner", "can_edit": true}
	}
	if c.extra != nil && c.extra(w, r, b) {
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a":
		env(200, map[string]any{"id": "org_a", "name": "Acme", "state": "active", "revision": 3, "seats": map[string]any{"used": 1, "limit": nil}, "your_role": "owner", "your_seat": "assigned"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/billing":
		env(200, view())
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_a/billing/profile":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		rev, _ := p["expected_revision"].(float64)
		p["revision"] = rev + 1
		delete(p, "expected_revision")
		c.profile = p
		env(200, view())
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/billing/contacts":
		env(200, map[string]any{"items": view()["contacts"], "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/billing/contacts":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		env(201, map[string]any{"account_id": p["account_id"], "kind": p["kind"], "role": "auditor", "receives": true})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/billing/contacts/acct_bill":
		env(200, map[string]any{"removed": true, "account_id": "acct_bill", "org_id": "org_a"})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgBillingCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func (c *orgBillingCtl) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

func TestOrganizationBillingVerbs(t *testing.T) {
	c := &orgBillingCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	out, errs, code := run("organization", "billing", "show", "org_a")
	if code != 0 || !strings.Contains(out, "none yet") || !strings.Contains(out, "KeepState, Inc., San Francisco, CA") || !strings.Contains(out, "receives nothing now") {
		t.Fatalf("billing show with no details: %d\n%s%s", code, out, errs)
	}
	// the first details: revision 0, the fields given
	out, errs, code = run("organization", "billing", "set", "org_a", "--legal-name", "Acme Widgets GmbH", "--address-line1", "Hauptstrasse 1", "--city", "Berlin",
		"--postal-code", "10115", "--country", "DE", "--tax-id", "DE123456789", "--tax-id-type", "eu_vat")
	sent := c.last("PUT /api/v2/orgs/org_a/billing/profile")
	if code != 0 || !strings.Contains(sent, `"expected_revision":0`) || !strings.Contains(sent, `"legal_name":"Acme Widgets GmbH"`) || !strings.Contains(sent, `"tax_id_type":"eu_vat"`) ||
		!strings.Contains(out, "Hauptstrasse 1, Berlin, 10115, DE") || !strings.Contains(out, "statements already issued do not change") {
		t.Fatalf("billing set: %d %q\n%s%s", code, sent, out, errs)
	}
	// a second change keeps every other field and names the revision read
	out, errs, code = run("organization", "billing", "set", "org_a", "--po-number", "PO-7")
	sent = c.last("PUT /api/v2/orgs/org_a/billing/profile")
	if code != 0 || !strings.Contains(sent, `"expected_revision":1`) || !strings.Contains(sent, `"po_number":"PO-7"`) || !strings.Contains(sent, `"city":"Berlin"`) || !strings.Contains(sent, `"tax_id":"DE123456789"`) {
		t.Fatalf("billing set keeps the rest: %d %q\n%s%s", code, sent, out, errs)
	}
	if _, _, code := run("organization", "billing", "set", "org_a", "--no-tax-id"); code != 0 || !strings.Contains(c.last("PUT"), `"tax_id":""`) || !strings.Contains(c.last("PUT"), `"tax_id_type":""`) {
		t.Errorf("--no-tax-id: %d %q", code, c.last("PUT"))
	}
	// refused before anything is sent
	for _, bad := range [][]string{
		{"organization", "billing", "set", "org_a"},
		{"organization", "billing", "set", "org_a", "--country", "de"},
		{"organization", "billing", "set", "org_a", "--no-tax-id", "--tax-id", "X1"},
		{"organization", "billing", "set", "acct_notanorg", "--city", "Berlin"},
		{"organization", "billing", "show"}, // no context chosen
		{"organization", "billing", "contact", "add", "acct_x", "--organization", "org_a", "--kind", "postcards"},
	} {
		n := c.count()
		if _, _, code := run(bad...); code != exitUsage || c.count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, c.count()-n)
		}
	}
	out, _, code = run("organization", "billing", "contacts", "org_a", "--json")
	if code != 0 || !strings.Contains(out, `"count": 2`) && !strings.Contains(out, `"count":2`) {
		t.Errorf("contacts: %d %s", code, out)
	}
	out, _, code = run("organization", "billing", "contact", "add", "acct_aud", "--organization", "org_a", "--kind", "statements")
	if code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/billing/contacts"), `"kind":"statements"`) || !strings.Contains(out, "receives statement notices") {
		t.Errorf("contact add: %d %s", code, out)
	}
	if out, _, code := run("organization", "billing", "contact", "remove", "acct_bill", "--organization", "org_a"); code != 0 || !strings.Contains(out, "no longer receives") {
		t.Errorf("contact remove: %d %s", code, out)
	}
	// with a context chosen, the organization is the context's
	if out, errs, code := run("context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	if out, errs, code := run("organization", "billing", "show"); code != 0 || !strings.Contains(out, "Billing details of org_a") {
		t.Errorf("billing show in the context: %d %s%s", code, out, errs)
	}
}
