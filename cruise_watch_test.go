// KS-076 client: ks cruise status --watch against a recording control plane
// serving GET /api/jobs/{id}/status and /events?since= exactly as
// ctl/jobs_status.go shapes them.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type watchCtl struct {
	mu         sync.Mutex
	requests   []string
	eventReads int
	endless    bool // the job never finishes (the Ctrl-C case)
	noRoute    bool // an older control plane without the status route
}

func (c *watchCtl) status(done bool) map[string]any {
	money := func(standing, note string, v any) map[string]any {
		return map[string]any{"microusd": v, "standing": standing, "note": note}
	}
	st := map[string]any{"job_id": "job_w", "state": "verifying", "state_label": "Checking the result", "verdict": nil, "goal": "g",
		"ladder_pos": 0, "rungs": 2, "attempts": 1,
		"current_attempt":      map[string]any{"id": "att_1", "rung": 0, "model": "claude-haiku-4-5", "state": "usage-received", "started_at": "2026-09-26T10:00:00Z"},
		"pending_verification": map[string]any{"attempt_id": "att_1", "since_seq": 4, "since": "2026-09-26T10:05:00Z"},
		"last_save_point":      map[string]any{"checkpoint_id": "ckpt_9", "attempt_id": "att_1", "seq": 3, "at": "2026-09-26T10:04:00Z"},
		"spend": map[string]any{
			"model":                  money("unavailable", "at least one attempt has no reconciled cost yet", nil),
			"validation":             money("unavailable", "the host-side verifier is not metered per job", nil),
			"runtime":                money("unavailable", "session runtime is metered on the fleet per session", nil),
			"storage":                money("unavailable", "checkpoint storage is metered per account", nil),
			"spend_ceiling_microusd": 2000000, "attempts_reconciled": 0, "attempts_unreconciled": 1},
		"savings":      map[string]any{"shown": false, "reason": "no savings figure: there is no matched baseline for this job"},
		"next_actions": []any{map[string]any{"action": "cancel", "label": "Cancel the job", "request": "POST /api/jobs/job_w/cancel"}},
		"follow":       map[string]any{"events": "GET /api/jobs/job_w/events?since=4", "cursor": 4, "poll_after_s": 1, "max_backoff_s": 4, "note": "follow"},
	}
	if done {
		st["state"], st["state_label"], st["verdict"] = "accepted", "Accepted: the checks passed", "accepted"
		st["current_attempt"], st["pending_verification"] = nil, nil
		st["spend"].(map[string]any)["model"] = money("reconciled", "the sum of the attempts' reconciled costs", 123456)
		st["next_actions"] = []any{map[string]any{"action": "artifact", "label": "Download the accepted result", "request": "GET /api/jobs/job_w/artifact"}}
		st["follow"] = map[string]any{"events": "GET /api/jobs/job_w/events?since=6", "cursor": 6, "poll_after_s": 0, "max_backoff_s": 60, "note": "the job has finished"}
	}
	return st
}

func (c *watchCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	reads := c.eventReads
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch {
	case r.URL.Path == "/api/jobs/job_w":
		_ = enc.Encode(map[string]any{"id": "job_w", "state": "verifying"})
	case r.URL.Path == "/api/jobs/job_w/status":
		if c.noRoute {
			http.NotFound(w, r)
			return
		}
		_ = enc.Encode(c.status(!c.endless && reads >= 2))
	case r.URL.Path == "/api/jobs/job_w/events":
		c.mu.Lock()
		c.eventReads++
		n := c.eventReads
		c.mu.Unlock()
		if c.endless {
			_ = enc.Encode([]any{})
			return
		}
		if n == 1 {
			w.WriteHeader(503)
			_ = enc.Encode(map[string]any{"error": map[string]any{"type": "ks_internal", "message": "the events ledger did not answer"}})
			return
		}
		// the service answers since=4 with seq 5 and 6; a stale 4 is repeated
		// here to prove the client never prints an event twice
		_ = enc.Encode([]any{
			map[string]any{"seq": 4, "ts": "2026-09-26T10:05:00Z", "type": "verify.start", "attempt_id": "att_1", "detail": map[string]any{}},
			map[string]any{"seq": 5, "ts": "2026-09-26T10:06:00Z", "type": "verify.pass", "attempt_id": "att_1", "detail": map[string]any{}},
			map[string]any{"seq": 6, "ts": "2026-09-26T10:06:01Z", "type": "job.accepted", "attempt_id": nil, "detail": map[string]any{"verdict": "accepted"}},
		})
	default:
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func watchEnv(cfg string) []string {
	return append(fastEnv(cfg), "KS_WATCH_SECOND_MS=20")
}

func TestCruiseWatchFollowsReconnectsAndEnds(t *testing.T) {
	c := &watchCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), watchEnv(cfg), "cruise", "status", "job_w", "--watch")
	if code != 0 {
		t.Fatalf("watch: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"job job_w · Checking the result (verifying)",
		"in flight      attempt att_1 on rung 1 (claude-haiku-4-5)",
		"verifier       result PENDING for attempt att_1",
		"last save      ckpt_9 (attempt att_1",
		"model spend    unavailable — at least one attempt has no reconciled cost yet",
		"runtime        unavailable — session runtime is metered on the fleet per session",
		"storage        unavailable — checkpoint storage is metered per account",
		"savings        not shown: no savings figure",
		"next           Cancel the job: ks cruise cancel job_w",
		"job job_w · Accepted: the checks passed (accepted)",
		"model spend    $0.123456 (reconciled)",
		"next           Download the accepted result: ks cruise artifact job_w",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("watch lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "verify.pass") != 1 || strings.Count(out, "job.accepted") != 1 || strings.Contains(out, "verify.start") {
		t.Errorf("events not printed exactly once from the cursor:\n%s", out)
	}
	if !strings.Contains(errs, "reconnecting in") || !strings.Contains(errs, "from event 4") || !strings.Contains(errs, "nothing further will happen") {
		t.Errorf("reconnect or end not said:\n%s", errs)
	}
	// unavailable is never $0 before the figure exists
	first := out[:strings.Index(out, "Accepted")]
	if strings.Contains(first, "$0") {
		t.Errorf("an unavailable figure reads $0:\n%s", first)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sinceFour := 0
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("watching sent %q", r)
		}
		if r == "GET /api/jobs/job_w/events?since=4" {
			sinceFour++
		}
	}
	if sinceFour != 2 {
		t.Errorf("the reconnect did not resume from the same cursor: %v", c.requests)
	}
}

func TestCruiseWatchCtrlCEndsOnlyTheWatch(t *testing.T) {
	c := &watchCtl{endless: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	cmd := exec.Command(bin, "cruise", "status", "job_w", "--watch")
	cmd.Env = append(os.Environ(), watchEnv(cfg)...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		n := c.eventReads
		c.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("the watch did not follow:\n%s%s", so.String(), se.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch exited %v\n%s", err, se.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Ctrl-C did not end the watch")
	}
	if !strings.Contains(se.String(), "stopped watching; nothing was sent") {
		t.Errorf("stop line missing:\n%s", se.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("Ctrl-C sent %q: it must end only the watch", r)
		}
	}
}

func TestCruiseWatchOnAnOlderControlPlaneSaysSo(t *testing.T) {
	c := &watchCtl{noRoute: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), watchEnv(cfg), "cruise", "status", "job_w", "--watch")
	if code != exitFailed || !strings.Contains(errs, "does not serve a job's live status") {
		t.Fatalf("older control plane: exit %d\n%s", code, errs)
	}
}
