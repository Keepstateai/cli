// Deciding a permission request through ks mcp: an agent's waiting requests
// are listed, and approve or deny act only after the confirm step, for that
// exact request at its revision, once. A request already decided or expired
// is refused in plain words. Run in both protocol eras.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// approvalsCtl adds permission requests to the MCP fake.
type approvalsCtl struct {
	mcpCtl
	rows map[string]map[string]any
}

func (c approvalsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v2/approvals") {
		c.mcpCtl.ServeHTTP(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	data := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": v})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"schema_version":2,"error":{"code":%q,"type":%q,"message":%q,"work_started":"no"}}`, typ, typ, msg)
	}
	id := strings.TrimPrefix(strings.TrimSuffix(p, "/decision"), "/api/v2/approvals/")
	switch {
	case r.Method == "GET" && p == "/api/v2/approvals":
		items := []any{}
		for _, k := range []string{"apr_1", "apr_2", "apr_3", "apr_4", "apr_5", "apr_6"} {
			if row, ok := c.rows[k]; ok && row["session_id"] == r.URL.Query().Get("session_id") && row["decision"] == "pending" {
				items = append(items, row)
			}
		}
		data(200, map[string]any{"items": items})
	case r.Method == "GET":
		row, ok := c.rows[id]
		if !ok {
			refuse(404, "ks_not_found", "no such approval")
			return
		}
		data(200, row)
	case r.Method == "POST" && strings.HasSuffix(p, "/decision"):
		row, ok := c.rows[id]
		var body struct {
			Decision string  `json:"decision"`
			Revision float64 `json:"expected_revision"`
			Hash     string  `json:"action_hash"`
		}
		_ = json.Unmarshal(b, &body)
		_, strict := c.rows["_strict"] // a service that takes no "via" field
		switch {
		case strict && strings.Contains(string(b), `"via"`):
			refuse(400, "ks_body_unknown_field", "the request body carries a field this route does not take: unknown field \"via\"")
		case !ok:
			refuse(404, "ks_not_found", "no such approval")
		case row["decision"] != "pending":
			refuse(409, "ks_approval_not_pending", "already decided")
		case body.Revision != row["revision"].(float64) || body.Hash != row["exact_action_hash"]:
			refuse(409, "ks_revision_conflict", "the request moved")
		default:
			row["decision"] = map[string]string{"approve": "approved", "deny": "denied"}[body.Decision]
			row["revision"] = row["revision"].(float64) + 1
			data(200, map[string]any{"approval": row, "decision": row["decision"], "decided_at": "2026-11-04T10:00:00Z"})
		}
	default:
		refuse(404, "ks_not_found", "no such route")
	}
}

// postedDecisions are the decision requests the fake saw, with their bodies.
func (c approvalsCtl) postedDecisions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.reqs {
		if strings.HasPrefix(r, "POST /api/v2/approvals/") {
			out = append(out, r)
		}
	}
	return out
}

func approvalRowT(id, requester, decision string, revision float64, expires time.Time) map[string]any {
	return map[string]any{"id": id, "session_id": "session_1", "requester": requester, "action_kind": "Bash", "human_scope": "Run: npm test " + id,
		"arguments": map[string]any{"command": "npm test"}, "exact_action_hash": "hash_" + id, "revision": revision, "decision": decision,
		"expires_at": expires.UTC().Format(time.RFC3339), "requested_for_task": "task_1"}
}

func approvalsEnv(t *testing.T) (approvalsCtl, string, []string) {
	t.Helper()
	inner := newInteropCtl()
	inner.names["session_1"] = "review"
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Minute)
	c := approvalsCtl{mcpCtl: mcpCtl{inner}, rows: map[string]map[string]any{
		"apr_1": approvalRowT("apr_1", "agent:agent_1", "pending", 2, later),
		"apr_2": approvalRowT("apr_2", "agent:agent_1", "pending", 5, later),
		"apr_3": approvalRowT("apr_3", "agent:agent_1", "pending", 1, earlier),
		"apr_4": approvalRowT("apr_4", "agent:agent_1", "pending", 1, later),
		"apr_5": approvalRowT("apr_5", "agent:agent_1", "denied", 3, later),
		"apr_6": approvalRowT("apr_6", "agent:agent_other", "pending", 1, later),
	}}
	syncFixture(t, &inner.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	env := append(fastEnv(cfg), "KS_CALLER_ID=mcp-test", "HC_TERMINAL_ID=", "HERDR_PANE_ID=", "HOME="+t.TempDir())
	return c, bin, env
}

type mcpEra struct {
	name   string
	meta   map[string]any // the modern era's _meta; nil for legacy
	elicit bool
	open   func(c *mcpClient)
}

