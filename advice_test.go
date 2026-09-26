// KS-066 client: attributed advice and honest disagreement. The fake
// control plane serves the two reads exactly as ctl/workspace_advice_view.go
// shapes them. QA-066-1 (a missing opinion is named), QA-066-2 (disagreement
// kept, no consensus), QA-066-3 (advice on an ended instruction is history),
// the gate, the task show summary, and the window drawer.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type adviceCtl struct {
	mu        sync.Mutex
	requests  []string
	caps      string // extra capability rows
	scenario  string // "timeout", "disagree", "history"
	adviceErr bool
}

func party(id, name, sess string) map[string]any {
	return map[string]any{"agent_id": id, "agent_name": name, "session_id": "ses_" + sess, "session_name": sess}
}

func consult(id, state, label, adviserID, adviserName, standing string, reply string) map[string]any {
	c := map[string]any{"id": id, "state": state, "label": label,
		"asker": party("agt_main0001", "main", "checkout"), "adviser": party(adviserID, adviserName, "review"),
		"source_task_id": "tsk_q", "task_standing": standing, "requested_at": "2026-09-26T10:00:00Z", "deadline": "2026-09-26T10:10:00Z",
		"context":  map[string]any{"policy": "question_only", "hashes": []string{"sha256:ctx1"}, "grant_id": "grt_1", "grant_revision": 3},
		"question": "is the migration reversible?", "response": nil, "note": "no reply yet; nothing is shown in its place"}
	if reply != "" {
		c["delivered_at"], c["received_at"] = "2026-09-26T10:01:00Z", "2026-09-26T10:02:00Z"
		c["response"] = map[string]any{"text": reply, "by": adviserID, "attempt_id": "att_" + adviserName, "verification": "unverified"}
		c["note"] = "the adviser's own words, attributed; advice is unverified and authorises nothing"
	}
	return c
}

func (c *adviceCtl) set() map[string]any {
	standing, state := "current", "running"
	var cs []map[string]any
	out := map[string]any{"task_id": "tsk_q", "pending": []string{}, "unavailable": []string{}}
	switch c.scenario {
	case "timeout":
		cs = []map[string]any{
			consult("csl_a", "replied", "Replied", "agt_rev1", "alice", standing, "yes: the down migration exists"),
			consult("csl_b", "expired", "Unavailable (no reply before the deadline)", "agt_rev2", "bob", standing, ""),
		}
		out["replied"], out["complete"] = 1, false
		out["unavailable"] = []string{"bob (review, agt_rev2): unavailable (no reply before the deadline)"}
		out["note"] = "not every adviser has replied: a synthesis must name the missing opinions (pending and unavailable, listed here) and may not claim agreement without them. No consensus, vote or score is computed"
	case "disagree", "history":
		if c.scenario == "history" {
			standing, state = "history", "succeeded"
		}
		cs = []map[string]any{
			consult("csl_a", "replied", "Replied", "agt_rev1", "alice", standing, "yes: the down migration exists"),
			consult("csl_b", "replied", "Replied", "agt_rev2", "bob", standing, "no: it drops a column irreversibly"),
		}
		out["replied"], out["complete"] = 2, true
		out["note"] = "every adviser asked has replied. Each reply is its own adviser's words; no consensus, vote or score is computed, and advice is unverified"
	}
	out["consultations"], out["task_state"], out["standing"] = cs, state, standing
	return out
}

