// An error the person has read leaves "N need you". ks agent status shows
// the agent to a person, as the console's agent page does, so an error it
// shows is marked read on the service (POST /api/v2/agents/{id}/status/read)
// and leaves the count in ks agent list --all and the console on every
// device. The error is still listed, in the same words. What waits on an
// action (blocked) is never marked read, and neither is anything shown only
// as --json.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type readCtl struct {
	*statusCtl
	mu    sync.Mutex
	marks []string // the bodies of the read marks received
}

func (c *readCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v2/agents/") && strings.HasSuffix(r.URL.Path, "/status/read") {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.marks = append(c.marks, r.URL.Path+" "+string(b))
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"schema_version":2,"data":{"agent_id":"agt_main0001","state":"error","read":true,"needs_you":false}}`)
		return
	}
	c.statusCtl.ServeHTTP(w, r)
}

func (c *readCtl) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.marks...)
}

func readFixture(t *testing.T, record map[string]any) (*readCtl, string, string) {
	t.Helper()
	rec := ""
	if record != nil {
		b, _ := json.Marshal(record)
		rec = string(b)
	}
	c := &readCtl{statusCtl: &statusCtl{agentCtl: newAgentCtl(), record: rec}}
	syncFixture(t, &c.agentCtl.mu)
	syncFixture(t, &c.statusCtl.mu)
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return c, bin, cfg
}

func errRecord(extra map[string]any) map[string]any {
	r := map[string]any{"agent_id": "agt_main0001", "session_id": agentSessionRecord, "name": "main", "state": "error", "reason": "last_failed",
		"msg": "The last instruction failed: the tests did not pass.", "error_id": "err_0123456789abcdef", "needs_you": true, "source": "keepstate",
		"action": map[string]any{"label": "Read why", "verb": "ks task show tsk_1 --session " + agentSessionShort, "console": "#agent/x/y"}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func TestAgentStatusShowsAnErrorAndMarksItRead(t *testing.T) {
	c, bin, cfg := readFixture(t, errRecord(nil))
	out := runOK(t, bin, cfg, "agent", "status", "main", "--session", agentSessionShort)
	for _, want := range []string{
		"  error          The last instruction failed: the tests did not pass.",
		"  next           Read why: ks task show tsk_1 --session " + agentSessionShort,
		`  marked read    it no longer counts in "N need you"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if m := c.got(); len(m) != 1 || m[0] != `/api/v2/agents/agt_main0001/status/read {"error_id":"err_0123456789abcdef"}` {
		t.Errorf("marks %v", m)
	}
}

func TestAgentStatusMarksNothingElseRead(t *testing.T) {
	for name, tc := range map[string]struct {
		record map[string]any
		args   []string
	}{
		"read already":  {errRecord(map[string]any{"read": true, "needs_you": false}), nil},
		"no error id":   {errRecord(map[string]any{"error_id": nil}), nil},
		"a permission":  {map[string]any{"agent_id": "agt_main0001", "state": "blocked", "kind": "permission", "reason": "approval_pending", "msg": "Run: npm test", "needs_you": true}, nil},
		"json":          {errRecord(nil), []string{"--json"}},
		"older service": {nil, nil},
	} {
		c, bin, cfg := readFixture(t, tc.record)
		out := runOK(t, bin, cfg, append([]string{"agent", "status", "main", "--session", agentSessionShort}, tc.args...)...)
		if m := c.got(); len(m) != 0 {
			t.Errorf("%s: marked %v", name, m)
		}
		if strings.Contains(out, "marked read") {
			t.Errorf("%s: says marked read:\n%s", name, out)
		}
		// an error read already is still said, and says it is read
		if name == "read already" && (!strings.Contains(out, "  error          The last instruction failed") || !strings.Contains(out, "  read           yes")) {
			t.Errorf("a read error is no longer said:\n%s", out)
		}
	}
}

// The list counts what still needs the person: a read error is listed and
// counted as failed, not as needing you; the terminal's root status rests.
func TestAReadErrorLeavesTheNeedYouLine(t *testing.T) {
	unread := statusRecord{AgentID: "a", Name: "broke", State: "error", ErrorID: "e1", NeedsYou: true}
	read := statusRecord{AgentID: "b", Name: "done-with", State: "error", ErrorID: "e2", Read: true}
	asks := statusRecord{AgentID: "c", Name: "asks", State: "blocked", Kind: "permission", NeedsYou: true}
	if got := needYouLine(countRecords([]statusRecord{read})); got != "Nothing needs you \u00b7 1 failed" {
		t.Errorf("only a read error: %q", got)
	}
	if got := needYouLine(countRecords([]statusRecord{read, unread, asks})); got != "2 need you \u00b7 2 failed" {
		t.Errorf("a read error, an unread one and a permission: %q", got)
	}
	// an older service sends no read field: an error counts as before
	if c := countRecords([]statusRecord{{State: "error"}}); c.NeedsYou != 1 {
		t.Errorf("an error from an older service: %+v", c)
	}
	if r := aggregateStatus([]statusRecord{read}); r.State != "idle" {
		t.Errorf("root status with only a read error: %+v", r)
	}
	if r := aggregateStatus([]statusRecord{read, unread}); r.State != "error" {
		t.Errorf("root status with an unread error: %+v", r)
	}
	lines := strings.Join(allAgentsLines(agentStatusList{Items: []statusRecord{read}}), "\n")
	if !strings.Contains(lines, "Error ") || !strings.Contains(lines, "done-with") {
		t.Errorf("a read error is no longer listed:\n%s", lines)
	}
	ag := &agentRow{ID: "b", Name: "done-with"}
	if (homeRow{agent: ag, status: &read}).rowNeedsYou() || !(homeRow{agent: ag, status: &unread}).rowNeedsYou() {
		t.Errorf("the app's home list counts a read error, or not an unread one")
	}
}
