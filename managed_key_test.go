// managed_key_test.go: model calls on KeepState's key from the command line.
// A session's calls go on the person's own key or on KeepState's key, per
// provider; KeepState's key is paid from purchased credit at the provider's
// list price plus the spread, held per call. Every new request is asked only
// where the service lists KeepState's key as available, so against a service
// that does not, every command asks exactly what it asked before.
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

// mkCtl is a control plane that offers (or does not offer) KeepState's key:
// the capability row, the account's model-keys document, a run's steps, a
// session's bindings and limit, its usage and its meter, each recorded.
type mkCtl struct {
	mu        sync.Mutex
	requests  []string
	bodies    map[string][]string // "METHOD path" -> the raw bodies it received
	capRow    string              // managed_inference availability; "" leaves the row out
	capNote   string              // the row's customer reason when it is not available
	served    bool                // GET/PUT /api/v2/account/model-keys exist
	doc       map[string]any      // the model-keys document
	putRefuse string              // a refusal code for the PUT
	refuse    string              // a refusal code for the session creation
	noManaged bool                // the created record omits managed
	record    map[string]any      // GET /api/v2/sessions/session_rec1
	checks    []any               // the preflight's checks
	usage     map[string]any      // the usage document
	meter     map[string]any      // the legacy meter
}

func newMKCtl() *mkCtl {
	return &mkCtl{bodies: map[string][]string{}, capRow: "available", served: true, doc: managedDoc(true, "own", "own"),
		record: map[string]any{"id": "session_rec1", "revision": 3, "harness": "claude-code"}}
}

// managedDoc is the contract's model-keys document: both providers offered at
// a 4% spread, the book's limits and the account's purchased credit.
func managedDoc(available bool, anthropic, openai string) map[string]any {
	prov := func(models ...any) map[string]any {
		return map[string]any{"standing": "offered", "note": "", "spread_pct": 4, "models": models}
	}
	return map[string]any{
		"defaults": map[string]any{"anthropic": anthropic, "openai": openai},
		"revision": 2,
		"managed": map[string]any{
			"available": available, "note": "KeepState's key is not offered yet; your own key works as before", "currency": "USD", "price_book": "v1.4", "rate_version": "r1",
			"providers": map[string]any{
				"anthropic": prov(map[string]any{"id": "claude-sonnet-5-5", "standing": "offered", "note": "",
					"list_microusd_per_mtok":    map[string]any{"input": 3000000, "output": 15000000, "cache_write": 3750000, "cache_read": 300000},
					"charged_microusd_per_mtok": map[string]any{"input": 3120000, "output": 15600000, "cache_write": 3900000, "cache_read": 312000}}),
				"openai": prov(map[string]any{"id": "gpt-6-luna", "standing": "offered", "note": "",
					"list_microusd_per_mtok":    map[string]any{"input": 1250000, "output": 10000000, "cache_write": 0, "cache_read": 125000},
					"charged_microusd_per_mtok": map[string]any{"input": 1300000, "output": 10400000, "cache_write": 0, "cache_read": 130000}}),
			},
			"limits":  map[string]any{"session_default_microusd": 20000000, "daily_microusd": 100000000, "monthly_microusd": 1000000000, "concurrent_calls": 8},
			"account": map[string]any{"purchased_credit_microusd": 50000000, "headroom_microusd": 50000000, "spent_today_microusd": 0, "spent_this_month_microusd": 0, "open_holds_microusd": 0, "held_by_operator": false},
		},
	}
}

var managedRefusals = map[string][2]any{
	"ks_insufficient_credit":       {402, "no purchased credit on the account: model use on KeepState's key is paid from purchased credit"},
	"ks_managed_unavailable":       {409, "KeepState's key is not offered for anthropic now"},
	"ks_managed_model_not_offered": {409, "claude-opus-5-5 is not offered on KeepState's key"},
	"ks_managed_cap_reached":       {429, "the account's daily limit on KeepState's key ($100.00) is reached"},
	"ks_spend_limit_reached":       {429, "the session's spend limit on KeepState's key ($20.00) is reached"},
}

