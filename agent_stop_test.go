// KS-035: agent stop is confirmed by naming the agent, says the session
// keeps running and may be charged, offers Save and pause separately and
// never performs it; Ctrl-C in a window leaves it (covered by
// TestAgentOpenDetachesOnInterruptAndCancelsNothing).
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

type stopCtl struct {
	mu    sync.Mutex
	calls []string
}

func (c *stopCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetstp0000000000000000000000001", "short_id": "fleetstp0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.URL.Path == "/api/v2/agents/agent_1/stop":
		env(map[string]any{"agent_id": "agent_1", "interrupt_requested_task": "tsk_1", "task_state": "cancelling", "held_tasks": []string{"tsk_2", "tsk_3"},
			"hold": map[string]any{"id": "hold_1"}, "session_runtime_state": "running",
			"charges": "the session is preserved and keeps running, so runtime and storage may still be charged",
			"park":    map[string]any{"action": "save_and_pause", "label": "Save and pause", "request": "POST /api/v2/sessions/session_1/pause"},
			"resume":  "release the hold: ks agent queue resume main"})
	default:
		w.WriteHeader(404)
	}
}

func TestAgentStopSaysChargesContinueAndNeverParks(t *testing.T) {
	c := &stopCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	stopped := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := 0
		for _, x := range c.calls {
			if strings.HasSuffix(x, "/stop") {
				n++
			}
		}
		return n
	}
	for _, args := range [][]string{{}, {"--yes"}, {"--confirm", "other"}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, append([]string{"agent", "stop", "main", "--session", "fleetstp"}, args...)...); code != exitUsage || !strings.Contains(errs, "may still be charged") {
			t.Errorf("%v: %d\n%s", args, code, errs)
		}
	}
	if stopped() != 0 {
		t.Fatal("an unconfirmed stop was sent")
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "stop", "main", "--session", "fleetstp", "--confirm", "main")
	if code != 0 || stopped() != 1 || !strings.Contains(out, "tsk_1 now reads cancelling") || !strings.Contains(out, "not claimed stopped") ||
		!strings.Contains(out, "may still be charged") || !strings.Contains(out, "Save and pause (separate, not done)") {
		t.Fatalf("stop: %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.calls {
		if strings.HasSuffix(x, "/pause") || strings.Contains(x, "/cancel") || strings.Contains(x, "control-leases") {
			t.Fatalf("stop performed another action: %v", c.calls)
		}
	}
}
