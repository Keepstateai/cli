package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestMCPImportSessionPlansThenCreatesOnlyOnConfirm(t *testing.T) {
	e := newImportEnv(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	env := append(append([]string{}, e.env...), "KS_WAIT_POLL_MS=50", "KS_CALLER_ID=mcp-test", "HC_TERMINAL_ID=", "HERDR_PANE_ID=")
	c := startMCP(t, e.bin, env)
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	args := map[string]any{"harness": "claude-code", "session": "latest", "name": "from mcp"}

	// the tool is listed with a schema that holds no key or secret
	tools, _ := resultOf(t, c.call("tools/list", nil))["tools"].([]any)
	var def map[string]any
	for _, x := range tools {
		if x.(map[string]any)["name"] == "ks_import_session" {
			def = x.(map[string]any)
		}
	}
	if def == nil {
		t.Fatal("ks_import_session is not listed")
	}
	props := def["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, want := range []string{"harness", "session", "project", "with_files", "brief", "name", "model", "confirm"} {
		if _, ok := props[want]; !ok {
			t.Errorf("the schema lacks %s", want)
		}
	}
	for name := range props {
		for _, bad := range []string{"key", "token", "secret", "password"} {
			if strings.Contains(strings.ToLower(name), bad) {
				t.Errorf("a %q property: %s", bad, name)
			}
		}
	}
	if ann := def["annotations"].(map[string]any); ann["readOnlyHint"] != false || ann["destructiveHint"] != false {
		t.Errorf("annotations: %v", ann)
	}

	// the first call plans: nothing is created, and the plan is the person's to read
	r := resultOf(t, c.call("tools/call", toolCall("ks_import_session", args, nil)))
	var plan struct {
		Needs   bool   `json:"needs_confirmation"`
		Plan    string `json:"plan"`
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(textOf(r)), &plan); err != nil || !plan.Needs || len(plan.Confirm) != 16 {
		t.Fatalf("the first call is a plan: %v %v", r, err)
	}
	for _, want := range []string{"NEW KeepState agent session", "its conversation goes up", "Secrets replaced on this machine", "1 connection_string", "re-reads the whole imported conversation", "digest "} {
		if !strings.Contains(plan.Plan, want) {
			t.Errorf("the plan does not say %q:\n%s", want, plan.Plan)
		}
	}
	allFakesAbsent(t, "the plan", []byte(plan.Plan))
	fixtureBarrier()
	for _, none := range []string{"POST /api/v2/preflight", "POST /api/v2/sessions", "POST /api/v2/imports/apply"} {
		if e.c.index(none) >= 0 {
			t.Fatalf("the first call asked %s", none)
		}
	}
	// a wrong confirm creates nothing
	wrong := map[string]any{"confirm": "0000000000000000"}
	for k, v := range args {
		wrong[k] = v
	}
	if r := resultOf(t, c.call("tools/call", toolCall("ks_import_session", wrong, nil))); r["isError"] != true || !strings.Contains(textOf(r), "nothing was created") {
		t.Fatalf("a wrong confirm: %v", r)
	}
	fixtureBarrier()
	if e.c.countExact("POST /api/v2/sessions") > 0 {
		t.Fatal("a wrong confirm created a session")
	}
	// the right one imports once
	right := map[string]any{"confirm": plan.Confirm}
	for k, v := range args {
		right[k] = v
	}
	r = resultOf(t, c.call("tools/call", toolCall("ks_import_session", right, nil)))
	if r["isError"] != false || !strings.Contains(textOf(r), `"import_id":"imp_1"`) {
		t.Fatalf("the confirmed call: %v", r)
	}
	fixtureBarrier()
	if n := e.c.countExact("POST /api/v2/sessions"); n != 1 || len(e.c.applies) != 1 {
		t.Fatalf("one session and one apply: %d, %d", n, len(e.c.applies))
	}
	conv := e.c.upload("conversation")
	if conv == nil || !conv.Done {
		t.Fatalf("the conversation was not uploaded: %+v", conv)
	}
	allFakesAbsent(t, "what the service was sent", e.c.bodies.Bytes())
	// the confirm value acts once
	if r := resultOf(t, c.call("tools/call", toolCall("ks_import_session", right, nil))); r["isError"] != true || e.c.count("POST /api/v2/imports/apply") != 1 {
		t.Fatalf("a confirm acted twice: %v", r)
	}
}

func TestMCPImportSessionRefusesAConfirmForASessionThatMoved(t *testing.T) {
	e := newImportEnv(t)
	p := e.session("claude-code", "claude-code-2.1.312.jsonl", "")
	env := append(append([]string{}, e.env...), "KS_WAIT_POLL_MS=50", "KS_CALLER_ID=mcp-test", "HC_TERMINAL_ID=", "HERDR_PANE_ID=")
	c := startMCP(t, e.bin, env)
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	args := map[string]any{"harness": "claude-code"}
	r := resultOf(t, c.call("tools/call", toolCall("ks_import_session", args, nil)))
	var plan struct {
		Confirm string `json:"confirm"`
	}
	_ = json.Unmarshal([]byte(textOf(r)), &plan)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"parentUuid":null,"isSidechain":false,"type":"user","uuid":"late","cwd":"/Users/alice/projects/shop","sessionId":"` + claudeSID + `","message":{"role":"user","content":"one more thing"}}` + "\n")
	f.Close()
	right := map[string]any{"confirm": plan.Confirm, "harness": "claude-code"}
	if r := resultOf(t, c.call("tools/call", toolCall("ks_import_session", right, nil))); r["isError"] != true || !strings.Contains(textOf(r), "nothing was created") {
		t.Fatalf("a confirm for a session that moved: %v", r)
	}
	fixtureBarrier()
	if e.c.countExact("POST /api/v2/sessions") > 0 {
		t.Fatal("a stale confirm created a session")
	}
}

func TestMCPImportSessionArgvIsTheVerbsOwn(t *testing.T) {
	a := map[string]any{"harness": "cursor", "session": "3f2a1c", "with_files": true, "name": "n", "project": "/p"}
	if got := strings.Join(importTargetArgv(a), " "); got != "import cursor --chat=3f2a1c --json --no-input --project=/p --name=n --with-files --plan" {
		t.Fatalf("plan argv: %q", got)
	}
	tool := findMCPTool("ks_import_session")
	argv := strings.Join(tool.argv(map[string]any{"harness": "codex", "brief": true}, false), " ")
	if argv != "import codex --latest --json --no-input --brief" {
		t.Fatalf("argv: %q", argv)
	}
	importDigests.Lock()
	importDigests.m = map[string]string{canonicalArgs(map[string]any{"harness": "codex", "brief": true}): strings.Repeat("a", 64)}
	importDigests.Unlock()
	if got := strings.Join(tool.argv(map[string]any{"harness": "codex", "brief": true}, true), " "); got != "import codex --latest --json --no-input --brief --execute="+strings.Repeat("a", 64) {
		t.Fatalf("confirmed argv: %q", got)
	}
}
