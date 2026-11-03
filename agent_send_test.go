// ks agent send: a fresh submission each call, the sender's tag, the --if
// refusal that sends nothing, and the permission request it queues behind.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestAgentSendIsANewInstructionEveryTime(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"done"}
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "review", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	for i := 0; i < 2; i++ {
		if out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "send", "review", "check the tests"); code != 0 || !strings.Contains(out, "sent: task") {
			t.Fatalf("send %d: %d\n%s%s", i, code, out, errs)
		}
	}
	sid := regexp.MustCompile(`"submission_id":"([0-9a-f]+)"`)
	var ids []string
	c.mu.Lock()
	for _, r := range c.reqs {
		if strings.HasPrefix(r, "POST /api/v2/agents/agent_1/tasks") && strings.Contains(r, "check the tests") {
			if m := sid.FindStringSubmatch(r); m != nil {
				ids = append(ids, m[1])
			}
			var body struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(r[strings.Index(r, "{"):]), &body)
			if body.Text != `<ks from="lead">check the tests</ks>` {
				t.Errorf("the text is not tagged with its sender: %s", r)
			}
		}
	}
	c.mu.Unlock()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("two sends of the same words are not two submissions: %v", ids)
	}
}

func TestAgentSendIfRefusesAndSendsNothing(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"working"}
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "busy", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	n := c.count("POST /api/v2/agents/agent_1/tasks")
	_, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "send", "busy", "more", "--if", "idle")
	if code != exitConflict || !strings.Contains(errs, "busy is working, not idle; nothing was sent") || c.count("POST /api/v2/agents/agent_1/tasks") != n {
		t.Fatalf("--if idle against a working agent: %d\n%s", code, errs)
	}
	if _, _, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "send", "busy", "more", "--if", "working"); code != 0 || c.count("POST /api/v2/agents/agent_1/tasks") != n+1 {
		t.Fatalf("--if working against a working agent: %d", code)
	}
	if _, _, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "send", "busy", "more", "--if", "sometimes"); code != exitUsage {
		t.Fatalf("--if outside the words: %d", code)
	}
}

func TestAgentSendQueuesBehindAPermissionRequest(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"blocked"}
	c.kinds["agent_1"] = "permission"
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "asks", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "send", "asks", "and then this")
	if code != 0 || !strings.Contains(out, "it waits behind the permission request: Run: npm test") || !strings.Contains(out, "this send decided nothing") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	if c.count("POST /api/v2/approvals") != 0 {
		t.Fatal("a send decided a permission request")
	}
}
