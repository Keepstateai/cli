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

// runCtl is a control plane for `ks run --agent`: a capability registry, a
// preflight, session creation, provisioning and task submission, each
// recorded so a test can assert what was (and was not) asked for.
type runCtl struct {
	mu        sync.Mutex
	requests  []string
	idemKeys  []string
	workspace string // agent.workspace availability
	blockers  []string
	prov      map[string]any // the provisioning answer
}

func (c *runCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		c.idemKeys = append(c.idemKeys, r.URL.Path+"="+k)
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"client","note":"not certified here"},{"id":"preflight","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`, c.workspace)
	case r.Method == "POST" && r.URL.Path == "/api/v2/preflight":
		b := []any{}
		for _, x := range c.blockers {
			b = append(b, x)
		}
		env(200, map[string]any{"account_id": "acct_t", "blockers": b, "ready": len(b) == 0})
	case r.Method == "POST" && r.URL.Path == "/api/v2/sessions":
		env(201, map[string]any{"id": "session_abc123", "revision": 1, "primary_agent_id": "agent_main1"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/provision"):
		env(200, c.prov)
	case r.Method == "GET" && r.URL.Path == "/api/v2/tasks":
		env(200, map[string]any{"items": []any{}, "next_cursor": ""})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tasks"):
		env(201, map[string]any{"id": "task_first1", "replayed": false})
	default:
		env(404, map[string]any{})
	}
}

func (c *runCtl) asked(req string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if r == req || strings.HasPrefix(r, req) {
			return true
		}
	}
	return false
}

func readyAnswer() map[string]any {
	return map[string]any{"session_id": "session_abc123", "agent_id": "agent_main1", "ready": true, "agent_activity": "ready", "cleaned_up": false,
		"note":   "the agent is Ready: its runner is waiting for a task, and no model call has been made. Session time is billed from now until the session is parked or killed",
		"phases": []any{map[string]any{"phase": "create_guest", "done": true, "detail": "a machine was created"}, map[string]any{"phase": "start_agent", "done": true, "detail": "running"}}}
}

func runAgentEnv(t *testing.T, c *runCtl) (string, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func TestRunAgentReachesReadyAndSaysSo(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer()}
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--name", "checkout")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "agent main is Ready in session session_abc123") {
		t.Fatalf("no Ready line for an agent the service observed ready:\n%s", out)
	}
	if !strings.Contains(out, "no model call") || !strings.Contains(out, "billed") {
		t.Fatalf("the Ready answer does not carry the service's disclosure:\n%s", out)
	}
	for _, req := range []string{"POST /api/v2/preflight", "POST /api/v2/sessions", "POST /api/v2/sessions/session_abc123/provision"} {
		if !c.asked(req) {
			t.Fatalf("%s was not asked: %v", req, c.requests)
		}
	}
	// the mutations carry an operation id, so a repeat is one machine
	var provKey bool
	for _, k := range c.idemKeys {
		if strings.HasPrefix(k, "/api/v2/sessions/session_abc123/provision=") {
			provKey = true
		}
	}
	if !provKey {
		t.Fatalf("provisioning was sent without an Idempotency-Key: %v", c.idemKeys)
	}
	if c.asked("POST /api/v2/agents/") {
		t.Fatal("a task was submitted though none was given: no model call may be caused by a bare run")
	}
}

func TestRunAgentWithATaskSubmitsItOnlyAfterReady(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer()}
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--task", "run the tests")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !c.asked("POST /api/v2/agents/agent_main1/tasks") {
		t.Fatalf("the first task was not submitted: %v", c.requests)
	}
	// and not before the answer that said Ready
	c.mu.Lock()
	defer c.mu.Unlock()
	prov, task := -1, -1
	for i, r := range c.requests {
		if strings.HasSuffix(r, "/provision") {
			prov = i
		}
		if strings.HasSuffix(r, "/tasks") && strings.HasPrefix(r, "POST") {
			task = i
		}
	}
	if !(prov >= 0 && task > prov) {
		t.Fatalf("the task was submitted before Ready was observed: %v", c.requests)
	}
}

