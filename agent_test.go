// agent_test: the guards for the agent group. A scripted control plane on
// the loopback serves the capability registry, the session inventory, the
// agents, their tasks, their pending permission requests and the session
// event stream, and every case below is measured through the built binary
// against it.
//
// The properties under test are the ones a person would be hurt by if they
// were wrong: an agent that is already being steered is REFUSED rather
// than taken over, a window's lease token never reaches the terminal or
// the operations journal, a decision is bound to the exact action it was
// read from and is refused rather than applied when that action changed,
// a displaced window stops submitting instead of quietly submitting as a
// stranger, a save that is still stopping is never reported as parked,
// detaching leaves the agent working, an instruction submitted twice is
// queued once, and every verb here refuses cleanly while the control plane
// reports the capability unavailable — which is what it reports today.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// to change an action the moment it is read, to refuse a decision, a
// submission or a renewal in one of the named ways, to keep a session
// stopping for as long as the case needs, or to keep the event stream
// open until the client goes away.
//
// Every field is read and written under the mutex, including from the
// handler goroutines, so a case that watches a window while the window is
// still running is clean under the race detector.
type agentCtl struct {
	mu             sync.Mutex
	capability     string // what /api/capabilities reports for agent.workspace
	held           bool   // another window holds control of the primary agent
	endless        bool   // the event stream never ends by itself
	requests       []string
	taskBodies     []string
	decisionBodies []string
	submissions    map[string]map[string]any // submission id -> the task it created
	created        int

	approvals     map[string]map[string]any // the permission requests, by id
	bumpOnRead    bool                      // the exact action changes the moment it is read
	decisionFault string                    // the typed refusal the decision route answers with

	taskFault  string        // the typed refusal the submission route answers with
	leaseTTL   time.Duration // how long a control lease is handed out for
	renewals   int
	renewFault string // the typed refusal the renewal route answers with

	paused         bool
	stopAfter      int // reads of the session before it says parked; negative never parks
	stateReads     int
	resumes        int
	resumeStopping bool // the first resume is refused: the save is still completing
}

func newAgentCtl() *agentCtl {
	return &agentCtl{capability: "available", submissions: map[string]map[string]any{}, leaseTTL: 30 * time.Minute,
		approvals: map[string]map[string]any{
			"apr_1": approvalDoc("apr_1", "shell", "run the database migration", "pending", 2, "9f8e"),
			// one that was decided somewhere else already, so it is not waiting
			"apr_old": approvalDoc("apr_old", "shell", "delete the build cache", "denied", 4, "77aa"),
		}}
}

func approvalDoc(id, kind, summary, state string, revision int, hash string) map[string]any {
	return map[string]any{"id": id, "session_id": agentSessionRecord, "kind": kind, "summary": summary,
		"arguments_json": map[string]any{"cmd": "migrate"}, "expires_at": "2026-09-20T12:05:00Z",
		"state": state, "revision": revision, "exact_action_hash": hash}
}

// addApproval puts one more request in front of the agent.
func (c *agentCtl) addApproval(id, summary string, revision int, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approvals[id] = approvalDoc(id, "shell", summary, "pending", revision, hash)
}

func (c *agentCtl) approval(id string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	ap := c.approvals[id]
	if ap == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range ap {
		out[k] = v
	}
	return out
}

func (c *agentCtl) counts() (renewals, resumes, stateReads int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewals, c.resumes, c.stateReads
}