func (c *mkCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	key := r.Method + " " + r.URL.Path
	c.requests = append(c.requests, key)
	if len(raw) > 0 {
		c.bodies[key] = append(c.bodies[key], string(raw))
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code string) {
		x := managedRefusals[code]
		w.WriteHeader(x[0].(int))
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": code, "type": code, "message": x[1]}})
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	switch {
	case r.URL.Path == "/api/capabilities":
		rows := []any{
			map[string]any{"id": "agent.workspace", "availability": "available", "summary": "s", "surface": "client"},
			map[string]any{"id": "keys.inventory", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "preflight", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "budget.admission", "availability": "available", "summary": "s", "surface": "api"},
		}
		if c.capRow != "" {
			row := map[string]any{"id": "managed_inference", "availability": c.capRow, "summary": "s", "surface": "api"}
			if c.capNote != "" {
				row["unavailable_reason"] = c.capNote
			}
			rows = append(rows, row)
		}
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "x", "price_book": "v1.4", "capabilities": rows, "limits": map[string]any{},
			"protocol": map[string]any{"current": 3, "minimum_client": 1, "header": "X-KS-Protocol"}})
	case r.URL.Path == "/api/v2/account/model-keys" && c.served && r.Method == "GET":
		env(200, c.doc)
	case r.URL.Path == "/api/v2/account/model-keys" && c.served && r.Method == "PUT":
		if c.putRefuse != "" {
			refuse(c.putRefuse)
			return
		}
		d := c.doc
		if defs, ok := body["defaults"].(map[string]any); ok {
			for p, v := range defs {
				d["defaults"].(map[string]any)[p] = v
			}
		}
		d["revision"] = 3
		env(200, d)
	case r.Method == "POST" && r.URL.Path == "/api/v2/preflight":
		chk := c.checks
		if chk == nil {
			chk = []any{map[string]any{"check": "account", "status": "pass", "detail": "the account is active", "category": "authentication"}}
		}
		blockers := []any{}
		for _, x := range chk {
			if m, ok := x.(map[string]any); ok && m["status"] == "block" {
				blockers = append(blockers, m["detail"])
			}
		}
		env(200, map[string]any{"account_id": "acct_t", "cohort_state": "active", "credit_microusd": 50000000, "registry_version": "t", "checks": chk, "blockers": blockers,
			"ready": len(blockers) == 0, "not_checked": []any{}, "reservation": "a preflight reserves nothing", "limits": map[string]any{"workspace_bytes": 52428800}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys":
		env(200, map[string]any{"items": []any{map[string]any{"id": "vlt_one1", "provider": "anthropic", "alias": "prod", "last4": "abcd", "enabled": true, "revision": 1}}, "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions":
		if c.refuse != "" {
			refuse(c.refuse)
			return
		}
		rec := map[string]any{"id": "session_abc123", "revision": 1, "primary_agent_id": "agent_main1", "runner_model": "claude-sonnet-5-5", "harness": "claude-code"}
		if m, ok := body["managed"]; ok && !c.noManaged {
			rec["managed"] = m
		}
		env(201, rec)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/bindings"):
		env(200, map[string]any{"session_id": "session_rec1", "keys": body["keys"], "managed": body["managed"], "revision": 4, "expected_seen": body["expected_revision"]})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/provision"):
		env(200, readyAnswer())
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{{"id": "fleetmanaged000000000000000aaaa", "short_id": "fleetmanaged", "name": "checkout", "runtime_state": "parked",
			"record_id": "session_rec1", "agent_activity": "unavailable", "task_state": "unavailable", "key_alias": "unbound", "observed_at": "x", "last_activity_at": "x",
			"created_at": "x", "image": "base", "budget_tokens": 1, "execution_epoch": 1}}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_rec1":
		env(200, c.record)
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/sessions/session_rec1":
		env(200, map[string]any{"id": "session_rec1", "revision": 5, "limits": body["limits"]})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_rec1/usage":
		env(200, c.usage)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/meter"):
		_ = json.NewEncoder(w).Encode(c.meter)
	default:
		env(404, map[string]any{})
	}
}

func (c *mkCtl) asked(req string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if r == req {
			return true
		}
	}
	return false
}

func (c *mkCtl) body(req string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	bs := c.bodies[req]
	if len(bs) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(bs[len(bs)-1]), &m)
	return m
}

func (c *mkCtl) rawBody(req string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	bs := c.bodies[req]
	if len(bs) == 0 {
		return ""
	}
	return bs[len(bs)-1]
}

func mkEnv(t *testing.T, c *mkCtl) (string, string) {
	t.Helper()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// ---- ks run --agent ------------------------------------------------------------

func TestManagedRunOnKeepStatesKey(t *testing.T) {
	c := newMKCtl()
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	b := c.body("POST /api/v2/sessions")
	if jsonOf(b["managed"]) != `["anthropic"]` || b["spend_limit_microusd"] != float64(20000000) {
		t.Fatalf("the creation did not choose KeepState's key with the book's spend limit: %v", b)
	}
	if c.asked("POST /api/v2/sessions/session_abc123/bindings") || c.asked("GET /api/v2/keys") {
		t.Fatalf("a run on KeepState's key bound or looked up an own key: %v", c.requests)
	}
	if !c.asked("POST /api/v2/sessions/session_abc123/provision") {
		t.Fatalf("no machine was asked for: %v", c.requests)
	}
	all := out + errs
	for _, want := range []string{"KeepState's key", "list price plus 4%", "purchased credit", "$20.00"} {
		if !strings.Contains(all, want) {
			t.Errorf("the run does not say %q:\n%s", want, all)
		}
	}
	// a spend limit of its own
	if _, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate", "--spend-limit", "35.50"); code != 0 {
		t.Fatalf("--spend-limit: %d %s", code, errs)
	}
	if b := c.body("POST /api/v2/sessions"); b["spend_limit_microusd"] != float64(35500000) {
		t.Fatalf("--spend-limit 35.50 sent %v", b["spend_limit_microusd"])
	}
}

func TestManagedRunFollowsTheAccountDefault(t *testing.T) {
	c := newMKCtl()
	c.doc = managedDoc(true, "managed", "own")
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if b := c.body("POST /api/v2/sessions"); jsonOf(b["managed"]) != `["anthropic"]` {
		t.Fatalf("the account's default (KeepState's key) was not followed: %v", b)
	}
	if c.asked("POST /api/v2/sessions/session_abc123/bindings") {
		t.Fatal("an own key was bound on a run that follows the KeepState default")
	}
	if !strings.Contains(out+errs, "your account's default") {
		t.Errorf("the run does not say the key came from the account's default:\n%s%s", out, errs)
	}
}

func TestManagedRunOwnKeyOverAManagedDefault(t *testing.T) {
	for _, arg := range []string{"own", "vlt_one1"} {
		c := newMKCtl()
		c.doc = managedDoc(true, "managed", "own")
		bin, cfg := mkEnv(t, c)
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", arg)
		if code != 0 {
			t.Fatalf("--key %s: exit %d\n%s\n%s", arg, code, out, errs)
		}
		b := c.body("POST /api/v2/sessions")
		if m, ok := b["managed"]; !ok || jsonOf(m) != `[]` {
			t.Fatalf("--key %s over a KeepState default must send managed: [] (got %v)", arg, b)
		}
		if bb := c.body("POST /api/v2/sessions/session_abc123/bindings"); jsonOf(bb["keys"]) != `{"anthropic":"vlt_one1"}` {
			t.Fatalf("--key %s: the own key was not bound: %v", arg, bb)
		}
	}
}

// A service that does not list KeepState's key is asked exactly what it was
// asked before: no model-keys read, no new field in any body.
func TestManagedRunAgainstAServiceWithoutIt(t *testing.T) {
	for _, row := range []string{"", "unavailable"} {
		c := newMKCtl()
		c.capRow, c.served = row, false
		bin, cfg := mkEnv(t, c)
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--name", "today")
		if code != 0 {
			t.Fatalf("row %q: exit %d\n%s\n%s", row, code, out, errs)
		}
		want := []string{"GET /api/capabilities", "POST /api/v2/preflight", "GET /api/v2/keys", "POST /api/v2/sessions", "POST /api/v2/sessions/session_abc123/bindings", "POST /api/v2/sessions/session_abc123/provision"}
		c.mu.Lock()
		got := strings.Join(c.requests, "\n")
		c.mu.Unlock()
		if got != strings.Join(want, "\n") {
			t.Fatalf("row %q: the requests changed:\n%s\nwant:\n%s", row, got, strings.Join(want, "\n"))
		}
		if raw := c.rawBody("POST /api/v2/sessions"); raw != `{"mode":"agent","name":"today"}` {
			t.Fatalf("row %q: the creation body changed: %s", row, raw)
		}
		if raw := c.rawBody("POST /api/v2/sessions/session_abc123/bindings"); raw != `{"expected_revision":1,"keys":{"anthropic":"vlt_one1"}}` {
			t.Fatalf("row %q: the binding body changed: %s", row, raw)
		}
	}
	// listed, but the route is not served: today's bodies
	c := newMKCtl()
	c.served = false
	bin, cfg := mkEnv(t, c)
	if out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--name", "today"); code != 0 {
		t.Fatalf("route not served: %d\n%s%s", code, out, errs)
	}
	if raw := c.rawBody("POST /api/v2/sessions"); raw != `{"mode":"agent","name":"today"}` {
		t.Fatalf("route not served: the creation body changed: %s", raw)
	}
}

