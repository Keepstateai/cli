// ks agent copy-to-org against a fake control plane, and the two
// ks mcp tools beside it. The command plans first and creates nothing; --yes or
// --execute carries out exactly a plan; every refusal the service gives is
// shown with what did NOT happen; the MCP copy tool needs a person's answer
// and no value the calling model writes ever copies.
//
// Sabotage: KS_TEST_TAGS=ks_sabotage go test -run TestMCPModelSuppliedValueNeverCopies
// builds a binary that lets a model-supplied confirm act, and that test must
// fail on its assertions.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// copyCtl adds the copy routes to the MCP fake (session_1 / agent_1 "review").
type copyCtl struct {
	mcpCtl
	digest     string // the digest the plan answers
	planRefuse [3]string
	copyRefuse [3]string
	library    map[string]any // the library outcome the copy answers, nil for none
	plans      int
}

func (c *copyCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v2/agents/agent_1/org-cop") {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": v})
	}
	refuse := func(ref [3]string) bool {
		if ref[0] == "" {
			return false
		}
		var code int
		fmt.Sscanf(ref[0], "%d", &code)
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"schema_version":2,"request_id":"r","error":{"code":%q,"type":%q,"message":%q,"retryable":false,"work_started":"no","next_action":""}}`, ref[1], ref[1], ref[2])
		return true
	}
	switch {
	case r.Method == "POST" && p == "/api/v2/agents/agent_1/org-copy-plan":
		if refuse(c.planRefuse) {
			return
		}
		c.plans++
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		var share any
		shareWords := "The copy stays among the organization's own agents, parked; you can add it to the pool later."
		if s, ok := req["share"].(map[string]any); ok {
			share = s
			shareWords = "The copy is also added to the organization's pool as @" + fmt.Sprint(s["handle"]) + ", version 1."
		}
		data(201, map[string]any{"id": fmt.Sprintf("acplan_%d", c.plans), "plan_digest": c.digest, "org_id": req["org_id"], "org_name": "Acme Inc.",
			"agent_id": "agent_1", "agent_name": "review", "name": "review session 2",
			"point":         map[string]any{"id": "ck_1", "taken_at": "2026-11-14T10:00:00Z", "words": "saved 3 hours ago (2026-11-14 10:00 UTC)"},
			"copies":        []string{"the agent's workspace as of its saved point"},
			"does_not_copy": []string{"your key bindings"},
			"warnings":      []string{"Everything in this agent's files becomes the organization's."},
			"who_can_see":   "you, who made the copy, and the organization's owners and admins",
			"source_stays":  "your agent stays yours and is not changed",
			"keys":          map[string]any{"managed": []string{}, "note": "your organization's own keys and defaults decide"},
			"storage":       map[string]any{"up_to_bytes": 46137344, "monthly_microusd": 4152, "words": "up to 44 MB stored for the organization"},
			"billing":       "the copy is the organization's: paid from the organization's credit",
			"share":         share, "share_words": shareWords,
			"earlier_copies": []any{map[string]any{"copied_at": "2026-11-13T09:00:00Z", "name": "review", "org_name": "Acme Inc."}},
			"state":          "planned"})
	case r.Method == "POST" && p == "/api/v2/agents/agent_1/org-copy":
		if refuse(c.copyRefuse) {
			return
		}
		res := map[string]any{"session_id": "session_copy1", "agent_ids": []string{"agent_copy1"}, "name": "review session 2", "org_id": "org_a", "org_name": "Acme Inc.",
			"checkpoint_id": "ck_copy1", "runtime_state": "parked", "note": "the copy is the organization's now"}
		if c.library != nil {
			res["library"] = c.library
		}
		data(201, res)
	case r.Method == "GET" && p == "/api/v2/agents/agent_1/org-copies":
		data(200, map[string]any{"copies": []any{map[string]any{"copied_at": "2026-11-13T09:00:00Z", "name": "review", "org_id": "org_a", "org_name": "Acme Inc."}}})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`))
	}
}