// leaseDoc is the control lease as the open and renewal routes answer it:
// the documented spelling, and an expiry a case can make as short as it
// needs the renewal loop to be quick.
func (c *agentCtl) leaseDoc() map[string]any {
	ttl := c.leaseTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return map[string]any{"lease_id": "lse_1", "fence": 12, "lease_token": agentLeaseToken,
		"expires_at": time.Now().UTC().Add(ttl).Format(time.RFC3339), "mode": "steer"}
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
		c.mu.Lock()
		lease := c.leaseDoc()
		c.mu.Unlock()
		env(200, map[string]any{"agent": found, "reconnected": reconnected,
			"lease":  lease,
			"resume": map[string]any{"after_seq": 41, "cursor": "cur_41"}, "queue_depth": 2})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tasks"):
		raw, _ := readAllBody(r)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		sub, _ := body["submission_id"].(string)
		c.mu.Lock()
		c.taskBodies = append(c.taskBodies, string(raw))
		if ft := c.taskFault; ft != "" {
			c.mu.Unlock()
			fault(409, ft, "a window opened at 12:04 holds control of this agent now")
			return
		}
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
		c.mu.Lock()
		var items []map[string]any
		for _, id := range sortedApprovalIDs(c.approvals) {
			if c.approvals[id]["state"] == "pending" {
				items = append(items, c.approvals[id])
			}
		}
		c.mu.Unlock()
		if items == nil {
			items = []map[string]any{}
		}
		env(200, map[string]any{"items": items, "observed_at": "x"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/approvals/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/approvals/")
		c.mu.Lock()
		ap := c.approvals[id]
		if ap == nil {
			c.mu.Unlock()
			fault(404, "ks_not_found", "no such permission request")
			return
		}
		answer := map[string]any{}
		for k, v := range ap {
			answer[k] = v
		}
		if c.bumpOnRead {
			// the exact action changes the instant it has been read: this is
			// the race the revision and the hash exist to lose safely
			ap["revision"] = ap["revision"].(int) + 1
			ap["exact_action_hash"] = "changed"
			ap["summary"] = "run the database migration, then publish it"
		}
		c.mu.Unlock()
		env(200, answer)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/decision"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/approvals/"), "/decision")
		raw, _ := readAllBody(r)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.decisionBodies = append(c.decisionBodies, string(raw))
		ap := c.approvals[id]
		forced := c.decisionFault
		c.mu.Unlock()
		if ap == nil {
			fault(404, "ks_not_found", "no such permission request")
			return
		}
		if forced != "" {
			fault(409, forced, "this request cannot be decided as it was read")
			return
		}
		c.mu.Lock()
		hashNow, _ := ap["exact_action_hash"].(string)
		revNow, _ := ap["revision"].(int)
		c.mu.Unlock()
		if h, _ := body["action_hash"].(string); h != hashNow {
			fault(409, "ks_approval_hash_mismatch", "the exact action changed after it was read")
			return
		}
		if rev, _ := body["expected_revision"].(float64); int(rev) != revNow {
			fault(409, "ks_revision_conflict", "the request moved on while it was being decided")
			return
		}
		decision, _ := body["decision"].(string)
		c.mu.Lock()
		ap["state"] = decision + "d"
		ap["revision"] = revNow + 1
		decided := map[string]any{}
		for k, v := range ap {
			decided[k] = v
		}
		c.mu.Unlock()
		env(200, map[string]any{"approval": decided, "decision": decision, "decided_at": "2026-09-20T12:02:00Z"})
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v2/control-leases/"):
		c.mu.Lock()
		c.renewals++
		ft := c.renewFault
		lease := c.leaseDoc()
		c.mu.Unlock()
		if ft != "" {
			fault(409, ft, "this window is not the controller of that agent")
			return
		}
		env(200, lease)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pause"):
		c.mu.Lock()
		c.paused = true
		state := "parked"
		if c.stopAfter != 0 {
			state = "stopping"
		}
		c.mu.Unlock()
		env(200, map[string]any{"runtime_state": state, "checkpoint_safe": true,
			"note": "the save is written; the machine is winding down"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/resume"):
		c.mu.Lock()
		c.resumes++
		stopping := c.resumeStopping && c.stateReads == 0
		c.mu.Unlock()
		if stopping {
			fault(409, "ks_stopping", "the previous save is still completing")
			return
		}
		env(200, map[string]any{"runtime_state": "running", "note": "resumed from the last save"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/") && !strings.Contains(r.URL.Path, "/events/"):
		c.mu.Lock()
		c.stateReads++
		state := "parked"
		if c.stopAfter < 0 || c.stateReads <= c.stopAfter {
			state = "stopping"
		}
		c.mu.Unlock()
		env(200, map[string]any{"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort,
			"runtime_state": state, "fleet_state": state, "record_id": agentSessionRecord, "observed_at": "x"})
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
		{"agent", "approve", "apr_1", "--session", agentSessionShort},
		{"agent", "deny", "apr_1", "--session", agentSessionShort},
		{"agent", "pause", "main", "--session", agentSessionShort},
		{"agent", "resume", "main", "--session", agentSessionShort},
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
		for _, route := range []string{"/api/v2/agents", "/api/v2/tasks", "/events/stream", "/api/v2/approvals", "/api/v2/control-leases", "/pause", "/resume"} {
			if strings.Contains(r, route) {
				t.Errorf("a disabled verb reached the service: %s", r)
			}
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

func sortedApprovalIDs(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// deciding a permission request
// ---------------------------------------------------------------------

// A decision is about the action the person read, and the client proves
// it: it reads the request again IMMEDIATELY before deciding and sends
// that revision and that hash of the exact action with the decision. The
// scripted control plane checks both, so a decision that carried anything
// else would be refused here rather than applied.
func TestAgentApproveAndDenyDecideTheVersionTheyJustRead(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.addApproval("apr_2", "write over the deployment config", 5, "1a2b")
	dir := t.TempDir()

	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "approve", "apr_1", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("approve: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "approved permission request apr_1: shell — run the database migration") {
		t.Errorf("approve did not say what it decided:\n%s", out)
	}
	// the read comes first, and the decision carries what the read said
	seq := c.seen()
	read, decide := -1, -1
	for i, r := range seq {
		if r == "GET /api/v2/approvals/apr_1" {
			read = i
		}
		if r == "POST /api/v2/approvals/apr_1/decision" && decide < 0 {
			decide = i
		}
	}
	if read < 0 || decide < 0 || read > decide {
		t.Fatalf("the request was not read immediately before it was decided: %v", seq)
	}
	bodies := c.decisionBodies
	if len(bodies) != 1 {
		t.Fatalf("decisions sent: %d", len(bodies))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &body)
	if body["decision"] != "approve" || body["expected_revision"] != float64(2) || body["action_hash"] != "9f8e" {
		t.Errorf("the decision did not bind to the action that was read: %v", body)
	}
	if ap := c.approval("apr_1"); ap["state"] != "approved" {
		t.Errorf("the request is %v after an approval", ap["state"])
	}

	// deny is the same verb with the other word, and it names the one it denied
	out, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "deny", "apr_2", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("deny: exit %d\n%s%s", code, out, errs)
	}
	data := parseEnvelope(t, out)["data"].(map[string]any)
	if data["decision"] != "deny" {
		t.Errorf("decision: %v", data["decision"])
	}
	if ap, ok := data["approval"].(map[string]any); !ok || ap["id"] != "apr_2" {
		t.Errorf("approval: %v", data["approval"])
	}
	if ap := c.approval("apr_2"); ap["state"] != "denyd" { // the fake's own bookkeeping
		t.Errorf("the request is %v after a denial", ap["state"])
	}

	// a request this account has never seen decides nothing
	_, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "approve", "apr_nope", "--session", agentSessionShort)
	if code != 2 || !strings.Contains(errs, "no permission request apr_nope") {
		t.Errorf("unknown request: exit %d\n%s", code, errs)
	}
	if len(c.decisionBodies) != 2 {
		t.Errorf("an unknown request sent a decision anyway: %v", c.decisionBodies)
	}
}

// The four refusals that all mean the same thing: this is not the request
// you read. Each one is named, nothing is decided, nothing is retried,
// and each one tells the person to read the request again.
func TestAgentDecisionRefusesWhatChangedOrExpired(t *testing.T) {
	t.Run("the action changed between the read and the decision", func(t *testing.T) {
		c, bin, cfg := agentFixture(t)
		c.mu.Lock()
		c.bumpOnRead = true
		c.mu.Unlock()
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "approve", "apr_1", "--session", agentSessionShort)
		if code != exitConflict {
			t.Fatalf("exit %d (want %d)\n%s", code, exitConflict, errs)
		}
		for _, want := range []string{"no longer the action you read", "nothing was approved", "It now reads:", "Read it again", "Remote work started: no."} {
			if !strings.Contains(errs, want) {
				t.Errorf("refusal lacks %q:\n%s", want, errs)
			}
		}
		if n := len(c.decisionBodies); n != 1 {
			t.Errorf("a refused decision was sent %d times; it must never be retried", n)
		}
		if ap := c.approval("apr_1"); ap["state"] != "pending" {
			t.Errorf("the request is %v; a refused decision must decide nothing", ap["state"])
		}
	})
	for _, tc := range []struct {
		fault, kind string
		wants       []string
	}{
		{"ks_approval_expired", "approval_expired", []string{"expired before the decision arrived", "the agent was not given this permission", "read the agent's current requests again"}},
		{"ks_approval_not_pending", "approval_not_pending", []string{"already decided elsewhere", "read the agent's current requests again"}},
		{"ks_revision_conflict", "revision_conflict", []string{"moved on while it was being decided", "Read it again"}},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			c, bin, cfg := agentFixture(t)
			c.mu.Lock()
			c.decisionFault = tc.fault
			c.mu.Unlock()
			out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "approve", "apr_1", "--session", agentSessionShort)
			if code != exitConflict {
				t.Fatalf("exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
			}
			for _, want := range append(tc.wants, "Remote work started: no.") {
				if !strings.Contains(errs, want) {
					t.Errorf("refusal lacks %q:\n%s", want, errs)
				}
			}
			// and the same refusal in a script's shape
			out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "deny", "apr_1", "--session", agentSessionShort, "--json")
			if code != exitConflict {
				t.Fatalf("--json: exit %d\n%s", code, out)
			}
			e, ok := parseEnvelope(t, out)["error"].(map[string]any)
			if !ok || e["code"] != tc.kind || e["work_started"] != "no" {
				t.Errorf("refusal document: %v", e)
			}
		})
	}
	// a request that is already decided is refused before anything is sent
	t.Run("already decided", func(t *testing.T) {
		c, bin, cfg := agentFixture(t)
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "approve", "apr_old", "--session", agentSessionShort)
		if code != exitConflict || !strings.Contains(errs, "is already denied") {
			t.Errorf("exit %d\n%s", code, errs)
		}
		if len(c.decisionBodies) != 0 {
			t.Errorf("a decided request was decided again: %v", c.decisionBodies)
		}
	})
}

