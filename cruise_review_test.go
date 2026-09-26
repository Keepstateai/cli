// KS-077 client: the Cruise review screen, resume and cancel from it, and
// cleanup reported apart from accounting, against a recording control plane
// serving GET /api/jobs/{id}/status as ctl/jobs_status.go shapes it.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type reviewCtl struct {
	mu        sync.Mutex
	requests  []string
	bodies    []string
	state     string // review | running | cancelled
	remaining *int64 // nil: unavailable
	cleanup   string // pending | confirmed | not_needed
	decided   bool
}

func (c *reviewCtl) status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	cost := int64(40000)
	money := map[string]any{"microusd": nil, "standing": "unavailable", "note": "at least one attempt has no reconciled cost yet"}
	if c.remaining != nil {
		spent := 2000000 - *c.remaining
		money = map[string]any{"microusd": spent, "standing": "reconciled", "note": "reconciled"}
	}
	st := map[string]any{"job_id": "job_r", "state": c.state, "state_label": map[string]string{"review": "Waiting for your decision", "running": "An attempt is running", "cancelled": "Cancelled"}[c.state],
		"ladder_pos": 0, "rungs": 1, "attempts": 2,
		"spend": map[string]any{"model": money, "validation": map[string]any{"standing": "unavailable", "note": "v"}, "runtime": map[string]any{"standing": "unavailable", "note": "session runtime is metered per session"},
			"storage": map[string]any{"standing": "unavailable", "note": "s"}, "spend_ceiling_microusd": 2000000},
		"savings": map[string]any{"shown": false, "reason": "no matched baseline"}, "next_actions": []any{},
		"follow": map[string]any{"cursor": 7, "poll_after_s": 0, "max_backoff_s": 60, "note": "nothing runs until a person decides"},
		"review": nil, "cleanup": nil}
	if c.state == "review" {
		rem := any(nil)
		standing := "unavailable"
		if c.remaining != nil {
			rem, standing = *c.remaining, "known"
		}
		st["review"] = map[string]any{"reason": "ladder-exhausted: every rung failed the checks",
			"attempts": []any{
				map[string]any{"id": "att_1", "rung": 0, "model": "claude-haiku-4-5", "verdict": "fail", "reason": "tests failed", "check_result": "fail", "tests_tail": "FAILED test_inventory.py::test_count\n1 failed", "policy_problems": []string{}, "cost_microusd": cost},
				map[string]any{"id": "att_2", "rung": 0, "model": "claude-haiku-4-5", "verdict": nil, "reason": "", "check_result": "unavailable", "tests_tail": "", "policy_problems": []string{"wrote to test_inventory.py"}, "cost_microusd": nil},
			},
			"remaining_microusd": rem, "remaining_standing": standing,
			"options": []any{
				map[string]any{"action": "resume", "label": "Resume with the same ladder", "request": "POST /api/jobs/job_r/resume", "effect": "a new approval revision on the same ladder and the same spend ceiling; earlier attempts, costs and results are kept"},
				map[string]any{"action": "resume_ladder", "label": "Resume with a different ladder", "request": "POST /api/jobs/job_r/resume {\"ladder\": [...]}", "effect": "a new approval revision with the ladder you name; the spend ceiling is unchanged"},
				map[string]any{"action": "cancel", "label": "Cancel the job", "request": "POST /api/jobs/job_r/cancel", "effect": "stops the job and releases its parked sessions through a tracked cleanup; attempts, costs and results are kept"},
			}}
	}
	if c.state == "cancelled" {
		notes := map[string]string{"pending": "the worker has not yet confirmed that the job's sessions are gone", "confirmed": "the worker confirmed its sessions for this job are gone", "not_needed": "no worker ever held the job, so nothing ran to clean up"}
		st["cleanup"] = map[string]any{"state": c.cleanup, "at": "2026-09-26T12:00:00Z", "note": notes[c.cleanup]}
	}
	return st
}

