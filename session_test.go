// KS-021 client half: every page is read, the table keeps identity and
// state at 80 and 120 columns, a short id resolves only when unique, and
// the empty state says how to start.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// inventoryCtl serves the registry with session.list and a paged fleet
// inventory of n sessions, remembering every request.
type inventoryCtl struct {
	n     int
	pages atomic.Int32 // read by the test while the server may still be serving
	// noFleetState answers the way the service has since 2026-09-20: rows
	// carry no fleet_state (the engine's own vocabulary is not exposed).
	noFleetState bool
}

func (c *inventoryCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		if r.Header.Get("Authorization") != "Bearer guard-test-token" {
			w.WriteHeader(401)
			return
		}
		c.pages.Add(1)
		start := 0
		if cur := r.URL.Query().Get("cursor"); cur != "" {
			start, _ = strconv.Atoi(cur)
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 200
		}
		items := []map[string]any{}
		for i := start; i < c.n && i < start+limit; i++ {
			state := "running"
			task := "unavailable"
			activity := "unavailable"
			if i%3 == 1 {
				state, task, activity = "parked", "2 queued", "paused"
			}
			if i%3 == 2 {
				task, activity = "idle", "ready"
			}
			if r.URL.Query().Get("state") != "" && r.URL.Query().Get("state") != state {
				continue
			}
			id := fmt.Sprintf("%08x%024x", i/2, i) // pairs share an 8-char prefix
			items = append(items, map[string]any{"id": id, "short_id": id[:10], "name": id[:6], "runtime_state": state, "fleet_state": state, "image": "base",
				"budget_tokens": 500000, "execution_epoch": 1, "created_at": "2026-09-20T00:00:00Z", "last_activity_at": fmt.Sprintf("2026-09-20T10:%02d:00Z", i%60),
				"last_checkpoint_id": map[bool]string{true: "ckpt_1", false: ""}[i%3 == 1], "agent_activity": activity, "task_state": task, "key_alias": "unavailable", "observed_at": "2026-09-20T12:00:00Z"})
			if c.noFleetState {
				delete(items[len(items)-1], "fleet_state")
			}
		}
		next := ""
		if start+limit < c.n {
			next = strconv.Itoa(start + limit)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": map[string]any{"items": items, "next_cursor": next, "observed_at": "x"}})
	default:
		w.WriteHeader(404)
	}
}

func TestSessionListReadsEveryPageAndKeepsColumns(t *testing.T) {
	c := &inventoryCtl{n: 1000}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	// QA-021-1: the complete result, across five pages
	out, errs, code := auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS=80"), "session", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var env struct {
		Data struct {
			Count    int              `json:"count"`
			Sessions []map[string]any `json:"sessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Count != 1000 || len(env.Data.Sessions) != 1000 {
		t.Fatalf("json: %v count %d", err, env.Data.Count)
	}
	if c.pages.Load() < 5 {
		t.Errorf("pages read: %d", c.pages.Load())
	}
	// VER-021-1: at 80 and 120 columns no line exceeds the width, and identity and state survive
	for _, width := range []int{80, 120} {
		out, _, code := auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS="+strconv.Itoa(width)), "ls", "--state", "parked")
		if code != 0 {
			t.Fatalf("width %d: exit %d", width, code)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) < 3 {
			t.Fatalf("width %d: %q", width, out)
		}
		for _, l := range lines[:len(lines)-1] {
			if len([]rune(l)) > width {
				t.Errorf("width %d: line wider than the terminal: %q", width, l)
			}
		}
		// QA-021-2: a parked session with queued work is not labeled like an idle running one
		if !strings.Contains(lines[1], "parked") || !strings.Contains(lines[1], "2 queued") {
			t.Errorf("width %d: parked row lost its state or task: %q", width, lines[1])
		}
		if width >= 110 && !strings.Contains(lines[0], "KEY") {
			t.Errorf("width %d: wide table lacks the key column", width)
		}
	}
	// the alias and the filter agree with the verb
	if out, _, _ := auditExec(t, bin, cfg, dir, append(fastEnv(cfg), "COLUMNS=80"), "session", "list", "--state", "running"); strings.Contains(out, "parked") {
		t.Error("filter leaked another state")
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "list", "--state", "busy"); code != 2 || !strings.Contains(errs, "running, parked or dead") {
		t.Errorf("bad state: %d %s", code, errs)
	}
}

func TestSessionShowResolvesOnlyUniquePrefixes(t *testing.T) {
	c := &inventoryCtl{n: 6}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	// ids 0 and 1 share their first 8 characters; 9 characters tell them apart
	id0 := fmt.Sprintf("%08x%024x", 0, 0)
	id1 := fmt.Sprintf("%08x%024x", 0, 1)
	// QA-021-3: an ambiguous prefix names the candidates and does nothing
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "show", id0[:8])
	if code != 2 || !strings.Contains(errs, "matches 2 sessions") || !strings.Contains(errs, id1[:10]) {
		t.Errorf("ambiguous: %d %s", code, errs)
	}
	// a unique prefix resolves; the full id resolves; an unknown one is named
	out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "show", id1[:31]+"1", "--json")
	if code != 0 || !strings.Contains(out, `"id":"`+id1+`"`) {
		t.Errorf("unique prefix: %d %s", code, out)
	}
	if out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "show", id0); code != 0 || !strings.Contains(out, "session "+id0) {
		t.Errorf("full id: %d %s", code, out)
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "session", "show", "zzzz"); code != 2 || !strings.Contains(errs, "no session of yours") {
		t.Errorf("unknown: %d %s", code, errs)
	}
}

func TestSessionListEmptyStateAndCapabilityGate(t *testing.T) {
	srv := httptest.NewServer(&inventoryCtl{n: 0})
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "ls")
	if code != 0 || !strings.Contains(out, "No sessions. Start one: ks run") {
		t.Errorf("empty state: %d %q", code, out)
	}
	// a control plane without the capability disables the verb with the reason, and sends no list request
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[],"limits":{}}}`)
			return
		}
		t.Errorf("request reached the service: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer old.Close()
	bin2, cfg2 := buildAndAuth(t, old)
	if _, errs, code := auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "session", "list"); code == 0 || !strings.Contains(errs, "session.list") {
		t.Errorf("without the capability: %d %s", code, errs)
	}
	_ = url.Values{}
}

// TestSessionShowPrintsNoBlankFleetState: the service stopped exposing the
// engine's own state word (its inventory test forbids fleet_state), but
// `ks session show` kept printing "state running (fleet: )" with an empty
// value, and its --json re-emitted "fleet_state":"". Found against
// production on 2026-09-25. A value the service does not send is not shown.
func TestSessionShowPrintsNoBlankFleetState(t *testing.T) {
	c := &inventoryCtl{n: 2, noFleetState: true}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	id := fmt.Sprintf("%08x%024x", 0, 0)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "show", id)
	if code != 0 {
		t.Fatalf("show: %d %s", code, out)
	}
	if strings.Contains(out, "(fleet: )") {
		t.Errorf("session show prints an empty fleet state:\n%s", out)
	}
	js, _, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "show", id, "--json")
	if strings.Contains(js, `"fleet_state":""`) {
		t.Errorf("session show --json emits an empty fleet_state: %s", js)
	}
}