func TestManagedSpendLimitRefusedBeforeAnyRequest(t *testing.T) {
	c := newMKCtl()
	bin, cfg := mkEnv(t, c)
	for _, args := range [][]string{
		{"run", "--agent", "--spend-limit", "10"},
		{"run", "--agent", "--key", "own", "--spend-limit", "10"},
		{"run", "--agent", "--key", "vlt_one1", "--spend-limit", "10"},
		{"run", "--agent", "--key", "keepstate", "--spend-limit", "ten"},
		{"run", "--agent", "--key", "keepstate", "--spend-limit", "0"},
		{"run", "--agent", "--key", "keepstate", "--spend-limit", "1.234"},
		{"run", "--key", "keepstate"},
	} {
		c.mu.Lock()
		c.requests = nil
		c.mu.Unlock()
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d\n%s", args, code, exitUsage, errs)
		}
		c.mu.Lock()
		n := len(c.requests)
		c.mu.Unlock()
		if n != 0 {
			t.Errorf("%v reached the service: %v", args, c.requests)
		}
	}
}

func TestManagedRunRefusedWhereNotOffered(t *testing.T) {
	// the registry says no: its reason, nothing asked beyond the registry and the preflight
	c := newMKCtl()
	c.capRow, c.capNote = "unavailable", "not available: bring your own key is the only path in this release"
	bin, cfg := mkEnv(t, c)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate")
	if code == 0 || !strings.Contains(errs, "bring your own key is the only path in this release") || !strings.Contains(errs, "Nothing was created") {
		t.Fatalf("not listed: %d\n%s", code, errs)
	}
	if c.asked("POST /api/v2/sessions") || c.asked("GET /api/v2/account/model-keys") {
		t.Fatalf("a run on an unoffered key reached creation: %v", c.requests)
	}
	// listed, but the account's document says not now: the service's note
	c = newMKCtl()
	c.doc = managedDoc(false, "own", "own")
	bin, cfg = mkEnv(t, c)
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate")
	if code == 0 || !strings.Contains(errs, "KeepState's key is not offered yet; your own key works as before") || c.asked("POST /api/v2/sessions") {
		t.Fatalf("document says unavailable: %d\n%s\n%v", code, errs, c.requests)
	}
}

