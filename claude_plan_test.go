package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// planCtl is a control plane for the Claude plan sign-in: the capability
// registry (with the plan's row when listed), the key routes, the session
// routes a run uses, and a release mirror so doctor asks nothing of the
// network. Every request and body is recorded.
type planCtl struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string][]string
	listed   bool             // the registry lists claude_plan_key as available
	keys     []map[string]any // GET /api/v2/keys
	fail     *planFail        // a refusal for POST /api/v2/keys
	mk       *mkCtl           // the run's other routes
	url      string           // where the fake listens
}

type planFail struct {
	status int
	typ    string
	msg    string
}

func newPlanCtl() *planCtl {
	return &planCtl{bodies: map[string][]string{}, mk: newMKCtl()}
}

func planRow(last4, state string) map[string]any {
	row := map[string]any{"id": "vlt_plan0001", "provider": "claude_plan", "kind": "claude_plan", "alias": "", "last4": last4,
		"enabled": true, "revision": 1, "created_at": "2026-11-22T10:00:00Z", "updated_at": "2026-11-22T10:00:00Z",
		"expires_at": "2027-11-22T10:00:00Z", "state": state}
	return row
}

func apiKeyRow(id, provider string) map[string]any {
	return map[string]any{"id": id, "provider": provider, "alias": "prod", "last4": "abcd", "enabled": true, "revision": 1,
		"created_at": "t", "updated_at": "t"}
}

func (c *planCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	c.mu.Lock()
	key := r.Method + " " + r.URL.Path
	c.requests = append(c.requests, key)
	if len(raw) > 0 {
		c.bodies[key] = append(c.bodies[key], string(raw))
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/TAG":
		fmt.Fprint(w, "dev")
	case r.URL.Path == "/healthz":
		fmt.Fprint(w, "ok")
	case r.URL.Path == "/api/whoami":
		fmt.Fprint(w, `{"account_id":"acct_t","cohort_state":"active"}`)
	case r.URL.Path == "/api/capabilities":
		rows := []any{
			map[string]any{"id": "agent.workspace", "availability": "available", "summary": "s", "surface": "client"},
			map[string]any{"id": "keys.inventory", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "preflight", "availability": "available", "summary": "s", "surface": "api"},
			map[string]any{"id": "budget.admission", "availability": "available", "summary": "s", "surface": "api"},
		}
		if c.listed {
			rows = append(rows, map[string]any{"id": "claude_plan_key", "availability": "available", "summary": "s", "surface": "api", "since_protocol": 1})
		}
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "x", "price_book": "v1.4", "capabilities": rows, "limits": map[string]any{},
			"protocol": map[string]any{"current": 3, "minimum_client": 1, "header": "X-KS-Protocol"}})
	case r.Method == "GET" && r.URL.Path == "/api/v2/keys":
		c.mu.Lock()
		items := append([]map[string]any(nil), c.keys...)
		c.mu.Unlock()
		env(200, map[string]any{"items": items, "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/keys":
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if c.fail != nil {
			w.WriteHeader(c.fail.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"type": c.fail.typ, "message": c.fail.msg}})
			return
		}
		secret := fmt.Sprint(body["secret"])
		env(201, planRow(secret[len(secret)-4:], "ok"))
	case r.Method == "DELETE" || strings.HasSuffix(r.URL.Path, "/deletion-plan"):
		env(200, map[string]any{})
	default:
		c.mk.ServeHTTP(w, r)
	}
}

func (c *planCtl) posts(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies["POST "+path]...)
}

func (c *planCtl) asked(req string) bool {
	c.mu.Lock()
	for _, r := range c.requests {
		if r == req {
			c.mu.Unlock()
			return true
		}
	}
	c.mu.Unlock()
	return c.mk.asked(req)
}

// planEnv builds the binary under test signed in against the fake and an
// environment whose HOME and temporary directory are fresh, so a file the
// run leaves behind can be found.
func planEnv(t *testing.T, c *planCtl) (bin, cfg, home string, env []string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	c.url = srv.URL
	bin, cfg = buildAndAuth(t, srv)
	home = t.TempDir()
	env = append(fastEnv(cfg), "HOME="+home, "TMPDIR="+home, "KS_UPDATE_BASE="+srv.URL, "NO_COLOR=1")
	return bin, cfg, home, env
}

