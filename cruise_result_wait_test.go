// ks cruise result JOB --wait: follows the job's live status until nothing
// further happens, then prints the receipt card once (the badge exactly as
// served, the chain, the limitations, the changeset, spend that is never
// shown as $0, and the pull-request line). A job that fails, waits for
// review or is cancelled ends the wait with its own card and one action.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type receiptCtl struct {
	mu       sync.Mutex
	requests []string
	states   []string // served in order, one per status read; the last repeats
	reads    int
	pr       any // the result's pull_request field, when served
	verdict  string
	noRoute  bool
}

func (c *receiptCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if r.Method != "GET" {
		c.mu.Unlock()
		w.WriteHeader(405)
		return
	}
	switch r.URL.Path {
	case "/api/jobs/job_r/status":
		if c.noRoute {
			c.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		i := c.reads
		if i >= len(c.states) {
			i = len(c.states) - 1
		}
		c.reads++
		state, pr, verdict := c.states[i], c.pr, c.verdict
		c.mu.Unlock()
		_ = enc.Encode(receiptStatus(state, pr, verdict))
	default:
		c.mu.Unlock()
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func receiptStatus(state string, pr any, verdict string) map[string]any {
	labels := map[string]string{"queued": "Waiting to start", "running": "Working on it", "verifying": "Checking the result",
		"accepted": "Accepted: the checks passed", "failed": "Failed", "review": "Waiting for your review", "cancelled": "Cancelled"}
	money := func(note string) map[string]any {
		return map[string]any{"microusd": nil, "standing": "unavailable", "note": note}
	}
	st := map[string]any{"job_id": "job_r", "state": state, "state_label": labels[state], "goal": "g", "ladder_pos": 0, "rungs": 2, "attempts": 1,
		"spend": map[string]any{"model": money("not reconciled yet"), "validation": money("not metered per job"),
			"runtime": money("metered per session"), "storage": money("metered per account"), "spend_ceiling_microusd": 2000000},
		"savings": map[string]any{"shown": false, "reason": "no baseline"},
		"follow":  map[string]any{"cursor": 3, "poll_after_s": 1, "max_backoff_s": 4},
	}
	if state == "running" || state == "verifying" {
		st["current_attempt"] = map[string]any{"id": "att_1", "rung": 0, "model": "claude-haiku-4-5", "state": "running", "started_at": "2026-11-03T10:00:00Z"}
	}
	if verdict != "" {
		st["verdict"] = verdict
	}
	if state == "accepted" {
		res := map[string]any{"artifact_sha256": strings.Repeat("a", 64), "artifact_bytes": 2048, "download": "GET /api/jobs/job_r/artifact",
			"badge": "verified", "manifest_sha": strings.Repeat("d", 64), "manifest_version": 2,
			"limitations": []string{"the verification covers these exact bytes"},
			"provenance": map[string]any{"attempt_id": "att_1", "rung": 0, "model": "claude-haiku-4-5", "candidate_digest": strings.Repeat("c", 64),
				"checks": map[string]any{"outcome": "passed", "counts": map[string]any{"collected": 4, "passed": 4, "failed": 0, "skipped": 0}, "runs": 1}},
			"changeset": map[string]any{"sha256": strings.Repeat("e", 64), "changes": 3, "download": "GET /api/jobs/job_r/changeset"}}
		if pr != nil {
			res["pull_request"] = pr
		}
		st["result"] = res
		st["follow"] = map[string]any{"cursor": 9, "poll_after_s": 0, "note": "the job has finished"}
	}
	if state == "failed" || state == "cancelled" || state == "review" {
		st["follow"] = map[string]any{"cursor": 9, "poll_after_s": 0}
	}
	return st
}

func receiptFixture(t *testing.T, c *receiptCtl) (string, string) {
	t.Helper()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func waitEnv(cfg string) []string { return append(fastEnv(cfg), "KS_WATCH_SECOND_MS=5") }

func TestResultWaitFollowsTheJobAndPrintsTheReceiptOnceAtAccepted(t *testing.T) {
	c := &receiptCtl{states: []string{"queued", "running", "verifying", "accepted"}}
	bin, cfg := receiptFixture(t, c)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), waitEnv(cfg), "cruise", "result", "job_r", "--wait")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if n := strings.Count(stdout, "badge          verified"); n != 1 {
		t.Errorf("the receipt printed %d times, want once:\n%s", n, stdout)
	}
	for _, want := range []string{
		"receipt for job job_r: Accepted: the checks passed",
		"checks           passed: collected 4, passed 4, failed 0, skipped 0; runs 1",
		"- the verification covers these exact bytes",
		"changeset      3 file change(s), sha256 " + strings.Repeat("e", 64),
		"model spend    unavailable",
		"runtime        unavailable",
		"Pull request: none yet. KeepState opens pull requests once GitHub is connected. Get the change now: ks cruise apply job_r",
		"Download the files: ks cruise result job_r --download",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("receipt lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "model spend    unavailable; not reconciled yet") {
		t.Errorf("spend note:\n%s", stdout)
	}
	if strings.Contains(stdout, "$0") || strings.Contains(stdout, "\u2014") {
		t.Errorf("a missing figure read as zero:\n%s", stdout)
	}
	// each change is said once, on stderr, and never as the receipt
	for _, want := range []string{"job job_r: Queued.", "job job_r: Attempt 1 (claude-haiku-4-5): running.", "job job_r: Attempt 1 (claude-haiku-4-5): verifying."} {
		if strings.Count(stderr, want) != 1 {
			t.Errorf("progress %q not said exactly once:\n%s", want, stderr)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if !strings.HasPrefix(r, "GET /api/jobs/job_r/status") {
			t.Errorf("the wait made another request: %s", r)
		}
	}
	if c.reads != 4 {
		t.Errorf("status read %d times, want 4 (one per state)", c.reads)
	}
}

func TestResultWaitJSONCarriesTheReceiptAndNoPullRequest(t *testing.T) {
	c := &receiptCtl{states: []string{"running", "accepted"}}
	bin, cfg := receiptFixture(t, c)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), waitEnv(cfg), "cruise", "result", "job_r", "--wait", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	var doc struct {
		Data struct {
			State       string          `json:"state"`
			Verified    bool            `json:"verified"`
			PullRequest json.RawMessage `json:"pull_request"`
			Next        []string        `json:"next"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if doc.Data.State != "accepted" || !doc.Data.Verified || string(doc.Data.PullRequest) != "null" {
		t.Errorf("json: %s", stdout)
	}
	if len(doc.Data.Next) == 0 || doc.Data.Next[0] != "ks cruise apply job_r" {
		t.Errorf("next actions: %v", doc.Data.Next)
	}
}

func TestAServedPullRequestIsShownOnlyAsASafeHTTPSLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   any
		want string
		bad  []string
	}{
		{"https", map[string]any{"url": "https://github.com/acme/shop/pull/12", "number": 12, "state": "open"},
			"Pull request #12: https://github.com/acme/shop/pull/12 (open)", nil},
		{"javascript", map[string]any{"url": "javascript:alert(1)", "number": 12, "state": "open"},
			"Pull request #12: the service named a link that is not a plain https address, so it is not shown", []string{"javascript"}},
		{"escapes", map[string]any{"url": "https://example.com/\x1b]8;;https://evil/\x07x", "number": 7, "state": "open\x1b[2J"},
			"Pull request #7: the service named a link that is not a plain https address, so it is not shown", []string{"\x1b", "evil"}},
		{"http", map[string]any{"url": "http://example.com/pull/3", "number": 3, "state": "merged"},
			"Pull request #3: the service named a link that is not a plain https address, so it is not shown", []string{"http://"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.pr)
			var pr livePullRequest
			if err := json.Unmarshal(raw, &pr); err != nil {
				t.Fatal(err)
			}
			got := pullRequestLine("job_r", &pr)
			if got != tc.want {
				t.Errorf("line %q, want %q", got, tc.want)
			}
			for _, b := range tc.bad {
				if strings.Contains(got, b) {
					t.Errorf("line carries %q: %q", b, got)
				}
			}
		})
	}
	// end to end: a served link reaches the receipt, and the apply action stays
	c := &receiptCtl{states: []string{"accepted"}, pr: map[string]any{"url": "https://github.com/acme/shop/pull/12", "number": 12, "state": "open"}}
	bin, cfg := receiptFixture(t, c)
	out := runOK(t, bin, cfg, "cruise", "result", "job_r")
	if !strings.Contains(out, "Pull request #12: https://github.com/acme/shop/pull/12 (open)") || strings.Contains(out, "none yet") {
		t.Errorf("served pull request:\n%s", out)
	}
}

func TestResultWaitEndsOnAFailedJobWithTheErrorCardAndOneAction(t *testing.T) {
	c := &receiptCtl{states: []string{"running", "failed"}, verdict: "the checks failed on every rung"}
	bin, cfg := receiptFixture(t, c)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), waitEnv(cfg), "cruise", "result", "job_r", "--wait")
	if code != exitFailed {
		t.Fatalf("exit %d, want %d\n%s%s", code, exitFailed, stdout, stderr)
	}
	if !strings.Contains(stdout, "job job_r failed: the checks failed on every rung") || !strings.Contains(stdout, "Read why: ks cruise logs job_r") {
		t.Errorf("error card:\n%s", stdout)
	}
	if strings.Contains(stdout, "badge") || strings.Contains(stdout, "Pull request") {
		t.Errorf("a failed job printed a receipt:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Next: ks cruise logs job_r") {
		t.Errorf("one action:\n%s", stderr)
	}
}

func TestResultWaitEndsOnReviewAndOnCancelWithTheirOwnAction(t *testing.T) {
	c := &receiptCtl{states: []string{"verifying", "review"}}
	bin, cfg := receiptFixture(t, c)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), waitEnv(cfg), "cruise", "result", "job_r", "--wait")
	if code != exitConflict || !strings.Contains(stdout, "Review it: ks cruise review job_r") || !strings.Contains(stderr, "Next: ks cruise review job_r") {
		t.Errorf("review: exit %d\n%s%s", code, stdout, stderr)
	}
	c2 := &receiptCtl{states: []string{"queued", "cancelled"}}
	bin2, cfg2 := receiptFixture(t, c2)
	stdout, stderr, code = auditExec(t, bin2, cfg2, t.TempDir(), waitEnv(cfg2), "cruise", "result", "job_r", "--wait")
	if code != exitConflict || !strings.Contains(stdout, "job job_r was cancelled; it has no result") {
		t.Errorf("cancelled: exit %d\n%s%s", code, stdout, stderr)
	}
}

func TestResultWaitOnAControlPlaneWithoutTheStatusRoute(t *testing.T) {
	c := &receiptCtl{states: []string{"running"}, noRoute: true}
	bin, cfg := receiptFixture(t, c)
	_, stderr, code := auditExec(t, bin, cfg, t.TempDir(), waitEnv(cfg), "cruise", "result", "job_r", "--wait")
	if code != exitFailed || !strings.Contains(stderr, "does not serve a job's result") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
}

// The status words a job stream reports: the receipts pane and the
// terminal status use these, so they are pinned here.
func TestJobStatusWordsFollowTheJobState(t *testing.T) {
	for _, tc := range []struct {
		state, wantState, wantKind, wantMsg string
	}{
		{"queued", "working", "", "Queued."},
		{"running", "working", "", "Attempt 1 (claude-haiku-4-5): running."},
		{"verifying", "working", "", "Attempt 1 (claude-haiku-4-5): verifying."},
		{"escalating", "working", "", "Attempt 1: escalating."},
		{"review", "blocked", "question", "Review the result."},
		{"accepted", "done", "", "Accepted: verified."},
		{"failed", "error", "", "the checks failed"},
		{"cancelled", "idle", "", "Cancelled."},
		{"something_new", "", "", ""},
	} {
		raw, _ := json.Marshal(receiptStatus(tc.state, nil, "the checks failed"))
		var st jobLiveStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if tc.state == "escalating" {
			st.CurrentAttempt = nil
		}
		s, k, m := jobStatusWords(st)
		if s != tc.wantState || k != tc.wantKind || m != tc.wantMsg {
			t.Errorf("%s: (%q,%q,%q), want (%q,%q,%q)", tc.state, s, k, m, tc.wantState, tc.wantKind, tc.wantMsg)
		}
	}
}