func TestManagedRunRecordWithoutTheChoiceStartsNoMachine(t *testing.T) {
	c := newMKCtl()
	c.noManaged = true
	bin, cfg := mkEnv(t, c)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate")
	if code == 0 || !strings.Contains(errs, "did not record KeepState's key") {
		t.Fatalf("a record without the choice: %d\n%s", code, errs)
	}
	if c.asked("POST /api/v2/sessions/session_abc123/provision") {
		t.Fatal("a machine was started for a session the service did not put on KeepState's key")
	}
}

func TestManagedRefusalsInTheServicesWords(t *testing.T) {
	nexts := map[string]string{
		"ks_insufficient_credit":       "add credit in the console",
		"ks_managed_unavailable":       "--key own",
		"ks_managed_model_not_offered": "ks key list",
		"ks_managed_cap_reached":       "--key own",
		"ks_spend_limit_reached":       "--spend-limit",
	}
	for code, x := range managedRefusals {
		c := newMKCtl()
		c.refuse = code
		bin, cfg := mkEnv(t, c)
		_, errs, exit := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--key", "keepstate")
		if exit != exitConflict {
			t.Errorf("%s: exit %d, want %d", code, exit, exitConflict)
		}
		if !strings.Contains(errs, x[1].(string)) || !strings.Contains(errs, code) {
			t.Errorf("%s: not in the service's words:\n%s", code, errs)
		}
		if !strings.Contains(errs, nexts[code]) {
			t.Errorf("%s: no next step %q:\n%s", code, nexts[code], errs)
		}
	}
}