// ---------------------------------------------------------------------
// the live window: a process that is still running while the case reads it
// ---------------------------------------------------------------------

// windowProc is one open window under test: the process, the pipe that is
// its keyboard, and everything it has said so far.
type windowProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	so  *lockedBuf
	se  *lockedBuf
}

func openWindow(t *testing.T, bin, cfg string, args ...string) *windowProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	w := &windowProc{cmd: cmd, in: in, so: &lockedBuf{}, se: &lockedBuf{}}
	cmd.Stdout, cmd.Stderr = w.so, w.se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return w
}

func (w *windowProc) text() string { return w.so.String() + w.se.String() }

// typeLine is a person typing one line into the window.
func (w *windowProc) typeLine(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(w.in, line+"\n"); err != nil {
		t.Fatalf("typing %q: %v", line, err)
	}
}

// waitFor waits for the window to say something, or reports what it said
// instead. Every case that reads a running window goes through here, so
// none of them races the process.
func (w *windowProc) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(w.text(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the window never said %q:\n%s", want, w.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// detach closes the window the way a person does, and requires it to be a
// success: detaching is what was asked for.
func (w *windowProc) detach(t *testing.T) {
	t.Helper()
	if err := w.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detaching exited %v\n%s", err, w.text())
		}
	case <-time.After(20 * time.Second):
		_ = w.cmd.Process.Kill()
		t.Fatal("the window did not detach on the interrupt")
	}
}

