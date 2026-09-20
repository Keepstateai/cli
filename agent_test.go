// agent_test: the guards for the agent group. A scripted control plane on
// the loopback serves the capability registry, the session inventory, the
// agents, their tasks, their pending permission requests and the session
// event stream, and every case below is measured through the built binary
// against it.
//
// The properties under test are the ones a person would be hurt by if they
// were wrong: an agent that is already being steered is REFUSED rather
// than taken over, a window's lease token never reaches the terminal,
// detaching leaves the agent working, an instruction submitted twice is
// queued once, and every verb here refuses cleanly while the control plane
// reports the capability unavailable — which is what it reports today.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	agentSessionRecord = "session_rec1"
	agentSessionShort  = "agentsession00"
)

// agentCtl is the control plane in miniature: it answers in the v2
// envelope, remembers every request, and can be told to report the
// capability as unavailable, to hold the control lease in another window,
// or to keep the event stream open until the client goes away.
type agentCtl struct {
	mu          sync.Mutex
	capability  string // what /api/capabilities reports for agent.workspace
	held        bool   // another window holds control of the primary agent
	endless     bool   // the event stream never ends by itself
	requests    []string
	taskBodies  []string
	submissions map[string]map[string]any // submission id -> the task it created
	created     int
}

func newAgentCtl() *agentCtl {
	return &agentCtl{capability: "available", submissions: map[string]map[string]any{}}
}

func (c *agentCtl) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func (c *agentCtl) bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.taskBodies...)
}

