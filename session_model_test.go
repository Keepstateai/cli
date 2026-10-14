// Model choice: a session carries the model its agents call; ks session
// model reads it and says whether it can change now, and changes it only
// against the revision just read, while the session is paused; the
// service's refusals are printed with the next action.
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

type modelCtl struct {
	mu      sync.Mutex
	patches []map[string]any
	calls   []string
	model   string // the parked session's recorded model
	rev     int64
}

func (c *modelCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg, next string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no", "next_action": next}})
	}
	fleet := func(id, short, name, rec, state string) map[string]any {
		return map[string]any{"id": id, "short_id": short, "name": name, "runtime_state": state, "record_id": rec,
			"agent_activity": "ready", "task_state": "none", "key_alias": "prod", "observed_at": "x", "last_activity_at": "x", "created_at": "x", "image": "claude", "budget_tokens": 1, "execution_epoch": 1}
	}
	record := func(rec, name, state, model string, rev int64) map[string]any {
		return map[string]any{"id": rec, "account_id": "acct_x", "revision": rev, "name": name, "mode": "agent", "runtime_state": state, "primary_agent_id": "agent_" + rec, "execution_epoch": 1, "runner_model": model}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{
			fleet("fleetrun000000000000000000000001", "fleetrun0000", "checkout", "session_run", "running"),
			fleet("fleetpark00000000000000000000002", "fleetpark000", "reviewer", "session_park", "parked"),
			fleet("fleetdead00000000000000000000003", "fleetdead000", "old", "session_dead", "dead"),
		}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		rec := r.URL.Query().Get("session_id")
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_" + rec, "session_id": rec, "name": "main", "is_primary": true, "activity": "ready"}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_run":
		env(200, record("session_run", "checkout", "running", "claude-haiku-4-5-20251001", 3))
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_dead":
		env(200, record("session_dead", "old", "dead", "claude-haiku-4-5-20251001", 2))
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_park":
		c.mu.Lock()
		model, rev := c.model, c.rev
		c.mu.Unlock()
		env(200, record("session_park", "reviewer", "parked", model, rev))
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.patches = append(c.patches, body)
		rev := c.rev
		c.mu.Unlock()
		model, _ := body["runner_model"].(string)
		switch {
		case strings.HasSuffix(r.URL.Path, "session_run"):
			refuse(409, "ks_session_not_paused", "the session is running; pause it to change its model. The change takes effect when the session resumes", "")
		case strings.HasSuffix(r.URL.Path, "session_dead"):
			refuse(409, "ks_session_killed", "this session has ended; its model cannot change", "")
		case model != "claude-sonnet-5" && model != "claude-haiku-4-5-20251001":
			refuse(422, "ks_model_invalid", "runner_model: not a model offered to sessions; offered: claude-haiku-4-5-20251001, claude-sonnet-5", "ks cruise models")
		case int64(body["expected_revision"].(float64)) != rev:
			refuse(409, "ks_revision_conflict", "the record changed since it was read", "")
		default:
			c.mu.Lock()
			c.model, c.rev = model, rev+1
			c.mu.Unlock()
			env(200, record("session_park", "reviewer", "parked", model, rev+1))
		}
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "ks_not_found", "message": "no such route"}})
	}
}

func (c *modelCtl) patched() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.patches...)
}

func TestSessionModelReadsAndChangesTheModelOnlyWhilePaused(t *testing.T) {
	c := &modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// no model: the recorded model and whether it can change now; nothing is sent
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark")
	if code != 0 || !strings.Contains(out, "session fleetpark000 runs claude-haiku-4-5-20251001") || !strings.Contains(out, "can change now") {
		t.Fatalf("read of a paused session: %d\n%s%s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetrun")
	if code != 0 || !strings.Contains(out, "runs claude-haiku-4-5-20251001") || !strings.Contains(out, "pause it first") || !strings.Contains(out, "ks agent pause main --session fleetrun0000") {
		t.Fatalf("read of a running session: %d\n%s", code, out)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetdead")
	if code != 0 || !strings.Contains(out, "has ended") {
		t.Fatalf("read of an ended session: %d\n%s", code, out)
	}
	if len(c.patched()) != 0 {
		t.Fatalf("a read sent a change: %v", c.patched())
	}
	// the change: the record is read for its revision, and the PATCH names it
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark", "claude-sonnet-5")
	if code != 0 || !strings.Contains(out, "session fleetpark000 will run claude-sonnet-5 from its next resume") {
		t.Fatalf("change: %d\n%s%s", code, out, errs)
	}
	ps := c.patched()
	if len(ps) != 1 || ps[0]["runner_model"] != "claude-sonnet-5" || ps[0]["expected_revision"] != float64(5) {
		t.Fatalf("the change sent: %v", ps)
	}
	// the --json receipt carries the record as the service answered it
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark", "--json")
	var doc map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc["data"].(map[string]any)["runner_model"] != "claude-sonnet-5" {
		t.Fatalf("read --json after the change: %d %s", code, out)
	}
	// running: the service's refusal, with the pause as the next action
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetrun", "claude-sonnet-5")
	if code != exitConflict || !strings.Contains(errs, "pause it to change its model") || !strings.Contains(errs, "ks agent pause main --session fleetrun0000") {
		t.Fatalf("change of a running session: %d\n%s", code, errs)
	}
	// a model the catalog does not offer: the service's message names the offered ones
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetpark", "claude-opus-9")
	if code == 0 || !strings.Contains(errs, "offered: claude-haiku-4-5-20251001, claude-sonnet-5") || !strings.Contains(errs, "ks cruise models") {
		t.Fatalf("change to an unoffered model: %d\n%s", code, errs)
	}
	// ended: nothing changes
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "model", "fleetdead", "claude-sonnet-5")
	if code != exitConflict || !strings.Contains(errs, "has ended") {
		t.Fatalf("change of an ended session: %d\n%s", code, errs)
	}
	// ks session show prints the recorded model
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "show", "fleetpark")
	if code != 0 || !strings.Contains(out, "runner model   claude-sonnet-5") {
		t.Fatalf("session show: %d\n%s", code, out)
	}
}