// ---- ks session key set ----------------------------------------------------------

func TestManagedSessionKeySet(t *testing.T) {
	c := newMKCtl()
	bin, cfg := mkEnv(t, c)
	dir := t.TempDir()
	// KeepState's key for the harness's provider
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate")
	if code != 0 {
		t.Fatalf("keepstate: %d\n%s%s", code, out, errs)
	}
	if raw := c.rawBody("POST /api/v2/sessions/session_rec1/bindings"); raw != `{"expected_revision":3,"managed":["anthropic"]}` {
		t.Fatalf("keepstate body: %s", raw)
	}
	if !strings.Contains(out, "KeepState's key") {
		t.Errorf("the answer does not say KeepState's key:\n%s", out)
	}
	// a Codex session's provider is OpenAI; --provider names another
	c.mu.Lock()
	c.record = map[string]any{"id": "session_rec1", "revision": 3, "harness": "codex"}
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate"); code != 0 {
		t.Fatalf("codex: %d %s", code, errs)
	}
	if raw := c.rawBody("POST /api/v2/sessions/session_rec1/bindings"); raw != `{"expected_revision":3,"managed":["openai"]}` {
		t.Fatalf("codex body: %s", raw)
	}
	// back to an own key on a session on KeepState's key: managed loses the provider
	c.mu.Lock()
	c.record = map[string]any{"id": "session_rec1", "revision": 3, "harness": "claude-code", "managed": []any{"anthropic", "openai"}}
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "vlt_one1"); code != 0 {
		t.Fatalf("own: %d %s", code, errs)
	}
	if raw := c.rawBody("POST /api/v2/sessions/session_rec1/bindings"); raw != `{"expected_revision":3,"keys":{"anthropic":"vlt_one1"},"managed":["openai"]}` {
		t.Fatalf("own over managed body: %s", raw)
	}
	// a record that predates the choice: today's body exactly
	c.mu.Lock()
	c.record = map[string]any{"id": "session_rec1", "revision": 3}
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "vlt_one1"); code != 0 {
		t.Fatalf("legacy: %d %s", code, errs)
	}
	if raw := c.rawBody("POST /api/v2/sessions/session_rec1/bindings"); raw != `{"expected_revision":3,"keys":{"anthropic":"vlt_one1"}}` {
		t.Fatalf("legacy body: %s", raw)
	}
	// a spend limit after the bind, on the bind's revision
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--key", "keepstate", "--spend-limit", "50", "--provider", "anthropic")
	if code != 0 {
		t.Fatalf("spend limit: %d\n%s%s", code, out, errs)
	}
	if raw := c.rawBody("PATCH /api/v2/sessions/session_rec1"); raw != `{"expected_revision":4,"limits":{"model_admission_limit_microusd":50000000}}` {
		t.Fatalf("spend limit body: %s", raw)
	}
	if !strings.Contains(out, "$50.00") {
		t.Errorf("the new limit is not said:\n%s", out)
	}
	// the limit alone
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "key", "set", "fleetmanaged", "--spend-limit", "75"); code != 0 {
		t.Fatalf("limit alone: %d %s", code, errs)
	}
	if raw := c.rawBody("PATCH /api/v2/sessions/session_rec1"); raw != `{"expected_revision":3,"limits":{"model_admission_limit_microusd":75000000}}` {
		t.Fatalf("limit alone body: %s", raw)
	}
	// refused before any request: a spend limit with an own key; neither flag
	for _, args := range [][]string{
		{"session", "key", "set", "fleetmanaged", "--key", "vlt_one1", "--spend-limit", "5"},
		{"session", "key", "set", "fleetmanaged"},
		{"session", "key", "set", "fleetmanaged", "--key", "keepstate", "--provider", "openrouter"},
	} {
		c.mu.Lock()
		c.requests = nil
		c.mu.Unlock()
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		c.mu.Lock()
		n := len(c.requests)
		c.mu.Unlock()
		if code != exitUsage || n != 0 {
			t.Errorf("%v: exit %d, %d request(s)\n%s", args, code, n, errs)
		}
	}
}

