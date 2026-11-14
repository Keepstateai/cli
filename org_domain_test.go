// The organization work-domain verbs and the offer verbs against a fake
// control plane: what each sends (method, path and body), what each prints,
// and the refusals, whose words are the service's own.
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

type orgDomainCtl struct {
	mu      sync.Mutex
	sent    []string
	domain  string
	mode    string
	noOffer bool // there is no offer to join
	off     bool // the work-domain switch is off: every route answers its refusal
}

func (c *orgDomainCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, kind, words string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"code": kind, "type": kind, "message": words, "retryable": false, "work_started": "no", "next_action": ""}})
	}
	view := func() map[string]any {
		return map[string]any{"org_id": "org_a", "domain": c.domain, "mode": c.mode, "created_at": "2026-11-01T00:00:00Z", "pending_requests": 2, "members_without_seat": 1}
	}
	req := func(state string) map[string]any {
		return map[string]any{"id": "dreq_1", "account_id": "acct_ada", "email": "ada@gleam.test", "state": state, "created_at": "2026-11-02T00:00:00Z"}
	}
	if c.off {
		refuse(404, "ks_org_domains_off", "work domains are not turned on for this service yet")
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/domain":
		env(200, view())
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_none/domain":
		env(200, map[string]any{"org_id": "org_none", "domain": "", "mode": ""})
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_a/domain":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		if p["mode"] != "auto" && p["mode"] != "approve" {
			refuse(422, "ks_domain_mode_invalid", "a mode is auto or approve")
			return
		}
		c.mode = p["mode"].(string)
		env(200, view())
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_none/domain":
		refuse(409, "ks_no_domain", "this organization holds no work domain")
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_member/domain":
		refuse(403, "ks_forbidden", "only an owner may change the work domain")
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/domain":
		c.domain, c.mode = "", ""
		env(200, map[string]any{"org_id": "org_a", "domain": "", "mode": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/domain/requests":
		if r.URL.Query().Get("cursor") == "" {
			env(200, map[string]any{"items": []any{req("pending")}, "next_cursor": "2"})
			return
		}
		env(200, map[string]any{"items": []any{map[string]any{"id": "dreq_2", "account_id": "acct_bo", "email": "bo@gleam.test", "state": "pending", "created_at": "2026-11-03T00:00:00Z"}}, "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/domain/requests/dreq_1/approve":
		env(200, req("approved"))
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/domain/requests/dreq_1/decline":
		env(200, req("declined"))
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v2/orgs/org_a/domain/requests/dreq_old/"):
		refuse(409, "ks_request_decided", "this request was already decided")
	case r.Method == "GET" && r.URL.Path == "/api/v2/domain-offer":
		env(200, map[string]any{"state": "offered", "org_id": "org_a", "org_name": "Acme", "domain": "gleam.test", "mode": "auto", "seat_state": "unassigned",
			"headline": "Acme is on gleam.test.", "words": "Your colleagues use Acme. Join it to work with them."})
	case r.Method == "POST" && r.URL.Path == "/api/v2/domain-offer/join" && c.noOffer:
		refuse(409, "ks_no_domain_offer", "there is no offer to join")
	case r.Method == "POST" && r.URL.Path == "/api/v2/domain-offer/join":
		env(200, map[string]any{"state": "joined", "org_id": "org_a", "org_name": "Acme", "headline": "You joined Acme.", "words": "You are a member."})
	case r.Method == "POST" && r.URL.Path == "/api/v2/domain-offer/dismiss":
		env(200, map[string]any{"state": "declined"})
	default:
		refuse(404, "ks_not_found", "no such resource")
	}
}

func (c *orgDomainCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func (c *orgDomainCtl) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

func TestOrganizationDomainVerbs(t *testing.T) {
	c := &orgDomainCtl{domain: "gleam.test", mode: "approve"}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// show: the domain, its mode, and the counts an owner or admin sees
	out, errs, code := run("organization", "domain", "org_a")
	if code != 0 || c.last("GET /api/v2/orgs/org_a/domain ") == "" || !strings.Contains(out, "work domain gleam.test") || !strings.Contains(out, "approve: an owner or admin approves") ||
		!strings.Contains(out, "waiting requests 2") || !strings.Contains(out, "members without a seat 1") {
		t.Fatalf("domain show: %d\n%s%s", code, out, errs)
	}
	out, _, code = run("organization", "domain", "org_a", "--json")
	var dj struct {
		Data struct {
			Domain          string `json:"domain"`
			Mode            string `json:"mode"`
			PendingRequests int    `json:"pending_requests"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &dj) != nil || dj.Data.Domain != "gleam.test" || dj.Data.Mode != "approve" || dj.Data.PendingRequests != 2 {
		t.Errorf("domain show --json: %d %s", code, out)
	}
	if out, errs, code := run("organization", "domain", "org_none"); code != 0 || !strings.Contains(out, "holds no work domain") {
		t.Errorf("an organization with no domain: %d\n%s%s", code, out, errs)
	}

	// set: the mode, as given
	out, errs, code = run("organization", "domain", "set", "org_a", "--mode", "auto")
	if code != 0 || c.last("PUT /api/v2/orgs/org_a/domain ") != `PUT /api/v2/orgs/org_a/domain {"mode":"auto"}` || !strings.Contains(out, "auto: people who sign up") {
		t.Fatalf("domain set: %d %q\n%s%s", code, c.last("PUT"), out, errs)
	}
	for _, bad := range [][]string{{"organization", "domain", "set", "org_a"}, {"organization", "domain", "set", "org_a", "--mode", "open"}, {"organization", "domain", "set", "acct_x", "--mode", "auto"},
		{"organization", "domain", "approve", "org_a"}} {
		n := c.count()
		if _, _, code := run(bad...); code != exitUsage || c.count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, c.count()-n)
		}
	}
	// refusals: the service's words are printed as they come
	if _, errs, code := run("organization", "domain", "set", "org_none", "--mode", "auto"); code == 0 || !strings.Contains(errs, "this organization holds no work domain") {
		t.Errorf("no domain: %d %s", code, errs)
	}
	if _, errs, code := run("organization", "domain", "set", "org_member", "--mode", "auto"); code == 0 || !strings.Contains(errs, "only an owner may change the work domain") {
		t.Errorf("not an owner: %d %s", code, errs)
	}

	// remove
	out, errs, code = run("organization", "domain", "remove", "org_a")
	if code != 0 || c.last("DELETE /api/v2/orgs/org_a/domain ") == "" || !strings.Contains(out, "no longer holds a work domain") {
		t.Fatalf("domain remove: %d\n%s%s", code, out, errs)
	}

	// requests: both pages
	out, errs, code = run("organization", "domain", "requests", "org_a")
	if code != 0 || !strings.Contains(out, "ada@gleam.test") || !strings.Contains(out, "bo@gleam.test") || c.last("GET /api/v2/orgs/org_a/domain/requests?cursor=2") == "" {
		t.Fatalf("domain requests: %d\n%s%s", code, out, errs)
	}
	out, _, code = run("organization", "domain", "requests", "org_a", "--json")
	var rj struct {
		Data struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &rj) != nil || rj.Data.Count != 2 {
		t.Errorf("domain requests --json: %d %s", code, out)
	}

	// approve and decline: no body, the row's new state
	out, errs, code = run("organization", "domain", "approve", "org_a", "dreq_1")
	if code != 0 || c.last("POST /api/v2/orgs/org_a/domain/requests/dreq_1/approve") != "POST /api/v2/orgs/org_a/domain/requests/dreq_1/approve " || !strings.Contains(out, "ada@gleam.test: approved") {
		t.Fatalf("domain approve: %d %q\n%s%s", code, c.last("POST"), out, errs)
	}
	out, errs, code = run("organization", "domain", "decline", "org_a", "dreq_1")
	if code != 0 || c.last("POST /api/v2/orgs/org_a/domain/requests/dreq_1/decline") == "" || !strings.Contains(out, "ada@gleam.test: declined") || strings.Contains(out, "join the organization") {
		t.Fatalf("domain decline: %d\n%s%s", code, out, errs)
	}
	if _, errs, code := run("organization", "domain", "approve", "org_a", "dreq_old"); code == 0 || !strings.Contains(errs, "this request was already decided") {
		t.Errorf("a decided request: %d %s", code, errs)
	}

	// the switch off: the service's own words
	c.mu.Lock()
	c.off = true
	c.mu.Unlock()
	if _, errs, code := run("organization", "domain", "org_a"); code == 0 || !strings.Contains(errs, "work domains are not turned on for this service yet") {
		t.Errorf("switch off: %d %s", code, errs)
	}
}

func TestOrganizationOfferVerbs(t *testing.T) {
	c := &orgDomainCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// show: the service's headline and words, then how to answer
	out, errs, code := run("organization", "offer")
	if code != 0 || c.last("GET /api/v2/domain-offer ") == "" || !strings.Contains(out, "Acme is on gleam.test.") || !strings.Contains(out, "Join it to work with them.") || !strings.Contains(out, "ks organization offer join") {
		t.Fatalf("offer: %d\n%s%s", code, out, errs)
	}
	out, errs, code = run("organization", "offer", "join")
	if code != 0 || c.last("POST /api/v2/domain-offer/join") != "POST /api/v2/domain-offer/join " || !strings.Contains(out, "You joined Acme.") || strings.Contains(out, "ks organization offer join") {
		t.Fatalf("offer join: %d %q\n%s%s", code, c.last("POST"), out, errs)
	}
	out, errs, code = run("organization", "offer", "dismiss")
	if code != 0 || c.last("POST /api/v2/domain-offer/dismiss") != "POST /api/v2/domain-offer/dismiss " || !strings.Contains(out, "declined") {
		t.Fatalf("offer dismiss: %d %q\n%s%s", code, c.last("POST"), out, errs)
	}
	if out, _, code := run("organization", "offer", "--json"); code != 0 || !strings.Contains(out, `"state": "offered"`) && !strings.Contains(out, `"state":"offered"`) {
		t.Errorf("offer --json: %d %s", code, out)
	}
	// a refusal prints the service's words
	c.mu.Lock()
	c.noOffer = true
	c.mu.Unlock()
	if _, errs, code := run("organization", "offer", "join"); code == 0 || !strings.Contains(errs, "there is no offer to join") {
		t.Errorf("no offer: %d %s", code, errs)
	}
	c.mu.Lock()
	c.off = true
	c.mu.Unlock()
	if _, errs, code := run("organization", "offer", "join"); code == 0 || !strings.Contains(errs, "work domains are not turned on for this service yet") {
		t.Errorf("switch off: %d %s", code, errs)
	}
}