// runPlan runs the binary with standard input and the environment, and
// returns what it printed on each stream and its exit code.
func runPlan(t *testing.T, bin string, env []string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return o.String(), e.String(), code
}

func noToken(t *testing.T, what, text string) {
	t.Helper()
	if tok := fixtureClaudeToken(); leaks(text, tok) || strings.Contains(text, "sk-ant-oat") {
		t.Errorf("%s holds a piece of the token:\n%s", what, text)
	}
}

// ---- ks login claude, with the token on standard input --------------------------

func TestLoginClaudeStoresAPipedTokenOnceAndShowsItNowhere(t *testing.T) {
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	bin, _, home, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, tok+"\n", "login", "claude")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	posts := c.posts("/api/v2/keys")
	if len(posts) != 1 {
		t.Fatalf("the service received %d requests", len(posts))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(posts[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["provider"] != "claude_plan" || body["secret"] != tok || len(body) != 2 {
		t.Errorf("the request was %d keys, provider %v", len(body), body["provider"])
	}
	if strings.Count(posts[0], tok) != 1 {
		t.Error("the token must be in the request exactly once")
	}
	for _, want := range []string{"Signed in to your Claude plan", "…" + tok[len(tok)-4:], "22 Nov 2027", "ks run --agent --key claude-plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("the card does not say %q:\n%s", want, out)
		}
	}
	noToken(t, "standard output", out)
	noToken(t, "standard error", errs)
	if hits := treeHoldsToken(t, home, tok); len(hits) > 0 {
		t.Errorf("the token was written to a file: %v", hits)
	}
}

func TestLoginClaudeJSONNeverCarriesTheToken(t *testing.T) {
	tok := fixtureClaudeToken()
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "export CLAUDE_CODE_OAUTH_TOKEN="+tok+"\n", "login", "claude", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc.Data["ok"] != true || doc.Data["key_id"] != "vlt_plan0001" || doc.Data["last4"] != tok[len(tok)-4:] ||
		doc.Data["expires_at"] != "2027-11-22T10:00:00Z" || doc.Data["state"] != "ok" || len(doc.Data) != 5 {
		t.Errorf("the answer was %v", doc.Data)
	}
	noToken(t, "--json output", out)
	noToken(t, "standard error", errs)
	if strings.Contains(out, "secret") {
		t.Error("the answer names a secret field")
	}
}

func TestLoginClaudeRefusesWhatIsNotATokenWithoutEchoingIt(t *testing.T) {
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	for name, in := range map[string]string{"words": "my-private-words-xyz123\n", "an API key": "sk-ant-api03-" + strings.Repeat("A", 40) + "\n", "nothing": ""} {
		out, errs, code := runPlan(t, bin, env, in, "login", "claude")
		if code != exitUsage {
			t.Errorf("%s: exit %d, want %d\n%s%s", name, code, exitUsage, out, errs)
		}
		if strings.Contains(out+errs, "xyz123") || strings.Contains(out+errs, "api03") {
			t.Errorf("%s: what was typed was echoed:\n%s%s", name, out, errs)
		}
		if !strings.Contains(errs, "Next: ks login claude") {
			t.Errorf("%s: no Next line:\n%s", name, errs)
		}
	}
	if n := len(c.posts("/api/v2/keys")); n != 0 {
		t.Errorf("%d requests were sent for input that is not a token", n)
	}
}

func TestLoginClaudeRefusalsOfTheServiceAreDesigned(t *testing.T) {
	tok := fixtureClaudeToken()
	for _, tc := range []struct {
		name       string
		fail       planFail
		code       int
		says, next string
	}{
		{"switched off", planFail{404, "ks_claude_plan_off", "the flag is off"}, exitFailed, "not available on this service yet", "Next: ks key add --provider anthropic"},
		{"an organization", planFail{422, "ks_claude_plan_personal", "organization"}, exitUsage, "belongs to one person", "Next: ks context use personal"},
		{"not a token", planFail{422, "ks_claude_plan_token_invalid", "bad shape"}, exitUsage, "not a Claude plan token", "Next: ks login claude"},
		{"not signed in", planFail{401, "ks_unauthorized", "sign in first"}, exitAuth, "sign in first", "Next: ks login"},
	} {
		c := newPlanCtl()
		c.fail = &tc.fail
		bin, _, _, env := planEnv(t, c)
		out, errs, code := runPlan(t, bin, env, tok+"\n", "login", "claude")
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\n%s%s", tc.name, code, tc.code, out, errs)
		}
		if !strings.Contains(strings.ToLower(errs), strings.ToLower(tc.says)) || !strings.Contains(errs, tc.next) {
			t.Errorf("%s: the refusal does not say %q with %q:\n%s", tc.name, tc.says, tc.next, errs)
		}
		if strings.Count(errs, "Next:") != 1 {
			t.Errorf("%s: want one Next line:\n%s", tc.name, errs)
		}
		if leaks(out+errs, tok[13:]) { // the fixed prefix may be named; the rest may not
			t.Errorf("%s: output holds a piece of the token:\n%s%s", tc.name, out, errs)
		}
	}
}