// ---- ks key list and ks key default ---------------------------------------------

func TestManagedKeyListShowsKeepStatesKey(t *testing.T) {
	c := newMKCtl()
	c.doc = managedDoc(true, "managed", "own")
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "key", "list")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	var anth, open string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "keepstate") && strings.Contains(l, "anthropic") {
			anth = l
		}
		if strings.HasPrefix(l, "keepstate") && strings.Contains(l, "openai") {
			open = l
		}
	}
	if !strings.Contains(anth, "KeepState's key") || !strings.Contains(anth, "list price plus 4%") || !strings.Contains(anth, "default") {
		t.Errorf("anthropic row: %q\n%s", anth, out)
	}
	if !strings.Contains(open, "list price plus 4%") || strings.Contains(open, "default for new") {
		t.Errorf("openai row: %q\n%s", open, out)
	}
	if !strings.Contains(out, "vlt_one1") {
		t.Errorf("the own keys are gone:\n%s", out)
	}
	jout, _, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "key", "list", "--json")
	if !strings.Contains(jout, `"managed"`) || !strings.Contains(jout, `"defaults"`) {
		t.Errorf("--json carries no managed document:\n%s", jout)
	}
	// not listed: today's output and today's requests
	for _, row := range []string{"", "unavailable"} {
		c2 := newMKCtl()
		c2.capRow = row
		bin2, cfg2 := mkEnv(t, c2)
		out2, _, code2 := auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "key", "list")
		if code2 != 0 || strings.Contains(out2, "keepstate") || c2.asked("GET /api/v2/account/model-keys") {
			t.Errorf("row %q: %d %v\n%s", row, code2, c2.requests, out2)
		}
	}
	// listed, document says unavailable: no rows
	c3 := newMKCtl()
	c3.doc = managedDoc(false, "own", "own")
	bin3, cfg3 := mkEnv(t, c3)
	if out3, _, _ := auditExec(t, bin3, cfg3, t.TempDir(), fastEnv(cfg3), "key", "list"); strings.Contains(out3, "KeepState's key") {
		t.Errorf("rows for a key that is not offered:\n%s", out3)
	}
}

func TestManagedKeyDefault(t *testing.T) {
	c := newMKCtl()
	c.doc = managedDoc(true, "managed", "own")
	bin, cfg := mkEnv(t, c)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default")
	if code != 0 || !strings.Contains(out, "anthropic") || !strings.Contains(out, "KeepState's key") || !strings.Contains(out, "your own key") {
		t.Fatalf("read: %d\n%s%s", code, out, errs)
	}
	if c.asked("PUT /api/v2/account/model-keys") {
		t.Fatal("reading the defaults changed them")
	}
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default", "openai", "keepstate")
	if code != 0 {
		t.Fatalf("set: %d\n%s%s", code, out, errs)
	}
	if raw := c.rawBody("PUT /api/v2/account/model-keys"); raw != `{"defaults":{"openai":"managed"},"expected_revision":2}` {
		t.Fatalf("PUT body: %s", raw)
	}
	if !strings.Contains(out, "existing sessions") {
		t.Errorf("the answer does not say existing sessions are unchanged:\n%s", out)
	}
	if _, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default", "anthropic", "own"); code != 0 {
		t.Fatal("set own")
	}
	if raw := c.rawBody("PUT /api/v2/account/model-keys"); raw != `{"defaults":{"anthropic":"own"},"expected_revision":3}` {
		t.Fatalf("PUT own body: %s", raw)
	}
	// refused before any request
	for _, args := range [][]string{{"key", "default", "openrouter", "own"}, {"key", "default", "openai", "managed-ish"}, {"key", "default", "openai"}} {
		c.mu.Lock()
		c.requests = nil
		c.mu.Unlock()
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		c.mu.Lock()
		n := len(c.requests)
		c.mu.Unlock()
		if code != exitUsage || n != 0 {
			t.Errorf("%v: exit %d, %d request(s)\n%s", args, code, n, errs)
		}
	}
	// the service's refusal, in its words
	c.mu.Lock()
	c.putRefuse = "ks_managed_unavailable"
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "key", "default", "anthropic", "keepstate")
	if code == 0 || !strings.Contains(errs, "KeepState's key is not offered for anthropic now") {
		t.Fatalf("refusal: %d\n%s", code, errs)
	}
}

