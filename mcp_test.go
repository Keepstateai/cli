// ks mcp driven by a scripted stdio client against a fake control plane, no
// model: both protocol eras, the confirm digest and the elicitation round
// trips, the refusals, and the safety properties (no secret in any schema,
// stdout only JSON-RPC, signed out in plain words, the end of input exits 0).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// mcpCtl adds the inventory to interopCtl, for the verbs that resolve a
// session by --session.
type mcpCtl struct{ *interopCtl }

func (c mcpCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && (r.URL.Path == "/api/v2/sessions" || r.URL.Path == "/api/v2/queue-holds" || r.URL.Path == "/api/v2/tasks") {
		c.mu.Lock()
		c.reqs = append(c.reqs, r.Method+" "+r.URL.RequestURI()+" ")
		items := []any{}
		if r.URL.Path == "/api/v2/sessions" {
			for id, name := range c.names {
				items = append(items, map[string]any{"id": id, "short_id": id, "name": name, "runtime_state": "running", "record_id": id})
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"items": items, "next_cursor": ""}})
		return
	}
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop") {
		c.mu.Lock()
		c.reqs = append(c.reqs, r.Method+" "+r.URL.RequestURI()+" ")
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"agent_id": "agent_1", "stop_requested": true, "queue_held": true}})
		return
	}
	c.interopCtl.ServeHTTP(w, r)
}

func (c *interopCtl) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

type mcpClient struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	all   []string
	n     int
}

func startMCP(t *testing.T, bin string, env []string, args ...string) *mcpClient {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"mcp"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &mcpClient{t: t, cmd: cmd, in: in, lines: make(chan string, 64)}
	go func() {
		sc := bufio.NewScanner(outp)
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return c
}

func (c *mcpClient) write(v any) {
	b, _ := json.Marshal(v)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// next reads the next message the server wrote.
func (c *mcpClient) next() map[string]any {
	c.t.Helper()
	select {
	case l, ok := <-c.lines:
		if !ok {
			c.t.Fatal("the server closed its output")
		}
		c.all = append(c.all, l)
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
			c.t.Fatalf("stdout carried a line that is not JSON-RPC: %q", l)
		}
		return m
	case <-time.After(60 * time.Second):
		c.t.Fatal("no answer from the server")
	}
	return nil
}

// call sends one request and answers its response.
func (c *mcpClient) call(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.n++
	id := c.n
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.write(req)
	for {
		m := c.next()
		if f, ok := m["id"].(float64); ok && int(f) == id {
			return m
		}
	}
}

func modernMeta(caps map[string]any) map[string]any {
	return map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": caps,
		"io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}
}

func toolCall(name string, args map[string]any, meta map[string]any) map[string]any {
	p := map[string]any{"name": name, "arguments": args}
	if meta != nil {
		p["_meta"] = meta
	}
	return p
}

func resultOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("not a result: %v", m)
	}
	return r
}

func textOf(r map[string]any) string {
	cs, _ := r["content"].([]any)
	if len(cs) == 0 {
		return ""
	}
	s, _ := cs[0].(map[string]any)["text"].(string)
	return s
}

func mcpEnv(t *testing.T) (*interopCtl, string, []string) {
	t.Helper()
	c := newInteropCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(mcpCtl{c})
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	env := append(fastEnv(cfg), "KS_WAIT_POLL_MS=50", "KS_CALLER_ID=mcp-test", "HC_TERMINAL_ID=", "HERDR_PANE_ID=", "HOME="+t.TempDir())
	return c, bin, env
}

