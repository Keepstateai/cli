// The invitation, role, seat, seat-limit and audit verbs against a fake
// control plane: what each sends (each change names the revision the service
// last answered), what each prints, and the refusals made before anything is
// sent.
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

type orgMembersCtl struct {
	mu    sync.Mutex
	sent  []string          // "METHOD path body"
	keys  map[string]string // method+path -> Idempotency-Key
	audit map[string][]any  // cursor -> items
}

func (c *orgMembersCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.Path+" "+string(b))
	c.keys[r.Method+" "+r.URL.Path] = r.Header.Get("Idempotency-Key")
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	org := map[string]any{"id": "org_a", "name": "Acme", "state": "active", "revision": 7, "seats": map[string]any{"used": 2, "limit": nil}, "your_role": "owner", "your_seat": "assigned"}
	member := func(role, seat string) map[string]any {
		return map[string]any{"account_id": "acct_bob", "role": role, "seat_state": seat, "joined_at": "2026-10-30T00:00:00Z", "revision": 4}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a":
		env(200, org)
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/orgs/org_a":
		org["revision"], org["seats"] = 8, map[string]any{"used": 2, "limit": 10}
		env(200, org)
	case r.URL.Path == "/api/v2/orgs/org_a/members":
		env(200, map[string]any{"items": []any{member("member", "unassigned")}, "next_cursor": ""})
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		role, seat := "member", "unassigned"
		if v, ok := p["role"].(string); ok {
			role = v
		}
		if v, ok := p["seat_state"].(string); ok {
			seat = v
		}
		env(200, member(role, seat))
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/invitations":
		env(201, map[string]any{"invitation": map[string]any{"id": "oinv_1", "email": "ada@example.com", "role": "admin", "assign_seat": true, "state": "open", "expires_at": "2026-11-06T00:00:00Z"},
			"token": "ksi_secret0123", "email_sent": false, "email_note": "the invitation email was not sent (acs not configured); hand the token to the person yourself"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/invitations":
		env(200, map[string]any{"items": []any{
			map[string]any{"id": "oinv_1", "email": "ada@example.com", "role": "admin", "state": "open", "expires_at": "x"},
			map[string]any{"id": "oinv_2", "email": "grace@example.com", "role": "member", "state": "awaiting_confirmation", "expires_at": "x", "pending_account_id": "acct_g", "pending_email": "g@example.net"},
		}, "next_cursor": ""})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/invitations/oinv_1":
		env(200, map[string]any{"id": "oinv_1", "email": "ada@example.com", "state": "revoked"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/invitations/oinv_2/confirm":
		env(200, map[string]any{"account_id": "acct_g", "role": "member", "seat_state": "assigned"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/invitations/accept":
		env(200, map[string]any{"org": map[string]any{"id": "org_a", "name": "Acme"}, "member": map[string]any{"account_id": "acct_me", "role": "billing", "seat_state": "unassigned"}})
	case r.URL.Path == "/api/v2/orgs/org_a/audit":
		cur := r.URL.Query().Get("cursor")
		next := ""
		if cur == "" {
			next = "2"
		}
		env(200, map[string]any{"items": c.audit[cur], "next_cursor": next})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgMembersCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func TestOrganizationMemberVerbs(t *testing.T) {
	ev := func(seq int, prev, dig, action string) map[string]any {
		return map[string]any{"seq": seq, "action": action, "actor_id": "acct_me", "target_kind": "org", "target_id": "org_a", "recorded_at": "t", "prev_digest": prev, "digest": dig}
	}
	c := &orgMembersCtl{keys: map[string]string{}, audit: map[string][]any{
		"":  {ev(1, "", "d1", "org.created"), ev(2, "d1", "d2", "invitation.created")},
		"2": {ev(3, "d2", "d3", "member.joined")},
	}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	count := func() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

	// set: reads the revision, then names it
	out, errs, code := run("organization", "set", "org_a", "--seat-limit", "10")
	if code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a"), `"expected_revision":7`) || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a"), `"seat_limit":10`) || !strings.Contains(out, "2 of 10 used") {
		t.Fatalf("organization set: %d %q\n%s%s", code, c.last("PATCH"), out, errs)
	}
	for _, bad := range [][]string{{"organization", "set", "org_a"}, {"organization", "set", "org_a", "--seat-limit", "3", "--no-seat-limit"}, {"organization", "set", "org_a", "--name", " Acme"}} {
		n := count()
		if _, _, code := run(bad...); code != exitUsage || count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, count()-n)
		}
	}

	// role and seats: the member's revision is read first and named
	out, errs, code = run("member", "role", "acct_bob", "auditor", "--organization", "org_a")
	if code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"), `"expected_revision":4`) || !strings.Contains(out, "role auditor") {
		t.Fatalf("member role: %d\n%s%s", code, out, errs)
	}
	out, _, code = run("seat", "assign", "acct_bob", "--organization", "org_a")
	if code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"), `"seat_state":"assigned"`) || !strings.Contains(out, "seat assigned") {
		t.Fatalf("seat assign: %d %s", code, out)
	}
	out, _, code = run("seat", "release", "acct_bob", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "the organization's work is closed to this member") {
		t.Fatalf("seat release: %d %s", code, out)
	}
	for _, bad := range [][]string{{"member", "role", "acct_bob", "boss", "--organization", "org_a"}, {"seat", "assign", "acct_bob"}, {"member", "invite", "ada@example.com"}} {
		n := count()
		if _, _, code := run(bad...); code != exitUsage || count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, count()-n)
		}
	}
	if _, errs, code := run("seat", "assign", "acct_nobody", "--organization", "org_a"); code == 0 || !strings.Contains(errs, "is not a member") {
		t.Errorf("a seat for a stranger: %d %s", code, errs)
	}

	// invite: the token printed once, the mail's fate said
	out, errs, code = run("member", "invite", "ada@example.com", "--organization", "org_a", "--role", "admin", "--seat")
	sent := c.last("POST /api/v2/orgs/org_a/invitations")
	if code != 0 || !strings.Contains(sent, `"assign_seat":true`) || !strings.Contains(sent, `"role":"admin"`) || !strings.Contains(out, "ks invite accept ksi_secret0123") || !strings.Contains(out, "was not sent") {
		t.Fatalf("member invite: %d %q\n%s%s", code, sent, out, errs)
	}
	if k := c.keys["POST /api/v2/orgs/org_a/invitations"]; k != "" {
		t.Errorf("the invitation was sent under a replay key (%q): a replay would keep its token", k)
	}
	out, _, code = run("invite", "list", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "oinv_2") || !strings.Contains(out, "ks invite confirm oinv_2 --organization org_a --account acct_g") {
		t.Fatalf("invite list: %d %s", code, out)
	}
	if out, _, code := run("invite", "revoke", "oinv_1", "--organization", "org_a"); code != 0 || !strings.Contains(out, "withdrawn") {
		t.Errorf("invite revoke: %d %s", code, out)
	}
	if out, _, code := run("invite", "confirm", "oinv_2", "--organization", "org_a", "--account", "acct_g"); code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/invitations/oinv_2/confirm"), `"account_id":"acct_g"`) || !strings.Contains(out, "acct_g joined") {
		t.Errorf("invite confirm: %d %s", code, out)
	}
	// accept: the token from whatever carries it
	out, errs, code = run("invite", "accept", "  ks invite accept ksi_abc123DEF, then...")
	if code != 0 || !strings.Contains(c.last("POST /api/v2/invitations/accept"), `{"token":"ksi_abc123DEF"}`) || !strings.Contains(out, `You joined "Acme"`) || !strings.Contains(out, "you hold no seat") {
		t.Fatalf("invite accept: %d %q\n%s%s", code, c.last("POST /api/v2/invitations/accept"), out, errs)
	}
	n := count()
	if _, _, code := run("invite", "accept", "no token here"); code != exitUsage || count() != n {
		t.Errorf("accept without a token: %d", code)
	}

	// the audit log: both pages, and whether they chain
	out, _, code = run("organization", "audit", "org_a", "--json")
	var aj struct {
		Data struct {
			Count   int  `json:"count"`
			Chained bool `json:"chained"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &aj) != nil || aj.Data.Count != 3 || !aj.Data.Chained {
		t.Fatalf("organization audit: %d %s", code, out)
	}
	c.mu.Lock()
	c.audit["2"] = []any{ev(3, "dX", "d3", "member.joined")}
	c.mu.Unlock()
	if out, _, code := run("organization", "audit", "org_a"); code != 0 || !strings.Contains(out, "do not chain") {
		t.Errorf("a broken chain is not said: %d %s", code, out)
	}
}
