// The budget verbs and the organization's spend against a fake control
// plane: what each sends (a budget named by id, "organization", an account or
// a team; a change names the revision read; dollars to micro-USD exactly),
// what each prints (unavailable never as $0), and the refusals made before
// anything is sent.
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

type orgBudgetsCtl struct {
	mu   sync.Mutex
	sent []string
	keys map[string]string
}

func (c *orgBudgetsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	c.keys[r.Method+" "+r.URL.Path] = r.Header.Get("Idempotency-Key")
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	orgB := map[string]any{"id": "obud_org", "scope_kind": "org", "scope_id": "org_a", "scope_name": "Acme", "period": "month", "period_key": "2026-11",
		"amount_microusd": 1000000000, "alert_pcts": []int{50, 80, 100}, "at_limit": "pause", "charged_microusd": 12500000, "held_microusd": 500000, "percent": 1, "status": "under", "revision": 2,
		"overcommit": map[string]any{"children_microusd": 1200000000, "children": 2}, "words": "The organization's budget for November 2026: $13.00 of $1000.00 spent or held (1%)"}
	platB := map[string]any{"id": "obud_plat", "scope_kind": "team", "scope_id": "oteam_plat", "scope_name": "Platform", "scope_path": []string{"Engineering", "Platform"}, "period": "month",
		"amount_microusd": 500000000, "alert_pcts": []int{50, 80, 100}, "charged_microusd": nil, "held_microusd": nil, "percent": nil, "status": "unavailable", "revision": 7}
	plat := map[string]any{"id": "oteam_plat", "name": "Platform", "path": []string{"oteam_eng", "oteam_plat"}, "path_names": []string{"Engineering", "Platform"}, "depth": 2, "revision": 5, "members": []any{}}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/budgets":
		env(200, map[string]any{"items": []any{orgB, platB}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/teams":
		env(200, map[string]any{"items": []any{plat}, "next_cursor": ""})
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_a/budgets":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		if p["scope_kind"] == "member" {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_budget_above_lead","type":"ks_budget_above_lead","message":"a team lead sets budgets up to their own team's budget, and the Platform team's is $500.00; set $500.00 or less, or ask an owner, admin or billing member; nothing was changed","retryable":false,"work_started":"no","next_action":""}}`))
			return
		}
		env(200, platB)
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/budgets/obud_plat":
		env(200, platB)
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/budgets/obud_org":
		env(200, map[string]any{"id": "obud_org", "cleared": true})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/budgets/obud_plat/raise-requests":
		env(201, map[string]any{"id": "obrr_1", "budget_id": "obud_plat", "amount_microusd": 800000000, "recipients": []string{"acct_x", "acct_y"}, "words": "Asked the 2 people who may raise the Platform team's budget"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/spend":
		env(200, map[string]any{"period": "2026-11", "note": "n", "items": []any{
			map[string]any{"scope_kind": "org", "scope_id": "org_a", "name": "Acme", "charged_microusd": 12500000, "held_microusd": 500000, "provider_billed_tokens": 42000, "budget_id": "obud_org", "budget_microusd": 1000000000},
			map[string]any{"scope_kind": "member", "scope_id": "acct_bob", "name": "acct_bob", "charged_microusd": nil, "held_microusd": nil, "provider_billed_tokens": 0}}})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgBudgetsCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func TestOrganizationBudgetVerbs(t *testing.T) {
	c := &orgBudgetsCtl{keys: map[string]string{}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	count := func() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

	// list: each budget, its spend, unavailable never as zero, the overcommit
	out, errs, code := run("budget", "list", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "obud_org  the organization: $1000.00 a month; $12.50 spent, $0.50 held, 1%; under") ||
		!strings.Contains(out, "team Engineering/Platform: $500.00 a month; spent unavailable; unavailable") || strings.Contains(out, "$0.00 spent") ||
		!strings.Contains(out, "the 2 team budgets under it add up to $1200.00") || !strings.Contains(out, "shown, not counted") {
		t.Fatalf("budget list: %d\n%s%s", code, out, errs)
	}
	if out, _, code := run("budget", "list", "--organization", "org_a", "--period", "2026-10"); code != 0 || !strings.Contains(c.last("GET /api/v2/orgs/org_a/budgets"), "period=2026-10") {
		t.Errorf("budget list --period: %d %s", code, out)
	}
	// set a team's, by path; the revision read is named; dollars exactly
	out, errs, code = run("budget", "set", "--team", "Engineering/Platform", "--monthly", "512.50", "--alerts", "80,100", "--organization", "org_a")
	put := c.last("PUT /api/v2/orgs/org_a/budgets")
	if code != 0 || !strings.Contains(put, `"scope_id":"oteam_plat"`) || !strings.Contains(put, `"amount_microusd":512500000`) || !strings.Contains(put, `"expected_revision":7`) || !strings.Contains(put, `"alert_pcts":[80,100]`) || !strings.Contains(out, "Changed obud_plat") {
		t.Fatalf("budget set --team: %d %q\n%s%s", code, put, out, errs)
	}
	if c.keys["PUT /api/v2/orgs/org_a/budgets"] == "" {
		t.Error("budget set was sent without an idempotency key")
	}
	// the organization's, which exists: its revision; a new one: 0
	if _, _, code := run("budget", "set", "--organization-wide", "--monthly", "2000", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PUT"), `"scope_kind":"org"`) || !strings.Contains(c.last("PUT"), `"expected_revision":2`) {
		t.Errorf("budget set --organization-wide: %d %q", code, c.last("PUT"))
	}
	// a refusal is the service's words, with the way on
	if _, errs, code := run("budget", "set", "--member", "acct_ada", "--monthly", "600", "--organization", "org_a"); code == 0 || !strings.Contains(errs, "set $500.00 or less") || !strings.Contains(c.last("PUT"), `"expected_revision":0`) {
		t.Errorf("a lead above their team's budget: %d %s", code, errs)
	}
	// show by team name; clear by "organization"; ask by id
	if out, _, code := run("budget", "show", "Platform", "--organization", "org_a"); code != 0 || !strings.Contains(out, "spent unavailable") || !strings.Contains(out, "saved and paused, never killed") {
		t.Errorf("budget show: %d %s", code, out)
	}
	if out, _, code := run("budget", "clear", "organization", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("DELETE"), "expected_revision=2") || !strings.Contains(out, "Cleared the budget of the organization") {
		t.Errorf("budget clear: %d %s %q", code, out, c.last("DELETE"))
	}
	if out, _, code := run("budget", "ask", "obud_plat", "--monthly", "800", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/budgets/obud_plat/raise-requests"), `"amount_microusd":800000000`) || !strings.Contains(out, "Asked the 2 people") {
		t.Errorf("budget ask: %d %s", code, out)
	}
	// spend: unavailable is unavailable, the tokens are shown and not counted
	if out, _, code := run("organization", "spend", "org_a", "--by", "member"); code != 0 || !strings.Contains(c.last("GET /api/v2/orgs/org_a/spend"), "by=member") || !strings.Contains(out, "member acct_bob: unavailable spent, unavailable held") ||
		!strings.Contains(out, "$12.50 spent, $0.50 held; budget $1000.00; 42000 provider-billed tokens") || !strings.Contains(out, "shown, not counted") {
		t.Errorf("organization spend: %d %s", code, out)
	}
	// refused before anything is sent
	for _, bad := range [][]string{
		{"budget", "list"},
		{"budget", "list", "--organization", "org_a", "--period", "2026-13"},
		{"budget", "set", "--monthly", "5", "--organization", "org_a"},
		{"budget", "set", "--team", "Platform", "--member", "acct_x", "--monthly", "5", "--organization", "org_a"},
		{"budget", "set", "--organization-wide", "--monthly", "0", "--organization", "org_a"},
		{"budget", "set", "--organization-wide", "--monthly", "1.005", "--organization", "org_a"},
		{"budget", "set", "--organization-wide", "--monthly", "lots", "--organization", "org_a"},
		{"budget", "set", "--organization-wide", "--monthly", "5", "--alerts", "50,50", "--organization", "org_a"},
		{"budget", "ask", "obud_plat", "--monthly", "0.001", "--organization", "org_a"},
		{"organization", "spend", "org_a", "--by", "project"},
	} {
		n := count()
		if _, _, code := run(bad...); code != exitUsage || count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, count()-n)
		}
	}
	// an unknown budget is refused after reading, before any change
	n := count()
	if _, errs, code := run("budget", "clear", "Sales", "--organization", "org_a"); code != exitUsage || !strings.Contains(errs, "Sales") || count() != n+2 {
		t.Errorf("an unknown budget: %d %s (%d requests)", code, errs, count()-n)
	}
}

func TestPickBudget(t *testing.T) {
	budgets := []customerOrgBudget{
		{ID: "obud_o", ScopeKind: "org", ScopeID: "org_a"},
		{ID: "obud_t", ScopeKind: "team", ScopeID: "oteam_a"},
		{ID: "obud_m", ScopeKind: "member", ScopeID: "acct_m"},
	}
	teams := []customerOrgTeam{{ID: "oteam_a", Name: "Eng", PathNames: []string{"Eng"}}, {ID: "oteam_b", Name: "Ops", PathNames: []string{"Ops"}}}
	for ref, want := range map[string]string{"obud_t": "obud_t", "organization": "obud_o", "org_a": "obud_o", "acct_m": "obud_m", "Eng": "obud_t", "/Eng": "obud_t", "Ops": "", "nope": ""} {
		got, _ := pickBudget(budgets, teams, ref)
		id := ""
		if got != nil {
			id = got.ID
		}
		if id != want {
			t.Errorf("pickBudget(%q) = %q, want %q", ref, id, want)
		}
	}
}