func eraOf(name string, elicit bool) mcpEra {
	caps := map[string]any{}
	if elicit {
		caps["elicitation"] = map[string]any{}
	}
	if name == "legacy" {
		return mcpEra{name: name, elicit: elicit, open: func(c *mcpClient) {
			c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps})
		}}
	}
	return mcpEra{name: name, elicit: elicit, meta: modernMeta(caps), open: func(*mcpClient) {}}
}

type confirmPlan struct {
	Needs   bool   `json:"needs_confirmation"`
	Plan    string `json:"plan"`
	Confirm string `json:"confirm"`
}

func planOf(t *testing.T, r map[string]any) confirmPlan {
	t.Helper()
	var p confirmPlan
	if err := json.Unmarshal([]byte(textOf(r)), &p); err != nil || !p.Needs || len(p.Confirm) != 16 {
		t.Fatalf("not a plan: %v", r)
	}
	return p
}

func withConfirm(a map[string]any, digest string) map[string]any {
	out := map[string]any{"confirm": digest}
	for k, v := range a {
		out[k] = v
	}
	return out
}

func eraCall(t *testing.T, c *mcpClient, era mcpEra, tool string, args map[string]any) map[string]any {
	t.Helper()
	r := resultOf(t, c.call("tools/call", toolCall(tool, args, era.meta)))
	if era.meta != nil && r["resultType"] == nil {
		t.Fatalf("a modern result without resultType: %v", r)
	}
	return r
}

var planWords = []string{"review", "agent_1", "Bash", "Run: npm test", "npm test", "lets"}

// TestMCPListsAnAgentsRequestsAndDeniesWithTheDigest: deny only blocks, so
// the digest confirm is enough for it.
func TestMCPListsAnAgentsRequestsAndDeniesWithTheDigest(t *testing.T) {
	for _, name := range []string{"legacy", "modern"} {
		era := eraOf(name, false)
		t.Run(name, func(t *testing.T) {
			fake, bin, env := approvalsEnv(t)
			decisions := func() int { return fake.count("POST /api/v2/approvals/") }
			c := startMCP(t, bin, env)
			era.open(c)
			l, _ := json.Marshal(resultOf(t, c.call("tools/list", map[string]any{"_meta": era.meta})))
			if !strings.Contains(string(l), `"ks_agent_approve"`) || !strings.Contains(string(l), `"ks_agent_deny"`) {
				t.Fatalf("tools/list has no approve or deny: %s", l)
			}
			r := eraCall(t, c, era, "ks_approval_list", map[string]any{"session": "session_1", "agent": "review"})
			if r["isError"] != false || !strings.Contains(textOf(r), "apr_1") || strings.Contains(textOf(r), "apr_6") {
				t.Fatalf("the agent's waiting requests: %v", r)
			}
			args := map[string]any{"request": "apr_4", "session": "session_1"}
			plan := planOf(t, eraCall(t, c, era, "ks_agent_deny", args))
			for _, w := range append(append([]string{}, planWords[:5]...), "Deny", "refused") {
				if !strings.Contains(plan.Plan, w) {
					t.Errorf("the deny plan does not name %q: %s", w, plan.Plan)
				}
			}
			if r := eraCall(t, c, era, "ks_agent_deny", withConfirm(args, "0000000000000000")); r["isError"] != true || !strings.Contains(textOf(r), "nothing was decided") {
				t.Fatalf("a wrong digest: %v", r)
			}
			if r := eraCall(t, c, era, "ks_agent_deny", withConfirm(map[string]any{"request": "apr_1", "session": "session_1"}, plan.Confirm)); r["isError"] != true {
				t.Fatalf("a digest for another request: %v", r)
			}
			if decisions() != 0 {
				t.Fatal("the plan or a refused digest decided something")
			}
			if r := eraCall(t, c, era, "ks_agent_deny", withConfirm(args, plan.Confirm)); r["isError"] != false || decisions() != 1 || !strings.Contains(fake.lastReq("POST /api/v2/approvals/apr_4/decision"), `"decision":"deny"`) {
				t.Fatalf("the confirmed deny: %v", r)
			}
			if r := eraCall(t, c, era, "ks_agent_deny", withConfirm(args, plan.Confirm)); r["isError"] != true || !strings.Contains(textOf(r), "already denied") || decisions() != 1 {
				t.Fatalf("a reused digest: %v", r)
			}
			// a request that moved after its plan: nothing
			moved := map[string]any{"request": "apr_2", "session": "session_1"}
			p2 := planOf(t, eraCall(t, c, era, "ks_agent_deny", moved))
			fake.mu.Lock()
			fake.rows["apr_2"]["revision"] = float64(6)
			fake.mu.Unlock()
			if r := eraCall(t, c, era, "ks_agent_deny", withConfirm(moved, p2.Confirm)); r["isError"] != true || decisions() != 1 {
				t.Fatalf("a digest for a request that moved: %v", r)
			}
			// expired, and already decided: plain words, no plan
			if r := eraCall(t, c, era, "ks_agent_deny", map[string]any{"request": "apr_3", "session": "session_1"}); r["isError"] != true || !strings.Contains(textOf(r), "expired") || strings.Contains(textOf(r), "needs_confirmation") {
				t.Fatalf("an expired request: %v", r)
			}
			if r := eraCall(t, c, era, "ks_agent_deny", map[string]any{"request": "apr_5", "session": "session_1"}); r["isError"] != true || !strings.Contains(textOf(r), "already denied") {
				t.Fatalf("a decided request: %v", r)
			}
		})
	}
}