func (c *agentCtl) sawRequest(prefix string) bool {
	for _, r := range c.seen() {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

var agentRows = []map[string]any{
	{"id": "agt_main0001", "session_id": agentSessionRecord, "name": "main", "is_primary": true, "activity": "working",
		"observed_at": "2026-09-20T12:00:00Z", "active_task_id": "tsk_1", "queue_revision": 4, "revision": 9, "created_at": "2026-09-20T11:00:00Z"},
	{"id": "agt_twin0001", "session_id": agentSessionRecord, "name": "twin", "is_primary": false, "activity": "idle",
		"observed_at": "2026-09-20T12:00:00Z", "active_task_id": "", "queue_revision": 1, "revision": 2, "created_at": "2026-09-20T11:30:00Z"},
	{"id": "agt_twin0002", "session_id": agentSessionRecord, "name": "twin", "is_primary": false, "activity": "idle",
		"observed_at": "2026-09-20T12:00:00Z", "active_task_id": "", "queue_revision": 1, "revision": 2, "created_at": "2026-09-20T11:31:00Z"},
}

var agentTaskRows = []map[string]any{
	{"id": "tsk_0", "agent_id": "agt_main0001", "submission_id": "sub_0", "state": "done", "queue_seq": 6, "origin": "live", "content_ref": "ref_0", "created_at": "2026-09-20T11:50:00Z"},
	{"id": "tsk_1", "agent_id": "agt_main0001", "submission_id": "sub_1", "state": "running", "queue_seq": 7, "origin": "live", "content_ref": "ref_1", "created_at": "2026-09-20T11:55:00Z"},
	{"id": "tsk_2", "agent_id": "agt_main0001", "submission_id": "sub_2", "state": "queued", "queue_seq": 8, "origin": "live", "content_ref": "ref_2", "created_at": "2026-09-20T11:57:00Z"},
}

// the lease token authorises steering: the fake hands one out so the test
// can prove the client never prints it
const agentLeaseToken = "lease-token-never-printed"

func (c *agentCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	fault := func(code int, kind, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": message}})
	}
	agentID := func() string {
		p := strings.TrimPrefix(r.URL.Path, "/api/v2/agents/")
		return strings.SplitN(p, "/", 2)[0]
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		c.mu.Lock()
		availability := c.capability
		c.mu.Unlock()
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "2026-09-20T12:00:00Z", "price_book": "v1.3",
			"capabilities": []map[string]any{
				{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
				{"id": "agent.workspace", "availability": availability, "summary": "agent windows", "surface": "api", "note": "not open to accounts yet"},
			}, "limits": map[string]any{}})
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{{
			"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort, "name": "checkout", "runtime_state": "running",
			"fleet_state": "running", "record_id": agentSessionRecord, "agent_activity": "working", "task_state": "1 queued",
			"key_alias": "prod", "observed_at": "2026-09-20T12:00:00Z", "last_activity_at": "2026-09-20T12:00:00Z",
			"created_at": "2026-09-20T11:00:00Z", "image": "base", "budget_tokens": 500000, "execution_epoch": 1,
		}}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/agents":
		if r.URL.Query().Get("session_id") != agentSessionRecord {
			env(200, map[string]any{"items": []map[string]any{}})
			return
		}
		env(200, map[string]any{"items": agentRows, "observed_at": "x"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/open"):
		var body struct {
			TakeControl bool `json:"take_control"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := agentID()
		var found map[string]any
		for _, a := range agentRows {
			if a["id"] == id {
				found = a
			}
		}
		if found == nil {
			fault(404, "ks_not_found", "no such agent")
			return
		}
		c.mu.Lock()
		if c.held && !body.TakeControl {
			c.mu.Unlock()
			fault(409, "ks_controller_held", "a window opened at 12:00 is steering this agent")
			return
		}
		reconnected := c.held
		c.held = false
		c.mu.Unlock()
		env(200, map[string]any{"agent": found, "reconnected": reconnected,
			"lease":  map[string]any{"id": "lse_1", "fence": 12, "token": agentLeaseToken, "expires_at": "2026-09-20T12:30:00Z", "mode": "steer"},
			"resume": map[string]any{"after_seq": 41, "cursor": "cur_41"}, "queue_depth": 2})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tasks"):
		raw, _ := readAllBody(r)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		sub, _ := body["submission_id"].(string)
		c.mu.Lock()
		c.taskBodies = append(c.taskBodies, string(raw))
		if t, replayed := c.submissions[sub]; replayed {
			c.mu.Unlock()
			out := map[string]any{"replayed": true}
			for k, v := range t {
				out[k] = v
			}
			env(200, out)
			return
		}
		c.created++
		t := map[string]any{"id": fmt.Sprintf("tsk_new%d", c.created), "agent_id": agentID(), "submission_id": sub,
			"state": "queued", "queue_seq": 9, "origin": "standalone", "content_ref": "ref_new", "created_at": "2026-09-20T12:01:00Z"}
		c.submissions[sub] = t
		c.mu.Unlock()
		out := map[string]any{"replayed": false}
		for k, v := range t {
			out[k] = v
		}
		env(201, out)
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		var items []map[string]any
		for _, t := range agentTaskRows {
			if t["agent_id"] == r.URL.Query().Get("agent_id") {
				items = append(items, t)
			}
		}
		if items == nil {
			items = []map[string]any{}
		}
		env(200, map[string]any{"items": items, "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/approvals":
		env(200, map[string]any{"items": []map[string]any{{
			"id": "apr_1", "session_id": agentSessionRecord, "kind": "shell", "summary": "run the database migration",
			"arguments_json": map[string]any{"cmd": "migrate"}, "expires_at": "2026-09-20T12:05:00Z", "state": "pending",
			"revision": 2, "exact_action_hash": "9f8e",
		}}, "observed_at": "x"})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/events/stream"):
		c.serveStream(w, r)
	default:
		fault(404, "ks_not_found", "no such route")
	}
}

func (c *agentCtl) serveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	frame := func(kind, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		fl.Flush()
	}
	frame("hello", `{"epoch":7}`)
	frame("event", `{"event_id":"ev_1","stream_seq":42,"subject_type":"task","subject_id":"tsk_1","observed_at":"2026-09-20T12:00:03Z","payload":{"state":"running"}}`)
	frame("event", `{"event_id":"ev_2","stream_seq":43,"subject_type":"approval","subject_id":"apr_1","observed_at":"2026-09-20T12:00:05Z","payload":{"kind":"shell"}}`)
	c.mu.Lock()
	endless := c.endless
	c.mu.Unlock()
	if endless {
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				frame("heartbeat", `{}`)
			}
		}
	}
	frame("end", `{}`)
}

func readAllBody(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

// agentFixture builds the binary, signs it in against a fresh fake, and
// hands back everything the cases need.
func agentFixture(t *testing.T) (*agentCtl, string, string) {
	t.Helper()
	c := newAgentCtl()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return c, bin, cfg
}

func TestAgentListAndStatusReadTheSessionsAgents(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "list", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("list: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"AGENT", "main", "agt_main0001", "working", "primary", "tsk_1", "3 agent(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	// the same read as one JSON document
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "list", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("list --json: exit %d\n%s%s", code, out, errs)
	}
	data := parseEnvelope(t, out)["data"].(map[string]any)
	if data["count"] != float64(3) {
		t.Errorf("count: %v", data["count"])
	}

	// status: the activity, what is waiting, and what is being worked on
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "status", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"agent main (agt_main0001)", "activity       working", "queue          1 waiting", "current task   tsk_1 (running) at queue position 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "status", "main", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("status --json: exit %d\n%s", code, out)
	}
	data = parseEnvelope(t, out)["data"].(map[string]any)
	if data["queue_depth"] != float64(1) {
		t.Errorf("queue_depth: %v", data["queue_depth"])
	}
	if task, ok := data["current_task"].(map[string]any); !ok || task["id"] != "tsk_1" {
		t.Errorf("current_task: %v", data["current_task"])
	}
	// no session named, nothing read
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "list"); code != 2 || !strings.Contains(errs, "--session") {
		t.Errorf("without --session: exit %d\n%s", code, errs)
	}
	if c.sawRequest("POST ") {
		t.Errorf("a read verb sent a mutation: %v", c.seen())
	}
}

// An agent another window is steering is refused, named, and left alone:
// taking control is a decision the person makes with --take-control, never
// one the client makes for them.
func TestAgentOpenRefusesWhileAnotherWindowHoldsControl(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.held = true
	c.mu.Unlock()
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort)
	if code != exitConflict {
		t.Fatalf("held: exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
	}
	for _, want := range []string{"another window holds control of agent main", "Remote work started: no.", "Next: ks agent open main --session " + agentSessionShort + " --take-control"} {
		if !strings.Contains(errs, want) {
			t.Errorf("refusal lacks %q:\n%s", want, errs)
		}
	}
	if c.sawRequest("GET /api/v2/sessions/" + agentSessionRecord + "/events/stream") {
		t.Error("the refused window followed the stream anyway")
	}
	// with --take-control the lease moves here, and the window says so
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort, "--take-control", "--no-follow")
	if code != 0 {
		t.Fatalf("take-control: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "you hold control (lease lse_1, steer)") {
		t.Errorf("control line missing:\n%s", out)
	}
	if !strings.Contains(errs, "rejoined") {
		t.Errorf("header did not report the rejoin:\n%s", errs)
	}
}

// --json is one document on stdout, and the lease token — the credential
// that authorises steering — is in none of it.
func TestAgentOpenJSONIsOneDocumentWithoutTheLeaseToken(t *testing.T) {
	_, bin, cfg := agentFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort, "--no-follow", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	data := parseEnvelope(t, out)["data"].(map[string]any)
	control, ok := data["control"].(map[string]any)
	if !ok || control["held"] != true || control["lease_id"] != "lse_1" || control["fence"] != float64(12) {
		t.Errorf("control: %v", data["control"])
	}
	if _, leaked := control["token"]; leaked {
		t.Error("the lease token is in the document")
	}
	if strings.Contains(out+errs, agentLeaseToken) {
		t.Error("the lease token reached the terminal")
	}
	if data["queue_depth"] != float64(2) {
		t.Errorf("queue_depth: %v", data["queue_depth"])
	}
	if resume, ok := data["resume"].(map[string]any); !ok || resume["after_seq"] != float64(41) {
		t.Errorf("resume: %v", data["resume"])
	}
	pending, ok := data["approvals_pending"].([]any)
	if !ok || len(pending) != 1 {
		t.Fatalf("approvals_pending: %v", data["approvals_pending"])
	}
	if pending[0].(map[string]any)["id"] != "apr_1" {
		t.Errorf("approval: %v", pending[0])
	}
}

// The window resumes where the control plane said to, renders one line per
// journal event, and marks a permission request so it cannot be skimmed
// past.
func TestAgentOpenFollowsTheStreamFromTheResumePoint(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if !c.sawRequest("GET /api/v2/sessions/" + agentSessionRecord + "/events/stream?after_seq=41") {
		t.Errorf("the stream did not resume from the window's point: %v", c.seen())
	}
	if !strings.Contains(out, "42") || !strings.Contains(out, `{"state":"running"}`) {
		t.Errorf("the journal row was not rendered:\n%s", out)
	}
	if !strings.Contains(out, "!! permission request apr_1") {
		t.Errorf("the pending permission request was not prominent:\n%s", out)
	}
	if !strings.Contains(out, "!! 43") {
		t.Errorf("the permission event was not marked:\n%s", out)
	}
	// the window's facts are progress, not results: stdout carries events only
	if !strings.Contains(errs, "agent main (agt_main0001) in session "+agentSessionShort) {
		t.Errorf("header missing from stderr:\n%s", errs)
	}
	// --json follows as one object per line, each readable on its own
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "open", "main", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("--json follow: exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected the approval and two events, got %d lines:\n%s", len(lines), out)
	}
	for _, l := range lines {
		var doc map[string]any
		if err := json.Unmarshal([]byte(l), &doc); err != nil {
			t.Errorf("line is not JSON: %v (%q)", err, l)
		}
	}
}

// lockedBuf collects a running process's output for a test that reads it
// while the process is still writing.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Ctrl-C closes the window and nothing else: exit 0, a line that says the
// agent keeps working, and not one request that could stop it.
func TestAgentOpenDetachesOnInterruptAndCancelsNothing(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.mu.Unlock()
	cmd := exec.Command(bin, "agent", "open", "main", "--session", agentSessionShort)
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(so.String(), "42") {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("the window printed no event in time:\n%s%s", so.String(), se.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detaching exited %v\n%s%s", err, so.String(), se.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the window did not detach on the interrupt")
	}
	if !strings.Contains(se.String(), "[detached; the agent keeps working]") {
		t.Errorf("detach line missing:\n%s", se.String())
	}
	for _, r := range c.seen() {
		if strings.HasPrefix(r, "DELETE ") || strings.Contains(r, "cancel") || strings.Contains(r, "/close") {
			t.Errorf("detaching sent %q", r)
		}
	}
}

// An unknown name is named and nothing is opened; two agents of one name
// are both named and nothing is chosen; the id resolves either of them.
func TestAgentNameResolutionRefusesRatherThanGuessing(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	dir := t.TempDir()
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "open", "nope", "--session", agentSessionShort)
	if code != 2 || !strings.Contains(errs, `has no agent named "nope"`) || !strings.Contains(errs, "Next: ks agent list --session "+agentSessionShort) {
		t.Errorf("unknown name: exit %d\n%s", code, errs)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "open", "twin", "--session", agentSessionShort)
	if code != 2 || !strings.Contains(errs, `2 agents in session `+agentSessionShort+` are named "twin"`) || !strings.Contains(errs, "agt_twin0001, agt_twin0002") {
		t.Errorf("ambiguous name: exit %d\n%s", code, errs)
	}
	if c.sawRequest("POST /api/v2/agents/") {
		t.Errorf("an unresolved name opened something: %v", c.seen())
	}
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "status", "agt_twin0002", "--session", agentSessionShort)
	if code != 0 || !strings.Contains(out, "agent twin (agt_twin0002)") {
		t.Errorf("by id: exit %d\n%s%s", code, out, errs)
	}
	// an unknown session is refused by the session verbs' own resolution
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "list", "--session", "zzzz"); code != 2 || !strings.Contains(errs, "no session of yours") {
		t.Errorf("unknown session: exit %d\n%s", code, errs)
	}
}

// The instruction is submitted ONCE. Its submission id is written to the
// local journal before the request leaves, so the same instruction sent
// again is the same submission: the control plane replays the task it
// already queued, and the queue does not grow.
func TestAgentTellSubmitsOnceAndReplaysOnRetry(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "tell", "main", "run the tests", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("tell: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "queued: task tsk_new1 for agent main at queue position 9") {
		t.Errorf("first submission:\n%s", out)
	}
	// the submission id is on disk before the request: the journal carries it
	var recorded []localOp
	for _, o := range readLedger(t, cfg) {
		if o.Submission != "" {
			recorded = append(recorded, o)
		}
	}
	if len(recorded) != 1 || recorded[0].Path != "/api/v2/agents/agt_main0001/tasks" {
		t.Fatalf("the journal does not hold exactly one submission: %+v", recorded)
	}
	// the same instruction again: the same id, one task, and the client says so
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "tell", "main", "run the tests", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("retry: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "already queued: task tsk_new1") {
		t.Errorf("the retry did not report the replay:\n%s", out)
	}
	c.mu.Lock()
	created := c.created
	c.mu.Unlock()
	if created != 1 {
		t.Errorf("the control plane queued %d tasks for one instruction", created)
	}
	bodies := c.bodies()
	if len(bodies) != 2 {
		t.Fatalf("submissions sent: %d", len(bodies))
	}
	var first, second map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &first)
	_ = json.Unmarshal([]byte(bodies[1]), &second)
	if first["submission_id"] == "" || first["submission_id"] != second["submission_id"] {
		t.Errorf("the retry minted a new submission id: %v then %v", first["submission_id"], second["submission_id"])
	}
	if first["text"] != "run the tests" {
		t.Errorf("text sent: %v", first["text"])
	}
	// no lease is held here, so no lease field is sent at all
	for _, k := range []string{"origin", "fence", "lease_id", "lease_token"} {
		if _, present := first[k]; present {
			t.Errorf("a standalone submission carried %q", k)
		}
	}
	// a different instruction is a different submission
	if _, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "tell", "main", "run the linter", "--session", agentSessionShort); code != 0 {
		t.Fatalf("second instruction: exit %d", code)
	}
	c.mu.Lock()
	created = c.created
	c.mu.Unlock()
	if created != 2 {
		t.Errorf("a different instruction did not queue its own task (%d created)", created)
	}
	// an empty instruction queues nothing
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "tell", "main", "   ", "--session", agentSessionShort); code != 2 || !strings.Contains(errs, "the instruction is empty") {
		t.Errorf("empty instruction: exit %d\n%s", code, errs)
	}
}

// The capability is unavailable on the control plane today, so every verb
// in the group refuses with the reason and asks for nothing else. This is
// the state a customer meets, and it must stay clean.
func TestAgentVerbsRefuseWhileTheCapabilityIsUnavailable(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.capability = "unavailable"
	c.mu.Unlock()
	dir := t.TempDir()
	cases := [][]string{
		{"agent", "list", "--session", agentSessionShort},
		{"agent", "open", "main", "--session", agentSessionShort},
		{"agent", "status", "main", "--session", agentSessionShort},
		{"agent", "tell", "main", "run the tests", "--session", agentSessionShort},
	}
	for _, args := range cases {
		out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), args...)
		if code != exitFailed {
			t.Errorf("`ks %s`: exit %d (want %d)\n%s%s", strings.Join(args, " "), code, exitFailed, out, errs)
		}
		if !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "reports unavailable: not open to accounts yet") {
			t.Errorf("`ks %s` did not say why it is disabled:\n%s", strings.Join(args, " "), errs)
		}
	}
	for _, r := range c.seen() {
		if strings.Contains(r, "/api/v2/agents") || strings.Contains(r, "/api/v2/tasks") || strings.Contains(r, "/events/stream") {
			t.Errorf("a disabled verb reached the service: %s", r)
		}
	}
	// and the same refusal in a script's shape, one document, no data
	out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "list", "--session", agentSessionShort, "--json")
	if code != exitFailed {
		t.Fatalf("--json refusal: exit %d\n%s", code, out)
	}
	e, ok := parseEnvelope(t, out)["error"].(map[string]any)
	if !ok || e["code"] != "capability_unavailable" || e["work_started"] != "no" {
		t.Errorf("refusal document: %v", e)
	}
}
