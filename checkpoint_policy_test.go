// KS-053: the automatic-save policy is read and changed against the session
// revision; a failed save stays visible in the history with its reason and
// is never offered.
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

func TestCheckpointPolicyAndFailedSavesInTheHistory(t *testing.T) {
	var mu sync.Mutex
	var puts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(d any) { _ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": d}) }
		switch r.URL.Path {
		case "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetckp0000000000000000000000001", "short_id": "fleetckp0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case "/api/v2/sessions/session_1/checkpoint-policy":
			enabled := false
			if r.Method == "PUT" {
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				mu.Lock()
				puts = append(puts, b)
				mu.Unlock()
				enabled = b["enabled"] == true
			}
			env(map[string]any{"session_id": "session_1", "enabled": enabled, "interval_minutes": 15, "state": map[bool]string{true: "scheduled", false: "off"}[enabled],
				"certification": "no runner mode is certified for safe capture", "storage_effect": "each save is stored and metered", "session_revision": 7})
		case "/api/v2/sessions/session_1/checkpoints":
			env(map[string]any{"items": []any{
				map[string]any{"id": "ck_2", "state": "failed", "boundary": "idle", "reason": "the engine could not write the memory chunk", "created_at": "t2"},
				map[string]any{"id": "ck_1", "state": "valid", "boundary": "attempt_closed", "created_at": "t1", "parent_checkpoint_id": "ck_0", "chunks": map[string]int{"memory": 12, "disk": 40}},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), env, "session", "checkpoint-policy", "fleetckp")
	if code != 0 || !strings.Contains(out, "OFF") || !strings.Contains(out, "certified") || len(puts) != 0 {
		t.Fatalf("read: %d\n%s", code, out)
	}
	if _, _, code := auditExec(t, bin, cfg, t.TempDir(), env, "session", "checkpoint-policy", "fleetckp", "--interval", "15"); code != exitUsage {
		t.Fatalf("interval without --on: %d", code)
	}
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), env, "session", "checkpoint-policy", "fleetckp", "--on", "--interval", "15")
	if code != 0 || len(puts) != 1 || puts[0]["expected_revision"] != float64(7) || puts[0]["interval_minutes"] != float64(15) || !strings.Contains(out, "every 15 min") {
		t.Fatalf("on: %d %v\n%s", code, puts, out)
	}
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), env, "session", "checkpoints", "fleetckp")
	if code != 0 || !strings.Contains(out, "not restorable: the engine could not write the memory chunk") || !strings.Contains(out, "follows ck_0; chunks memory 12, disk 40") {
		t.Fatalf("history: %d\n%s", code, out)
	}
}
