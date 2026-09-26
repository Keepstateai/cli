// KS-034: the live window's model rendered in the service's order and
// labels within 80x24, unknown counts never 0, each item performed through
// the request the service names, destructive ones never preselected.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type viewCtl struct {
	mu    sync.Mutex
	calls []string
}

func (c *viewCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	req := func(m, pat, p string) map[string]any { return map[string]any{"method": m, "pattern": pat, "path": p} }
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions" && r.Method == "GET":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetvw00000000000000000000000001", "short_id": "fleetvw00000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.URL.Path == "/api/v2/agents/agent_1/view":
		env(map[string]any{
			"header": map[string]any{"account_id": "acct", "project_name": "payments", "session_id": "session_1", "session_name": "checkout", "agent_id": "agent_1", "agent_name": "main",
				"controller": "view_only", "runtime_state": "running", "activity": "working", "observed_at": "12:00:01", "key_routes": []string{"anthropic"}},
			"runner":       map[string]any{"label": "Claude Code 2.1.251 (reported)", "certification": "not certified", "reported": true},
			"conversation": req("GET", "/api/v2/sessions/{id}/events", "/api/v2/sessions/session_1/events"),
			"footer":       map[string]any{"queued": 2, "held": nil, "pending_approvals": 0, "advisers": nil},
			"palette": []any{
				map[string]any{"label": "Other agents", "effect": "list the agents you may open and the advisers connected to this one; open reuses the chosen agent", "requests": []any{req("GET", "/api/v2/agent-targets", "/api/v2/agent-targets?name=main")}},
				map[string]any{"label": "Pending work", "effect": "queued and held instructions in order", "requests": []any{req("GET", "/api/v2/agents/{id}/pending", "/api/v2/agents/agent_1/pending")}},
				map[string]any{"label": "Results", "effect": "the artifacts", "requests": []any{req("GET", "/api/v2/tasks/{id}/results", "/api/v2/tasks/<task>/results")}},
				map[string]any{"label": "Save and pause", "effect": "saves the whole session and pauses it", "destructive": true,
					"requests": []any{req("POST", "/api/v2/sessions/{id}/pause", "/api/v2/sessions/session_1/pause")},
					"confirm":  map[string]any{"title": "Save and pause checkout?", "body": []string{"All work in this session will pause after a safe save."}, "choices": []string{"Save and pause", "Cancel"}, "default": "Cancel"}},
				map[string]any{"label": "Sneaky", "effect": "asks the model", "inference": true, "requests": []any{req("GET", "/x", "/x")}},
				map[string]any{"label": "Future", "effect": "a route this client does not know", "requests": []any{req("PUT", "/api/v2/future", "/api/v2/future")}},
				map[string]any{"label": "Help", "effect": "current shortcuts", "local": true, "requests": []any{}},
			}})
	case r.URL.Path == "/api/v2/agents/agent_1/pending":
		env(map[string]any{"agent_id": "agent_1", "queue_revision": "qrev_1", "pending": []any{}})
	case r.URL.Path == "/api/v2/sessions/session_1/pause":
		env(map[string]any{"runtime_state": "stopping", "note": "the save is written"})
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no route"}}`)
	}
}

func (c *viewCtl) called(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.calls {
		if x == s {
			return true
		}
	}
	return false
}

func TestTheLiveViewFitsAndPerformsWhatTheServiceNames(t *testing.T) {
	c := &viewCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw")
	if code != 0 {
		t.Fatalf("view: %d\n%s%s", code, out, errs)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 24 {
		t.Errorf("%d lines: more than 24", len(lines))
	}
	for _, l := range lines {
		if utf8.RuneCountInString(l) > 80 {
			t.Errorf("wider than 80: %q", l)
		}
	}
	for _, want := range []string{"checkout/main · project payments · view_only", "queued 2 · held unknown · approvals 0 · advisers unknown", "  1 Other agents", "  2 Pending work", "  4 Save and pause !", "no default"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Other agents") > strings.Index(out, "Pending work") {
		t.Error("the menu is not in the service's order")
	}
	// a read item: exactly the named request
	if _, _, code := auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw", "--choose", "2"); code != 0 || !c.called("GET /api/v2/agents/agent_1/pending") {
		t.Fatalf("pending: %d", code)
	}
	// a placeholder path is not guessed
	out, _, _ = auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw", "--choose", "3")
	if !strings.Contains(out, "needs a value you choose") {
		t.Fatalf("placeholder:\n%s", out)
	}
	// destructive: never preselected; the default choice and --yes do nothing
	for _, args := range [][]string{{}, {"--yes"}, {"--confirm", "Cancel"}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, append([]string{"agent", "view", "main", "--session", "fleetvw", "--choose", "4"}, args...)...); code != exitUsage || !strings.Contains(errs, "never preselected") {
			t.Errorf("%v: %d\n%s", args, code, errs)
		}
	}
	if c.called("POST /api/v2/sessions/session_1/pause") {
		t.Fatal("an unconfirmed destructive item was performed")
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw", "--choose", "4", "--confirm", "Save and pause"); code != 0 || !c.called("POST /api/v2/sessions/session_1/pause") {
		t.Fatalf("confirmed: %d\n%s", code, errs)
	}
	// an item said to go through the model, or a route this client does not know: refused
	if _, _, code := auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw", "--choose", "5"); code != exitIntegrity {
		t.Fatalf("inference: %d", code)
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "view", "main", "--session", "fleetvw", "--choose", "6"); code != exitFailed || !strings.Contains(errs, "does not know how to make") || c.called("PUT /api/v2/future") {
		t.Fatalf("unknown route: %d\n%s", code, errs)
	}
}