func (c *reviewCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	c.bodies = append(c.bodies, string(b))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch {
	case r.URL.Path == "/api/jobs/job_r/status":
		_ = enc.Encode(c.status())
	case r.URL.Path == "/api/jobs/job_r/events":
		c.mu.Lock()
		decided, state := c.decided, c.state
		c.mu.Unlock()
		evs := []any{}
		if decided {
			by := map[string]any{"account_id": "acct_1", "credential": "bearer token", "token_fingerprint": "0123456789ab"}
			if state == "cancelled" {
				evs = append(evs, map[string]any{"seq": 8, "ts": "t", "type": "job.cancelled", "attempt_id": nil, "detail": map[string]any{"by": "customer", "decided_by": by, "manifest_version": 1, "teardown_pending": true}})
			} else {
				evs = append(evs, map[string]any{"seq": 8, "ts": "t", "type": "job.queued", "attempt_id": nil, "detail": map[string]any{"decided_by": by, "manifest_version": 2, "previous_manifest_version": 1, "ladder_changed": true}})
			}
		}
		_ = enc.Encode(evs)
	case r.Method == "POST" && r.URL.Path == "/api/jobs/job_r/resume":
		c.mu.Lock()
		if c.state != "review" {
			c.mu.Unlock()
			w.WriteHeader(409)
			_ = enc.Encode(map[string]any{"error": map[string]any{"type": "ks_job_state", "message": "the job left review before the resume landed"}})
			return
		}
		c.state, c.decided = "running", true
		c.mu.Unlock()
		_ = enc.Encode(map[string]any{"id": "job_r", "state": "queued", "manifest_version": 2})
	case r.Method == "POST" && r.URL.Path == "/api/jobs/job_r/cancel":
		c.mu.Lock()
		c.state, c.decided, c.cleanup = "cancelled", true, "pending"
		c.mu.Unlock()
		_ = enc.Encode(map[string]any{"id": "job_r", "state": "cancelled"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/models":
		_ = enc.Encode(map[string]any{"schema_version": 2, "data": map[string]any{"catalog_version": "v2", "models": []any{
			map[string]any{"id": "claude-sonnet-5", "family": "anthropic", "provider_route": "anthropic", "rung": 2, "availability": map[string]any{"standing": "listed"}}}}})
	default:
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func reviewFixture(t *testing.T, c *reviewCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func posts(c *reviewCtl) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func TestCruiseReviewScreenShowsEveryOptionAndNeverZeroForUnavailable(t *testing.T) {
	c := &reviewCtl{state: "review"}
	bin, cfg := reviewFixture(t, c)
	out := runOK(t, bin, cfg, "cruise", "review", "job_r")
	for _, want := range []string{
		"job job_r is IN REVIEW: nothing runs until you decide",
		"why it stopped ladder-exhausted: every rung failed the checks",
		"att_1  rung 1  claude-haiku-4-5  check fail  verdict fail  cost $0.04",
		"| FAILED test_inventory.py::test_count",
		"att_2  rung 1  claude-haiku-4-5  check unavailable  verdict none  cost unavailable",
		"policy   wrote to test_inventory.py",
		"ceiling        unavailable: earlier attempts have no reconciled cost",
		"Resume with the same ladder: ks cruise resume job_r",
		"Resume with a different ladder: ks cruise resume job_r --ladder family:model,...",
		"Cancel the job: ks cruise cancel job_r",
		"effect   stops the job and releases its parked sessions through a tracked cleanup",
		"accounting     model spend unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("an unavailable figure reads $0:\n%s", out)
	}
	if p := posts(c); len(p) != 0 {
		t.Errorf("review sent %v", p)
	}
}

func TestCruiseResumeShowsItsEffectAndExtraSpendThenRecordsTheDecision(t *testing.T) {
	rem := int64(1500000)
	c := &reviewCtl{state: "review", remaining: &rem}
	bin, cfg := reviewFixture(t, c)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "resume", "job_r", "--ladder", "anthropic:claude-sonnet-5")
	if code != 0 {
		t.Fatalf("resume: exit %d\n%s", code, errs)
	}
	for _, want := range []string{
		"resuming job job_r: a new approval revision with the ladder you name",
		"new ladder     anthropic:claude-sonnet-5",
		"may spend      up to $1.50 more: what remains of the unchanged $2.00 ceiling",
		"recorded as your decision: revision 2 replaces 1; decided by account acct_1 via an API token (token 0123456789ab)",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("resume lacks %q:\n%s", want, errs)
		}
	}
	// the effect is shown BEFORE the request is sent
	if strings.Index(errs, "may spend") > strings.Index(errs, "recorded as your decision") {
		t.Errorf("the spend was stated after acting:\n%s", errs)
	}
	if p := posts(c); len(p) != 1 || p[0] != "POST /api/jobs/job_r/resume" {
		t.Errorf("resume sent %v", p)
	}
}

func TestCruiseResumeWithUnknownRemainingSaysSoAndOutsideReviewSendsNothing(t *testing.T) {
	c := &reviewCtl{state: "review"}
	bin, cfg := reviewFixture(t, c)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "resume", "job_r")
	if code != 0 || !strings.Contains(errs, "may spend      NOT KNOWN") || strings.Contains(errs, "$0.00") {
		t.Fatalf("unknown remaining: exit %d\n%s", code, errs)
	}
	// now running: a second resume is refused before anything is sent
	c.mu.Lock()
	c.requests = nil
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "resume", "job_r")
	if code != exitConflict || !strings.Contains(errs, "resume is legal only from review, so nothing was sent") || len(posts(c)) != 0 {
		t.Fatalf("outside review: exit %d, posts %v\n%s", code, posts(c), errs)
	}
}

func TestCruiseCancelReportsCleanupApartFromAccounting(t *testing.T) {
	c := &reviewCtl{state: "review"}
	bin, cfg := reviewFixture(t, c)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "cancel", "job_r")
	if code != 0 || strings.TrimSpace(out) != "job_r" {
		t.Fatalf("cancel: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"cancelling job job_r (review): stops the job and releases its parked sessions through a tracked cleanup",
		"recorded as your decision on revision 1; decided by account acct_1",
		"cleanup        PENDING: the worker has not yet confirmed",
		"accounting: model spend unavailable",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("cancel lacks %q:\n%s", want, errs)
		}
	}
	for _, st := range []string{"confirmed", "not_needed"} {
		c.mu.Lock()
		c.cleanup = st
		c.mu.Unlock()
		out := runOK(t, bin, cfg, "cruise", "review", "job_r")
		want := map[string]string{"confirmed": "cleanup        confirmed at 2026-09-26T12:00:00Z", "not_needed": "cleanup        not needed: no worker ever held the job"}[st]
		if !strings.Contains(out, want) || !strings.Contains(out, "not in review: there is nothing to decide") {
			t.Errorf("%s: lacks %q:\n%s", st, want, out)
		}
	}
}
