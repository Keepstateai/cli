// ks session runner: reads what a session runs and changes any part of it in
// one request the service carries out step by step; the steps are printed as
// they happen, a refusal says nothing changed and what to do, and a change
// that failed part way says what changed and what did not, in the service's
// own words.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type runnerCtl struct {
	modelCtl
	rmu     sync.Mutex
	sent    []map[string]any
	headers []http.Header
	ops     map[string]map[string]any // operation id -> its record
	reads   map[string]int            // operation id -> reads so far
	change  map[string]any            // the latest change, as GET runner-change reads it
	harness string
	key     map[string]any
}

func (c *runnerCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": v})
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_park":
		c.mu.Lock()
		model, rev := c.model, c.rev
		c.mu.Unlock()
		rec := map[string]any{"id": "session_park", "account_id": "acct_x", "revision": rev, "name": "reviewer", "mode": "agent", "runtime_state": "parked",
			"primary_agent_id": "agent_session_park", "execution_epoch": 1, "runner_model": model, "harness": c.harness, "runner_key": c.key}
		if c.change != nil && c.change["state"] == "open" {
			rec["runner_change"] = c.change
		}
		data(200, rec)
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_park/runner-change":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.sent = append(c.sent, body)
		c.headers = append(c.headers, r.Header.Clone())
		id := "op_" + string(rune('a'+len(c.sent)))
		switch {
		case body["key"] == "vlt_bad":
			c.ops[id] = map[string]any{"operation_id": id, "state": "failed", "http_status": 422, "method": "POST", "path": r.URL.Path,
				"response": map[string]any{"schema_version": 2, "error": map[string]any{"code": "ks_key_invalid", "type": "ks_key_invalid", "message": "key vlt_bad is not an enabled OpenAI key of this account", "work_started": "no"}}}
		case body["runner_model"] == "gpt-broken":
			c.change = map[string]any{"id": "rc_2", "state": "failed", "step": "applying", "outcome": "nothing_changed",
				"words": "Nothing was changed. The model gpt-broken is not offered. The session is paused; resume it when you are ready."}
			c.ops[id] = map[string]any{"operation_id": id, "state": "failed", "http_status": 409, "method": "POST", "path": r.URL.Path,
				"response": map[string]any{"schema_version": 2, "data": c.change}}
		default:
			c.change = map[string]any{"id": "rc_1", "state": "open", "step": "pausing", "words": "Saving the session."}
			c.ops[id] = map[string]any{"operation_id": id, "state": "running", "method": "POST", "path": r.URL.Path}
		}
		data(202, map[string]any{"operation_id": id, "state": "running"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/operations/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/operations/")
		o := c.ops[id]
		if o == nil {
			w.WriteHeader(404)
			return
		}
		c.reads[id]++
		if o["state"] == "running" {
			switch c.reads[id] {
			case 1:
			case 2:
				c.change["step"] = "applying"
			default:
				c.change = map[string]any{"id": "rc_1", "state": "done", "step": "done", "outcome": "applied", "words": "Changed. It runs Codex CLI · GPT-6-Luna · KeepState's key."}
				c.harness = "codex"
				c.key = map[string]any{"provider": "openai", "route": "keepstate"}
				c.mu.Lock()
				c.model = "gpt-6-luna"
				c.rev++
				c.mu.Unlock()
				o["state"], o["http_status"], o["response"] = "succeeded", 200, map[string]any{"schema_version": 2, "data": c.change}
			}
		}
		data(200, o)
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/sessions/session_park/runner-change":
		c.sent = append(c.sent, map[string]any{"cancel": true})
		if c.change == nil || c.change["state"] != "open" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no change of this session is waiting", "work_started": "no"}})
			return
		}
		c.change = map[string]any{"id": "rc_9", "state": "cancelled", "step": "waiting", "words": "Cancelled. Nothing was changed."}
		data(200, c.change)
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_park/runner-change":
		if c.change == nil {
			w.WriteHeader(404)
			return
		}
		data(200, c.change)
	default:
		c.rmu.Unlock()
		c.modelCtl.ServeHTTP(w, r)
		c.rmu.Lock()
	}
}

