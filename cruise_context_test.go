// Cruise in an organization's context: the approval binds the
// organization's keys (read through the preflight, never the member's own
// key list), the job and every read of it go to the service in that context,
// the output says whose job it is, and the organization's refusals (no
// credit, a budget, a role that reads no jobs) come back in the service's
// words with the one step that fixes them. In the personal context nothing
// changes: no context is sent.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgCruiseFake struct {
	*fakeCtl
	mu      sync.Mutex
	asked   []string
	refusal string // "credit", "budget", "role": how POST /api/jobs?context= refuses
	pfOrg   []string
}

func (f *orgCruiseFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked = append(f.asked, r.Method+" "+r.URL.RequestURI())
	refusal := f.refusal
	f.mu.Unlock()
	ctx := r.URL.Query().Get("context")
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(code int, typ, msg string) {
		writeJSON(code, map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg}})
	}
	switch {
	case r.URL.Path == "/api/v2/orgs/org_a":
		writeJSON(200, map[string]any{"schema_version": 2, "data": map[string]any{"id": "org_a", "name": "Acme", "your_role": "member", "your_seat": "assigned", "seats": map[string]any{"used": 3, "limit": nil}}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys" && ctx != "":
		refuse(403, "ks_forbidden", "forbidden: your role in this organization (member) does not allow org.keys.manage")
	case r.Method == "POST" && r.URL.Path == "/api/v2/preflight":
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		org, _ := body["org_id"].(string)
		f.mu.Lock()
		f.pfOrg = append(f.pfOrg, org)
		f.mu.Unlock()
		keys := []any{map[string]any{"id": "vlt_anthropic1", "provider": "anthropic", "alias": "dev", "last4": "abcd", "enabled": true, "revision": 1, "created_at": "2026-09-01T00:00:00Z"}}
		if org != "" {
			keys = []any{map[string]any{"id": "vlt_orgkey1", "provider": "anthropic", "alias": "acme", "last4": "ORGK", "enabled": true, "revision": 1, "created_at": "2026-10-01T00:00:00Z"}}
		}
		writeJSON(200, map[string]any{"schema_version": 2, "data": map[string]any{"keys": keys, "ready": true,
			"checks": []any{map[string]any{"check": "workspace", "status": "pass", "detail": "ok", "category": "setup"}}}})
	case r.Method == "POST" && r.URL.Path == "/api/jobs" && ctx != "" && refusal == "credit":
		refuse(402, "ks_organization_no_credit", "this organization has no credit, and its Cruise jobs are billed to the organization, never to a member; nothing was queued")
	case r.Method == "POST" && r.URL.Path == "/api/jobs" && ctx != "" && refusal == "budget":
		refuse(429, "ks_budget_reached", "the spend ceiling $2.00 does not fit the Infra team's budget for November 2026 ($1.00); nothing was queued")
	case r.Method == "GET" && r.URL.Path == "/api/jobs" && ctx != "" && refusal == "role":
		refuse(403, "ks_forbidden", "your role in this organization (billing) reads its spend on the Budgets page, not its Cruise jobs")
	case r.Method == "GET" && r.URL.Path == "/api/jobs" && ctx == "org_a":
		writeJSON(200, []any{map[string]any{"id": "job_0123456789ab", "state": "running", "goal": "ACMEJOBGOAL", "org_id": "org_a", "created_by": "acct_me", "created_at": "2026-11-04T10:00:00Z"}})
	case r.Method == "GET" && r.URL.Path == "/api/jobs" && ctx == "":
		writeJSON(200, []any{map[string]any{"id": "job_personal1", "state": "accepted", "goal": "PERSONALJOBGOAL", "created_at": "2026-11-03T10:00:00Z"}})
	default:
		f.fakeCtl.ServeHTTP(w, r)
	}
}

func (f *orgCruiseFake) seenURIs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func TestCruiseInAnOrganizationsContext(t *testing.T) {
	f := &orgCruiseFake{fakeCtl: &fakeCtl{job: gateJob(), artifact: []byte("bytes")}}
	syncFixture(t, &f.fakeCtl.mu)
	syncFixture(t, &f.mu)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	repo := demoRepo(t)
	if out, errs, code := ksIn(t, bin, cfg, repo, "context", "use", "org_a"); code != 0 {
		t.Fatalf("context use: %d %s%s", code, out, errs)
	}
	if out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--goal", "make the inventory tests pass"); code != 0 {
		t.Fatalf("init: %d %s%s", code, out, errs)
	}
	// approve binds the organization's key, read through the preflight
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "approve")
	if code != 0 {
		t.Fatalf("approve in the organization's context: %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "vlt_orgkey1") || strings.Contains(out, "vlt_anthropic1") {
		t.Errorf("the approval does not bind the organization's key:\n%s", out)
	}
	if !strings.Contains(errs, "Context: organization org_a") || !strings.Contains(errs, "never from yours") {
		t.Errorf("approve does not say whose job it is:\n%s", errs)
	}
	m, err := readDraftAt(repo)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(m["routes"]); !strings.Contains(string(b), "vlt_orgkey1") || strings.Contains(string(b), "vlt_anthropic1") {
		t.Errorf("the draft's routes: %s", b)
	}
	// run: the route check and the preflight in the organization's context,
	// then one job in it
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "run")
	if code != 0 {
		t.Fatalf("run in the organization's context: %d\n%s%s", code, out, errs)
	}
	posted := false
	for _, a := range f.seenURIs() {
		if strings.HasPrefix(a, "GET /api/v2/keys") {
			t.Errorf("asked the key list (%s): a member reads the organization's keys through the preflight", a)
		}
		if strings.HasPrefix(a, "POST /api/jobs") {
			posted = true
			if a != "POST /api/jobs?context=org_a" {
				t.Errorf("the job was sent as %q, not in the organization's context", a)
			}
		}
	}
	if !posted {
		t.Errorf("run sent no job: %v", f.seenURIs())
	}
	f.mu.Lock()
	for _, o := range f.pfOrg {
		if o != "org_a" {
			t.Errorf("a preflight in the organization's context named %q", o)
		}
	}
	f.mu.Unlock()
	// status: the organization's jobs, said so; reads by id in the context
	out, errs, code = ksIn(t, bin, cfg, repo, "cruise", "status")
	if code != 0 || !strings.HasPrefix(out, "Context: organization org_a") || strings.Contains(out, "PERSONALJOBGOAL") {
		t.Errorf("cruise status in the organization's context: %d\n%s%s", code, out, errs)
	}
	for _, a := range f.seenURIs() {
		if strings.HasPrefix(a, "GET /api/jobs/") && !strings.Contains(a, "context=org_a") {
			t.Errorf("asked %s in the organization's context without it", a)
		}
	}
	// the organization's refusals, in the service's words with the fixing step
	for _, c := range []struct{ refusal, verb, kind, next string }{
		{"credit", "run", "ks_organization_no_credit", "ks organization buy"},
		{"budget", "run", "ks_budget_reached", "ks budget ask"},
		{"role", "status", "ks_forbidden", "ks organization spend"},
	} {
		f.mu.Lock()
		f.refusal = c.refusal
		f.mu.Unlock()
		out, errs, code := ksIn(t, bin, cfg, repo, "cruise", c.verb, "--json")
		var doc struct {
			Error struct {
				Kind       string `json:"code"`
				Message    string `json:"message"`
				NextAction string `json:"next_action"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(out), &doc)
		if code == 0 || doc.Error.Kind != c.kind || !strings.Contains(doc.Error.NextAction, c.next) || doc.Error.Message == "" || strings.Contains(doc.Error.NextAction, "ks login") {
			t.Errorf("%s refused for %s: %d %+v\n%s%s", c.verb, c.refusal, code, doc.Error, out, errs)
		}
	}
	f.mu.Lock()
	f.refusal, f.asked = "", nil
	f.mu.Unlock()
	// back to personal: no context anywhere, your own jobs
	if _, _, code := ksIn(t, bin, cfg, repo, "context", "use", "personal"); code != 0 {
		t.Fatal("context use personal")
	}
	out, _, code = ksIn(t, bin, cfg, repo, "cruise", "status", "job_0123456789ab")
	if code != 0 || strings.Contains(out, "Context:") {
		t.Errorf("cruise status by id in the personal context: %d\n%s", code, out)
	}
	for _, a := range f.seenURIs() {
		if strings.Contains(a, "context=") {
			t.Errorf("asked %s in the personal context with a context", a)
		}
	}
}