func TestMCPLegacyEra(t *testing.T) {
	_, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	r := resultOf(t, c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "x", "version": "1"}}))
	if r["protocolVersion"] != "2025-06-18" || r["serverInfo"].(map[string]any)["name"] != "keepstate" {
		t.Fatalf("initialize: %v", r)
	}
	if r := resultOf(t, c.call("initialize", map[string]any{"protocolVersion": "1999-01-01"})); r["protocolVersion"] != "2025-06-18" {
		t.Fatalf("an unknown version is answered with 2025-06-18: %v", r)
	}
	c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if m := c.call("ping", nil); m["result"] == nil {
		t.Fatalf("ping: %v", m)
	}
	l1, _ := json.Marshal(resultOf(t, c.call("tools/list", nil))["tools"])
	l2, _ := json.Marshal(resultOf(t, c.call("tools/list", nil))["tools"])
	if string(l1) != string(l2) {
		t.Fatal("tools/list is not deterministic")
	}
	var tools []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(l1, &tools)
	names := []string{}
	for _, x := range tools {
		names = append(names, x.Name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"ks_skill", "ks_agent_run", "ks_agent_send", "ks_agent_wait", "ks_agent_stop", "ks_task_cancel"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tools/list has no %s", want)
		}
	}
	words := map[string]bool{}
	for _, n := range names {
		for _, w := range strings.Split(n, "_") {
			words[w] = true
		}
	}
	for _, never := range []string{"delete", "kill", "remove", "key", "keys", "billing", "organization", "adviser", "restore", "fork", "apply", "trust"} {
		if words[never] {
			t.Errorf("tools/list offers a tool for %s: %s", never, joined)
		}
	}
	if c.call("no/such", nil)["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatal("an unknown method is not -32601")
	}
}

func TestMCPModernEra(t *testing.T) {
	_, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	d := resultOf(t, c.call("server/discover", map[string]any{"_meta": modernMeta(map[string]any{})}))
	if d["resultType"] != "complete" || fmt.Sprint(d["supportedVersions"]) != "[2026-07-28 2025-11-25 2025-06-18]" {
		t.Fatalf("server/discover: %v", d)
	}
	// no version in _meta, and no initialize was made: -32022
	e := c.call("tools/list", map[string]any{})["error"].(map[string]any)
	if e["code"].(float64) != -32022 {
		t.Fatalf("a request without a version: %v", e)
	}
	e = c.call("tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "1900-01-01", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}})["error"].(map[string]any)
	if e["code"].(float64) != -32022 || e["data"].(map[string]any)["requested"] != "1900-01-01" {
		t.Fatalf("an unsupported version: %v", e)
	}
	l := resultOf(t, c.call("tools/list", map[string]any{"_meta": modernMeta(map[string]any{})}))
	if l["resultType"] != "complete" || l["cacheScope"] != "private" || l["ttlMs"] == nil {
		t.Fatalf("modern tools/list: %v", l)
	}
	r := resultOf(t, c.call("tools/call", toolCall("ks_skill", map[string]any{}, modernMeta(map[string]any{}))))
	if r["resultType"] != "complete" || r["isError"] != false || !strings.Contains(textOf(r), "The parent rule") {
		t.Fatalf("modern ks_skill: %v", r)
	}
}

func TestMCPConfirmDigest(t *testing.T) {
	fake, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	args := map[string]any{"name": "review", "task": "review the change", "template": "reviewer", "outlive_parent": true}
	r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", args, nil)))
	var plan struct {
		Needs   bool   `json:"needs_confirmation"`
		Plan    string `json:"plan"`
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(textOf(r)), &plan); err != nil || !plan.Needs || len(plan.Confirm) != 16 || !strings.Contains(plan.Plan, "metered") {
		t.Fatalf("the first call is a plan: %v %v", r, err)
	}
	fixtureBarrier()
	if n := fake.total(); n != 0 {
		t.Fatalf("the first call made %d requests", n)
	}
	// a wrong digest: refused, nothing done
	wrong := map[string]any{"confirm": "0000000000000000"}
	for k, v := range args {
		wrong[k] = v
	}
	if r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", wrong, nil))); r["isError"] != true || !strings.Contains(textOf(r), "nothing was done") {
		t.Fatalf("a wrong digest: %v", r)
	}
	// the same digest for other arguments: refused
	other := map[string]any{"name": "review", "task": "something else", "template": "reviewer", "outlive_parent": true, "confirm": plan.Confirm}
	if r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", other, nil))); r["isError"] != true {
		t.Fatalf("a digest for other arguments: %v", r)
	}
	if fake.mutations() != 0 {
		t.Fatal("refused confirms sent mutations")
	}
	right := map[string]any{"confirm": plan.Confirm}
	for k, v := range args {
		right[k] = v
	}
	r = resultOf(t, c.call("tools/call", toolCall("ks_agent_run", right, nil)))
	if r["isError"] != false || !strings.Contains(textOf(r), `"session":"session_1"`) {
		t.Fatalf("the confirmed call: %v", r)
	}
	if n := fake.count("POST /api/v2/sessions "); n != 1 {
		t.Fatalf("the confirmed call created %d sessions", n)
	}
	// a digest acts once
	if r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", right, nil))); r["isError"] != true || fake.count("POST /api/v2/sessions ") != 1 {
		t.Fatalf("a digest acted twice: %v", r)
	}
	// an act tool acts at once: send to the child just started, tagged
	r = resultOf(t, c.call("tools/call", toolCall("ks_agent_send", map[string]any{"name": "review", "text": "and the tests"}, nil)))
	if r["isError"] != false || !strings.Contains(fake.lastReq("POST /api/v2/agents/agent_1/tasks"), `mcp-test`) {
		t.Fatalf("send: %v\n%s", r, fake.lastReq("POST /api/v2/agents/agent_1/tasks"))
	}
	// a stop is planned against the agent's revision, then confirmed by its name
	stop := map[string]any{"name": "review", "session": "session_1"}
	r = resultOf(t, c.call("tools/call", toolCall("ks_agent_stop", stop, nil)))
	_ = json.Unmarshal([]byte(textOf(r)), &plan)
	if !plan.Needs || fake.count("POST /api/v2/agents/agent_1/stop") != 0 {
		t.Fatalf("stop's first call: %v", r)
	}
	stop["confirm"] = plan.Confirm
	resultOf(t, c.call("tools/call", toolCall("ks_agent_stop", stop, nil)))
	if fake.count("POST /api/v2/agents/agent_1/stop") != 1 {
		t.Fatal("the confirmed stop was not sent once")
	}
}