func TestSessionRunnerReadsAndChangesInOneRequest(t *testing.T) {
	c := &runnerCtl{modelCtl: modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}, ops: map[string]map[string]any{}, reads: map[string]int{},
		harness: "claude-code", key: map[string]any{"provider": "anthropic", "route": "own", "key": map[string]any{"id": "vlt_own", "label": "Your Anthropic key", "last4": "7Q2a"}}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark")
	if code != 0 || !strings.Contains(out, "runs claude-code on claude-haiku-4-5-20251001, calling your Anthropic key …7Q2a") || !strings.Contains(out, "ks session runner fleetpark000 --harness") {
		t.Fatalf("the read: %d\n%s%s", code, out, errs)
	}
	if len(c.sent) != 0 {
		t.Fatal("a read sent a change")
	}

	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--harness", "codex", "--key", "keepstate", "--spend-limit", "20", "--resume")
	if code != 0 || !strings.Contains(out, "runs codex on gpt-6-luna, calling KeepState's key") || !strings.Contains(out, "Changed. It runs Codex CLI") {
		t.Fatalf("the change: %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(errs, "saving the session") && !strings.Contains(errs, "applying the change") {
		t.Fatalf("no step was printed as it happened:\n%s", errs)
	}
	b := c.sent[0]
	if b["harness"] != "codex" || b["key"] != "keepstate" || b["spend_limit_microusd"] != float64(20_000_000) || b["resume"] != true || b["expected_revision"] != float64(5) || b["stop_running"] != nil || b["runner_model"] != nil {
		t.Fatalf("the change sent: %v", b)
	}
	if h := c.headers[0]; h.Get("Idempotency-Key") == "" || h.Get("Prefer") != "respond-async" {
		t.Fatalf("the change was not sent as one operation: %v", h)
	}

	// a refusal: nothing changed, and what to do
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--key", "vlt_bad")
	if code == 0 || !strings.Contains(errs, "not an enabled OpenAI key") || !strings.Contains(errs, "nothing changed") || !strings.Contains(errs, "ks key list, then ks session runner fleetpark000 --key <key-id|keepstate>") {
		t.Fatalf("a refused key: %d\n%s", code, errs)
	}
	// a change that failed after its check says it in the service's words
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--model", "gpt-broken", "--stop-running")
	if code == 0 || !strings.Contains(errs, "Nothing was changed. The model gpt-broken is not offered. The session is paused; resume it when you are ready.") || !strings.Contains(errs, "Remote work started: yes") {
		t.Fatalf("a failed change: %d\n%s", code, errs)
	}
	if c.sent[2]["stop_running"] != true {
		t.Fatalf("--stop-running was not sent: %v", c.sent[2])
	}

	// --cancel: a change still waiting is cancelled; with none waiting, nothing was
	c.change = map[string]any{"id": "rc_9", "state": "open", "step": "waiting", "words": "Waiting for the instruction in progress to finish."}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--cancel")
	if code != 0 || !strings.Contains(out, "Cancelled. Nothing was changed.") {
		t.Fatalf("cancel: %d\n%s%s", code, out, errs)
	}
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--cancel"); code == 0 || !strings.Contains(errs, "no change of this session is waiting; nothing was cancelled") {
		t.Fatalf("cancel with none waiting: %d\n%s", code, errs)
	}

	// --resume without a change is a usage error, and nothing is sent
	n := len(c.sent)
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--resume"); code != exitUsage || !strings.Contains(errs, "go with a change") {
		t.Fatalf("--resume alone: %d\n%s", code, errs)
	}
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--key", "keepstate", "--spend-limit", "abc"); code != exitUsage || !strings.Contains(errs, "an amount in dollars") {
		t.Fatalf("a bad spend limit: %d\n%s", code, errs)
	}
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--cancel", "--model", "x"); code != exitUsage || !strings.Contains(errs, "takes no change of its own") {
		t.Fatalf("--cancel with a change: %d\n%s", code, errs)
	}
	if len(c.sent) != n {
		t.Fatal("a usage error sent a change")
	}
}

func TestSessionRunnerKeyWords(t *testing.T) {
	for _, tc := range []struct {
		k    *runnerKeyRead
		want string
	}{
		{nil, "not reported"},
		{&runnerKeyRead{Provider: "openai", Route: "keepstate"}, "KeepState's key"},
		{&runnerKeyRead{Provider: "openai", Route: "none"}, "no key for openai"},
		{&runnerKeyRead{Provider: "anthropic", Route: "own"}, "your key"},
	} {
		if got := runnerKeyWords(tc.k); got != tc.want {
			t.Fatalf("%+v: %q, want %q", tc.k, got, tc.want)
		}
	}
}