// A permission request is decided from inside the window, with two
// keystrokes and a name, and the window stays open either way: nobody has
// to leave what they are watching to say yes or no.
func TestAgentWindowDecidesWithoutLeavingIt(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.mu.Unlock()
	c.addApproval("apr_2", "write over the deployment config", 5, "1a2b")
	w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
	w.waitFor(t, `type "a <id>" to approve`)

	// naming the one to approve
	w.typeLine(t, "a apr_1")
	w.waitFor(t, "approved permission request apr_1: shell — run the database migration")
	// and the other one is denied, by the id it was shown under
	w.typeLine(t, "d apr_2")
	w.waitFor(t, "denied permission request apr_2")
	// the window is still following, and nothing was decided that was not named
	w.typeLine(t, "a")
	w.waitFor(t, "nothing is waiting for a decision")
	if strings.Contains(w.text(), agentLeaseToken) {
		t.Error("the lease token reached the terminal")
	}
	w.detach(t)
	if ap := c.approval("apr_1"); ap["state"] != "approved" {
		t.Errorf("apr_1 is %v", ap["state"])
	}
	if ap := c.approval("apr_2"); ap["state"] != "denyd" {
		t.Errorf("apr_2 is %v", ap["state"])
	}
}

// What is typed into a window that holds control is submitted as LIVE
// input: it carries the lease, the fence and the lease id, so the service
// can tell it from anything typed anywhere else. The token authorises
// that, so it appears in the request and NOWHERE else — not on the
// terminal, not in the document, not in the operations journal.
func TestAgentWindowSubmitsLiveInputUnderItsLease(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.mu.Unlock()
	w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
	w.waitFor(t, "anything else to send it to the agent as an instruction")
	w.typeLine(t, "run the tests")
	w.waitFor(t, "sent: task tsk_new1 at queue position 9")
	w.detach(t)

	bodies := c.bodies()
	if len(bodies) != 1 {
		t.Fatalf("submissions sent: %d (%v)", len(bodies), bodies)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &body)
	if body["text"] != "run the tests" || body["origin"] != "live" || body["fence"] != float64(12) ||
		body["lease_id"] != "lse_1" || body["lease_token"] != agentLeaseToken {
		t.Errorf("the live submission did not carry the window's authority: %v", body)
	}
	if body["submission_id"] == "" || body["submission_id"] == nil {
		t.Errorf("the live submission carried no submission id: %v", body)
	}
	// the secret is in the request and in nothing this client wrote down
	if strings.Contains(w.text(), agentLeaseToken) {
		t.Error("the lease token reached the terminal")
	}
	raw, err := os.ReadFile(filepath.Join(cfg, "keepstate", "operations.jsonl"))
	if err != nil {
		t.Fatalf("the window recorded no operation at all: %v", err)
	}
	if strings.Contains(string(raw), agentLeaseToken) {
		t.Error("the lease token was written to the operations journal")
	}
	if !strings.Contains(string(raw), "/api/v2/agents/agt_main0001/tasks") {
		t.Errorf("the submission was not recorded before it was sent:\n%s", raw)
	}
}