func TestMCPElicitation(t *testing.T) {
	fake, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	meta := modernMeta(map[string]any{"elicitation": map[string]any{}})
	args := map[string]any{"name": "helper", "task": "draft notes", "outlive_parent": true}
	r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", args, meta)))
	reqs, _ := r["inputRequests"].(map[string]any)
	ask, _ := reqs["confirm"].(map[string]any)
	state, _ := r["requestState"].(string)
	if r["resultType"] != "input_required" || ask["method"] != "elicitation/create" || state == "" || fake.mutations() != 0 {
		t.Fatalf("an elicitation-capable client is asked through its client: %v", r)
	}
	// declined: nothing
	p := toolCall("ks_agent_run", args, meta)
	p["requestState"] = state
	p["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "decline"}}
	if r := resultOf(t, c.call("tools/call", p)); !strings.Contains(textOf(r), "Not confirmed") || fake.mutations() != 0 {
		t.Fatalf("declined: %v", r)
	}
	// asked again, accepted: once
	r = resultOf(t, c.call("tools/call", toolCall("ks_agent_run", args, meta)))
	p["requestState"] = r["requestState"]
	p["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
	r = resultOf(t, c.call("tools/call", p))
	if r["resultType"] != "complete" || r["isError"] != false || fake.count("POST /api/v2/sessions ") != 1 {
		t.Fatalf("accepted: %v", r)
	}
}

func TestMCPLegacyElicitation(t *testing.T) {
	fake, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"elicitation": map[string]any{}}})
	c.n++
	c.write(map[string]any{"jsonrpc": "2.0", "id": c.n, "method": "tools/call", "params": toolCall("ks_agent_run", map[string]any{"name": "kid", "task": "t", "outlive_parent": true}, nil)})
	ask := c.next()
	if ask["method"] != "elicitation/create" || fake.mutations() != 0 {
		t.Fatalf("a legacy client that declared elicitation is asked with elicitation/create: %v", ask)
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": ask["id"], "result": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}})
	res := c.next()
	if r := resultOf(t, res); r["isError"] != false || fake.count("POST /api/v2/sessions ") != 1 {
		t.Fatalf("accepted in the client: %v", res)
	}
}

