// Adviser connections on the command line: a connection is granted only
// after BOTH ends have been shown by session and agent and the person has
// confirmed the adviser by name; --yes never confirms one, and --no-input
// without --confirm connects nothing.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type adviserCtl struct {
	mu     sync.Mutex
	posts  []map[string]any
	grants []map[string]any
	calls  []string
	// capability is agent.workspace's availability as this fake publishes
	// it ("" reads available); forbid answers the grant route as a
	// control plane does for a person without the operator role on an end
	capability string
	forbid     bool
	keys       []string // Idempotency-Key of each DELETE
}

func (c *adviserCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	sess := func(id, short, name, rec string) map[string]any {
		return map[string]any{"id": id, "short_id": short, "name": name, "runtime_state": "running", "record_id": rec,
			"agent_activity": "ready", "task_state": "none", "key_alias": "unbound", "observed_at": "x", "last_activity_at": "x",
			"created_at": "x", "image": "claude", "budget_tokens": 1, "execution_epoch": 1}
	}
	agent := func(id, sessRec string) map[string]any {
		return map[string]any{"id": id, "session_id": sessRec, "name": "main", "is_primary": true, "activity": "ready"}
	}
	q := r.URL.Query()
	c.mu.Lock()
	avail, forbid := c.capability, c.forbid
	if r.Method == "DELETE" {
		c.keys = append(c.keys, r.Header.Get("Idempotency-Key"))
	}
	c.mu.Unlock()
	if avail == "" {
		avail = "available"
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`, avail)
	case forbid && r.Method == "POST" && r.URL.Path == "/api/v2/adviser-grants":
		w.WriteHeader(403)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_forbidden","type":"ks_forbidden","message":"this needs the operator role on session session_rev","work_started":"no"}}`)
	case r.URL.Path == "/api/v2/sessions" && q.Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{
			sess("fleetsrc00000000000000000000aaaa", "fleetsrc0000", "checkout", "session_src"),
			sess("fleetrev00000000000000000000bbbb", "fleetrev0000", "reviewer", "session_rev"),
			sess("fleetbi100000000000000000000cccc", "fleetbi10000", "billing", "session_bi1"),
			sess("fleetbi200000000000000000000dddd", "fleetbi20000", "billing", "session_bi2"),
		}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		switch q.Get("session_id") {
		case "session_src":
			env(200, map[string]any{"items": []any{agent("agent_src", "session_src")}})
		case "session_rev":
			env(200, map[string]any{"items": []any{agent("agent_rev", "session_rev")}})
		default:
			env(200, map[string]any{"items": []any{}})
		}
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents/agent_rev":
		env(200, agent("agent_rev", "session_rev"))
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_src":
		env(200, map[string]any{"id": "session_src", "name": "checkout", "project_id": "proj_1"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_rev":
		env(200, map[string]any{"id": "session_rev", "name": "reviewer", "project_id": "proj_1"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/adviser-grants":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.posts = append(c.posts, body)
		c.mu.Unlock()
		env(201, map[string]any{"id": "grant_0001", "source_agent_id": body["source_agent_id"], "target_agent_id": body["target_agent_id"],
			"scope": "advice", "expires_at": "2026-09-27T00:00:00Z", "grant_revision": 1, "approved_by": "acct_x", "live": true,
			"limits": body["limits"], "context_policy": "question_only",
			"source_agent_name": "main", "source_session_name": "checkout", "target_agent_name": "main", "target_session_name": "reviewer"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/adviser-grants":
		c.mu.Lock()
		defer c.mu.Unlock()
		env(200, map[string]any{"items": c.grants, "next_cursor": ""})
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/adviser-grants/grant_0001":
		env(200, map[string]any{"id": "grant_0001", "source_agent_id": "agent_src", "target_agent_id": "agent_rev", "scope": "advice",
			"expires_at": "2026-09-27T00:00:00Z", "grant_revision": 1, "revoked_at": "2026-09-26T12:00:00Z", "live": false,
			"source_agent_name": "main", "source_session_name": "checkout", "target_agent_name": "main", "target_session_name": "reviewer"})
	default:
		env(404, nil)
	}
}

func (c *adviserCtl) postCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

func TestAConnectionIsGrantedOnlyAfterBothEndsAreShownAndTheAdviserIsConfirmed(t *testing.T) {
	c := &adviserCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// no confirmation at all (stdin is not a terminal here): nothing connected,
	// exit 2, the flag named -- and BOTH ends were shown first
	out, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000")
	if code != exitUsage || !strings.Contains(errs, "--confirm reviewer/main") || c.postCount() != 0 {
		t.Fatalf("unconfirmed connect: %d out=%q err=%q posts=%d", code, out, errs, c.postCount())
	}
	for _, want := range []string{"checkout/main", "reviewer/main", "session session_rev", "project proj_1", "question", "16,384", "expires", "nothing was connected"} {
		if !strings.Contains(errs, want) && !strings.Contains(out, want) {
			t.Errorf("the confirmation does not show %q:\n%s", want, errs)
		}
	}
	// --yes does NOT confirm a connection
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000", "--yes"); code != exitUsage || c.postCount() != 0 {
		t.Fatalf("--yes confirmed a connection: %d %s", code, errs)
	}
	// --no-input without --confirm: exit 2, naming the flag
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000", "--no-input"); code != exitUsage || !strings.Contains(errs, "--no-input") || !strings.Contains(errs, "--confirm") || c.postCount() != 0 {
		t.Fatalf("--no-input: %d %s", code, errs)
	}
	// a confirmation naming a DIFFERENT agent than the one shown: refused
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000", "--confirm", "checkout/main"); code != exitUsage || !strings.Contains(errs, "does not name the adviser shown") || c.postCount() != 0 {
		t.Fatalf("mismatched confirmation: %d %s", code, errs)
	}
	// a session name two sessions share is refused, listing both
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "billing/main", "--session", "fleetsrc0000", "--confirm", "billing/main"); code != exitUsage || !strings.Contains(errs, "fleetbi10000") || !strings.Contains(errs, "fleetbi20000") || c.postCount() != 0 {
		t.Fatalf("ambiguous adviser: %d %s", code, errs)
	}

	// confirmed: ONE grant, with the limits and the question-only policy; the
	// --json stdout is exactly one envelope and the display went to stderr
	out, errs, code = auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000",
		"--confirm", "reviewer/main", "--max-consultations", "3", "--expires", "2h", "--json")
	if code != 0 || c.postCount() != 1 {
		t.Fatalf("confirmed connect: %d out=%s err=%s", code, out, errs)
	}
	var envOut map[string]any
	if err := json.Unmarshal([]byte(out), &envOut); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	d := envOut["data"].(map[string]any)
	if d["confirmed"] != true || d["grant"].(map[string]any)["id"] != "grant_0001" || d["adviser"].(map[string]any)["session_name"] != "reviewer" {
		t.Fatalf("the receipt: %v", d)
	}
	if !strings.Contains(errs, "reviewer/main") {
		t.Fatalf("the display did not reach stderr under --json: %s", errs)
	}
	p := c.posts[0]
	lim, _ := p["limits"].(map[string]any)
	if p["source_agent_id"] != "agent_src" || p["target_agent_id"] != "agent_rev" || p["context_policy"] != "question_only" ||
		lim["max_consultations"] != float64(3) || lim["max_tokens_per_consultation"] != float64(16384) || p["expires_seconds"] != float64(7200) {
		t.Fatalf("the grant sent: %v", p)
	}
	// by id as well
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "agent_rev", "--session", "fleetsrc0000", "--confirm", "agent_rev"); code != 0 || c.postCount() != 2 {
		t.Fatalf("connect by id: %d %s", code, errs)
	}
}

