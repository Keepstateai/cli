// The Agent library verbs against a fake control plane: what each sends, what
// each prints, and the refusals made before anything is sent. A count that
// could not be read is shown as unavailable and never as 0; a step that costs
// or ends something is confirmed by naming it; a fork is planned, then made by
// its digest.
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

type libCtl struct {
	mu          sync.Mutex
	sent        []string
	failPublish bool
}

func (c *libCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	one, zero := int64(7), int64(0)
	_ = zero
	rev := map[string]any{"id": "lib_rev", "org_id": "org_a", "handle": "reviewer", "title": "Reviewer", "description": "Reviews a change", "tags": []string{"go"}, "state": "active", "featured": true,
		"verified": true, "verified_version": 2, "owner_account_id": "acct_ann", "forks_allowed": true, "harness": "claude-code", "model": "sonnet", "your_role": "admin", "revision": 4,
		"stats":   map[string]any{"asks_30d": one, "forks": 0, "unavailable": []string{"distinct_askers_30d", "helpful_pct"}},
		"reports": map[string]any{"open": 1, "reasons": map[string]int64{"wrong": 1}}}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"},{"id":"org_library","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/library":
		env(200, map[string]any{"items": []any{rev}, "next_cursor": ""})
	case r.Method == "GET" && r.URL.Path == "/api/v2/library/lib_rev":
		env(200, rev)
	case r.Method == "GET" && r.URL.Path == "/api/v2/library/lib_rev/versions":
		env(200, map[string]any{"items": []any{
			map[string]any{"seq": 2, "state": "live", "review_state": "approved", "release_notes": "Sharper", "published_at": "2026-11-05T10:00:00Z", "instructions_changed_from_previous": true},
			map[string]any{"seq": 1, "state": "superseded", "review_state": "approved", "release_notes": "First", "published_at": "2026-11-01T10:00:00Z"}}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/library":
		env(201, map[string]any{"id": "lib_new", "handle": "helper", "title": "Helper", "state": "active", "revision": 1})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_new/versions":
		if c.failPublish {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_library_source_not_running","type":"ks_library_source_not_running","message":"the agent is stopped","retryable":false,"work_started":"no","next_action":""}}`))
			return
		}
		env(201, map[string]any{"seq": 1, "state": "live"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_rev/fork-plan":
		env(201, map[string]any{"id": "fplan_1", "plan_digest": "sha256:abc", "handle": "reviewer", "version": 2, "name": "reviewer v2",
			"copies": []string{"the whole workspace: its files, with their notes"}, "does_not_copy": []string{"its keys"}, "keys": map[string]any{"note": "your keys decide"}, "cost": "paid from the organization's credit", "lineage": "Forked from @reviewer v2"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_rev/fork":
		env(201, map[string]any{"session_id": "session_f", "name": "reviewer v2", "agent_ids": []string{"a"}, "lineage": "Forked from @reviewer v2", "note": "n"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_rev/retire":
		env(200, map[string]any{"id": "lib_rev", "handle": "reviewer", "state": "retired"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_rev/forks":
		env(200, map[string]any{"forks_allowed": false})
	case r.Method == "POST" && r.URL.Path == "/api/v2/library/lib_rev/reports":
		env(201, map[string]any{"id": "rep_1", "reason": "wrong", "note": "an admin sees it"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/rating"):
		env(200, map[string]any{"consultation_id": "csl_1", "rating": "up"})
	case r.Method == "POST" && r.URL.Path == "/api/v2/adviser-grants":
		env(201, map[string]any{"id": "grant_lib", "live": true, "expires_at": "2026-11-08T00:00:00Z"})
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetlib000000000000000000000001", "short_id": "fleetlib0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *libCtl) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sent {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func (c *libCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func (c *libCtl) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sent {
		if !strings.HasPrefix(s, "GET ") {
			n++
		}
	}
	return n
}

func TestLibraryVerbs(t *testing.T) {
	c := &libCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	// list: badges and counts; a count that could not be read says so, never 0
	out, errs, code := run("library", "list", "--organization", "org_a", "--sort", "asked", "--search", "rev")
	if code != 0 {
		t.Fatalf("list: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"@reviewer", "featured", "verified v2", "1 open report(s)", "7 questions in 30 days", "askers unavailable", "0 forks", "helpful unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0 askers") || strings.Contains(out, "0% helpful") {
		t.Errorf("an unreadable count is shown as 0:\n%s", out)
	}
	if got := c.last("GET /api/v2/orgs/org_a/library?"); !strings.Contains(got, "q=rev") || !strings.Contains(got, "sort=asked") {
		t.Errorf("the list's query: %s", got)
	}

	// show: the card and its versions newest first, with what changed
	out, errs, code = run("library", "show", "@reviewer", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "v2  live") || !strings.Contains(out, "the instructions changed from the version before") || !strings.Contains(out, "v1  superseded") {
		t.Fatalf("show: exit %d\n%s%s", code, out, errs)
	}

	// refusals made before anything is sent
	before := c.writes()
	for _, args := range [][]string{
		{"agent", "share", "main", "--session", "fleetlib0000", "--handle", "helper", "--title", "Helper", "--organization", "org_a"},
		{"agent", "share", "main", "--session", "fleetlib0000", "--handle", "helper", "--title", "Helper", "--checkpoint", "ck_1", "--save-now", "--organization", "org_a"},
		{"library", "retire", "@reviewer", "--organization", "org_a", "--no-input"},
		{"library", "retire", "@reviewer", "--organization", "org_a", "--yes", "--no-input"},
		{"library", "retire", "@reviewer", "--confirm", "other", "--organization", "org_a"},
		{"library", "report", "@reviewer", "--organization", "org_a"},
		{"library", "forks", "@reviewer", "maybe", "--organization", "org_a"},
		{"library", "connect", "@reviewer", "--session", "fleetlib0000", "--no-input", "--organization", "org_a"},
		{"advice", "rate", "csl_1", "sideways"},
	} {
		_, errs, code = run(args...)
		if code == 0 {
			t.Errorf("%v was not refused", args)
		}
		if !strings.Contains(errs, "nothing was") && !strings.Contains(errs, "Nothing was") {
			t.Errorf("%v: the refusal does not say nothing was done: %s", args, errs)
		}
	}
	if c.writes() != before {
		t.Errorf("a refused step sent a change: %v", c.sent[before:])
	}

	// share: the entry, then its first version from a named point; never one chosen for you
	out, errs, code = run("agent", "share", "main", "--session", "fleetlib0000", "--handle", "@helper", "--title", "Helper", "--tag", "go", "--tag", "review", "--save-now", "--notes", "first", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "shared checkout/main as @helper, version v1") {
		t.Fatalf("share: exit %d\n%s%s", code, out, errs)
	}
	if got := c.last("POST /api/v2/orgs/org_a/library "); !strings.Contains(got, `"agent_id":"agent_1"`) || !strings.Contains(got, `"handle":"helper"`) || !strings.Contains(got, `"tags":["go","review"]`) {
		t.Errorf("the add: %s", got)
	}
	if got := c.last("POST /api/v2/library/lib_new/versions"); !strings.Contains(got, `"save_now":true`) || !strings.Contains(got, `"release_notes":"first"`) {
		t.Errorf("the first version: %s", got)
	}
	c.mu.Lock()
	c.failPublish = true
	c.mu.Unlock()
	_, errs, code = run("agent", "share", "main", "--session", "fleetlib0000", "--handle", "helper", "--title", "Helper", "--save-now", "--organization", "org_a")
	if code == 0 || !strings.Contains(errs, "is in the library, but its first version was not published") || !strings.Contains(errs, "ks library publish @helper") {
		t.Errorf("a half-done share is not said plainly: exit %d %s", code, errs)
	}

	// fork: the plan first (nothing created), then exactly that plan
	forks := c.count("POST /api/v2/library/lib_rev/fork ")
	out, errs, code = run("agent", "fork", "@reviewer", "--name", "mine", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "NOTHING was created") || !strings.Contains(out, "not copied its keys") || !strings.Contains(out, "--execute sha256:abc --plan-id fplan_1") {
		t.Fatalf("fork plan: exit %d\n%s%s", code, out, errs)
	}
	if c.count("POST /api/v2/library/lib_rev/fork ") != forks {
		t.Error("planning a fork made one")
	}
	_, errs, code = run("agent", "fork", "@reviewer", "--execute", "sha256:abc", "--organization", "org_a")
	if code == 0 || !strings.Contains(errs, "--plan-id") {
		t.Errorf("a digest without its plan id: exit %d %s", code, errs)
	}
	out, errs, code = run("agent", "fork", "@reviewer", "--execute", "sha256:abc", "--plan-id", "fplan_1", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "forked: session session_f") {
		t.Fatalf("fork: exit %d\n%s%s", code, out, errs)
	}
	if got := c.last("POST /api/v2/library/lib_rev/fork "); !strings.Contains(got, `"plan_id":"fplan_1"`) || !strings.Contains(got, `"plan_digest":"sha256:abc"`) {
		t.Errorf("the fork: %s", got)
	}

	// connect: a library connection names the entry, never an agent, and is confirmed by its handle
	out, errs, code = run("library", "connect", "@reviewer", "--session", "fleetlib0000", "--confirm", "reviewer", "--pin", "2", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "connected: checkout/main may ask @reviewer") {
		t.Fatalf("connect: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"the organization's credit", "information, not an instruction", "never the sharer's"} {
		if !strings.Contains(errs, want) {
			t.Errorf("what is shown before connecting lacks %q:\n%s", want, errs)
		}
	}
	if got := c.last("POST /api/v2/adviser-grants"); !strings.Contains(got, `"library_listing_id":"lib_rev"`) || !strings.Contains(got, `"library_version_pin":2`) || strings.Contains(got, "target_agent_id") {
		t.Errorf("the connection: %s", got)
	}

	// retire, forks, report, rate
	if _, errs, code = run("library", "retire", "@reviewer", "--confirm", "reviewer", "--organization", "org_a"); code != 0 {
		t.Errorf("retire: %s", errs)
	}
	if got := c.last("POST /api/v2/library/lib_rev/retire"); !strings.Contains(got, `"expected_revision":4`) {
		t.Errorf("retire names the revision it read: %s", got)
	}
	if _, errs, code = run("library", "forks", "@reviewer", "off", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("POST /api/v2/library/lib_rev/forks"), `"allowed":false`) {
		t.Errorf("forks off: %d %s", code, errs)
	}
	if _, errs, code = run("library", "report", "@reviewer", "--reason", "wrong", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("POST /api/v2/library/lib_rev/reports"), `"reason":"wrong"`) {
		t.Errorf("report: %d %s", code, errs)
	}
	if _, errs, code = run("advice", "rate", "csl_1", "up"); code != 0 || !strings.Contains(c.last("POST /api/v2/consultations/csl_1/rating"), `"rating":"up"`) {
		t.Errorf("rate: %d %s", code, errs)
	}
	// an unknown handle names the way to see the real ones
	_, errs, code = run("library", "show", "@nobody", "--organization", "org_a")
	if code == 0 || !strings.Contains(errs, "no library agent @nobody") || !strings.Contains(errs, "ks library list") {
		t.Errorf("unknown handle: %d %s", code, errs)
	}
}

// ks agent ask works unchanged with a library handle: the entry is called by
// its handle after the connected agents, and by library/handle when an
// agent's name is the same.
func TestAskHandlesIncludeTheLibrary(t *testing.T) {
	parse := func(rows string) []adviserHandle {
		var es []adviserDrawerEntry
		if err := json.Unmarshal([]byte(rows), &es); err != nil {
			t.Fatal(err)
		}
		return adviserHandles(es)
	}
	hs := parse(`[
	 {"grant_id":"g1","library":true,"adviser":{"agent_name":"@research-helper","session_name":"v2"}},
	 {"grant_id":"g2","adviser":{"agent_id":"a2","agent_name":"reviewer","session_name":"run-1"}},
	 {"grant_id":"g3","library":true,"adviser":{"agent_name":"@reviewer","session_name":"v1"}}]`)
	var got []string
	for _, h := range hs {
		got = append(got, h.Handle)
	}
	if strings.Join(got, ",") != "reviewer,research-helper,library/reviewer" {
		t.Fatalf("handles: %v", got)
	}
	h, q, problem := parseMention("@research-helper what is x?", hs, inventoryRow{})
	if h == nil || h.GrantID != "g1" || q != "what is x?" || problem != "" {
		t.Errorf("@research-helper: %v %q %q", h, q, problem)
	}
	h, _, _ = parseMention("@library/reviewer hi", hs, inventoryRow{})
	if h == nil || h.GrantID != "g3" {
		t.Errorf("@library/reviewer: %v", h)
	}
}