// TestMCPApproveNeedsAPersonsAnswer: a host that cannot ask approves nothing,
// nor does a confirm value with no person behind it.
func TestMCPApproveNeedsAPersonsAnswer(t *testing.T) {
	for _, name := range []string{"legacy", "modern"} {
		era := eraOf(name, false)
		t.Run(name, func(t *testing.T) {
			fake, bin, env := approvalsEnv(t)
			c := startMCP(t, bin, env)
			era.open(c)
			args := map[string]any{"request": "apr_1", "session": "session_1"}
			r := eraCall(t, c, era, "ks_agent_approve", args)
			if r["isError"] != true || !strings.Contains(textOf(r), "needs a person's answer") || !strings.Contains(textOf(r), "KeepState console") ||
				!strings.Contains(textOf(r), "ks agent approve apr_1 --session session_1") || strings.Contains(textOf(r), "needs_confirmation") {
				t.Fatalf("a host without elicitation: %v", r)
			}
			// a digest-only call, with no human: refused
			deny := planOf(t, eraCall(t, c, era, "ks_agent_deny", args))
			if r := eraCall(t, c, era, "ks_agent_approve", withConfirm(args, deny.Confirm)); r["isError"] != true || !strings.Contains(textOf(r), "only a person's answer") {
				t.Fatalf("a confirm value given to approve: %v", r)
			}
			if fake.count("POST /api/v2/approvals/") != 0 {
				t.Fatal("approve without a person decided something")
			}
		})
	}
}

func TestMCPApproveModernAsksThePerson(t *testing.T) {
	era := eraOf("modern", true)
	fake, bin, env := approvalsEnv(t)
	decisions := func() int { return fake.count("POST /api/v2/approvals/") }
	c := startMCP(t, bin, env)
	args := map[string]any{"request": "apr_1", "session": "session_1"}
	ask := func(a map[string]any) (string, map[string]any) {
		t.Helper()
		r := eraCall(t, c, era, "ks_agent_approve", a)
		reqs, _ := r["inputRequests"].(map[string]any)
		q, _ := reqs["decision"].(map[string]any)
		params, _ := q["params"].(map[string]any)
		msg, _ := params["message"].(string)
		if r["resultType"] != "input_required" || q["method"] != "elicitation/create" || r["requestState"] == "" {
			t.Fatalf("the person is not asked: %v", r)
		}
		for _, w := range append(planWords, "Approve or Deny?") {
			if !strings.Contains(msg, w) {
				t.Errorf("the question does not name %q: %s", w, msg)
			}
		}
		return r["requestState"].(string), params["requestedSchema"].(map[string]any)
	}
	answer := func(a map[string]any, state string, resp map[string]any) map[string]any {
		t.Helper()
		p := toolCall("ks_agent_approve", a, era.meta)
		p["requestState"] = state
		p["inputResponses"] = map[string]any{"decision": resp}
		return resultOf(t, c.call("tools/call", p))
	}
	state, schema := ask(args)
	if !strings.Contains(fmt.Sprint(schema), "approve deny") {
		t.Fatalf("the question does not offer Approve or Deny: %v", schema)
	}
	if decisions() != 0 {
		t.Fatal("asking decided something")
	}
	// decline, and cancel: nothing
	if r := answer(args, state, map[string]any{"action": "decline"}); !strings.Contains(textOf(r), "declined") || decisions() != 0 {
		t.Fatalf("declined: %v", r)
	}
	state, _ = ask(args)
	if r := answer(args, state, map[string]any{"action": "cancel"}); !strings.Contains(textOf(r), "cancelled") || decisions() != 0 {
		t.Fatalf("cancelled: %v", r)
	}
	// accept: approves once
	state, _ = ask(args)
	accept := map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}
	if r := answer(args, state, accept); r["isError"] != false || decisions() != 1 || !strings.Contains(fake.lastReq("POST /api/v2/approvals/apr_1/decision"), `"decision":"approve"`) {
		t.Fatalf("accepted: %v", r)
	}
	// a replay of the same answer: refused
	if r := answer(args, state, accept); r["isError"] != true || decisions() != 1 {
		t.Fatalf("a replayed answer: %v", r)
	}
	// an answer about a request that moved after the question: refused
	moved := map[string]any{"request": "apr_2", "session": "session_1"}
	s2, _ := ask(moved)
	fake.mu.Lock()
	fake.rows["apr_2"]["revision"] = float64(6)
	fake.rows["apr_2"]["exact_action_hash"] = "hash_changed"
	fake.mu.Unlock()
	if r := answer(moved, s2, accept); r["isError"] != true || decisions() != 1 {
		t.Fatalf("an answer about a request that moved: %v", r)
	}
	// a made-up request state: refused
	if r := answer(args, "0000000000000000", accept); r["isError"] != true || decisions() != 1 {
		t.Fatalf("a made-up request state: %v", r)
	}
	// expired: plain words, the person is not asked
	if r := eraCall(t, c, era, "ks_agent_approve", map[string]any{"request": "apr_3", "session": "session_1"}); r["isError"] != true || !strings.Contains(textOf(r), "expired") {
		t.Fatalf("an expired request: %v", r)
	}
	// the person may answer Deny to the same question
	s4, _ := ask(map[string]any{"request": "apr_4", "session": "session_1"})
	if r := answer(map[string]any{"request": "apr_4", "session": "session_1"}, s4, map[string]any{"action": "accept", "content": map[string]any{"decision": "deny"}}); r["isError"] != false ||
		!strings.Contains(fake.lastReq("POST /api/v2/approvals/apr_4/decision"), `"decision":"deny"`) || decisions() != 2 {
		t.Fatalf("the person chose Deny: %v", r)
	}
}