func TestRunAgentRefusedWhereAgentModeIsNotAvailableCreatesNothing(t *testing.T) {
	c := &runCtl{workspace: "unavailable", prov: readyAnswer()}
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent")
	if code == 0 {
		t.Fatalf("run --agent succeeded where agent mode is unavailable:\n%s", out)
	}
	if !strings.Contains(errOut, "agent.workspace") {
		t.Fatalf("the refusal does not name the capability:\n%s", errOut)
	}
	if c.asked("POST /api/v2/sessions") {
		t.Fatal("a session was created although agent mode is unavailable")
	}
}

// QA-029: a blocker stops the run before anything is created or billed.
func TestRunAgentStopsOnAPreflightBlockerBeforeCreatingAnything(t *testing.T) {
	c := &runCtl{workspace: "available", blockers: []string{"no enabled anthropic key on the account"}, prov: readyAnswer()}
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent")
	if code == 0 {
		t.Fatalf("run --agent proceeded past a blocker:\n%s", out)
	}
	if !strings.Contains(errOut, "no enabled anthropic key") || !strings.Contains(errOut, "nothing was created") {
		t.Fatalf("the blocker and its consequence are not stated:\n%s", errOut)
	}
	if c.asked("POST /api/v2/sessions") {
		t.Fatal("a session was created past a preflight blocker")
	}
}

// QA-030-2 / "do not print a successful Ready line for a merely allocated
// machine": a failed setup and a not-yet-ready agent are each their own
// failure, and neither prints Ready.
func TestRunAgentNeverPrintsReadyForAnAgentThatIsNotReady(t *testing.T) {
	failed := map[string]any{"session_id": "session_abc123", "agent_id": "agent_main1", "ready": false, "agent_activity": "unavailable", "cleaned_up": true,
		"note":   "setup did not complete; the machine it created was destroyed, so nothing is left running or billed. Nothing was sent to a model",
		"phases": []any{map[string]any{"phase": "start_agent", "done": false, "detail": "the agent could not be started"}, map[string]any{"phase": "cleanup", "done": true, "detail": "destroyed"}}}
	slow := map[string]any{"session_id": "session_abc123", "agent_id": "agent_main1", "ready": false, "agent_activity": "starting", "cleaned_up": false,
		"note":   "a supervisor is running in the machine, and the agent has not reported Ready",
		"phases": []any{map[string]any{"phase": "start_agent", "done": true, "detail": "running"}}}
	for _, tc := range []struct {
		name string
		prov map[string]any
		exit int
		want string
	}{
		{"setup failed and was cleaned up", failed, exitFailed, "destroyed"},
		{"running, not yet ready", slow, exitTemporary, "ks agent status main --session session_abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &runCtl{workspace: "available", prov: tc.prov}
			bin, cfg := runAgentEnv(t, c)
			out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--task", "x")
			if code != tc.exit {
				t.Fatalf("exit %d, want %d\n%s\n%s", code, tc.exit, out, errOut)
			}
			if strings.Contains(out+errOut, "is Ready") {
				t.Fatalf("Ready was printed for an agent that is not ready:\n%s\n%s", out, errOut)
			}
			if !strings.Contains(out+errOut, tc.want) {
				t.Fatalf("the output does not say %q:\n%s\n%s", tc.want, out, errOut)
			}
			if c.asked("POST /api/v2/agents/") {
				t.Fatal("a task was submitted to an agent that is not Ready")
			}
		})
	}
}

func TestRunFlagsThatBelongToAgentModeAreRefusedWithoutIt(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer()}
	bin, cfg := runAgentEnv(t, c)
	for _, args := range [][]string{{"run", "--task", "x"}, {"run", "--open"}, {"run", "--name", "n"}, {"run", "--agent", "--image", "base"}} {
		out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if code != exitUsage {
			t.Fatalf("%v: exit %d, want %d\n%s\n%s", args, code, exitUsage, out, errOut)
		}
	}
	if c.asked("POST /api/v2/sessions") || c.asked("POST /api/sessions") {
		t.Fatalf("a refused command line created a session: %v", c.requests)
	}
}
