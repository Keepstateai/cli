// ks agent wait: the first frame that leaves working ends it, a note ends
// --children any once, all waits for every child, and a timeout is reason
// timeout with exit 0.
package main

import (
	"strings"
	"testing"
)

func TestAgentWaitReturnsAtTheFirstFrameThatIsNotWorking(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"working"}
	c.frames["session_1"] = []string{"working", "working", "blocked", "done"}
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "review", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	out, errs, code := run([]string{"KS_CALLER_ID=lead", "KS_WAIT_POLL_MS=10000"}, "agent", "wait", "review", "--json")
	if code != 0 {
		t.Fatalf("wait: %d\n%s%s", code, out, errs)
	}
	d := jsonData(t, out)
	st, _ := d["status"].(map[string]any)
	if d["reason"] != "status" || st["state"] != "blocked" || st["msg"] != "frame blocked" {
		t.Fatalf("the wait did not end at the first frame that left working: %v", d)
	}
	if !strings.Contains(c.first("GET /api/v2/sessions/session_1/events/stream"), "status=1") {
		t.Fatalf("the stream was not asked for status frames: %v", c.reqs)
	}
}

func TestAgentWaitTimeoutIsACheckIn(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "slow", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "wait", "slow", "--timeout", "1", "--json")
	if code != 0 || jsonData(t, out)["reason"] != "timeout" {
		t.Fatalf("a timeout: %d\n%s%s", code, out, errs)
	}
}

func TestAgentWaitWithoutTheStatusRouteReadsTheAgent(t *testing.T) {
	c := newInteropCtl()
	c.noStatus = true
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "old", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	out, _, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "wait", "old", "--timeout", "1", "--json")
	st, _ := jsonData(t, out)["status"].(map[string]any)
	if code != 0 || st["source"] != "client" || st["state"] != "working" {
		t.Fatalf("an older control plane: %d %v", code, st)
	}
}

func TestAgentWaitChildrenAnyReturnsOnANoteOnce(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"done"}
	c.notes = []map[string]any{{"seq": 1, "agent_id": "agent_1", "agent_name": "review", "session_id": "session_1", "kind": "turn_ended", "state": "done", "msg": "Finished instruction task_1."}}
	_, _, run := interopEnv(t, c)
	out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "wait", "--children", "any", "--timeout", "2", "--json")
	d := jsonData(t, out)
	note, _ := d["note"].(map[string]any)
	if code != 0 || d["reason"] != "message" || d["agent"] != "review" || note["msg"] != "Finished instruction task_1." {
		t.Fatalf("--children any: %d %v\n%s", code, d, errs)
	}
	// the same note is not answered twice
	out, _, code = run([]string{"KS_CALLER_ID=lead"}, "agent", "wait", "--children", "any", "--timeout", "1", "--json")
	if code != 0 || jsonData(t, out)["reason"] != "timeout" {
		t.Fatalf("a note seen once was answered again: %s", out)
	}
	if !strings.Contains(c.first("GET /api/v2/callers/cl_"), "/notes ") || c.count("GET /api/v2/callers/") < 2 || !strings.Contains(c.lastReq("GET /api/v2/callers/"), "after=1") {
		t.Fatalf("the cursor was not kept: %q", c.lastReq("GET /api/v2/callers/"))
	}
}

func TestAgentWaitChildrenAll(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_1"] = []string{"working", "working", "working", "working", "done"}
	c.status["agent_2"] = []string{"idle"}
	_, _, run := interopEnv(t, c)
	for _, n := range []string{"one", "two"} {
		if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", n, "t"); code != 0 {
			t.Fatalf("run %s: %d %s", n, code, errs)
		}
	}
	out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "wait", "--children", "all", "--timeout", "5", "--json")
	d := jsonData(t, out)
	kids, _ := d["children"].([]any)
	if code != 0 || d["reason"] != "status" || len(kids) != 2 {
		t.Fatalf("--children all: %d %v\n%s", code, d, errs)
	}
	for _, k := range kids {
		st := k.(map[string]any)["status"].(map[string]any)
		if st["state"] == "working" {
			t.Fatalf("all returned while a child works: %v", kids)
		}
	}
	// usage: a name and --children, or neither
	for _, bad := range [][]string{{"agent", "wait"}, {"agent", "wait", "one", "--children", "all"}, {"agent", "wait", "--children", "some"}} {
		if _, _, code := run(nil, bad...); code != exitUsage {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}
