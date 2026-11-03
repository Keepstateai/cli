// The chosen context reaches every list: in an organization's context the
// client asks for that organization's rows, so your personal agents, jobs
// and agents to consult never appear there, and a list with no organization
// view is refused in the service's words with the step back to personal.
// The fake answers as a control plane that ignored the context would: the
// personal rows whenever no context is sent.
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

type ctxListCtl struct {
	*agentCtl
	mu    sync.Mutex
	asked []string
}

func (c *ctxListCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.asked = append(c.asked, r.Method+" "+r.URL.RequestURI())
	c.mu.Unlock()
	ctx := r.URL.Query().Get("context")
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"schema_version":2,"request_id":"r","error":{"code":"ks_context_unsupported","type":"ks_context_unsupported","message":"this has no organization view yet, so nothing was read or changed in the organization's context; what it shows is your personal account, which you reach in the personal context","retryable":false,"work_started":"no","next_action":"ks context use personal"}}`)
	}
	switch {
	case r.URL.Path == "/api/v2/orgs/org_a":
		env(200, map[string]any{"id": "org_a", "name": "Acme", "your_role": "owner", "your_seat": "assigned", "seats": map[string]any{"used": 2, "limit": nil}})
	case r.URL.Path == "/api/v2/agent-status" && ctx == "org_a":
		env(200, map[string]any{"items": []any{srec("agt_org", "ses_org", "acmebuild", "acmework", "working", "", "Working.")}, "counts": map[string]any{}, "truncated": false})
	case r.URL.Path == "/api/v2/agent-status":
		env(200, map[string]any{"items": []any{srec("agt_mine", "ses_mine", "mybuild", "PERSONALVOGON", "blocked", "permission", "shell: rm")}, "counts": map[string]any{}, "truncated": false})
	case (r.URL.Path == "/api/jobs" || r.URL.Path == "/api/v2/agent-registry") && ctx != "":
		refuse()
	case r.URL.Path == "/api/jobs":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"job_personal1","state":"accepted","goal":"PERSONALJOB","created_at":"2026-11-03T10:00:00Z"}]`)
	case r.URL.Path == "/api/v2/agent-registry":
		env(200, map[string]any{"items": []any{map[string]any{"selector": "PERSONALADVISER/main", "agent_id": "agt_mine", "session_id": "ses_mine"}}, "next_cursor": ""})
	default:
		c.agentCtl.ServeHTTP(w, r)
	}
}

func TestEveryListAgreesWithTheChosenContext(t *testing.T) {
	c := &ctxListCtl{agentCtl: newAgentCtl()}
	syncFixture(t, &c.agentCtl.mu)
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	if out, errs, code := run("context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	// every agent: the organization's that are yours, never the personal
	out, errs, code := run("agent", "list", "--all")
	if code != 0 || !strings.Contains(out, "acmebuild") || strings.Contains(out, "mybuild") || strings.Contains(out, "PERSONALVOGON") {
		t.Fatalf("agent list --all in the organization's context: %d\n%s%s", code, out, errs)
	}
	if !strings.HasPrefix(out, "Context: organization org_a") || !strings.Contains(out, "Nothing needs you") {
		t.Errorf("the list does not say its context, or counts the personal agent:\n%s", out)
	}
	js, _, code := run("agent", "list", "--all", "--json")
	var doc struct {
		Data struct {
			Context string         `json:"context"`
			Items   []statusRecord `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil || code != 0 || doc.Data.Context != "org_a" || len(doc.Data.Items) != 1 || doc.Data.Items[0].AgentID != "agt_org" {
		t.Errorf("agent list --all --json in the organization's context: %d %v %s", code, err, js)
	}
	// lists with no organization view: refused in words, the step back named,
	// nothing personal shown
	for _, verb := range [][]string{{"cruise", "status"}, {"adviser", "discover"}} {
		out, errs, code := run(verb...)
		if code == 0 || !strings.Contains(errs, "no organization view") || !strings.Contains(errs, "ks context use personal") ||
			strings.Contains(out+errs, "PERSONALJOB") || strings.Contains(out+errs, "PERSONALADVISER") {
			t.Errorf("ks %s in the organization context: %d\n%s%s", strings.Join(verb, " "), code, out, errs)
		}
	}
	c.mu.Lock()
	for _, a := range c.asked {
		for _, p := range []string{"/api/v2/agent-status", "/api/jobs", "/api/v2/agent-registry"} {
			if strings.HasPrefix(strings.SplitN(a, " ", 2)[1], p) && !strings.Contains(a, "context=org_a") {
				t.Errorf("asked %s in the organization's context without it", a)
			}
		}
	}
	c.asked = nil
	c.mu.Unlock()

	// back to personal: the personal rows, and no context sent
	if _, _, code := run("context", "use", "personal"); code != 0 {
		t.Fatal("context use personal")
	}
	out, _, code = run("agent", "list", "--all")
	if code != 0 || !strings.Contains(out, "mybuild") || strings.Contains(out, "acmebuild") || strings.Contains(out, "Context:") {
		t.Errorf("agent list --all in the personal context: %d\n%s", code, out)
	}
	_, _, _ = run("cruise", "status") // the personal jobs, asked for as always
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := false
	for _, a := range c.asked {
		jobs = jobs || a == "GET /api/jobs"
		if strings.Contains(a, "context=") {
			t.Errorf("asked %s in the personal context with a context", a)
		}
	}
	if !jobs {
		t.Errorf("cruise status in the personal context did not ask for the personal jobs: %v", c.asked)
	}
}