func TestAdviserListShowsLiveAndRevokedAndDisconnectSaysWhatStops(t *testing.T) {
	c := &adviserCtl{grants: []map[string]any{
		{"id": "grant_live", "source_agent_id": "agent_src", "target_agent_id": "agent_rev", "live": true, "grant_revision": 1, "expires_at": "2026-09-27T00:00:00Z",
			"source_session_name": "checkout", "source_agent_name": "main", "target_session_name": "reviewer", "target_agent_name": "main"},
		{"id": "grant_gone", "source_agent_id": "agent_src", "target_agent_id": "agent_rev", "live": false, "revoked_at": "2026-09-26T10:00:00Z", "grant_revision": 1,
			"expires_at": "2026-09-27T00:00:00Z", "source_session_name": "checkout", "source_agent_name": "main", "target_session_name": "reviewer", "target_agent_name": "main"},
		{"id": "grant_old", "source_agent_id": "agent_rev", "target_agent_id": "agent_src", "live": false, "grant_revision": 1,
			"expires_at": "2026-09-20T00:00:00Z", "source_session_name": "reviewer", "source_agent_name": "main", "target_session_name": "checkout", "target_agent_name": "main"},
	}}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "list", "--session", "fleetsrc0000")
	if code != 0 || !strings.Contains(out, "grant_live") || !strings.Contains(out, "revoked") || !strings.Contains(out, "reviewer/main") ||
		!strings.Contains(out, "expired") || !strings.Contains(out, "advises") {
		t.Fatalf("list: %d %s %s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "adviser", "list", "--session", "fleetsrc0000", "--json")
	var m map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &m) != nil || m["data"].(map[string]any)["count"] != float64(3) {
		t.Fatalf("list --json: %d %s", code, out)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "adviser", "disconnect", "grant_0001")
	if code != 0 || !strings.Contains(out, "stops: every future consultation") || !strings.Contains(out, "not recalled") {
		t.Fatalf("disconnect: %d %s", code, out)
	}
}