func TestLoginClaudeInAnOrganizationContextStoresNothing(t *testing.T) {
	c := newPlanCtl()
	bin, cfg, _, env := planEnv(t, c)
	ctx, _ := json.Marshal(map[string]string{c.url: "org_acme"})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "context.json"), ctx, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, code := runPlan(t, bin, env, fixtureClaudeToken()+"\n", "login", "claude")
	if code != exitUsage || !strings.Contains(errs, "belongs to one person") || !strings.Contains(errs, "Next: ks context use personal") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if n := len(c.posts("/api/v2/keys")); n != 0 {
		t.Errorf("%d requests were sent in an organization's context", n)
	}
}

func TestClaudePlanErrMapsTheServicesNamedRefusals(t *testing.T) {
	for typ, want := range map[string]string{
		"ks_claude_plan_off":           "claude_plan_unavailable",
		"ks_claude_plan_personal":      "claude_plan_personal",
		"ks_claude_plan_token_invalid": "claude_plan_token_invalid",
		"ks_claude_plan_none":          "claude_plan_none",
		"ks_claude_plan_runner":        "claude_plan_runner",
	} {
		err := claudePlanErr(&hostedErr{Status: 422, Type: typ, Message: "the codex runner calls OpenAI"})
		ce, ok := err.(*cliError)
		if !ok || ce.Kind != want || ce.NextAction == "" || !strings.HasPrefix(ce.NextAction, "ks ") {
			t.Errorf("%s: %+v", typ, err)
		}
	}
	other := &hostedErr{Status: 500, Type: "ks_other", Message: "boom"}
	if claudePlanErr(other) != error(other) {
		t.Error("a refusal this does not know was changed")
	}
}

// ---- the key list, add and doctor ----------------------------------------------

