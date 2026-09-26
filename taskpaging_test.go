// KS-050: the task list follows the service's cursor to the end; nothing
// past the first page is dropped.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTheTaskListFollowsTheCursorPastTwoHundred(t *testing.T) {
	const total = 450
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(d any) { _ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": d}) }
		switch r.URL.Path {
		case "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetpag0000000000000000000000001", "short_id": "fleetpag0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case "/api/v2/agents":
			env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
		case "/api/v2/tasks":
			start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
			var items []any
			for i := start; i < start+200 && i < total; i++ {
				items = append(items, map[string]any{"id": fmt.Sprintf("tsk_%04d", i), "agent_id": "agent_1", "state": "succeeded", "queue_seq": i})
			}
			next := ""
			if start+200 < total {
				next = strconv.Itoa(start + 200)
			}
			env(map[string]any{"items": items, "next_cursor": next})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "task", "list", "--session", "fleetpag", "--json")
	if code != 0 || !strings.Contains(out, "tsk_0449") || !strings.Contains(out, "tsk_0250") {
		t.Fatalf("paging: %d\n%s", code, errs)
	}
}