// Refusals: a person without the operator role on an end is refused by the
// service and nothing is recorded as connected (exit 3); and while the
// control plane reports agent.workspace unavailable every adviser verb says
// so and sends nothing to the grant routes.
func TestAdviserRefusalsWrongRoleAndCapabilityUnavailable(t *testing.T) {
	c := &adviserCtl{forbid: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "adviser", "connect", "reviewer/main", "--session", "fleetsrc0000", "--confirm", "reviewer/main", "--json")
	if code != exitAuth || !strings.Contains(out, "ks_forbidden") || !strings.Contains(out, `"work_started":"no"`) {
		t.Fatalf("wrong role: exit %d out=%s err=%s", code, out, errs)
	}

	c.mu.Lock()
	c.forbid, c.capability, c.calls = false, "unavailable", nil
	c.mu.Unlock()
	for _, args := range [][]string{
		{"adviser", "connect", "reviewer/main", "--session", "fleetsrc0000", "--confirm", "reviewer/main"},
		{"adviser", "list", "--session", "fleetsrc0000"},
		{"adviser", "disconnect", "grant_0001"},
	} {
		_, errs, code := auditExec(t, bin, cfg, dir, env, args...)
		if code != exitFailed || !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "unavailable") {
			t.Errorf("`ks %s` while unavailable: exit %d\n%s", strings.Join(args, " "), code, errs)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, call := range c.calls {
		if strings.Contains(call, "adviser-grants") || strings.Contains(call, "/api/v2/agents") {
			t.Errorf("a disabled adviser verb reached the service: %s", call)
		}
	}
}

// Withdrawal is an idempotent mutation: it carries an Idempotency-Key, so a
// lost reply is recovered by reading the operation back, never by resending.
func TestAdviserDisconnectCarriesAnIdempotencyKey(t *testing.T) {
	c := &adviserCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "adviser", "disconnect", "grant_0001")
	if code != 0 || !strings.Contains(out, "disconnected grant_0001") {
		t.Fatalf("disconnect: %d %s %s", code, out, errs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) != 1 || !strings.HasPrefix(c.keys[0], "ksop_") {
		t.Fatalf("the withdrawal did not carry one idempotency key: %v", c.keys)
	}
}