func TestKeyListShowsThePlanRowAndOnlyThatRowCarriesTheNewFields(t *testing.T) {
	c := newPlanCtl()
	row := planRow("WxYz", "expiring")
	row["state_words"] = "Your Claude plan sign-in ends on 3 Dec 2026. Run ks login claude to renew it."
	row["expires_at"] = "2026-12-03T10:00:00Z"
	c.keys = []map[string]any{apiKeyRow("vlt_api00001", "anthropic"), row}
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "", "key", "list")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"Claude plan", "…WxYz", "ends 3 Dec 2026", "ends soon", "Run ks login claude to renew it."} {
		if !strings.Contains(out, want) {
			t.Errorf("the list does not say %q:\n%s", want, out)
		}
	}
	jout, _, _ := runPlan(t, bin, env, "", "key", "list", "--json")
	var doc struct {
		Data struct {
			Keys []map[string]any `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil || len(doc.Data.Keys) != 2 {
		t.Fatalf("%v\n%s", err, jout)
	}
	for _, k := range doc.Data.Keys {
		_, hasKind := k["kind"]
		_, hasState := k["state"]
		_, hasEnds := k["expires_at"]
		if k["provider"] == "anthropic" && (hasKind || hasState || hasEnds) {
			t.Errorf("an API key's row carries plan fields: %v", k)
		}
		if k["provider"] == "claude_plan" && (!hasKind || !hasState || !hasEnds) {
			t.Errorf("the plan row lost its fields: %v", k)
		}
	}
}

func TestKeyAddRefusesAPlanTokenAndPointsToLogin(t *testing.T) {
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	for _, p := range []string{"claude_plan", "claude-plan"} {
		out, errs, code := runPlan(t, bin, env, fixtureClaudeToken()+"\n", "key", "add", "--provider", p)
		if code != exitUsage || !strings.Contains(errs, "Next: ks login claude") {
			t.Errorf("%s: exit %d\n%s%s", p, code, out, errs)
		}
		noToken(t, "ks key add output", out+errs)
	}
	if n := len(c.posts("/api/v2/keys")); n != 0 {
		t.Errorf("%d requests carried a token through ks key add", n)
	}
}

func TestKeyListOffersTheSignInOnlyWhereTheServiceLists(t *testing.T) {
	for _, listed := range []bool{false, true} {
		c := newPlanCtl()
		c.listed = listed
		bin, _, _, env := planEnv(t, c)
		// the registry is read by a verb that gates on it, then cached
		runPlan(t, bin, env, "", "key", "list")
		out, _, _ := runPlan(t, bin, env, "", "key", "list")
		if got := strings.Contains(out, "ks login claude"); got != listed {
			t.Errorf("listed=%v: the offer shown=%v\n%s", listed, got, out)
		}
	}
}

func TestDoctorSaysWhereThePlanSignInStands(t *testing.T) {
	for _, tc := range []struct {
		state, words string
		code         int
		says         string
	}{
		{"ok", "", 0, "Claude plan sign-in: stored (…9999), ends 22 Nov 2027"},
		{"expiring", "Your Claude plan sign-in ends on 3 Dec 2026. Run ks login claude to renew it.", 0, "ends on 3 Dec 2026"},
		{"invalid", "Your Claude plan sign-in no longer works. Run ks login claude to sign in again.", 1, "no longer works"},
		{"expired", "", 1, "has ended"},
	} {
		c := newPlanCtl()
		row := planRow("9999", tc.state)
		if tc.words != "" {
			row["state_words"] = tc.words
		}
		c.keys = []map[string]any{row}
		bin, _, _, env := planEnv(t, c)
		out, errs, code := runPlan(t, bin, env, "", "doctor")
		if code != tc.code || !strings.Contains(out, tc.says) {
			t.Errorf("%s: exit %d, want %d; says %q?\n%s%s", tc.state, code, tc.code, tc.says, out, errs)
		}
	}
	// none stored: nothing about it
	c := newPlanCtl()
	bin, _, _, env := planEnv(t, c)
	out, _, _ := runPlan(t, bin, env, "", "doctor")
	if strings.Contains(out, "Claude plan") {
		t.Errorf("doctor mentions a sign-in that is not stored:\n%s", out)
	}
}

func TestPlanStateReadsTheDateWhenTheServiceSentOnlyIt(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for ends, want := range map[string]string{
		"2027-11-22T00:00:00Z": "ok",
		"2026-12-05T00:00:00Z": "expiring", // 13 days
		"2026-12-07T00:00:00Z": "ok",       // 15 days
		"2026-11-21T00:00:00Z": "expired",
		"":                     "ok",
	} {
		if got := planState(customerKey{ExpiresAt: ends}, now); got != want {
			t.Errorf("ends %q: %s, want %s", ends, got, want)
		}
	}
	if got := planState(customerKey{State: "invalid", ExpiresAt: "2030-01-01T00:00:00Z"}, now); got != "invalid" {
		t.Errorf("the service's word was overruled: %s", got)
	}
}

// ---- ks run, ks agent run and ks session key set -------------------------------

func planRunCtl(keys ...map[string]any) *planCtl {
	c := newPlanCtl()
	c.listed = true
	c.keys = keys
	return c
}

func keyBlocked() []any {
	return []any{map[string]any{"check": "account", "status": "pass", "detail": "the account is active"},
		map[string]any{"check": "key", "status": "block", "detail": "no enabled anthropic key on the account", "next_action": "ks key add --provider anthropic"}}
}

func TestRunOnTheClaudePlanByName(t *testing.T) {
	c := planRunCtl(apiKeyRow("vlt_api00001", "anthropic"), planRow("9999", "ok"))
	c.mk.checks = keyBlocked() // a service that counts API keys only would block here
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "", "run", "--agent", "--key", "claude-plan")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.posts("/api/v2/sessions")[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["claude_plan"] != true {
		t.Errorf("the creation did not ask for the plan: %v", body)
	}
	if _, has := body["managed"]; has {
		t.Errorf("a plan run named KeepState's key too: %v", body["managed"])
	}
	if c.asked("POST /api/v2/sessions/session_abc123/bindings") {
		t.Error("a run on the plan bound an API key after it was created")
	}
	if !c.asked("POST /api/v2/sessions/session_abc123/provision") {
		t.Errorf("no machine was asked for: %v", c.requests)
	}
	if !strings.Contains(out+errs, "your Claude plan") || !strings.Contains(out+errs, "KeepState adds no charge") {
		t.Errorf("the run does not say the calls go on the plan:\n%s%s", out, errs)
	}
}

func TestRunWithNoKeyGoesOnThePlanWhenThereIsOne(t *testing.T) {
	c := planRunCtl(planRow("9999", "ok"))
	c.mk.checks = keyBlocked()
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "", "run", "--agent")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(c.posts("/api/v2/sessions")[0]), &body)
	if body["claude_plan"] != true {
		t.Errorf("a run with no key of its own did not take the plan: %v", body)
	}
}

func TestRunKeepsAnApiKeyWhenThereIsOneAndNoChoice(t *testing.T) {
	c := planRunCtl(apiKeyRow("vlt_one1", "anthropic"), planRow("9999", "ok"))
	c.mk.checks = nil
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, "", "run", "--agent")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(c.posts("/api/v2/sessions")[0]), &body)
	if _, has := body["claude_plan"]; has {
		t.Errorf("an account with an API key had its run moved to the plan: %v", body)
	}
	if !c.asked("POST /api/v2/sessions/session_abc123/bindings") {
		t.Error("the API key was not bound")
	}
}

func TestRunRefusesAPlanThatCannotBeUsedBeforeAnythingExists(t *testing.T) {
	for _, state := range []string{"invalid", "expired"} {
		for _, args := range [][]string{{"run", "--agent", "--key", "claude-plan"}, {"run", "--agent"}} {
			c := planRunCtl(planRow("9999", state))
			c.mk.checks = keyBlocked()
			bin, _, _, env := planEnv(t, c)
			out, errs, code := runPlan(t, bin, env, "", args...)
			if code == 0 || !strings.Contains(errs, "Next: ks login claude") || !strings.Contains(strings.ToLower(errs), "claude plan sign-in") {
				t.Errorf("%s %v: exit %d\n%s%s", state, args, code, out, errs)
			}
			if c.asked("POST /api/v2/sessions") {
				t.Errorf("%s %v: a session was created on a sign-in that cannot be used", state, args)
			}
		}
	}
}

func TestRunWithNoKeyAndNoPlanSaysHowToSignIn(t *testing.T) {
	c := planRunCtl()
	c.mk.checks = keyBlocked()
	bin, _, _, env := planEnv(t, c)
	runPlan(t, bin, env, "", "key", "list") // the registry is cached by a verb
	_, errs, code := runPlan(t, bin, env, "", "run", "--agent", "--no-input")
	if code == 0 || !strings.Contains(errs, "sign in with your Claude plan: ks login claude") || strings.Count(errs, "Next:") != 1 {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if c.asked("POST /api/v2/sessions") {
		t.Error("a session was created with no key")
	}
}

func TestRunOnThePlanIsRefusedForARunnerThatIsNotClaudeCode(t *testing.T) {
	// no request is made: the refusal is the client's, before the service
	for _, p := range []string{"openai", "openrouter"} {
		_, err := chooseClaudePlan(hostedCreds{CTL: "http://127.0.0.1:1"}, p)
		ce, ok := err.(*cliError)
		if !ok || ce.Kind != "claude_plan_runner" || !strings.Contains(ce.Message, "Claude model only") || !strings.HasPrefix(ce.NextAction, "ks ") {
			t.Errorf("%s: %+v", p, err)
		}
	}
}

func TestPlainRunDoesNotTakeKeyClaudePlan(t *testing.T) {
	c := planRunCtl(planRow("9999", "ok"))
	bin, _, _, env := planEnv(t, c)
	_, errs, code := runPlan(t, bin, env, "", "run", "--key", "claude-plan")
	if code != exitUsage || !strings.Contains(errs, "belong to an agent session") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if c.asked("POST /api/v2/sessions") {
		t.Error("a session was created")
	}
}

func TestSessionKeySetBindsASessionToThePlan(t *testing.T) {
	c := planRunCtl(planRow("9999", "ok"), apiKeyRow("vlt_oa", "openai"))
	c.mk.bind = &mkBinding{Keys: map[string]any{"anthropic": "vlt_one1", "openai": "vlt_oa"}, Managed: []any{}}
	bin, _, _, env := planEnv(t, c)
	// the session list the verb resolves a session from
	out, errs, code := runPlan(t, bin, env, "", "session", "key", "set", "fleetmanaged", "--key", "claude-plan")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.posts("/api/v2/sessions/session_rec1/bindings")[0]), &body); err != nil {
		t.Fatal(err)
	}
	if jsonOf(body["keys"]) != `{"claude_plan":"vlt_plan0001","openai":"vlt_oa"}` {
		t.Errorf("the binding sent was %v; the plan replaces the Anthropic key and keeps the OpenAI one", body["keys"])
	}
	if !strings.Contains(out, "your Claude plan") {
		t.Errorf("the verb does not say the session is on the plan:\n%s", out)
	}
}

func TestUsageAndRunnerWordsNameTheClaudePlan(t *testing.T) {
	if got := keySourceText("claude_plan:anthropic"); !strings.Contains(got, "your Claude plan, no KeepState charge") {
		t.Errorf("keySourceText = %q", got)
	}
	if got := keySourceText("byok:anthropic,claude_plan:anthropic"); !strings.Contains(got, "your key and your Claude plan") {
		t.Errorf("a mix: %q", got)
	}
	if got := keySourceLabel("claude_plan:anthropic"); got != claudePlanSourceWords {
		t.Errorf("keySourceLabel = %q", got)
	}
	k := &runnerKeyRead{Provider: "anthropic", Route: "claude_plan"}
	k.Key = &struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Last4 string `json:"last4"`
	}{"vlt_plan0001", "Your Claude plan", "9999"}
	if got := runnerKeyWords(k); got != "your Claude plan …9999" {
		t.Errorf("runnerKeyWords = %q", got)
	}
	k.Problem = &struct {
		Words string `json:"words"`
	}{"Your Claude plan sign-in no longer works. Run ks login claude on your computer, then ask again."}
	if got := runnerKeyWords(k); !strings.Contains(got, "no longer works") {
		t.Errorf("a plan that no longer works is not said: %q", got)
	}
}

func TestClaudePlanSourceStringsKeepTheTokenOutOfEveryError(t *testing.T) {
	// every refusal this feature can make is built from fixed words; none
	// takes the text a person typed
	for _, ce := range []*cliError{claudePlanPersonalErr(), claudePlanTokenInvalidErr(), claudePlanUnavailableErr(), claudePlanNoneErr(),
		claudePlanRunnerErr("this agent calls OpenAI, not a Claude model"), claudePlanNotFinishedErr(), claudePlanUnreadErr(), claudePlanTimedOutErr(),
		claudePlanUnusableErr(customerKey{State: "invalid"})} {
		if ce.Title == "" || ce.NextAction == "" || !strings.HasPrefix(ce.NextAction, "ks ") || strings.Contains(ce.Message, "sk-ant-oat01") {
			t.Errorf("%s: %+v", ce.Kind, ce)
		}
	}
}