func (c *copyCtl) copies() int { return c.count("POST /api/v2/agents/agent_1/org-copy ") }
func (c *copyCtl) planCount() int {
	return c.count("POST /api/v2/agents/agent_1/org-copy-plan ")
}

func copyEnv(t *testing.T) (*copyCtl, string, string, []string) {
	t.Helper()
	inner := newInteropCtl()
	inner.names["session_1"] = "review"
	c := &copyCtl{mcpCtl: mcpCtl{inner}, digest: "sha256:aaaa1111"}
	syncFixture(t, &inner.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	env := append(fastEnv(cfg), "KS_CALLER_ID=mcp-test", "HC_TERMINAL_ID=", "HERDR_PANE_ID=", "HOME="+t.TempDir())
	return c, bin, cfg, env
}

func runCopy(t *testing.T, bin, cfg string, env []string, args ...string) (string, string, int) {
	t.Helper()
	full := append([]string{"agent", "copy-to-org", "review", "--session", "session_1"}, args...)
	return auditExec(t, bin, cfg, t.TempDir(), env, full...)
}

func TestAgentCopyToOrgPlansFirstAndCreatesNothing(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	out, errs, code := runCopy(t, bin, cfg, env, "--organization", "org_a")
	if code != 0 {
		t.Fatalf("the plan: %d %s %s", code, out, errs)
	}
	if c.copies() != 0 || c.planCount() != 1 {
		t.Fatalf("a plan made %d copies and %d plans", c.copies(), c.planCount())
	}
	for _, want := range []string{"NOTHING was created", "saved 3 hours ago", "Acme Inc.", "your key bindings", "WARNING: Everything in this agent's files becomes the organization's",
		"owners and admins", "up to 44 MB", "paid from the organization's credit", "your agent stays yours", "earlier", "sha256:aaaa1111",
		"--execute sha256:aaaa1111 --plan-id acplan_1", "--yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
	if body := c.first("POST /api/v2/agents/agent_1/org-copy-plan"); !strings.Contains(body, `"org_id":"org_a"`) || strings.Contains(body, "share") {
		t.Errorf("the plan request: %s", body)
	}
	// the point, the name, the project and the key choice travel
	_, _, code = runCopy(t, bin, cfg, env, "--organization", "org_a", "--point", "ck_9", "--name", "notes copy", "--project", "proj_1", "--managed", "anthropic", "--managed", "openai")
	if code != 0 {
		t.Fatal("the plan with options failed")
	}
	body := c.reqs[len(c.reqs)-1]
	for _, want := range []string{`"checkpoint_id":"ck_9"`, `"name":"notes copy"`, `"project_id":"proj_1"`, `"managed":["anthropic","openai"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("the plan request lacks %s: %s", want, body)
		}
	}
}

func TestAgentCopyToOrgYesCopiesExactlyThePlanShown(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	out, errs, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--yes")
	if code != 0 {
		t.Fatalf("--yes: %d %s %s", code, out, errs)
	}
	if c.planCount() != 1 || c.copies() != 1 {
		t.Fatalf("plans %d, copies %d", c.planCount(), c.copies())
	}
	if got := c.first("POST /api/v2/agents/agent_1/org-copy "); !strings.Contains(got, `"plan_id":"acplan_1"`) || !strings.Contains(got, `"plan_digest":"sha256:aaaa1111"`) {
		t.Errorf("the copy cites %s", got)
	}
	for _, want := range []string{"WARNING:", "copied agent review into Acme Inc.", "session_copy1", "parked", "ck_copy1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the result does not say %q:\n%s", want, out)
		}
	}
}

func TestAgentCopyToOrgExecuteByDigest(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	// with the plan id: that exact plan, no new plan
	if out, errs, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--execute", "sha256:aaaa1111", "--plan-id", "acplan_7"); code != 0 {
		t.Fatalf("execute: %d %s %s", code, out, errs)
	}
	if c.planCount() != 0 || c.copies() != 1 || !strings.Contains(c.first("POST /api/v2/agents/agent_1/org-copy "), `"plan_id":"acplan_7"`) {
		t.Fatalf("plans %d, copies %d", c.planCount(), c.copies())
	}
	// without it: a fresh plan with the same digest is carried out
	if _, _, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--execute", "sha256:aaaa1111"); code != 0 || c.planCount() != 1 || c.copies() != 2 {
		t.Fatalf("a fresh plan with the same digest: code %d, plans %d, copies %d", code, c.planCount(), c.copies())
	}
	// a plan that is not the one named now: refused, nothing copied
	c.digest = "sha256:bbbb2222"
	_, errs, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--execute", "sha256:aaaa1111")
	if code != exitConflict || !strings.Contains(errs, "changed since it was shown") || !strings.Contains(errs, "nothing was copied") || c.copies() != 2 {
		t.Fatalf("a changed plan: %d %s (copies %d)", code, errs, c.copies())
	}
}

func TestAgentCopyToOrgRefusalsSayWhatDidNotHappen(t *testing.T) {
	for _, row := range []struct {
		at, code, typ, msg, want string
	}{
		{"plan", "422", "ks_copy_not_personal", "this agent already belongs to an organization", "already belongs to an organization"},
		{"plan", "409", "ks_copy_no_point", "this agent has no saved point yet", "no saved point"},
		{"plan", "402", "ks_organization_no_credit", "this organization has no credit; nothing was planned", "no credit"},
		{"plan", "422", "ks_library_off", "the pool is not switched on", "not switched on"},
		{"plan", "409", "ks_library_handle_taken", "another entry already has the handle", "handle"},
		{"copy", "409", "ks_plan_stale", "this copy plan is executed; plan again", "plan again"},
		{"copy", "409", "ks_plan_digest_mismatch", "plan_digest must be the digest of the plan you were shown", "nothing was copied"},
		{"copy", "409", "ks_checkpoint_not_retained", "the engine no longer holds the planned saved point", "nothing was copied"},
		{"copy", "402", "ks_organization_no_credit", "this organization has no credit", "no credit"},
	} {
		t.Run(row.at+" "+row.typ, func(t *testing.T) {
			c, bin, cfg, env := copyEnv(t)
			ref := [3]string{row.code, row.typ, row.msg}
			if row.at == "plan" {
				c.planRefuse = ref
			} else {
				c.copyRefuse = ref
			}
			_, errs, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--yes")
			if code == 0 || !strings.Contains(errs, row.want) {
				t.Fatalf("%s: exit %d, %q", row.typ, code, errs)
			}
			if row.at == "copy" && (row.typ == "ks_plan_stale" || row.typ == "ks_plan_digest_mismatch" || row.typ == "ks_checkpoint_not_retained") &&
				(!strings.Contains(errs, "nothing was copied") || !strings.Contains(errs, "plan again")) {
				t.Errorf("a stale plan is told to plan again with what did not happen: %q", errs)
			}
		})
	}
}

func TestAgentCopyToOrgChecksBeforeAnythingIsSent(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	for name, args := range map[string][]string{
		"share without handle":     {"--organization", "org_a", "--share", "--title", "T"},
		"share without title":      {"--organization", "org_a", "--share", "--handle", "h"},
		"handle without share":     {"--organization", "org_a", "--handle", "h"},
		"plan id without execute":  {"--organization", "org_a", "--plan-id", "acplan_1"},
		"no organization anywhere": {},
		"an empty execute":         {"--organization", "org_a", "--execute", " "},
	} {
		if _, errs, code := runCopy(t, bin, cfg, env, args...); code != exitUsage || !strings.Contains(errs, "nothing was copied") {
			t.Errorf("%s: exit %d %q", name, code, errs)
		}
	}
	if c.planCount() != 0 || c.copies() != 0 {
		t.Errorf("a refused call sent %d plans and %d copies", c.planCount(), c.copies())
	}
}

func TestAgentCopyToOrgShareStep(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	c.library = map[string]any{"done": true, "entry_id": "lib_1", "handle": "notes", "version_id": "libv_1", "version": 1}
	share := []string{"--organization", "org_a", "--share", "--handle", "@notes", "--title", "Notes", "--description", "my notes", "--tag", "a", "--tag", "b", "--show-instructions", "--yes"}
	out, errs, code := runCopy(t, bin, cfg, env, share...)
	if code != 0 || !strings.Contains(out, "added as @notes, version v1") {
		t.Fatalf("a share: %d %s %s", code, out, errs)
	}
	body := c.first("POST /api/v2/agents/agent_1/org-copy-plan")
	for _, want := range []string{`"share":{`, `"handle":"notes"`, `"title":"Notes"`, `"description":"my notes"`, `"tags":["a","b"]`, `"show_instructions":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("the plan request lacks %s: %s", want, body)
		}
	}
	// the share failed at its second step: the copy is reported, and the exit is not success
	c.library = map[string]any{"done": false, "entry_id": "lib_1", "handle": "notes", "step": "publish", "code": "ks_library_source_not_running", "why": "the agent is stopped"}
	out, errs, code = runCopy(t, bin, cfg, env, share...)
	if code != exitFailed || !strings.Contains(out, "NOT added") || !strings.Contains(out, "publish") || !strings.Contains(errs, "the copy was made (session session_copy1)") || !strings.Contains(errs, "the agent is stopped") {
		t.Fatalf("a failed share: %d\nstdout %s\nstderr %s", code, out, errs)
	}
	// in JSON the partly done copy is one document, the error's
	out, errs, code = runCopy(t, bin, cfg, env, append(share, "--json")...)
	if code != exitFailed || strings.Count(out+errs, `"schema_version"`) != 1 || !strings.Contains(out+errs, "partly_done") || !strings.Contains(out+errs, "session_copy1") {
		t.Fatalf("a failed share as JSON: %d\n%s\n%s", code, out, errs)
	}
}

func TestAgentCopyToOrgJSONAndCopies(t *testing.T) {
	c, bin, cfg, env := copyEnv(t)
	out, _, code := runCopy(t, bin, cfg, env, "--organization", "org_a", "--json")
	var plan struct {
		Data struct {
			PlanDigest string `json:"plan_digest"`
			ID         string `json:"id"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || plan.Data.PlanDigest != "sha256:aaaa1111" || plan.Data.ID != "acplan_1" {
		t.Fatalf("the plan as JSON: %d %s", code, out)
	}
	out, _, code = runCopy(t, bin, cfg, env, "--organization", "org_a", "--yes", "--json")
	var res struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Data.SessionID != "session_copy1" {
		t.Fatalf("the copy as JSON: %d %s", code, out)
	}
	_ = c
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), env, "agent", "copies", "review", "--session", "session_1")
	if code != 0 || !strings.Contains(out, "Acme Inc.") || !strings.Contains(out, "2026-11-13") {
		t.Fatalf("copies: %d %s", code, out)
	}
}

// ---- ks mcp ------------------------------------------------------------------------------------

func copyToolArgs() map[string]any {
	return map[string]any{"agent": "review", "session": "session_1", "organization": "org_a"}
}

func TestMCPCopyToolsAreListedAndTheConfirmToolTakesNoConfirm(t *testing.T) {
	plan, ask := findMCPTool("ks_agent_copy_to_org_plan"), findMCPTool("ks_agent_copy_to_org")
	if plan == nil || ask == nil {
		t.Fatal("the copy tools are not listed")
	}
	if !ask.Human || ask.Kind != "confirm" || plan.Kind == "confirm" {
		t.Errorf("the copy tool is human %v kind %s; the plan tool is %s", ask.Human, ask.Kind, plan.Kind)
	}
	props := ask.schema()["properties"].(map[string]any)
	if _, has := props["confirm"]; has {
		t.Error("the copy tool takes a confirm value")
	}
	if _, has := props["digest"]; !has {
		t.Error("the copy tool takes no digest")
	}
	if got := strings.Join(ask.argv(map[string]any{"agent": "review", "session": "s", "organization": "org_a", "digest": "sha256:x"}, true), " "); got != "agent copy-to-org --json --no-input --session=s --organization=org_a --execute=sha256:x -- review" {
		t.Errorf("the copy tool's argv: %q", got)
	}
	if strings.Contains(strings.Join(copyPlanTarget(map[string]any{"agent": "review", "organization": "org_a", "digest": "sha256:x"}), " "), "--execute") {
		t.Error("the confirm step's plan read carries the digest as an execute")
	}
}

func TestMCPCopyPlanToolOnlyPlans(t *testing.T) {
	c, bin, _, env := copyEnv(t)
	cl := startMCP(t, bin, env)
	eraOf("legacy", false).open(cl)
	r := resultOf(t, cl.call("tools/call", toolCall("ks_agent_copy_to_org_plan", copyToolArgs(), nil)))
	if r["isError"] != false || !strings.Contains(textOf(r), "sha256:aaaa1111") || !strings.Contains(textOf(r), `"state":"planned"`) || !strings.Contains(textOf(r), "up_to_bytes") {
		t.Fatalf("the plan tool: %v", r)
	}
	if c.copies() != 0 || c.planCount() != 1 {
		t.Fatalf("the plan tool made %d copies and %d plans", c.copies(), c.planCount())
	}
}

// a person's answer to the copy tool in the modern era
func answerCopy(t *testing.T, cl *mcpClient, era mcpEra, args map[string]any, action, decision string) map[string]any {
	t.Helper()
	r := eraCall(t, cl, era, "ks_agent_copy_to_org", args)
	if r["resultType"] != "input_required" {
		return r // refused before any question
	}
	p := toolCall("ks_agent_copy_to_org", args, era.meta)
	p["requestState"] = r["requestState"]
	content := map[string]any{}
	if decision != "" {
		content["decision"] = decision
	}
	p["inputResponses"] = map[string]any{"decision": map[string]any{"action": action, "content": content}}
	return resultOf(t, cl.call("tools/call", p))
}

func TestMCPCopyNeedsAPersonsApprove(t *testing.T) {
	era := eraOf("modern", true)
	args := withDigest(copyToolArgs(), "sha256:aaaa1111")
	t.Run("approve copies exactly the plan", func(t *testing.T) {
		c, bin, _, env := copyEnv(t)
		cl := startMCP(t, bin, env)
		r := answerCopy(t, cl, era, args, "accept", "approve")
		if r["isError"] != false || c.copies() != 1 || c.planCount() < 2 {
			t.Fatalf("approve: %v (copies %d, plans %d)", r, c.copies(), c.planCount())
		}
		if !strings.Contains(c.first("POST /api/v2/agents/agent_1/org-copy "), `"plan_digest":"sha256:aaaa1111"`) {
			t.Errorf("the copy cites %s", c.first("POST /api/v2/agents/agent_1/org-copy "))
		}
	})
	t.Run("the question the person reads", func(t *testing.T) {
		_, bin, _, env := copyEnv(t)
		cl := startMCP(t, bin, env)
		r := eraCall(t, cl, era, "ks_agent_copy_to_org", args)
		raw, _ := json.Marshal(r)
		for _, want := range []string{"Copy agent review into Acme Inc.", "saved 3 hours ago", "WARNING", "becomes the organization", "owners and admins", "paid from the organization", "Approve or Deny"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("the person is not asked about %q: %s", want, raw)
			}
		}
	})
	t.Run("deny, decline and cancel copy nothing", func(t *testing.T) {
		for _, a := range [][2]string{{"accept", "deny"}, {"decline", ""}, {"cancel", ""}, {"accept", ""}} {
			c, bin, _, env := copyEnv(t)
			cl := startMCP(t, bin, env)
			r := answerCopy(t, cl, era, args, a[0], a[1])
			if c.copies() != 0 || !strings.Contains(textOf(r), "nothing was copied") && !strings.Contains(textOf(r), "nothing was decided") {
				t.Errorf("%v: %v (copies %d)", a, r, c.copies())
			}
		}
	})
	t.Run("a plan that changed since it was planned is refused with no question", func(t *testing.T) {
		c, bin, _, env := copyEnv(t)
		c.digest = "sha256:bbbb2222"
		cl := startMCP(t, bin, env)
		r := eraCall(t, cl, era, "ks_agent_copy_to_org", args)
		if r["isError"] != true || !strings.Contains(textOf(r), "changed since it was planned") || c.copies() != 0 || r["resultType"] == "input_required" {
			t.Fatalf("a changed plan: %v", r)
		}
	})
	t.Run("a host that cannot ask copies nothing", func(t *testing.T) {
		c, bin, _, env := copyEnv(t)
		cl := startMCP(t, bin, env)
		plain := eraOf("legacy", false)
		plain.open(cl)
		r := eraCall(t, cl, plain, "ks_agent_copy_to_org", args)
		if r["isError"] != true || !strings.Contains(textOf(r), "needs a person's answer") || !strings.Contains(textOf(r), "ks agent copy-to-org") || c.copies() != 0 {
			t.Fatalf("a host that cannot ask: %v", r)
		}
	})
}

func withDigest(a map[string]any, d string) map[string]any {
	out := map[string]any{"digest": d}
	for k, v := range a {
		out[k] = v
	}
	return out
}

// TestMCPModelSuppliedValueNeverCopies: whatever the host is, nothing the
// model writes into a tool call copies an agent. The sabotage build lets a
// model's confirm act; this fails there.
func TestMCPModelSuppliedValueNeverCopies(t *testing.T) {
	for _, name := range []string{"legacy", "modern"} {
		era := eraOf(name, false)
		t.Run(name, func(t *testing.T) {
			c, bin, _, env := copyEnv(t)
			cl := startMCP(t, bin, env)
			era.open(cl)
			args := withDigest(copyToolArgs(), "sha256:aaaa1111")
			first := eraCall(t, cl, era, "ks_agent_copy_to_org", args)
			if strings.Contains(textOf(first), "needs_confirmation") {
				t.Fatalf("the copy tool handed the model a confirm value: %v", first)
			}
			var offered struct {
				Confirm string `json:"confirm"`
			}
			_ = json.Unmarshal([]byte(textOf(first)), &offered)
			attempts := []map[string]any{
				withConfirm(args, "true"), withConfirm(args, "0000000000000000"), withConfirm(args, "sha256:aaaa1111"),
				withExtra(args, "decision", "approve"), withExtra(args, "yes", true),
			}
			if offered.Confirm != "" {
				attempts = append(attempts, withConfirm(args, offered.Confirm))
			}
			for _, a := range attempts {
				for i := 0; i < 2; i++ { // a first call may hold a step; a second must not complete it
					r := eraCall(t, cl, era, "ks_agent_copy_to_org", a)
					if r["isError"] == false && r["resultType"] != "input_required" {
						t.Fatalf("the copy tool with %v answered without error: %v", a, r)
					}
				}
			}
			if c.copies() != 0 {
				t.Fatalf("a model-supplied value copied an agent (%d copies)", c.copies())
			}
		})
	}
}

func withExtra(a map[string]any, k string, v any) map[string]any {
	out := map[string]any{k: v}
	for kk, vv := range a {
		out[kk] = vv
	}
	return out
}

// TestMCPCopyInstructionsAndDescription: what the model reads says the copy is
// a person's decision and that the files become the organization's.
func TestMCPCopyInstructionsAndDescription(t *testing.T) {
	ask := findMCPTool("ks_agent_copy_to_org")
	for _, want := range []string{"cannot copy on its own", "no confirm value", "becomes the organization's"} {
		if !strings.Contains(ask.Desc, want) {
			t.Errorf("the copy tool's description lacks %q", want)
		}
	}
}