// A window that has been displaced says so and STOPS. It does not fall
// back to a standalone submission: that would be this window steering an
// agent it was just told it no longer controls.
func TestAgentWindowStopsSubmittingWhenControlMoves(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.taskFault = "ks_controller_stale"
	c.mu.Unlock()
	w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
	w.waitFor(t, "anything else to send it to the agent as an instruction")
	w.typeLine(t, "run the tests")
	w.waitFor(t, "control moved to another window")
	for _, want := range []string{
		"nothing was sent, and this window will not submit as the controller again",
		"ks agent open main --session " + agentSessionShort + " --take-control",
	} {
		if !strings.Contains(w.text(), want) {
			t.Errorf("the displaced window did not say %q:\n%s", want, w.text())
		}
	}
	// a second instruction is refused here rather than submitted as a stranger
	w.typeLine(t, "run the linter")
	w.waitFor(t, "so nothing was sent to the agent")
	w.detach(t)

	bodies := c.bodies()
	if len(bodies) != 1 {
		t.Fatalf("the displaced window submitted %d times; the second must never leave the client: %v", len(bodies), bodies)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &body)
	if body["origin"] != "live" || body["lease_token"] != agentLeaseToken {
		t.Errorf("the one submission was not the live one: %v", body)
	}
	c.mu.Lock()
	created := c.created
	c.mu.Unlock()
	if created != 0 {
		t.Errorf("the control plane queued %d tasks for a displaced window", created)
	}
}

