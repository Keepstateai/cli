// KS-045 on the command line: the pending view and a move bound to a queue
// revision. QA-045-1 (a stale view is a conflict that shows the refreshed
// positions and retries nothing), QA-045-2 (an instruction that has started
// is never moved), the first line shown only when the service returns it,
// and the refusals: wrong role, capability unavailable.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type queueCtl struct {
	mu       sync.Mutex
	rev      string
	operator bool   // the caller may read instructions (summaries returned)
	moveMode string // ok | conflict | not_queued | forbid
	unavail  bool
	moves    []map[string]any
	calls    []string
	// replace and bulk cancel
	replaceMode string // ok | conflict | not_queued | replayed
	replaces    []map[string]any
	bulkMode    string // ok | conflict
	bulks       []map[string]any
}

func (c *queueCtl) item(id string, pos int, state, summary string) map[string]any {
	it := map[string]any{"task_id": id, "position": pos, "queue_seq": pos + 10, "state": state, "submitter_type": "account", "submitter_id": "acct_1",
		"origin": "cli", "created_at": "x", "age_seconds": 90, "movable": state == "queued", "summary_shown": c.operator}
	if c.operator {
		it["summary"] = summary
	}
	return it
}

func (c *queueCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, e map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		e["work_started"] = "no"
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": e})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		a := "available"
		if c.unavail {
			a = "unavailable"
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}],"limits":{}}}`, a)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetque0000000000000000000000001", "short_id": "fleetque0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case strings.HasPrefix(r.URL.Path, "/api/v2/tasks/") && r.Method == "GET":
		env(200, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/api/v2/tasks/"), "agent_id": "agent_1", "state": "queued", "revision": 1})
	case r.URL.Path == "/api/v2/agents/agent_1/pending":
		env(200, map[string]any{"agent_id": "agent_1", "queue_revision": c.rev,
			"active":  c.item("tsk_run", 0, "running", "deploy the site"),
			"pending": []any{c.item("tsk_a", 1, "queued", "write the tests \x1b[2J"), c.item("tsk_b", 2, "queued", "fix the bug"), c.item("tsk_c", 3, "held", "announce")}})
	case strings.HasSuffix(r.URL.Path, "/replace") && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.replaces = append(c.replaces, body)
		switch {
		case c.replaceMode == "not_queued":
			refuse(409, map[string]any{"code": "ks_task_not_queued", "type": "ks_task_not_queued", "message": "only pending work is replaced; this instruction is running"})
		case c.replaceMode == "conflict" || body["expected_queue_revision"] != c.rev:
			refuse(409, map[string]any{"code": "ks_queue_revision_conflict", "type": "ks_queue_revision_conflict", "message": "the queue changed since it was read; nothing was cancelled or submitted",
				"queue_revision": "qrev_new", "pending": []any{c.item("tsk_b", 1, "queued", "fix the bug")}})
		default:
			env(200, map[string]any{"superseded": map[string]any{"id": "tsk_a", "state": "cancelled"}, "replacement": map[string]any{"id": "tsk_new", "state": "queued"},
				"position": 1, "queue_revision": "qrev_after", "replayed": c.replaceMode == "replayed", "pending": []any{c.item("tsk_new", 1, "queued", "run only unit tests")}})
		}
	case strings.HasSuffix(r.URL.Path, "/cancel-preview") && r.Method == "GET":
		env(200, map[string]any{"agent_id": "agent_1", "queue_revision": c.rev, "states": []string{"queued", "held"},
			"affected": []any{c.item("tsk_a", 1, "queued", "write the tests"), c.item("tsk_c", 2, "held", "announce")}, "by_state": map[string]int{"queued": 1, "held": 1},
			"excluded_active": c.item("tsk_run", 0, "running", "deploy"), "note": "a preview changes nothing"})
	case strings.HasSuffix(r.URL.Path, "/cancel-pending") && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.bulks = append(c.bulks, body)
		if c.bulkMode == "conflict" || body["expected_queue_revision"] != c.rev {
			refuse(409, map[string]any{"code": "ks_queue_revision_conflict", "type": "ks_queue_revision_conflict", "message": "the queue changed since the preview was read; nothing was cancelled",
				"queue_revision": "qrev_new", "preview": map[string]any{"agent_id": "agent_1", "queue_revision": "qrev_new", "states": []string{"queued", "held"},
					"affected": []any{c.item("tsk_a", 1, "queued", "write the tests"), c.item("tsk_d", 2, "queued", "a new one")}}})
			return
		}
		env(200, map[string]any{"agent_id": "agent_1", "cancelled": []string{"tsk_a", "tsk_c"}, "by_state": map[string]int{"queued": 1, "held": 1}, "queue_revision": "qrev_after"})
	case strings.HasSuffix(r.URL.Path, "/move") && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.moves = append(c.moves, body)
		switch {
		case c.moveMode == "forbid":
			refuse(403, map[string]any{"code": "ks_forbidden", "type": "ks_forbidden", "message": "moving needs the operator role"})
		case c.moveMode == "not_queued":
			refuse(409, map[string]any{"code": "ks_task_not_queued", "type": "ks_task_not_queued", "message": "only a queued task moves; this one is running, and an executing or settled instruction is never edited"})
		case c.moveMode == "conflict" || body["expected_queue_revision"] != c.rev:
			refuse(409, map[string]any{"code": "ks_queue_revision_conflict", "type": "ks_queue_revision_conflict",
				"message": "the queue changed since it was read; nothing was moved. The current positions are attached", "queue_revision": "qrev_new",
				"pending": []any{c.item("tsk_b", 1, "queued", "fix the bug"), c.item("tsk_a", 2, "queued", "write the tests")}})
		default:
			env(200, map[string]any{"task_id": "tsk_b", "from_position": 2, "to_position": 1, "queue_revision": "qrev_after",
				"pending": []any{c.item("tsk_b", 1, "queued", "fix the bug"), c.item("tsk_a", 2, "queued", "write the tests")}})
		}
	default:
		refuse(404, map[string]any{"code": "ks_not_found", "type": "ks_not_found", "message": "no such route in this fake: " + r.URL.Path})
	}
}

func (c *queueCtl) moveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.moves)
}

func TestThePendingViewShowsFirstLinesOnlyWhenReturned(t *testing.T) {
	c := &queueCtl{rev: "qrev_1", operator: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "list", "main", "--session", "fleetque")
	if code != 0 || !strings.Contains(out, "executing now") || !strings.Contains(out, "tsk_run") || !strings.Contains(out, "write the tests") ||
		strings.Contains(out, "\x1b") || !strings.Contains(out, "held") || !strings.Contains(out, "--queue-revision qrev_1") {
		t.Fatalf("operator view: %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	c.operator = false
	c.mu.Unlock()
	out, _, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "agent", "queue", "list", "main", "--session", "fleetque")
	if code != 0 || strings.Contains(out, "write the tests") || !strings.Contains(out, "first line not shown") {
		t.Fatalf("reader view: %d\n%s", code, out)
	}
}

func TestAMoveIsBoundToTheQueueRevisionAndNeverRetried(t *testing.T) {
	c := &queueCtl{rev: "qrev_1", operator: true, moveMode: "ok"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// exactly one of --before/--after, never relative to itself: refused locally
	for _, args := range [][]string{{"tsk_b"}, {"tsk_b", "--before", "tsk_a", "--after", "tsk_a"}, {"tsk_b", "--before", "tsk_b"}} {
		if _, _, code := auditExec(t, bin, cfg, dir, env, append(append([]string{"task", "move"}, args...), "--session", "fleetque")...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if c.moveCount() != 0 {
		t.Fatal("an invalid move was sent")
	}
	// bound to the revision the person read
	out, errs, code := auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--before", "tsk_a", "--session", "fleetque", "--queue-revision", "qrev_1")
	if code != 0 || !strings.Contains(out, "moved tsk_b from position 2 to 1") || !strings.Contains(errs, "the queue revision you gave") {
		t.Fatalf("move: %d\n%s%s", code, out, errs)
	}
	if c.moves[0]["expected_queue_revision"] != "qrev_1" || c.moves[0]["before_task_id"] != "tsk_a" {
		t.Fatalf("the move sent: %v", c.moves[0])
	}
	// a stale view: nothing moves, the refreshed positions are shown, ONE request
	c.mu.Lock()
	c.moves = nil
	c.mu.Unlock()
	out, errs, code = auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--before", "tsk_a", "--session", "fleetque", "--queue-revision", "qrev_OLD")
	if code != exitConflict || !strings.Contains(errs, "nothing is retried") || !strings.Contains(errs, "the queue as it is now") ||
		!strings.Contains(errs, "queue revision qrev_new") || !strings.Contains(errs, "--queue-revision qrev_new") || c.moveCount() != 1 {
		t.Fatalf("stale: %d moves=%d\n%s%s", code, c.moveCount(), out, errs)
	}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--before", "tsk_a", "--session", "fleetque", "--queue-revision", "qrev_OLD", "--json")
	if !strings.Contains(out, `"code":"ks_queue_revision_conflict"`) || !strings.Contains(out, `"queue_revision":"qrev_new"`) {
		t.Fatalf("stale --json carries no refreshed view: %s", out)
	}
	// without --queue-revision: the view is read now and shown before the move
	_, errs, code = auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--after", "tsk_a", "--session", "fleetque")
	if code != 0 || !strings.Contains(errs, "pending, in dispatch order") || !strings.Contains(errs, "the queue as read just now (qrev_1)") {
		t.Fatalf("read-now move: %d\n%s", code, errs)
	}
	// an instruction that has started is never moved
	c.mu.Lock()
	c.moveMode = "not_queued"
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_run", "--before", "tsk_a", "--session", "fleetque", "--queue-revision", "qrev_1"); code != exitConflict || !strings.Contains(errs, "never edited") {
		t.Fatalf("not queued: %d\n%s", code, errs)
	}
	// the wrong role
	c.mu.Lock()
	c.moveMode = "forbid"
	c.mu.Unlock()
	if out, _, code := auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--before", "tsk_a", "--session", "fleetque", "--queue-revision", "qrev_1", "--json"); code != exitAuth || !strings.Contains(out, "ks_forbidden") {
		t.Fatalf("forbid: %d %s", code, out)
	}
	// the capability unavailable: said so, no move sent
	c.mu.Lock()
	c.unavail, c.moves = true, nil
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "task", "move", "tsk_b", "--before", "tsk_a", "--session", "fleetque"); code != exitFailed || !strings.Contains(errs, "agent.workspace") || c.moveCount() != 0 {
		t.Fatalf("unavailable: %d\n%s", code, errs)
	}
}

// KS-045 replace: bound to a queue revision, submitted once under a
// submission id recorded first (a repeat reuses it), a stale view refused
// with the refreshed positions and not retried, an instruction in flight
// never replaced.
func TestAReplacementIsBoundToTheQueueAndSubmittedOnce(t *testing.T) {
	c := &queueCtl{rev: "qrev_1", operator: true, replaceMode: "ok"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	if _, _, code := auditExec(t, bin, cfg, dir, env, "task", "replace", "tsk_a", "--session", "fleetque"); code != exitUsage {
		t.Fatalf("no text: %d", code)
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "task", "replace", "tsk_a", "--text", "run only unit tests", "--session", "fleetque", "--queue-revision", "qrev_1")
	if code != 0 || !strings.Contains(out, "replaced tsk_a with tsk_new") || !strings.Contains(errs, "content kept") {
		t.Fatalf("replace: %d\n%s%s", code, out, errs)
	}
	first := c.replaces[0]
	if first["expected_queue_revision"] != "qrev_1" || first["text"] != "run only unit tests" || first["submission_id"] == "" || first["submission_id"] == nil {
		t.Fatalf("the replacement sent: %v", first)
	}
	// the same replacement again reuses the recorded submission id
	c.mu.Lock()
	c.replaceMode = "replayed"
	c.mu.Unlock()
	out, _, code = auditExec(t, bin, cfg, dir, env, "task", "replace", "tsk_a", "--text", "run only unit tests", "--session", "fleetque", "--queue-revision", "qrev_1")
	if code != 0 || !strings.Contains(out, "already replaced") || c.replaces[1]["submission_id"] != first["submission_id"] {
		t.Fatalf("repeat: %d %v\n%s", code, c.replaces, out)
	}
	// a stale view: nothing replaced, refreshed positions shown, one request
	c.mu.Lock()
	c.replaceMode, c.replaces = "ok", nil
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "task", "replace", "tsk_a", "--text", "other", "--session", "fleetque", "--queue-revision", "qrev_OLD")
	if code != exitConflict || !strings.Contains(errs, "nothing is retried") || !strings.Contains(errs, "queue revision qrev_new") || len(c.replaces) != 1 {
		t.Fatalf("stale: %d %d\n%s", code, len(c.replaces), errs)
	}
	// in flight: never replaced
	c.mu.Lock()
	c.replaceMode = "not_queued"
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "task", "replace", "tsk_run", "--text", "x", "--session", "fleetque", "--queue-revision", "qrev_1"); code != exitConflict || !strings.Contains(errs, "ks task cancel tsk_run") {
		t.Fatalf("not queued: %d\n%s", code, errs)
	}
}

// KS-045 bulk cancellation: a preview changes nothing; the confirmation
// names the preview's revision; a queue that moved is refused with the
// current preview and nothing is retried; --yes confirms nothing.
func TestBulkCancellationIsPreviewedAndConfirmedByRevision(t *testing.T) {
	c := &queueCtl{rev: "qrev_1", operator: true, bulkMode: "ok"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	for _, args := range [][]string{{}, {"--yes"}, {"--preview", "--confirm", "qrev_1"}, {"--preview", "--states", "running"}} {
		if _, _, code := auditExec(t, bin, cfg, dir, env, append([]string{"agent", "queue", "cancel", "main", "--session", "fleetque"}, args...)...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "queue", "cancel", "main", "--session", "fleetque", "--preview")
	if code != 0 || !strings.Contains(out, "would cancel 2") || !strings.Contains(out, "never included, executing now: tsk_run") || !strings.Contains(out, "--confirm qrev_1") || len(c.bulks) != 0 {
		t.Fatalf("preview: %d\n%s%s", code, out, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "queue", "cancel", "main", "--session", "fleetque", "--confirm", "qrev_1")
	if code != 0 || !strings.Contains(out, "cancelled 2 pending instruction(s): tsk_a, tsk_c") || c.bulks[0]["expected_queue_revision"] != "qrev_1" {
		t.Fatalf("confirm: %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	c.bulks = nil
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "queue", "cancel", "main", "--session", "fleetque", "--confirm", "qrev_OLD")
	if code != exitConflict || !strings.Contains(errs, "nothing was cancelled and nothing is retried") || !strings.Contains(errs, "tsk_d") ||
		!strings.Contains(errs, "--confirm qrev_new") || len(c.bulks) != 1 {
		t.Fatalf("stale confirm: %d %d\n%s", code, len(c.bulks), errs)
	}
}