func TestMCPRefusalsAndSafety(t *testing.T) {
	fake, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	// a destructive verb that is not a tool: refused, nothing sent
	for _, name := range []string{"ks_session_delete", "ks_agent_remove", "ks_kill", "ks_key_delete", "ks_organization_buy"} {
		m := c.call("tools/call", toolCall(name, map[string]any{"session": "session_1"}, nil))
		if e, ok := m["error"].(map[string]any); !ok || e["code"].(float64) != -32602 {
			t.Errorf("%s: %v", name, m)
		}
	}
	// arguments outside the schema, such as a key, are refused
	if r := resultOf(t, c.call("tools/call", toolCall("ks_agent_run", map[string]any{"name": "x", "task": "t", "key": "vlt_1"}, nil))); r["isError"] != true {
		t.Fatalf("an argument outside the schema: %v", r)
	}
	if fake.mutations() != 0 || fake.total() != 0 {
		t.Fatal("refused calls made requests")
	}
	// --read-only: only reads are listed, and an acting tool is refused
	ro := startMCP(t, bin, env, "--read-only")
	ro.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	l, _ := json.Marshal(resultOf(t, ro.call("tools/list", nil)))
	if strings.Contains(string(l), "ks_agent_run") || strings.Contains(string(l), "ks_agent_send") || !strings.Contains(string(l), "ks_agent_read") {
		t.Fatalf("--read-only lists: %s", l)
	}
	if r := resultOf(t, ro.call("tools/call", toolCall("ks_agent_stop", map[string]any{"name": "x"}, nil))); r["isError"] != true || !strings.Contains(textOf(r), "read-only") || fake.total() != 0 {
		t.Fatalf("--read-only stop: %v", r)
	}
	// every line on stdout was JSON-RPC (next() refuses any other); the end
	// of input exits 0
	_ = c.in.Close()
	for range c.lines {
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("the end of input did not exit 0: %v", err)
	}
}

func TestMCPSignedOut(t *testing.T) {
	bin := sharedKS(t)
	c := startMCP(t, bin, []string{"XDG_CONFIG_HOME=" + t.TempDir(), "HOME=" + t.TempDir()})
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	r := resultOf(t, c.call("tools/call", toolCall("ks_session_list", map[string]any{}, nil)))
	if r["isError"] != true || textOf(r) != "Not signed in. Run ks login in a terminal." {
		t.Fatalf("signed out: %v", r)
	}
}

func TestMCPSchemasHoldNoSecretAndNameRealVerbs(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range mcpTools {
		if seen[tool.Name] || !strings.HasPrefix(tool.Name, "ks_") {
			t.Errorf("tool name %q is repeated or not ks_", tool.Name)
		}
		seen[tool.Name] = true
		s := tool.schema()
		if s["additionalProperties"] != false {
			t.Errorf("%s accepts properties outside its schema", tool.Name)
		}
		for name := range s["properties"].(map[string]any) {
			l := strings.ToLower(name)
			for _, bad := range []string{"key", "token", "secret", "password", "credential"} {
				if strings.Contains(l, bad) {
					t.Errorf("%s has a %q property", tool.Name, name)
				}
			}
		}
		if (tool.Kind == "confirm") != (tool.Plan != nil) {
			t.Errorf("%s: a confirm tool has a plan, and only one does", tool.Name)
		}
		for _, f := range tool.Fixed {
			if f == "--yes" {
				t.Errorf("%s passes --yes", tool.Name)
			}
		}
		if tool.Kind != "read" && !strings.Contains(tool.Desc, "confirm") && !strings.Contains(tool.Desc, "at once") {
			t.Errorf("%s does not disclose whether it acts at once", tool.Name)
		}
		if !strings.Contains(tool.Desc, "data") && tool.Name != "ks_skill" && tool.Name != "ks_cruise_models" {
			t.Errorf("%s does not say its results are data", tool.Name)
		}
	}
}

func TestMCPArgvKeepsArgumentsInTheirPlace(t *testing.T) {
	got := strings.Join(findMCPTool("ks_agent_list").argv(map[string]any{"all": true}, false), " ")
	if got != "agent list --json --no-input --all" {
		t.Fatalf("ks_agent_list with all: %q", got)
	}
	got = strings.Join(findMCPTool("ks_agent_send").argv(map[string]any{"name": "review", "text": "--yes", "session": "-s"}, false), " ")
	if got != "agent send --json --no-input --session=-s -- review --yes" {
		t.Fatalf("a value that looks like a flag is kept a value: %q", got)
	}
}
