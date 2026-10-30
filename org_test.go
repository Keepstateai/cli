// The organization verbs against a fake control plane: what each sends, what
// each prints, how the lists follow their pages, and the refusals: a name the
// service would refuse is never sent, a member list needs its organization,
// the service's own refusal reads in its words, and a control plane without
// organizations says so.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgCtl struct {
	mu       sync.Mutex
	creates  []map[string]any
	keys     []string
	paths    []string
	off      bool
	bare404  bool
	orgPages map[string][]any // cursor -> items
	memPages map[string][]any
}

func (c *orgCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.Method+" "+r.URL.RequestURI())
	if c.bare404 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if c.off {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_organizations_off","type":"ks_organizations_off","message":"organizations are not switched on for this service yet; your personal account works as before","retryable":false,"work_started":"no","next_action":""}}`))
		return
	}
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	org := map[string]any{"id": "org_a", "name": "Acme Inc.", "state": "active", "created_by": "acct_me", "created_at": "2026-10-30T00:00:00Z",
		"updated_at": "2026-10-30T00:00:00Z", "revision": 1, "seats": map[string]any{"used": 1, "limit": nil}, "your_role": "owner", "your_seat": "assigned"}
	page := func(pages map[string][]any) {
		cur := r.URL.Query().Get("cursor")
		next := ""
		if cur == "" && pages["2"] != nil {
			next = "2"
		}
		env(200, map[string]any{"items": pages[cur], "next_cursor": next, "observed_at": "x"})
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.creates = append(c.creates, b)
		c.keys = append(c.keys, r.Header.Get("Idempotency-Key"))
		org["name"] = b["name"]
		env(201, org)
	case r.URL.Path == "/api/v2/orgs":
		page(c.orgPages)
	case r.URL.Path == "/api/v2/orgs/org_b":
		lim := 5
		env(200, map[string]any{"id": "org_b", "name": "Beta", "state": "active", "revision": 3, "created_at": "2026-10-30T00:00:00Z",
			"seats": map[string]any{"used": 5, "limit": lim}, "your_role": "member", "your_seat": "unassigned"})
	case r.URL.Path == "/api/v2/orgs/org_a/members":
		page(c.memPages)
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func TestOrganizationVerbs(t *testing.T) {
	c := &orgCtl{
		orgPages: map[string][]any{
			"":  {map[string]any{"id": "org_a", "name": "Acme Inc.", "your_role": "owner", "your_seat": "assigned", "seats": map[string]any{"used": 1, "limit": nil}}},
			"2": {map[string]any{"id": "org_b", "name": "Beta", "your_role": "member", "your_seat": "unassigned", "seats": map[string]any{"used": 5, "limit": 5}}},
		},
		memPages: map[string][]any{
			"":  {map[string]any{"account_id": "acct_me", "role": "owner", "seat_state": "assigned", "joined_at": "2026-10-30T00:00:00Z", "you": true}},
			"2": {map[string]any{"account_id": "acct_them", "role": "billing", "seat_state": "unassigned", "joined_at": "2026-10-30T01:00:00Z"}},
		},
	}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()

	// create: the name is sent as given, once, under an idempotency key
	out, errs, code := auditExec(t, bin, cfg, dir, env, "organization", "create", "Acme Inc.")
	if code != 0 || len(c.creates) != 1 || c.creates[0]["name"] != "Acme Inc." || c.keys[0] == "" {
		t.Fatalf("create: %d %v %v\n%s%s", code, c.creates, c.keys, out, errs)
	}
	for _, want := range []string{`Organization "Acme Inc." created: org_a`, "You are its owner and hold its first seat", "Nothing of your personal account moved into it", "Seats: 1 used, no limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("create says %q, want %q", out, want)
		}
	}
	// a name the service would refuse is never sent
	for _, bad := range []string{" Acme", "Acme ", strings.Repeat("a", 81), "Acme\nInc"} {
		n := len(c.paths)
		if _, errs, code := auditExec(t, bin, cfg, dir, env, "organization", "create", bad); code != exitUsage || len(c.paths) != n || !strings.Contains(errs, "nothing was created") {
			t.Errorf("name %q: exit %d, %d requests\n%s", bad, code, len(c.paths)-n, errs)
		}
	}

	// list: both pages, each organization with your role and seat
	out, errs, code = auditExec(t, bin, cfg, dir, env, "organization", "list")
	if code != 0 || !strings.Contains(out, "org_a") || !strings.Contains(out, "org_b") || !strings.Contains(out, "5 of 5 used") || !strings.Contains(out, "unassigned") {
		t.Fatalf("list: %d\n%s%s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "organization", "list", "--json")
	var lj struct {
		Data struct {
			Count    int  `json:"count"`
			Complete bool `json:"complete"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &lj) != nil || lj.Data.Count != 2 || !lj.Data.Complete {
		t.Fatalf("list --json: %d %s", code, out)
	}

	// show: a member without a seat is told what that means
	out, errs, code = auditExec(t, bin, cfg, dir, env, "organization", "show", "org_b")
	if code != 0 || !strings.Contains(out, "5 of 5 used") || !strings.Contains(out, "you hold no seat, so the organization's work is closed to you") {
		t.Fatalf("show: %d\n%s%s", code, out, errs)
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "organization", "show", "org_nope"); code == 0 || !strings.Contains(errs, "no such resource") {
		t.Errorf("show of an organization not yours: %d %s", code, errs)
	}

	// members: needs the organization, follows the pages, marks you
	n := len(c.paths)
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "member", "list"); code != exitUsage || len(c.paths) != n || !strings.Contains(errs, "--organization") {
		t.Errorf("member list without an organization: %d, %d requests\n%s", code, len(c.paths)-n, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "member", "list", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "acct_me (you)") || !strings.Contains(out, "acct_them") || !strings.Contains(out, "billing") {
		t.Fatalf("member list: %d\n%s%s", code, out, errs)
	}

	// the service's refusal, in its words
	c.mu.Lock()
	c.off = true
	c.mu.Unlock()
	for _, args := range [][]string{{"organization", "list"}, {"organization", "create", "Acme"}, {"member", "list", "--organization", "org_a"}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, args...); code == 0 || !strings.Contains(errs, "organizations are not switched on for this service yet") {
			t.Errorf("%v with the service's switch off: %d %s", args, code, errs)
		}
	}
	// a control plane that predates organizations
	c.mu.Lock()
	c.off, c.bare404 = false, true
	c.mu.Unlock()
	for _, args := range [][]string{{"organization", "list"}, {"organization", "show", "org_a"}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, args...); code == 0 || !strings.Contains(errs, "this control plane does not serve organizations") {
			t.Errorf("%v on a control plane without organizations: %d %s", args, code, errs)
		}
	}
}
