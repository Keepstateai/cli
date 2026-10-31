// ks organization buy and ks organization credits against a fake control
// plane: the preview before anything is sent, the checkout only with --yes,
// the refused tax id's two ways on, and the credit read back.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationBuyAndCredits(t *testing.T) {
	refuseTax := false
	c := &orgBillingCtl{profile: map[string]any{"legal_name": "Acme Widgets GmbH", "address_line1": "Hauptstrasse 1", "city": "Berlin", "postal_code": "10115", "country": "DE", "tax_id": "DE123456789", "tax_id_type": "eu_vat", "revision": 2}}
	c.extra = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		env := func(code int, data any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/billing/checkout":
			if refuseTax && !strings.Contains(string(body), "without_tax_id") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(422)
				_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_tax_id_refused","type":"ks_tax_id_refused","message":"Polar did not accept this VAT number (DE123456789). Check it, or continue without it (Polar will charge VAT). Nothing was charged","retryable":false,"work_started":"no","next_action":""}}`))
				return true
			}
			env(201, map[string]any{"checkout_id": "co_1", "url": "https://polar.test/co_1", "amount_micros": 50000000, "org_id": "org_a", "org_name": "Acme",
				"note": "You are buying for Acme. The receipt from Polar will name Acme Widgets GmbH, VAT id DE123456789. The credit is the organization's, never a member's."})
			return true
		case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/credits":
			env(200, map[string]any{"org_id": "org_a", "balance_micros": 50000000, "purchased_micros": 50000000, "has_bought": true,
				"entries": []any{map[string]any{"seq": 1, "ts": "t1", "kind": "grant", "amount_micros": 50000000, "ref": "polar:ord_1", "balance_after_micros": 50000000}},
				"orders":  []any{map[string]any{"checkout_id": "co_1", "order_id": "ord_1", "amount_micros": 50000000, "status": "paid", "po_number": "PO-9", "created_at": "t0"}}})
			return true
		}
		return false
	}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// the preview: who, what the receipt names; nothing sent to buy
	out, errs, code := run("organization", "buy", "org_a", "--amount", "50")
	if code != 0 || !strings.Contains(out, "You are buying $50.00 of credit for the organization org_a. The receipt from Polar will name Acme Widgets GmbH, VAT id DE123456789.") ||
		!strings.Contains(out, "Nothing was bought") || c.last("POST /api/v2/orgs/org_a/billing/checkout") != "" {
		t.Fatalf("buy preview: %d\n%s%s", code, out, errs)
	}
	out, errs, code = run("organization", "buy", "org_a", "--amount", "50", "--po-number", "PO-9", "--yes")
	sent := c.last("POST /api/v2/orgs/org_a/billing/checkout")
	if code != 0 || !strings.Contains(sent, `"amount_micros":50000000`) || !strings.Contains(sent, `"po_number":"PO-9"`) || strings.Contains(sent, "without_tax_id") ||
		!strings.Contains(out, "https://polar.test/co_1") || !strings.Contains(out, "never a member's") {
		t.Fatalf("buy: %d %q\n%s%s", code, sent, out, errs)
	}
	for _, bad := range [][]string{{"organization", "buy", "org_a"}, {"organization", "buy", "org_a", "--amount", "0.5"}, {"organization", "buy", "org_a", "--amount", "50.005"}, {"organization", "buy", "--amount", "50"}} {
		n := c.count()
		if _, _, code := run(bad...); code != exitUsage || c.count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, c.count()-n)
		}
	}
	// a tax id Polar refused: both ways on said; without it, sent as asked
	c.mu.Lock()
	refuseTax = true
	c.mu.Unlock()
	if _, errs, code := run("organization", "buy", "org_a", "--amount", "50", "--yes"); code != exitConflict || !strings.Contains(errs, "Two ways on") || !strings.Contains(errs, "--without-tax-id") {
		t.Errorf("a refused tax id: %d %s", code, errs)
	}
	if out, _, code := run("organization", "buy", "org_a", "--amount", "50", "--without-tax-id"); code != 0 || !strings.Contains(out, "without its VAT id (Polar will charge VAT)") {
		t.Errorf("the preview without the tax id: %d %s", code, out)
	}
	if _, _, code := run("organization", "buy", "org_a", "--amount", "50", "--without-tax-id", "--yes"); code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/billing/checkout"), `"without_tax_id":true`) {
		t.Errorf("buy without the tax id: %d %q", code, c.last("POST"))
	}
	out, _, code = run("organization", "credits", "org_a")
	if code != 0 || !strings.Contains(out, "The organization org_a's credit: $50.00") || !strings.Contains(out, "PO PO-9") || !strings.Contains(out, "grant") {
		t.Errorf("credits: %d %s", code, out)
	}
}
