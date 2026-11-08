// Approving a permission request through ks mcp needs a person, and the
// record says which way it was decided. A confirm value, a digest or any
// other argument the calling model can write never approves; only a person's
// answer in the host app does, and the decision the service stores names
// how it was made: a person's answer through ks mcp, a confirm value the
// model passed (deny only), or nothing (a terminal or the console).
//
// Sabotage: KS_TEST_TAGS=ks_sabotage go test -run TestMCPModelSuppliedConfirmNeverApproves
// builds a binary that lets a model-supplied confirm approve, and that test
// must fail on its assertions.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPModelSuppliedConfirmNeverApproves: whatever the host is, nothing the
// model writes into a tool call decides a request as approved.
func TestMCPModelSuppliedConfirmNeverApproves(t *testing.T) {
	for _, name := range []string{"legacy", "modern"} {
		for _, elicit := range []bool{false, true} {
			era := eraOf(name, elicit)
			label := name + "/no-elicitation"
			if elicit {
				label = name + "/elicitation"
			}
			t.Run(label, func(t *testing.T) {
				fake, bin, env := approvalsEnv(t)
				c := startMCP(t, bin, env)
				era.open(c)
				args := map[string]any{"request": "apr_1", "session": "session_1"}
				// the digest the deny tool hands out, the one value a model can read
				plain := eraOf(name, false) // a host that cannot ask hands the digest out
				c0 := startMCP(t, bin, env)
				plain.open(c0)
				digest := planOf(t, eraCall(t, c0, plain, "ks_agent_deny", args)).Confirm
				attempts := []map[string]any{
					withConfirm(args, digest),
					withConfirm(args, "true"),
					withConfirm(args, "0000000000000000"),
					{"request": "apr_1", "session": "session_1", "decision": "approve"},
					{"request": "apr_1", "session": "session_1", "approve": true},
					{"request": "apr_1", "session": "session_1", "yes": true},
				}
				// the model's real route: ask approve for a plan, pass its confirm back
				ask := func(a map[string]any) map[string]any {
					if name == "legacy" && elicit {
						return askAndDecline(t, c, a)
					}
					return eraCall(t, c, era, "ks_agent_approve", a)
				}
				first := ask(args)
				if strings.Contains(textOf(first), "needs_confirmation") {
					t.Fatalf("approve handed the model a confirm value: %v", first)
				}
				var offered struct {
					Confirm string `json:"confirm"`
				}
				_ = json.Unmarshal([]byte(textOf(first)), &offered)
				if offered.Confirm != "" {
					attempts = append(attempts, withConfirm(args, offered.Confirm))
				}
				for _, a := range attempts {
					// twice: a first call may hold a step, a second must not complete it
					for i := 0; i < 2; i++ {
						r := ask(a)
						if r["isError"] == false && r["resultType"] != "input_required" {
							t.Fatalf("approve with %v answered without error: %v", a, r)
						}
					}
				}
				for _, line := range fake.postedDecisions() {
					if strings.Contains(line, `"decision":"approve"`) {
						t.Fatalf("a model-supplied value approved a request: %s", line)
					}
				}
			})
		}
	}
}

// askAndDecline calls the approve tool on a legacy host that can ask, and
// answers the question the way no person would have approved: declined.
func askAndDecline(t *testing.T, c *mcpClient, a map[string]any) map[string]any {
	t.Helper()
	c.n++
	id := c.n
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": toolCall("ks_agent_approve", a, nil)})
	for {
		m := c.next()
		if m["method"] == "elicitation/create" {
			c.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"action": "decline"}})
			continue
		}
		if f, ok := m["id"].(float64); ok && int(f) == id {
			return resultOf(t, m)
		}
	}
}

