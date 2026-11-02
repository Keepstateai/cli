// ks agent run against a fake control plane that also serves callers,
// parents, notes, status records and status frames: what a child start
// sends and in which order, that the cost line comes before anything
// exists, that --bg comes back at once without following, and the
// refusals made before anything is created.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// interopCtl is a control plane for the agent-callable verbs.
type interopCtl struct {
	mu        sync.Mutex
	reqs      []string // METHOD path?query body
	sessions  int
	tasks     int
	names     map[string]string           // session id -> name
	status    map[string][]string         // agent id -> the states GET status answers, in order; the last one stays
	kinds     map[string]string           // agent id -> blocked kind
	frames    map[string][]string         // session id -> the status frames' states the stream sends after hello
	taskState string                      // what GET /api/v2/tasks/{id} answers
	notes     []map[string]any            // the caller's notes, seq ascending
	journal   map[string][]map[string]any // session id -> journal rows
	noStatus  bool                        // the status route answers 404 (an older control plane)
	noParents bool                        // the callers and parent routes answer 404
	refuse    string                      // a typed refusal the parent PUT answers
	freeTier  bool                        // provisioning answers ks_free_tier_one_session
	ends      int
}

func newInteropCtl() *interopCtl {
	return &interopCtl{names: map[string]string{}, status: map[string][]string{}, kinds: map[string]string{}, frames: map[string][]string{},
		journal: map[string][]map[string]any{}, taskState: "done"}
}

func (c *interopCtl) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reqs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (c *interopCtl) first(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.reqs {
		if strings.HasPrefix(r, prefix) {
			return r
		}
	}
	return ""
}

func (c *interopCtl) lastReq(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.reqs) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.reqs[i], prefix) {
			return c.reqs[i]
		}
	}
	return ""
}

func (c *interopCtl) index(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, r := range c.reqs {
		if strings.HasPrefix(r, prefix) {
			return i
		}
	}
	return -1
}

func (c *interopCtl) mutations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reqs {
		if !strings.HasPrefix(r, "GET ") {
			n++
		}
	}
	return n
}

func (c *interopCtl) statusDoc(agent string) map[string]any {
	seq := c.status[agent]
	state := "working"
	if len(seq) > 0 {
		state = seq[0]
		if len(seq) > 1 {
			c.status[agent] = seq[1:]
		}
	}
	d := map[string]any{"agent_id": agent, "session_id": strings.Replace(agent, "agent_", "session_", 1), "state": state, "reason": state, "msg": "Words for " + state + ".", "source": "keepstate",
		"needs_you": state == "blocked" || state == "error"}
	if state == "blocked" {
		d["kind"] = c.kinds[agent]
		if c.kinds[agent] == "permission" {
			d["msg"] = "Run: npm test"
		}
	}
	return d
}