// The lease is short on purpose, so holding control is something a window
// keeps doing rather than something it was once granted. It renews while
// it is open; when a renewal fails it says the control is gone and
// submits nothing more as the controller.
func TestAgentWindowRenewsItsControlLease(t *testing.T) {
	t.Run("renewed while the window is open", func(t *testing.T) {
		c, bin, cfg := agentFixture(t)
		c.mu.Lock()
		c.endless = true
		c.leaseTTL = 2 * time.Second
		c.mu.Unlock()
		w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
		w.waitFor(t, "anything else to send it to the agent as an instruction")
		waitUntil(t, "the window to renew its control", func() bool {
			renewals, _, _ := c.counts()
			return renewals >= 2
		})
		// still the controller, and still not printing what it renews with
		w.typeLine(t, "run the tests")
		w.waitFor(t, "sent: task tsk_new1")
		if strings.Contains(w.text(), agentLeaseToken) {
			t.Error("a renewal printed the lease token")
		}
		w.detach(t)
		if !c.sawRequest("PUT /api/v2/control-leases/lse_1") {
			t.Errorf("the lease was never renewed: %v", c.seen())
		}
	})
	t.Run("a renewal that fails ends the control", func(t *testing.T) {
		c, bin, cfg := agentFixture(t)
		c.mu.Lock()
		c.endless = true
		c.leaseTTL = 2 * time.Second
		c.renewFault = "ks_lease_expired"
		c.mu.Unlock()
		w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
		w.waitFor(t, "could not be renewed")
		w.waitFor(t, "this window no longer holds control and will submit nothing as the controller")
		w.typeLine(t, "run the tests")
		w.waitFor(t, "so nothing was sent to the agent")
		w.detach(t)
		if len(c.bodies()) != 0 {
			t.Errorf("a window that lost its lease submitted anyway: %v", c.bodies())
		}
	})
}

// ---------------------------------------------------------------------
// saving, stopping, parked
// ---------------------------------------------------------------------

// Pause saves and parks, and says the two things separately, because they
// are two things: the save is durable immediately, and the session is
// still stopping. The client waits for the stop itself and never prints a
// sleep or asks anybody to go and look.
func TestAgentPauseSavesThenWaitsForParked(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.stopAfter = 1 // one read says stopping, the next says parked
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "pause", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("pause: exit %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(errs, "the save is safe, and stopping is still finishing") {
		t.Errorf("pause did not separate the save from the stop:\n%s", errs)
	}
	if !strings.Contains(errs, "is still stopping after") {
		t.Errorf("pause showed no progress while it waited:\n%s", errs)
	}
	if !strings.Contains(out, "is parked: the save is complete and safe, and session time has stopped") {
		t.Errorf("pause did not report the session as parked:\n%s", out)
	}
	for _, bad := range []string{"sleep ", "check the console", "try again in"} {
		if strings.Contains(out+errs, bad) {
			t.Errorf("pause told the person to %q instead of waiting:\n%s%s", bad, out, errs)
		}
	}
	if _, _, reads := c.counts(); reads < 2 {
		t.Errorf("pause polled the session %d times; it must read it until it is parked", reads)
	}
	// a session that is already parked when it is asked needs no waiting
	c2, bin2, cfg2 := agentFixture(t)
	out, errs, code = auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "agent", "pause", "main", "--session", agentSessionShort, "--json")
	if code != 0 {
		t.Fatalf("pause (already parked): exit %d\n%s%s", code, out, errs)
	}
	data := parseEnvelope(t, out)["data"].(map[string]any)
	if data["runtime_state"] != "parked" || data["checkpoint_safe"] != true {
		t.Errorf("pause document: %v", data)
	}
	if _, _, reads := c2.counts(); reads != 0 {
		t.Errorf("a session that answered parked was polled %d times anyway", reads)
	}
}