// TestMCPApprovalIsRecordedByHowItWasDecided: the decision's body names the
// way: an elicited answer, a model's confirm value, or (a terminal) nothing.
func TestMCPApprovalIsRecordedByHowItWasDecided(t *testing.T) {
	const person, model = `"via":"ks-mcp:elicitation"`, `"via":"ks-mcp:model-confirm"`
	era := eraOf("modern", true)
	fake, bin, env := approvalsEnv(t)
	// a value inherited from the model's own shell must never label a decision
	c := startMCP(t, bin, append(env, "KS_DECIDED_VIA=ks-mcp:elicitation"))
	answer := func(req, decision string) {
		t.Helper()
		a := map[string]any{"request": req, "session": "session_1"}
		r := eraCall(t, c, era, "ks_agent_approve", a)
		state, _ := r["requestState"].(string)
		p := toolCall("ks_agent_approve", a, era.meta)
		p["requestState"] = state
		p["inputResponses"] = map[string]any{"decision": map[string]any{"action": "accept", "content": map[string]any{"decision": decision}}}
		if r := resultOf(t, c.call("tools/call", p)); r["isError"] != false {
			t.Fatalf("%s %s: %v", decision, req, r)
		}
	}
	answer("apr_1", "approve")
	if got := fake.lastReq("POST /api/v2/approvals/apr_1/decision"); !strings.Contains(got, `"decision":"approve"`) || !strings.Contains(got, person) {
		t.Fatalf("an approval a person answered is not recorded as one: %s", got)
	}
	answer("apr_2", "deny")
	if got := fake.lastReq("POST /api/v2/approvals/apr_2/decision"); !strings.Contains(got, `"decision":"deny"`) || !strings.Contains(got, person) {
		t.Fatalf("a denial a person answered is not recorded as one: %s", got)
	}
	// a deny the model confirmed with the digest is recorded as the model's
	legacy := eraOf("legacy", false)
	c2 := startMCP(t, bin, append(env, "KS_DECIDED_VIA=ks-mcp:elicitation"))
	legacy.open(c2)
	args := map[string]any{"request": "apr_4", "session": "session_1"}
	plan := planOf(t, eraCall(t, c2, legacy, "ks_agent_deny", args))
	if r := eraCall(t, c2, legacy, "ks_agent_deny", withConfirm(args, plan.Confirm)); r["isError"] != false {
		t.Fatalf("the confirmed deny: %v", r)
	}
	if got := fake.lastReq("POST /api/v2/approvals/apr_4/decision"); !strings.Contains(got, model) || strings.Contains(got, person) {
		t.Fatalf("a deny the model confirmed is not recorded as the model's: %s", got)
	}
	// a terminal's decision names no channel
	cfg := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "XDG_CONFIG_HOME="); ok {
			cfg = v
		}
	}
	if _, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "agent", "deny", "apr_6", "--session", "session_1"); code != 0 {
		t.Fatalf("a terminal deny: %d %s", code, errs)
	}
	if got := fake.lastReq("POST /api/v2/approvals/apr_6/decision"); !strings.Contains(got, `"decision":"deny"`) || strings.Contains(got, `"via"`) {
		t.Fatalf("a terminal's decision carries a channel: %s", got)
	}
}

// TestMCPInstructionsNeverInviteAConfirmForApprove: the words the model reads
// do not say it can confirm an approval, and the approve tool takes no confirm.
func TestMCPInstructionsNeverInviteAConfirmForApprove(t *testing.T) {
	if strings.Contains(mcpInstructions, "ks_agent_approve and ks_agent_deny decide") || strings.Contains(mcpInstructions, "confirm only with their yes") {
		t.Fatalf("the instructions tell the model it may confirm an approval: %s", mcpInstructions)
	}
	if !strings.Contains(mcpInstructions, "ks_agent_approve") || !strings.Contains(mcpInstructions, "only a person") {
		t.Fatalf("the instructions do not say approving is a person's: %s", mcpInstructions)
	}
	tool := findMCPTool("ks_agent_approve")
	if _, has := tool.schema()["properties"].(map[string]any)["confirm"]; has {
		t.Fatal("the approve tool takes a confirm")
	}
	if strings.Contains(tool.Desc, "its confirm step") {
		t.Fatalf("the approve tool's description calls the person's answer a confirm step: %s", tool.Desc)
	}
}

// TestMCPApproveRefusedWhenTheServiceCannotRecordHow: a service that does not
// take the field refuses the decision; nothing is decided, and the person is
// told where to decide instead. An approval is never recorded as unlabelled
// through ks mcp.
func TestMCPApproveRefusedWhenTheServiceCannotRecordHow(t *testing.T) {
	era := eraOf("modern", true)
	fake, bin, env := approvalsEnv(t)
	fake.rows["_strict"] = map[string]any{}
	c := startMCP(t, bin, env)
	a := map[string]any{"request": "apr_1", "session": "session_1"}
	r := eraCall(t, c, era, "ks_agent_approve", a)
	p := toolCall("ks_agent_approve", a, era.meta)
	p["requestState"] = r["requestState"]
	p["inputResponses"] = map[string]any{"decision": map[string]any{"action": "accept", "content": map[string]any{"decision": "approve"}}}
	got := resultOf(t, c.call("tools/call", p))
	if got["isError"] != true || !strings.Contains(textOf(got), "nothing was decided") || !strings.Contains(textOf(got), "ks agent approve apr_1 --session session_1") {
		t.Fatalf("a service that cannot record how: %v", got)
	}
	if n := len(fake.postedDecisions()); n != 1 {
		t.Fatalf("the refused post count = %d, want the one refused attempt", n)
	}
}