func (c *interopCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.reqs = append(c.reqs, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"schema_version":2,"request_id":"r","error":{"code":%q,"type":%q,"message":%q,"retryable":false,"work_started":"no","next_action":""}}`, typ, typ, msg)
	}
	p := r.URL.Path
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case p == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"client"},{"id":"preflight","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.Method == "POST" && p == "/api/v2/preflight":
		env(200, map[string]any{"account_id": "acct_t", "blockers": []any{}, "ready": true})
	case r.Method == "GET" && p == "/api/v2/keys":
		env(200, map[string]any{"items": []any{map[string]any{"id": "vlt_one1", "provider": "anthropic", "last4": "abcd", "enabled": true, "revision": 1}}, "next_cursor": ""})
	case r.Method == "POST" && p == "/api/v2/callers":
		if c.noParents {
			refuse(404, "ks_not_found", "no such route")
			return
		}
		env(200, map[string]any{"ok": true})
	case r.Method == "POST" && strings.HasPrefix(p, "/api/v2/callers/") && strings.HasSuffix(p, "/end"):
		c.ends++
		env(200, map[string]any{"paused": []string{}})
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v2/callers/") && strings.HasSuffix(p, "/notes"):
		if c.noParents {
			refuse(404, "ks_not_found", "no such route")
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		items := []any{}
		for _, n := range c.notes {
			if s, _ := n["seq"].(int); int64(s) > after {
				items = append(items, n)
			}
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	case r.Method == "POST" && p == "/api/v2/sessions":
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		c.sessions++
		id := fmt.Sprintf("session_%d", c.sessions)
		c.names[id], _ = body["name"].(string)
		env(201, map[string]any{"id": id, "revision": 1, "primary_agent_id": fmt.Sprintf("agent_%d", c.sessions), "runner_model": "claude-haiku-4-5", "harness": "claude-code"})
	case r.Method == "POST" && strings.HasSuffix(p, "/bindings"):
		env(200, map[string]any{"revision": 2})
	case r.Method == "GET" && p == "/api/v2/agents":
		sid := r.URL.Query().Get("session_id")
		if _, ok := c.names[sid]; !ok {
			refuse(404, "ks_not_found", "no such session")
			return
		}
		aid := strings.Replace(sid, "session_", "agent_", 1)
		env(200, map[string]any{"items": []any{map[string]any{"id": aid, "session_id": sid, "name": c.names[sid], "is_primary": true, "activity": "working", "revision": 3}}})
	case r.Method == "PUT" && strings.HasSuffix(p, "/parent"):
		if c.noParents {
			refuse(404, "ks_not_found", "no such route")
			return
		}
		if c.refuse != "" {
			refuse(409, c.refuse, "this parent already runs 8 children")
			return
		}
		env(200, map[string]any{"revision": 4})
	case r.Method == "POST" && strings.HasSuffix(p, "/provision"):
		if c.freeTier {
			refuse(409, "ks_free_tier_one_session", "the free tier runs one session at a time")
			return
		}
		sid := strings.Split(p, "/")[4]
		env(200, map[string]any{"session_id": sid, "agent_id": strings.Replace(sid, "session_", "agent_", 1), "ready": false, "supervised": true, "agent_activity": "starting",
			"note": "supervised", "phases": []any{map[string]any{"phase": "start_agent", "done": true, "detail": "running"}}})
	case r.Method == "POST" && strings.HasSuffix(p, "/tasks"):
		c.tasks++
		env(201, map[string]any{"id": fmt.Sprintf("task_%d", c.tasks), "state": "queued", "queue_seq": c.tasks, "replayed": false})
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v2/tasks/"):
		env(200, map[string]any{"id": strings.TrimPrefix(p, "/api/v2/tasks/"), "state": c.taskState})
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v2/agents/") && strings.HasSuffix(p, "/status"):
		if c.noStatus {
			refuse(404, "ks_not_found", "no such route")
			return
		}
		env(200, c.statusDoc(strings.Split(p, "/")[4]))
	case r.Method == "GET" && strings.HasSuffix(p, "/events/stream"):
		sid := strings.Split(p, "/")[4]
		frames := c.frames[sid]
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: hello\ndata: {\"session_id\":%q}\n\n", sid)
		fl.Flush()
		for _, st := range frames {
			time.Sleep(20 * time.Millisecond)
			d, _ := json.Marshal(map[string]any{"agent_id": strings.Replace(sid, "session_", "agent_", 1), "state": st, "reason": st, "msg": "frame " + st})
			fmt.Fprintf(w, "event: event\ndata: {\"stream_seq\":1}\n\nevent: status\ndata: %s\n\n", d)
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		c.mu.Lock()
	case r.Method == "GET" && strings.HasSuffix(p, "/events"):
		sid := strings.Split(p, "/")[4]
		items := []any{}
		for _, e := range c.journal[sid] {
			items = append(items, e)
		}
		env(200, map[string]any{"items": items, "next_cursor": ""})
	default:
		refuse(404, "ks_not_found", "no such resource")
	}
}

func interopEnv(t *testing.T, c *interopCtl) (bin, cfg string, run func(env []string, args ...string) (string, string, int)) {
	t.Helper()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg = buildAndAuth(t, srv)
	dir := t.TempDir()
	run = func(env []string, args ...string) (string, string, int) {
		base := append(fastEnv(cfg), "KS_WAIT_POLL_MS=50", "KS_KEEPER_POLL_MS=50", "HC_TERMINAL_ID=", "HERDR_PANE_ID=", "KS_CALLER_ID=")
		return auditExec(t, bin, cfg, dir, append(base, env...), args...)
	}
	return bin, cfg, run
}

func jsonData(t *testing.T, out string) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	return env.Data
}

func TestAgentRunBackgroundReturnsAtOnce(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	out, errs, code := run([]string{"KS_CALLER_ID=orchestrator-1"}, "agent", "run", "--bg", "--name", "review", "--outlive-parent", "--template", "reviewer", "review the diff", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	d := jsonData(t, out)
	fm, _ := d["first_message"].(map[string]any)
	if d["agent"] != "agent_1" || d["session"] != "session_1" || d["outlive_parent"] != true || fm["sent"] != true || fm["task_id"] != "task_1" {
		t.Fatalf("the --bg answer does not carry the child: %v", d)
	}
	parent, _ := d["parent"].(map[string]any)
	if parent["kind"] != "caller" || !strings.HasPrefix(fmt.Sprint(parent["ref"]), "cl_") || parent["label"] != "orchestrator-1" {
		t.Fatalf("the parent is not this caller, named: %v", parent)
	}
	if c.count("GET /api/v2/sessions/session_1/events/stream") != 0 {
		t.Fatal("--bg followed the stream")
	}
	// the cost line before anything exists
	if !strings.Contains(errs, "This starts a new session for review; session time and model use are metered.") {
		t.Fatalf("no cost line:\n%s", errs)
	}
	// the order: caller, record, parent, machine, then the first instruction
	order := []string{"POST /api/v2/callers ", "POST /api/v2/sessions ", "PUT /api/v2/agents/agent_1/parent", "POST /api/v2/sessions/session_1/provision", "POST /api/v2/agents/agent_1/tasks"}
	last := -1
	for _, o := range order {
		i := c.index(o)
		if i <= last {
			t.Fatalf("%q out of order (at %d after %d): %v", o, i, last, c.reqs)
		}
		last = i
	}
	put := c.first("PUT /api/v2/agents/agent_1/parent")
	if !strings.Contains(put, `"caller_id":"cl_`) || !strings.Contains(put, `"outlive_parent":true`) || !strings.Contains(put, `"notify":true`) || !strings.Contains(put, `"expected_revision":3`) {
		t.Fatalf("the parent PUT: %s", put)
	}
	// the role's opening instructions joined in front of the task
	task := c.first("POST /api/v2/agents/agent_1/tasks")
	if !strings.Contains(task, `You are a reviewer.`) || !strings.Contains(task, `review the diff`) || strings.Index(task, "You are a reviewer.") > strings.Index(task, "review the diff") {
		t.Fatalf("the first instruction is not the role then the task: %s", task)
	}
}

