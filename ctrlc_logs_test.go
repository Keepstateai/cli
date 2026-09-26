// QA-035-3, the logs context: Ctrl-C while following logs stops following,
// changes nothing and exits cleanly.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCtrlCWhileFollowingLogsStopsFollowing(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		env := func(d any) { _ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": d}) }
		switch r.URL.Path {
		case "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetlog0000000000000000000000001", "short_id": "fleetlog0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case "/api/v2/sessions/session_1/logs":
			env(map[string]any{"runtime_state": "running", "entries": []any{map[string]any{"source": "agent", "kind": "x", "text": "line"}}, "next_cursor": "c", "follow": map[string]any{"ends": false}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	cmd := exec.Command(bin, "agent", "logs", "--session", "fleetlog", "--follow")
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a log line", func() bool { return strings.Contains(so.String(), "line") })
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit %v\n%s", err, se.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("following did not stop on Ctrl-C")
	}
	if !strings.Contains(so.String(), "stopped following; nothing on the session changed") {
		t.Errorf("no stop line:\n%s", so.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("following sent %s", c)
		}
	}
}
