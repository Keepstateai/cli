// The member show, set and remove verbs and leaving, against a fake control
// plane: what each sends, and a removal that runs only with the plan's
// digest confirmed.
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

type orgRemovalCtl struct {
	mu   sync.Mutex
	sent []string
}

func (c *orgRemovalCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.Path+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	member := map[string]any{"account_id": "acct_bob", "role": "member", "seat_state": "assigned", "revision": 3,
		"profile": map[string]any{"display_name": "Bob B.", "work_email": "bob@example.com", "timezone": "Europe/Paris"}}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob":
		env(200, member)
	case r.URL.Path == "/api/v2/orgs/org_a/members":
		env(200, map[string]any{"items": []any{member}, "next_cursor": ""})
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob":
		env(200, member)
	case r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob/removal-plan":
		env(200, map[string]any{"plan_id": "orp_1", "account_id": "acct_bob", "role": "member", "content": "transfer", "transfer_to": "acct_me",
			"plan_digest": "abcdef0123456789abcdef", "steps": []string{"their 1 organization sessions and 1 agents move to acct_me, tagged transferred", "their personal account is not touched"}})
	case r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob/remove":
		env(200, map[string]any{"removed": true, "sessions_transferred": 1, "profile_erase_after": "2026-11-29"})
	case r.URL.Path == "/api/v2/orgs/org_a/leave":
		env(200, map[string]any{"left": true, "profile_erase_after": "2026-11-29"})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgRemovalCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func TestOrganizationRemovalVerbs(t *testing.T) {
	c := &orgRemovalCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	out, _, code := run("member", "show", "acct_bob", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "Bob B.") || !strings.Contains(out, "bob@example.com") || !strings.Contains(out, "Europe/Paris") {
		t.Fatalf("member show: %d %s", code, out)
	}
	out, errs, code := run("member", "set", "acct_bob", "--organization", "org_a", "--display-name", "Bob", "--timezone", "Europe/Rome")
	sent := c.last("PATCH /api/v2/orgs/org_a/members/acct_bob")
	if code != 0 || !strings.Contains(sent, `"display_name":"Bob"`) || !strings.Contains(sent, `"timezone":"Europe/Rome"`) || !strings.Contains(sent, `"expected_revision":3`) {
		t.Fatalf("member set: %d %q\n%s%s", code, sent, out, errs)
	}
	if _, _, code := run("member", "set", "acct_bob", "--organization", "org_a"); code != exitUsage {
		t.Errorf("member set with nothing to set: %d", code)
	}

	// remove: the plan only, unless its digest is confirmed; --yes is not a confirmation
	for _, args := range [][]string{
		{"member", "remove", "acct_bob", "--organization", "org_a", "--content", "transfer"},
		{"member", "remove", "acct_bob", "--organization", "org_a", "--content", "transfer", "--yes"},
	} {
		out, errs, code := run(args...)
		if code != 0 || c.last("POST /api/v2/orgs/org_a/members/acct_bob/remove") != "" || !strings.Contains(out, "NOT removed") || !strings.Contains(errs, "tagged transferred") {
			t.Fatalf("%v: %d\n%s%s", args, code, out, errs)
		}
	}
	if _, _, code := run("member", "remove", "acct_bob", "--organization", "org_a", "--content", "transfer", "--confirm", "ffffffffffff"); code != exitUsage || c.last("POST /api/v2/orgs/org_a/members/acct_bob/remove") != "" {
		t.Errorf("a wrong digest: %d", code)
	}
	c.mu.Lock()
	n := len(c.sent)
	c.mu.Unlock()
	if _, _, code := run("member", "remove", "acct_bob", "--organization", "org_a"); code != exitUsage {
		t.Errorf("remove without a content choice: %d", code)
	}
	c.mu.Lock()
	if len(c.sent) != n {
		t.Errorf("remove without a content choice sent %d requests", len(c.sent)-n)
	}
	c.mu.Unlock()
	out, errs, code = run("member", "remove", "acct_bob", "--organization", "org_a", "--content", "transfer", "--confirm", "abcdef012345")
	if code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/members/acct_bob/remove"), `"plan_id":"orp_1"`) || !strings.Contains(out, "1 organization sessions moved to you") || !strings.Contains(out, "2026-11-29") {
		t.Fatalf("confirmed removal: %d\n%s%s", code, out, errs)
	}

	out, _, code = run("organization", "leave", "org_a")
	if code != 0 || !strings.Contains(out, "You left org_a") || c.last("POST /api/v2/orgs/org_a/leave") == "" {
		t.Fatalf("organization leave: %d %s", code, out)
	}
}