func TestAgentRunParentFromTheHcTerminal(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	out, errs, code := run([]string{"HC_TERMINAL_ID=t7"}, "agent", "run", "--bg", "--outlive-parent", "--name", "tests", "run the tests", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	reg := c.first("POST /api/v2/callers ")
	if !strings.Contains(reg, `"label":"hc:t7"`) {
		t.Fatalf("the caller is not the hc terminal: %s", reg)
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(reg[strings.Index(reg, "{"):]), &body)
	if put := c.first("PUT /api/v2/agents/agent_1/parent"); body.ID == "" || !strings.Contains(put, `"caller_id":"`+body.ID+`"`) {
		t.Fatalf("the parent PUT does not name the hc caller %s: %s", body.ID, put)
	}
	if p, _ := jsonData(t, out)["parent"].(map[string]any); p["label"] != "hc:t7" {
		t.Fatalf("the JSON does not print the identity: %v", p)
	}
}

func TestAgentRunNoParentSendsNone(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	out, errs, code := run(nil, "agent", "run", "--bg", "--no-parent", "--name", "solo", "draft notes", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if c.count("POST /api/v2/callers") != 0 || c.count("PUT /api/v2/agents/agent_1/parent") != 0 {
		t.Fatalf("--no-parent registered or set a parent: %v", c.reqs)
	}
	if d := jsonData(t, out); d["parent"] != nil {
		t.Fatalf("--no-parent answered a parent: %v", d["parent"])
	}
}

func TestAgentRunFollowsUntilTheInstructionFinishes(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"working", "done"}
	_, _, run := interopEnv(t, c)
	out, errs, code := run(nil, "agent", "run", "--no-parent", "--name", "review", "review it")
	if code != 0 || !strings.Contains(out, "review: done") || !strings.Contains(out, "ks agent read review") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	// still running: exit 4 and the words
	c2 := newInteropCtl()
	c2.taskState = "running"
	_, _, run2 := interopEnv(t, c2)
	out, errs, code = run2(nil, "agent", "run", "--no-parent", "--name", "slow", "a long task", "--timeout", "1")
	if code != exitTemporary || !strings.Contains(errs, "still running as slow") {
		t.Fatalf("a timeout is exit 4 and still running: %d\n%s\n%s", code, out, errs)
	}
}

func TestAgentRunRefusalsCreateNothing(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	for _, bad := range [][]string{
		{"agent", "run", "a task"},                                           // no --name
		{"agent", "run", "--name", "9bad", "a task"},                         // not a name
		{"agent", "run", "--name", "x", "--template", "nope", "a task"},      // no such role
		{"agent", "run", "--name", "x", "--parent", "p", "--no-parent", "t"}, // both
		{"agent", "run", "--name", "x", "--no-parent", "--outlive-parent", "t"},
		{"agent", "run", "--name", "x", "--spend-limit", "5", "t"}, // spend limit without KeepState's key
		{"agent", "run", "--name", "x", "   "},
	} {
		if _, errs, code := run(nil, bad...); code != exitUsage {
			t.Errorf("%v: exit %d\n%s", bad, code, errs)
		}
	}
	if n := c.mutations(); n != 0 {
		t.Fatalf("a refused run sent %d mutations: %v", n, c.reqs)
	}
	// a control plane that does not record parents: refused before anything exists
	c2 := newInteropCtl()
	c2.noParents = true
	_, _, run2 := interopEnv(t, c2)
	if _, errs, code := run2(nil, "agent", "run", "--name", "x", "t"); code != exitFailed || !strings.Contains(errs, "does not record a child's parent yet, so nothing was created") || c2.count("POST /api/v2/sessions") != 0 {
		t.Fatalf("no parents route: %d\n%s", code, errs)
	}
}

func TestAgentRunServiceRefusalsSayWhatExists(t *testing.T) {
	// the ninth child: the record exists with no machine, said plainly
	c := newInteropCtl()
	c.refuse = "ks_children_limit"
	_, _, run := interopEnv(t, c)
	_, errs, code := run([]string{"KS_CALLER_ID=busy"}, "agent", "run", "--name", "ninth", "t")
	if code != exitConflict || !strings.Contains(errs, "with no machine, so nothing runs or is metered") || !strings.Contains(errs, "the most one parent may run") || c.count("POST /api/v2/sessions/session_1/provision") != 0 {
		t.Fatalf("children limit: %d\n%s", code, errs)
	}
	// the free plan: plain words and one action
	c2 := newInteropCtl()
	c2.freeTier = true
	_, _, run2 := interopEnv(t, c2)
	_, errs, code = run2(nil, "agent", "run", "--no-parent", "--name", "second", "t")
	if code != exitConflict || !strings.Contains(errs, "the free plan runs one session at a time") || !strings.Contains(errs, "Next: add credit") || c2.count("POST /api/v2/agents/agent_1/tasks") != 0 {
		t.Fatalf("free tier: %d\n%s", code, errs)
	}
}

func TestAgentRunRefusesANameTheCallerUses(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=dup"}, "agent", "run", "--bg", "--outlive-parent", "--name", "review", "one"); code != 0 {
		t.Fatalf("first: %d %s", code, errs)
	}
	n := c.count("POST /api/v2/sessions ")
	if _, errs, code := run([]string{"KS_CALLER_ID=dup"}, "agent", "run", "--bg", "--outlive-parent", "--name", "review", "two"); code != exitConflict || !strings.Contains(errs, "already has a child named review") || c.count("POST /api/v2/sessions ") != n {
		t.Fatalf("a taken name: %d\n%s", code, errs)
	}
}