func (c *adviceCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	fault := func(code int, kind, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"type": kind, "message": msg}})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"session.list","availability":"available","summary":"s","surface":"api"}%s],"limits":{}}}`, c.caps)
	case r.URL.Path == "/api/v2/tasks/tsk_q/advice":
		if c.adviceErr {
			fault(503, "ks_advice_unreadable", "this instruction's consultations could not be read; nothing is shown in their place")
			return
		}
		env(200, c.set())
	case r.URL.Path == "/api/v2/consultations/csl_b":
		env(200, c.set()["consultations"].([]map[string]any)[1])
	case r.URL.Path == "/api/v2/consultations/csl_nope":
		fault(404, "ks_not_found", "no such consultation")
	case r.URL.Path == "/api/v2/tasks/tsk_q":
		env(200, map[string]any{"id": "tsk_q", "agent_id": "agt_main0001", "state": "running", "queue_seq": 1, "origin": "cli", "created_at": "2026-09-26T10:00:00Z", "revision": 1, "author_type": "user", "author_id": "u1"})
	default:
		fault(404, "ks_not_found", "not served by this fake")
	}
}

const adviceRow = `,{"id":"advisers.advice","availability":"available","summary":"s","surface":"api","since_protocol":2}`

func adviceFixture(t *testing.T, c *adviceCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

// aggregate is anything that would read as a consensus certificate.
var aggregate = regexp.MustCompile(`(?i)(\d+\s*%|\bmajority\b|\bagree(d|ment)?\b[^:]*$|\bscore\s*[:=]|\bvotes?\s*[:=]|\b\d+\s*/\s*\d+\b)`)

func noAggregateKeys(t *testing.T, js string) {
	t.Helper()
	for _, k := range []string{`"consensus"`, `"vote"`, `"votes"`, `"score"`, `"percent"`, `"percentage"`, `"majority"`, `"agreement"`} {
		if strings.Contains(js, k) {
			t.Errorf("the --json document carries an aggregate field %s:\n%s", k, js)
		}
	}
}

func TestQA0661AMissingOpinionIsNamed(t *testing.T) {
	c := &adviceCtl{caps: adviceRow, scenario: "timeout"}
	bin, cfg := adviceFixture(t, c)
	out := runOK(t, bin, cfg, "advice", "list", "tsk_q")
	for _, want := range []string{
		"consulted    2 adviser(s); 1 replied",
		"UNAVAILABLE  bob (review, agt_rev2): unavailable (no reply before the deadline)",
		"complete     NO: an answer built on this advice must name the missing opinions above",
		"reply        alice's own words (UNVERIFIED; by agt_rev1, attempt att_alice):",
		"| yes: the down migration exists",
		"consultation csl_b · Unavailable (no reply before the deadline)",
		"reply        none, and nothing is shown in its place",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestQA0662DisagreementIsKeptAndNoConsensusIsShown(t *testing.T) {
	c := &adviceCtl{caps: adviceRow, scenario: "disagree"}
	bin, cfg := adviceFixture(t, c)
	out := runOK(t, bin, cfg, "advice", "list", "tsk_q")
	for _, want := range []string{"| yes: the down migration exists", "| no: it drops a column irreversibly",
		"adviser      alice (session review, agt_rev1)", "adviser      bob (session review, agt_rev2)",
		"no consensus, vote or score is computed"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		if m := aggregate.FindString(l); m != "" {
			t.Errorf("an aggregate reads %q in %q", m, l)
		}
	}
	noAggregateKeys(t, runOK(t, bin, cfg, "advice", "list", "tsk_q", "--json"))
}

func TestQA0663AdviceOnAnEndedInstructionIsHistory(t *testing.T) {
	c := &adviceCtl{caps: adviceRow, scenario: "history"}
	bin, cfg := adviceFixture(t, c)
	out := runOK(t, bin, cfg, "advice", "list", "tsk_q")
	if !strings.Contains(out, "standing     HISTORY: the instruction that asked has ended; this is not current advice") ||
		strings.Contains(out, "current: the instruction") {
		t.Errorf("history not shown as history:\n%s", out)
	}
	out = runOK(t, bin, cfg, "advice", "show", "csl_b")
	if !strings.Contains(out, "for          instruction tsk_q; HISTORY") {
		t.Errorf("the consultation does not say it is history:\n%s", out)
	}
}

func TestAdviceShowTracesTheReplyToItsContext(t *testing.T) {
	c := &adviceCtl{caps: adviceRow, scenario: "disagree"}
	bin, cfg := adviceFixture(t, c)
	out := runOK(t, bin, cfg, "advice", "show", "csl_b")
	for _, want := range []string{
		"asked by     main (session checkout, agt_main0001)",
		"asked        2026-09-26T10:00:00Z · taken up 2026-09-26T10:01:00Z · answered 2026-09-26T10:02:00Z · deadline 2026-09-26T10:10:00Z",
		"context      policy question_only · shared set sha256:ctx1 · grant grt_1 revision 3",
		"question     is the migration reversible?",
		"reply        bob's own words (UNVERIFIED; by agt_rev2, attempt att_bob):",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// an unknown consultation is not found (usage-class, nothing shown)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "advice", "show", "csl_nope")
	if code == 0 || !strings.Contains(errs, "no such consultation") {
		t.Errorf("unknown consultation: exit %d\n%s", code, errs)
	}
}

// Without the advice row the verb is judged by agent.workspace, which is
// unavailable today: refused with the reason, no advice read.
func TestAdviceVerbsAreGated(t *testing.T) {
	c := &adviceCtl{scenario: "disagree", caps: `,{"id":"agent.workspace","availability":"unavailable","summary":"s","surface":"api","unavailable_reason":"agent sessions are not available in this release"}`}
	bin, cfg := adviceFixture(t, c)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "advice", "list", "tsk_q")
	if code != exitFailed || !strings.Contains(errs, "agent sessions are not available") {
		t.Fatalf("gated: exit %d\n%s", code, errs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if strings.Contains(r, "/advice") || strings.Contains(r, "/consultations/") {
			t.Errorf("a refused verb still read %s", r)
		}
	}
}

func TestAdviceSummaryAndDrawerCommand(t *testing.T) {
	c := &adviceCtl{scenario: "timeout"}
	var a taskAdviceDoc
	b, _ := json.Marshal(c.set())
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	s := strings.Join(adviceSummaryLines(a), "\n")
	for _, want := range []string{"UNAVAILABLE  bob", "- alice (session review, agt_rev1) · Replied · \"yes: the down migration exists\" (unverified); full: ks advice show csl_a",
		"- bob (session review, agt_rev2) · Unavailable (no reply before the deadline) · no reply; full: ks advice show csl_b"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	for _, line := range []string{"advice", "advice tsk_q"} {
		if w, arg, ok := readWindowLine(line); !ok || w != "advice" || (line == "advice tsk_q" && arg != "tsk_q") {
			t.Errorf("%q: %q %q %v", line, w, arg, ok)
		}
	}
	if _, _, ok := readWindowLine("advice me on this please"); ok {
		t.Error("a sentence starting with advice is read as the drawer command")
	}
	// an unreadable set is said, never "no advice"
	if p := adviceProblem(&hostedErr{Status: 503, Type: "ks_advice_unreadable", Message: "x"}); !strings.Contains(p, "not the same as none") {
		t.Errorf("problem: %q", p)
	}
}

func TestTaskShowCarriesTheAdviceSummary(t *testing.T) {
	c := &adviceCtl{scenario: "timeout", caps: adviceRow + `,{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}`}
	bin, cfg := adviceFixture(t, c)
	out := runOK(t, bin, cfg, "task", "show", "tsk_q")
	for _, want := range []string{"UNAVAILABLE  bob (review, agt_rev2)", "full: ks advice show csl_a", "complete     NO"} {
		if !strings.Contains(out, want) {
			t.Errorf("task show lacks %q:\n%s", want, out)
		}
	}
	c.mu.Lock()
	c.adviceErr = true
	c.mu.Unlock()
	out = runOK(t, bin, cfg, "task", "show", "tsk_q")
	if !strings.Contains(out, "advice         could not be READ, so it is not shown; that is not the same as none") {
		t.Errorf("an unreadable advice set is not said:\n%s", out)
	}
}

// The registry's own advisers.advice row decides, unavailable today, even
// where agent.workspace would read available: refused with that row's
// reason, and nothing is read.
func TestAdviceReadsItsOwnRegistryRow(t *testing.T) {
	c := &adviceCtl{scenario: "disagree", caps: `,{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}` +
		`,{"id":"advisers.advice","availability":"unavailable","summary":"s","surface":"api","since_protocol":2,"unavailable_reason":"advice reading needs a certified runner"}`}
	bin, cfg := adviceFixture(t, c)
	for _, args := range [][]string{{"advice", "list", "tsk_q"}, {"advice", "show", "csl_b"}} {
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if code != exitFailed || !strings.Contains(errs, "advisers.advice") || !strings.Contains(errs, "advice reading needs a certified runner") || strings.Contains(errs, "agent.workspace") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if strings.Contains(r, "/advice") || strings.Contains(r, "/consultations/") {
			t.Errorf("a refused verb still read %s", r)
		}
	}
}