// ---- what it cost: ks session usage, ks meter --------------------------------------

func managedUsage(keySource string, charged int64) map[string]any {
	return map[string]any{"session_id": "session_rec1", "rate_book_version": "r1", "observed_at": "x",
		"limits": map[string]any{"token_cap": 1000, "model_admission_limit_microusd": 20000000, "admission_policy": "managed"},
		"entries": []any{map[string]any{"kind": "model", "provider": "anthropic", "model": "claude-sonnet-5-5", "key_source": keySource, "calls": 3, "input_tokens": 100, "output_tokens": 50,
			"microusd": charged, "currency": "USD", "standing": "charged"}},
		"summary": map[string]any{"currency": "USD", "charged_microusd": charged,
			"model":   map[string]any{"tokens": 150, "input_output_tokens": 150, "cache_tokens": 0, "calls": 3, "key_source": keySource, "charged_microusd": charged, "managed_charged_microusd": charged, "charge_reason": "model use on KeepState's key is charged at the provider's list price plus the spread"},
			"runtime": map[string]any{"seconds": 0, "charged_microusd": 0, "rate_microusd_per_minute": 11000},
			"storage": map[string]any{"bytes": 0, "monthly_microusd": 0, "rate_microusd_per_gb_month": 90000},
			"budget":  map[string]any{"budget_tokens": 0}}}
}

func TestManagedUsageShowsTheCharge(t *testing.T) {
	c := newMKCtl()
	c.usage = managedUsage("managed:anthropic", 1234567)
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetmanaged")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "on KeepState's key: $1.23 charged") || !strings.Contains(out, "list price plus the spread") {
		t.Errorf("no charged line:\n%s", out)
	}
	if strings.Contains(out, "No charge") {
		t.Errorf("a charged session reads no charge:\n%s", out)
	}
	// own key: the wording as it was
	c.mu.Lock()
	u := managedUsage("byok", 0)
	u["summary"].(map[string]any)["model"] = map[string]any{"tokens": 150, "input_output_tokens": 150, "cache_tokens": 0, "calls": 3, "key_source": "byok", "charged_microusd": 0, "charge_reason": "KeepState charges nothing for model use on your own key"}
	c.usage = u
	c.mu.Unlock()
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetmanaged")
	if !strings.Contains(out, "model use      No charge: 150 tokens") || strings.Contains(out, "KeepState's key:") {
		t.Errorf("own-key wording changed:\n%s", out)
	}
}

func TestManagedMeterSaysTheCharge(t *testing.T) {
	c := newMKCtl()
	c.meter = map[string]any{"session": "fleetmanaged", "spent": 150, "budget": 1000, "billed_calls": 3, "key_sources": []any{"managed"}, "managed_charged_microusd": 1234567}
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "meter", "fleetmanaged")
	if code != 0 || !strings.Contains(out, "KeepState's key") || !strings.Contains(out, "$1.23 charged") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	c.meter = map[string]any{"session": "fleetmanaged", "spent": 150, "budget": 1000, "billed_calls": 3, "key_sources": []any{"byok"}}
	c.mu.Unlock()
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "meter", "fleetmanaged")
	if strings.Contains(out, "KeepState's key") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("own-key meter changed:\n%s", out)
	}
}

// ---- ks preflight ------------------------------------------------------------------

func TestManagedPreflightShowsTheCheck(t *testing.T) {
	c := newMKCtl()
	c.checks = []any{
		map[string]any{"check": "account", "status": "pass", "detail": "the account is active", "category": "authentication"},
		map[string]any{"check": "managed", "status": "warn", "detail": "KeepState's key is offered for anthropic; no purchased credit, so a run on it would be refused", "next_action": "add credit in the console", "category": "quota"},
	}
	bin, cfg := mkEnv(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "preflight")
	if code != 0 {
		t.Fatalf("a warning about KeepState's key made the preflight fail: %d\n%s%s", code, out, errs)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "managed") {
			line = l
		}
	}
	if !strings.Contains(line, "WARN") || !strings.Contains(line, "no purchased credit") || !strings.Contains(line, "add credit in the console") {
		t.Fatalf("the managed check: %q\n%s", line, out)
	}
	if !strings.Contains(out, "only a run on KeepState's key") {
		t.Errorf("the managed check does not say what it governs:\n%s", out)
	}
}