func TestMCPApproveLegacyAsksThePerson(t *testing.T) {
	era := eraOf("legacy", true)
	fake, bin, env := approvalsEnv(t)
	decisions := func() int { return fake.count("POST /api/v2/approvals/") }
	c := startMCP(t, bin, env)
	era.open(c)
	run := func(req string, reply map[string]any) map[string]any {
		t.Helper()
		c.n++
		id := c.n
		c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": toolCall("ks_agent_approve", map[string]any{"request": req, "session": "session_1"}, nil)})
		for {
			m := c.next()
			if m["method"] == "elicitation/create" {
				msg := m["params"].(map[string]any)["message"].(string)
				for _, w := range append(planWords, "Approve or Deny?") {
					if !strings.Contains(msg, w) {
						t.Errorf("the question does not name %q: %s", w, msg)
					}
				}
				if decisions() != 0 && reply["action"] != "accept" {
					t.Fatal("asking decided something")
				}
				c.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": reply})
				continue
			}
			if f, ok := m["id"].(float64); ok && int(f) == id {
				return resultOf(t, m)
			}
		}
	}
	if r := run("apr_1", map[string]any{"action": "decline"}); !strings.Contains(textOf(r), "declined") || decisions() != 0 {
		t.Fatalf("declined: %v", r)
	}
	if r := run("apr_1", map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}); r["isError"] != false || decisions() != 1 {
		t.Fatalf("accepted: %v", r)
	}
	// asked again about the same request: it is decided, refused before asking
	if r := run("apr_1", map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}); r["isError"] != true || !strings.Contains(textOf(r), "already approved") || decisions() != 1 {
		t.Fatalf("a request already approved: %v", r)
	}
	if r := run("apr_3", map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}); r["isError"] != true || !strings.Contains(textOf(r), "expired") || decisions() != 1 {
		t.Fatalf("an expired request: %v", r)
	}
}

func TestMCPReadOnlyStillListsOnlyReads(t *testing.T) {
	fake, bin, env := approvalsEnv(t)
	c := startMCP(t, bin, env, "--read-only")
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(resultOf(t, c.call("tools/list", nil)))
	_ = json.Unmarshal(raw, &listed)
	names := map[string]bool{}
	for _, x := range listed.Tools {
		names[x.Name] = true
	}
	if names["ks_agent_approve"] || names["ks_agent_deny"] || !names["ks_approval_list"] {
		t.Fatalf("--read-only lists: %v", names)
	}
	for _, tool := range mcpTools {
		if names[tool.Name] != (tool.Kind == "read") {
			t.Errorf("--read-only and %s (%s)", tool.Name, tool.Kind)
		}
	}
	if r := resultOf(t, c.call("tools/call", toolCall("ks_agent_approve", map[string]any{"request": "apr_1", "session": "session_1"}, nil))); r["isError"] != true || !strings.Contains(textOf(r), "read-only") || fake.total() != 0 {
		t.Fatalf("--read-only approve: %v", r)
	}
}
