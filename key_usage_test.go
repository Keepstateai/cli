// ks key usage: what each key has been used for. The command prints the
// service's figures as the service labels them: a charge on KeepState's key,
// an estimate (never a charge) on your own key, tokens only on a Claude plan,
// and the two kinds of money are never added into one figure. In an
// organization's context every request names it, and a refusal says whose keys
// these are.
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

type usageCtl struct {
	mu   sync.Mutex
	sent []string
}

func (c *usageCtl) all() string { c.mu.Lock(); defer c.mu.Unlock(); return strings.Join(c.sent, "\n") }
func (c *usageCtl) reset()      { c.mu.Lock(); defer c.mu.Unlock(); c.sent = nil }

const usageDocJSON = `{"period":{"name":"month","label":"This month (UTC)"},"context":"personal","observed_at":"x","notes":["An estimate is the tokens priced at the provider's list price from KeepState's rate book. It is not a bill."],
 "rows":[
  {"key_id":"vlt_prod","kind":"api_key","provider":"anthropic","alias":"prod","last4":"ABCD","label":"anthropic · prod","calls":4,"input_tokens":1000000,"output_tokens":100000,"cache_creation_tokens":50000,"cache_read_tokens":200000,"spend":{"kind":"estimate","micros":3220000,"words":"Estimate at list price"}},
  {"key_id":"vlt_plan","kind":"claude_plan","provider":"claude_plan","last4":"PPPP","label":"Claude plan","calls":3,"input_tokens":7000,"output_tokens":900,"cache_creation_tokens":0,"cache_read_tokens":30000,"spend":{"kind":"plan_covered","words":"Covered by the Claude plan"}},
  {"kind":"keepstate","label":"KeepState’s key","calls":5,"input_tokens":300000,"output_tokens":60000,"cache_creation_tokens":0,"cache_read_tokens":0,"spend":{"kind":"charged","micros":1437000,"words":"Charged by KeepState"}}],
 "total":{"calls":12,"input_tokens":1307000,"output_tokens":160900,"cache_creation_tokens":50000,"cache_read_tokens":230000,
  "charged":{"kind":"charged","micros":1437000,"words":"Charged by KeepState"},
  "estimate":{"kind":"estimate","micros":3220000,"words":"Estimate at list price"}}}`

