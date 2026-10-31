// The organization caps verbs against a fake control plane: a change keeps
// the caps it does not name as the owner last set them, names the revision
// read, and puts a cap back to the ceiling with null; refused before
// anything is sent when it would turn a cap off.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationCapsVerbs(t *testing.T) {
	lowered := map[string]any{"daily_cap_microusd": nil, "monthly_cap_microusd": 500000000, "concurrent_calls": nil}
	rev := 3
	c := &orgBillingCtl{}
	c.extra = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		env := func(code int, data any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
		}
		view := func() map[string]any {
			return map[string]any{"org_id": "org_a", "revision": rev, "lowered": lowered,
				"ceiling":  map[string]any{"daily_cap_microusd": 100000000, "monthly_cap_microusd": 1000000000, "concurrent_calls": 8},
				"in_force": map[string]any{"daily_cap_microusd": 100000000, "monthly_cap_microusd": 500000000, "concurrent_calls": 8}, "spent_today_microusd": 1250000}
		}
		if r.URL.Path != "/api/v2/orgs/org_a/managed-caps" {
			return false
		}
		if r.Method == "PUT" {
			var p map[string]any
			_ = json.Unmarshal(body, &p)
			delete(p, "expected_revision")
			lowered, rev = p, rev+1
		}
		env(200, view())
		return true
	}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	out, errs, code := run("organization", "caps", "show", "org_a")
	if code != 0 || !strings.Contains(out, "$100.00") || !strings.Contains(out, "$500.00") || !strings.Contains(out, "no (the ceiling)") || !strings.Contains(out, "$1.25 today") {
		t.Fatalf("caps show: %d\n%s%s", code, out, errs)
	}
	out, errs, code = run("organization", "caps", "set", "org_a", "--daily", "40", "--calls", "4")
	sent := c.last("PUT /api/v2/orgs/org_a/managed-caps")
	if code != 0 || !strings.Contains(sent, `"daily_cap_microusd":40000000`) || !strings.Contains(sent, `"concurrent_calls":4`) || !strings.Contains(sent, `"monthly_cap_microusd":500000000`) || !strings.Contains(sent, `"expected_revision":3`) {
		t.Fatalf("caps set: %d %q\n%s%s", code, sent, out, errs)
	}
	if _, _, code := run("organization", "caps", "set", "org_a", "--monthly-ceiling"); code != 0 || !strings.Contains(c.last("PUT /api/v2/orgs/org_a/managed-caps"), `"monthly_cap_microusd":null`) || !strings.Contains(c.last("PUT"), `"daily_cap_microusd":40000000`) {
		t.Errorf("back to the ceiling: %d %q", code, c.last("PUT"))
	}
	for _, bad := range [][]string{{"organization", "caps", "set", "org_a"}, {"organization", "caps", "set", "org_a", "--calls", "0"}, {"organization", "caps", "set", "org_a", "--daily", "0"},
		{"organization", "caps", "set", "org_a", "--daily", "5", "--daily-ceiling"}} {
		n := c.count()
		if _, _, code := run(bad...); code != exitUsage || c.count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, c.count()-n)
		}
	}
}
