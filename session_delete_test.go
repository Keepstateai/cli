// KS-060: a session is deleted through a plan, confirmed by naming it; the
// answer reports the runtime's stop and the content's standing apart, and
// never calls retained content erased.
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

func TestSessionDeletionReportsStopAndContentApart(t *testing.T) {
	var mu sync.Mutex
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(code int, d any) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": d})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"deletion.plans","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.URL.Path == "/api/v2/sessions":
			env(200, map[string]any{"items": []any{map[string]any{"id": "fleetdel0000000000000000000000001", "short_id": "fleetdel0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/sessions/session_1/deletion-plan":
			env(201, map[string]any{"id": "dplan_1", "expires_at": "x", "plan": map[string]any{"agents": map[string]any{"count": 1, "effect": "removed"}, "runtime_cleanup": "the fleet session is stopped for good", "retention": map[string]any{"checkpoint_content": "retained while the account exists"}}})
		case r.URL.Path == "/api/v2/sessions/session_1" && r.Method == "DELETE":
			mu.Lock()
			deletes = append(deletes, r.URL.Query().Get("plan_id"))
			mu.Unlock()
			env(200, map[string]any{"deleted": true, "executed_at": "t", "agents_removed": 1, "tasks_cancelled": 2,
				"runtime_cleanup": map[string]any{
					"runtime_stop":  map[string]any{"state": "stopped", "detail": "the engine killed the session; it cannot be revived", "operation": "dplan_1", "attempts": 1},
					"content_purge": map[string]any{"state": "retained_under_policy", "detail": "saved content is not purged by a session deletion"}},
				"retention": map[string]any{"checkpoint_content": "retained while the account exists"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), env, "session", "delete", "fleetdel", "--plan")
	if code != 0 || !strings.Contains(out, "NOTHING was deleted") || !strings.Contains(out, "--execute dplan_1") || len(deletes) != 0 {
		t.Fatalf("plan: %d\n%s", code, out)
	}
	for _, args := range [][]string{{"--execute", "dplan_1"}, {"--execute", "dplan_1", "--yes"}, {"--execute", "dplan_1", "--confirm", "other"}} {
		if _, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, append([]string{"session", "delete", "fleetdel"}, args...)...); code != exitUsage || !strings.Contains(errs, "nothing was deleted") {
			t.Errorf("%v: %d\n%s", args, code, errs)
		}
	}
	if len(deletes) != 0 {
		t.Fatal("an unconfirmed deletion was sent")
	}
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "session", "delete", "fleetdel", "--execute", "dplan_1", "--confirm", "fleetdel0000")
	if code != 0 || len(deletes) != 1 || !strings.Contains(out, "runtime   stopped: the engine killed") || !strings.Contains(out, "content   retained_under_policy") {
		t.Fatalf("execute: %d\n%s%s", code, out, errs)
	}
}