func (c *usageCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	org := r.URL.Query().Get("context")
	switch {
	case r.URL.Path == "/api/capabilities":
		_, _ = w.Write([]byte(`{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.4","capabilities":[{"id":"keys.inventory","availability":"available","summary":"s","surface":"api"},{"id":"keys.usage","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`))
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a":
		env(200, map[string]any{"id": "org_a", "name": "Acme", "state": "active", "revision": 3, "seats": map[string]any{"used": 1, "limit": nil}, "your_role": "member", "your_seat": "assigned"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_b":
		env(200, map[string]any{"id": "org_b", "name": "Beta", "state": "active", "revision": 3, "seats": map[string]any{"used": 1, "limit": nil}, "your_role": "admin", "your_seat": "assigned"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys" && org == "":
		env(200, map[string]any{"items": []any{map[string]any{"id": "vlt_prod", "provider": "anthropic", "alias": "prod", "last4": "ABCD", "enabled": true, "revision": 1}}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys" && org == "org_b":
		env(200, map[string]any{"items": []any{map[string]any{"id": "vlt_org", "provider": "anthropic", "alias": "", "last4": "WXYZ", "enabled": true, "revision": 1}}, "next_cursor": ""})
	case r.Method == "GET" && (r.URL.Path == "/api/v2/keys/usage" || r.URL.Path == "/api/v2/keys/vlt_prod/usage" || r.URL.Path == "/api/v2/keys/vlt_org/usage") && (org == "" || org == "org_b"):
		var d map[string]any
		_ = json.Unmarshal([]byte(usageDocJSON), &d)
		if p := r.URL.Query().Get("period"); p == "30d" {
			d["period"] = map[string]any{"name": "30d", "label": "Last 30 days"}
		}
		env(200, d)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/usage") && org == "org_a":
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_forbidden","type":"ks_forbidden","message":"your role does not reach this","retryable":false,"work_started":"no","next_action":""}}`))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func TestKeyUsageSaysWhatEachFigureIs(t *testing.T) {
	c := &usageCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	out, errs, code := run("key", "usage")
	if code != 0 {
		t.Fatalf("key usage: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"Usage by key", "This month (UTC)", "anthropic prod …ABCD", "1,000,000", "100,000", "250,000", "Total", "(estimate)", "$3.22", "$1.43 charged", "covered by the plan", "It is not a bill"} {
		if !strings.Contains(out, want) {
			t.Errorf("the screen lacks %q:\n%s", want, out)
		}
	}
	// an estimate is never printed as a charge; a charge is never an estimate
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "$3.22") && strings.Contains(l, "charged") && !strings.Contains(l, "$1.43") {
			t.Errorf("an estimate is printed as a charge: %s", l)
		}
		if strings.Contains(l, "$1.43") && strings.Contains(l, "estimate") && !strings.Contains(l, "$3.22") {
			t.Errorf("a charge is printed as an estimate: %s", l)
		}
	}
	if strings.Contains(out, "$4.65") {
		t.Errorf("the two kinds of money were added into one figure:\n%s", out)
	}
	if !strings.Contains(c.all(), "GET /api/v2/keys/usage ") || strings.Contains(c.all(), "context=") {
		t.Errorf("the personal request: %s", c.all())
	}

	// one key, by alias, over 30 days
	c.reset()
	out, errs, code = run("key", "usage", "prod", "--period", "30d")
	if code != 0 || !strings.Contains(out, "Last 30 days") || !strings.Contains(c.all(), "GET /api/v2/keys/vlt_prod/usage?period=30d") {
		t.Fatalf("one key: %d\n%s%s\n%s", code, out, errs, c.all())
	}
	// a period that is none is refused before anything is sent
	c.reset()
	if _, errs, code := run("key", "usage", "--period", "forever"); code == 0 || !strings.Contains(errs, "month, 30d or all") || strings.Contains(c.all(), "/usage") {
		t.Errorf("a bad period: %d %s\n%s", code, errs, c.all())
	}
	// the JSON is the service's document
	out, _, code = run("key", "usage", "--json")
	if code != 0 || !strings.Contains(out, `"kind":"estimate"`) || !strings.Contains(out, `"kind":"charged"`) || !strings.Contains(out, `"kind":"plan_covered"`) {
		t.Errorf("json: %d %s", code, out)
	}
}

func TestKeyUsageInAnOrganizationsContext(t *testing.T) {
	c := &usageCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// an admin's organization: the request names the context
	if _, errs, code := run("context", "use", "org_b"); code != 0 {
		t.Fatalf("context use: %d %s", code, errs)
	}
	c.reset()
	out, errs, code := run("key", "usage")
	if code != 0 || !strings.Contains(c.all(), "GET /api/v2/keys/usage?context=org_b") || !strings.Contains(out, "of the organization org_b") {
		t.Fatalf("the organization's usage: %d\n%s%s\n%s", code, out, errs, c.all())
	}
	// a key's id the inventory does not list is asked for as it is
	c.reset()
	if _, errs, code := run("key", "usage", "vlt_orgplan"); !strings.Contains(c.all(), "GET /api/v2/keys/vlt_orgplan/usage?context=org_b") {
		t.Errorf("an id the inventory does not list: %d %s\n%s", code, errs, c.all())
	}
	// every request in this context named it
	for _, l := range strings.Split(c.all(), "\n") {
		if strings.Contains(l, "/api/v2/keys") && !strings.Contains(l, "context=org_b") {
			t.Errorf("a request in the organization's context did not name it: %s", l)
		}
	}
	// without the role the refusal says whose keys these are, and shows no figure
	if _, errs, code := run("context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s", code, errs)
	}
	out, errs, code = run("key", "usage")
	if code == 0 || !strings.Contains(errs, "managed by its owners and admins") || strings.Contains(out, "1,000,000") {
		t.Errorf("a member: %d\nout %s\nerr %s", code, out, errs)
	}
}
