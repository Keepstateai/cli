// ks agent folder and ks agent move: your folders, made, renamed, ordered and
// deleted from the terminal, and an agent put in one by name, with spaces in
// quotes, through the same routes the console's menu uses.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type folderCtl struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	taken    bool
}

func (c *folderCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	folders := []any{
		map[string]any{"id": "folder_1", "name": "Research team", "position": 0, "agent_ids": []any{"agent_1"}},
		map[string]any{"id": "folder_2", "name": "Ops", "position": 1, "agent_ids": []any{}},
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetfold0000000000000000000000001", "short_id": "fleetfold000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents" && r.Method == "GET":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "helper", "activity": "working"}, map[string]any{"id": "agent_2", "session_id": "session_1", "name": "scribe", "activity": "working"}}})
	case r.URL.Path == "/api/v2/agent-status":
		env(200, map[string]any{"items": []any{map[string]any{"agent_id": "agent_1", "name": "helper", "session_name": "c", "state": "idle"}}, "counts": map[string]any{}})
	default:
		if strings.HasPrefix(r.URL.Path, "/api/v2/agent-folders") || strings.HasSuffix(r.URL.Path, "/folder") {
			c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
			c.bodies = append(c.bodies, string(b))
		}
		switch {
		case r.URL.Path == "/api/v2/agent-folders" && r.Method == "GET":
			env(200, map[string]any{"context": "", "folders": folders})
		case r.URL.Path == "/api/v2/agent-folders" && r.Method == "POST":
			if c.taken {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_folder_name_taken","type":"ks_folder_name_taken","message":"you already have a folder called Ops here; choose another name. Nothing was created"}}`)
				return
			}
			env(201, map[string]any{"id": "folder_3", "name": "Notes", "position": 2, "agent_ids": []any{}})
		case strings.HasPrefix(r.URL.Path, "/api/v2/agent-folders/") && r.Method == "PATCH":
			env(200, map[string]any{"id": "folder_1", "name": "Research", "position": 0, "agent_ids": []any{"agent_1"}})
		case strings.HasPrefix(r.URL.Path, "/api/v2/agent-folders/") && r.Method == "DELETE":
			env(200, map[string]any{"deleted": true, "folder_id": "folder_1", "agents_moved": 1})
		case r.URL.Path == "/api/v2/agents/agent_1/folder" && r.Method == "PUT":
			env(200, map[string]any{"agent_id": "agent_1", "folder_id": "folder_1"})
		default:
			w.WriteHeader(404)
		}
	}
}

func TestAgentFoldersFromTheTerminal(t *testing.T) {
	c := &folderCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	last := func() (string, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := len(c.requests) - 1
		return c.requests[n], c.bodies[n]
	}

	out, _, code := run("agent", "folder", "list")
	if code != 0 || !strings.Contains(out, "1. Research team (1)") || !strings.Contains(out, "helper (c)") || !strings.Contains(out, "2. Ops (0)") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	if out, _, code := run("agent", "folder", "create", "Notes  for me"); code != 0 || !strings.Contains(out, "created the folder Notes") {
		t.Fatalf("create: %d\n%s", code, out)
	}
	if req, body := last(); req != "POST /api/v2/agent-folders" || body != `{"name":"Notes  for me"}` {
		t.Errorf("create sent %s %s: the service tidies the spaces, the client sends the name as typed", req, body)
	}
	// a folder is named by its name, quoted when it has spaces, capitals aside
	if out, _, code := run("agent", "folder", "rename", "research TEAM", "Research"); code != 0 || !strings.Contains(out, "renamed the folder Research team to Research") {
		t.Fatalf("rename: %d\n%s", code, out)
	}
	if req, body := last(); req != "PATCH /api/v2/agent-folders/folder_1" || body != `{"name":"Research"}` {
		t.Errorf("rename sent %s %s", req, body)
	}
	if _, _, code := run("agent", "folder", "order", "Ops", "--position", "1"); code != 0 {
		t.Fatalf("order: %d", code)
	}
	if req, body := last(); req != "PATCH /api/v2/agent-folders/folder_2" || body != `{"position":0}` {
		t.Errorf("order sent %s %s: places count from 1 for a person, from 0 for the service", req, body)
	}
	if out, _, code := run("agent", "folder", "delete", "Research team"); code != 0 || !strings.Contains(out, "no agent was deleted") || !strings.Contains(out, "1 agent(s)") {
		t.Fatalf("delete: %d\n%s", code, out)
	}
	if req, _ := last(); req != "DELETE /api/v2/agent-folders/folder_1" {
		t.Errorf("delete sent %s", req)
	}
	// an agent in a folder, before another, and back at the top level
	if out, _, code := run("agent", "move", "helper", "--session", "fleetfold", "--folder", "Research team", "--before", "scribe"); code != 0 || !strings.Contains(out, "moved helper to the folder Research team") {
		t.Fatalf("move: %d\n%s", code, out)
	}
	if req, body := last(); req != "PUT /api/v2/agents/agent_1/folder" || body != `{"before_agent_id":"agent_2","folder_id":"folder_1"}` {
		t.Errorf("move sent %s %s", req, body)
	}
	if out, _, code := run("agent", "move", "helper", "--session", "fleetfold", "--top"); code != 0 || !strings.Contains(out, "moved helper to the top level") {
		t.Fatalf("move to the top: %d\n%s", code, out)
	}
	if _, body := last(); body != `{"folder_id":""}` {
		t.Errorf("the top level is an empty folder id: %s", body)
	}
	// refusals say what did not happen, and send nothing the client can know is wrong
	n := len(c.requests)
	for _, args := range [][]string{
		{"agent", "move", "helper", "--session", "fleetfold"},
		{"agent", "move", "helper", "--session", "fleetfold", "--top", "--folder", "Ops"},
		{"agent", "move", "helper", "--session", "fleetfold", "--top", "--before", "scribe"},
		{"agent", "move", "helper", "--session", "fleetfold", "--folder", "No such folder"},
		{"agent", "folder", "rename", "No such folder", "x"},
	} {
		if _, errs, code := run(args...); code != exitUsage || !strings.Contains(strings.ToLower(errs), "nothing was moved") && !strings.Contains(errs, "no folder named") {
			t.Errorf("%v: %d\n%s", args, code, errs)
		}
	}
	for _, r := range c.requests[n:] {
		if strings.HasPrefix(r, "PUT ") || strings.HasPrefix(r, "PATCH ") {
			t.Errorf("a refused command still sent %s", r)
		}
	}
	c.mu.Lock()
	c.taken = true
	c.mu.Unlock()
	if _, errs, code := run("agent", "folder", "create", "Ops"); code != exitUsage || !strings.Contains(errs, "nothing was created") || !strings.Contains(errs, "already have a folder called Ops") {
		t.Errorf("a taken name: %d\n%s", code, errs)
	}
}