// A stop that outlasts the bound is reported as a stop still in progress,
// with a way to keep waiting, and a non-zero exit: the save is safe, and
// "parked" would be a lie.
func TestAgentPauseTimesOutWithARetryPath(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.stopAfter = -1 // it never finishes stopping
	c.mu.Unlock()
	env := []string{"XDG_CONFIG_HOME=" + cfg, "KS_HTTP_TIMEOUT_MS=400", "KS_WAIT_TIMEOUT_MS=900"}
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "agent", "pause", "main", "--session", agentSessionShort)
	if code != exitTemporary {
		t.Fatalf("exit %d (want %d)\n%s%s", code, exitTemporary, out, errs)
	}
	for _, want := range []string{
		"is still stopping after",
		"the save it took is complete, and nothing else was started",
		"Remote work started: yes.",
		"Next: ks agent pause main --session " + agentSessionShort + " --wait-timeout 5m",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the timeout lacks %q:\n%s", want, errs)
		}
	}
	if strings.Contains(out, "parked") {
		t.Errorf("a session that never parked was reported as parked:\n%s", out)
	}
}

// Resume while the previous save is still completing is not an error to
// puzzle over: it is a wait. The client says so, waits for the stop to
// finish, and then resumes.
func TestAgentResumeWaitsOutTheStopThenResumes(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.resumeStopping = true
	c.stopAfter = 1
	c.mu.Unlock()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "resume", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"the previous save is still completing", "waiting for it to finish, then resuming", "finished stopping after"} {
		if !strings.Contains(errs, want) {
			t.Errorf("resume lacks %q:\n%s", want, errs)
		}
	}
	if !strings.Contains(out, "is resuming from its last save") {
		t.Errorf("resume did not report the session as resuming:\n%s", out)
	}
	if _, resumes, _ := c.counts(); resumes != 2 {
		t.Errorf("the resume was asked %d times; the refusal is retried once the stop is finished", resumes)
	}

	// and when the stop outlasts the bound, nothing was resumed and the
	// same ask is the way back
	c2, bin2, cfg2 := agentFixture(t)
	c2.mu.Lock()
	c2.resumeStopping = true
	c2.stopAfter = -1
	c2.mu.Unlock()
	env := []string{"XDG_CONFIG_HOME=" + cfg2, "KS_HTTP_TIMEOUT_MS=400", "KS_WAIT_TIMEOUT_MS=900"}
	out, errs, code = auditExec(t, bin2, cfg2, t.TempDir(), env, "agent", "resume", "main", "--session", agentSessionShort)
	if code != exitConflict {
		t.Fatalf("resume (still stopping): exit %d (want %d)\n%s%s", code, exitConflict, out, errs)
	}
	for _, want := range []string{"so nothing was resumed", "this can be asked again", "Remote work started: no.",
		"Next: ks agent resume main --session " + agentSessionShort} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, errs)
		}
	}
	if _, resumes, _ := c2.counts(); resumes != 1 {
		t.Errorf("a refused resume was asked %d times", resumes)
	}
}
