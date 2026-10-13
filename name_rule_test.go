// A new name the client sends follows the service's own name rule, checked
// before any request. The client used to check a new agent's name with
// agentNameSafe (a display rule: up to 64 characters, a leading digit or a
// dot allowed), so `ks run --agent --agent-name 1agent` and the app's new
// session went to the service and came back refused, after a preflight and a
// key lookup, instead of being refused here with nothing sent.
//
// The rule is one shared file, testdata/vectors/name-rule-v1.json, vendored
// from the service (testdata/vectors/SOURCE.json, the drift guard in
// vectors_ks074_test.go); the service holds its own rule to the same file.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type nameRuleVectors struct {
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	Rule     string   `json:"rule"`
	Accepted []string `json:"accepted"`
	Refused  []string `json:"refused"`
}

func loadNameRuleVectors(t *testing.T) nameRuleVectors {
	t.Helper()
	var v nameRuleVectors
	b, err := os.ReadFile("testdata/vectors/name-rule-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Name != "ks-name-rule" || v.Version != 1 || len(v.Accepted) == 0 || len(v.Refused) == 0 {
		t.Fatalf("name rule vectors: %v %q %d", err, v.Name, v.Version)
	}
	return v
}

func TestNewNamesFollowTheSharedNameRuleVectors(t *testing.T) {
	v := loadNameRuleVectors(t)
	if c04Name.String() != v.Rule {
		t.Errorf("c04Name is %q, the shared vector records the service's %q", c04Name.String(), v.Rule)
	}
	for _, n := range v.Accepted {
		if !c04Name.MatchString(n) {
			t.Errorf("c04Name refuses %q, which the service accepts", n)
		}
	}
	for _, n := range v.Refused {
		if c04Name.MatchString(n) {
			t.Errorf("c04Name accepts %q, which the service refuses", n)
		}
	}
}

// nameCtl records every request; nothing it answers matters, because a
// refused name must not reach it.
type nameCtl struct {
	mu       sync.Mutex
	requests []string
}

func (c *nameCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r",
		"error": map[string]any{"code": "ks_unavailable", "type": "ks_unavailable", "message": "the recording fake answers nothing"}})
}

func (c *nameCtl) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.requests
	c.requests = nil
	return out
}

func TestANewNameTheServiceRefusesIsAUsageErrorBeforeAnyRequest(t *testing.T) {
	c := &nameCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	long := strings.Repeat("a", 49)
	cases := [][]string{
		{"run", "--agent", "--agent-name", "1agent"},
		{"run", "--agent", "--agent-name", "a.b"},
		{"run", "--agent", "--agent-name", long},
		{"run", "--agent", "--name", "1agent"},
		{"run", "--agent", "--name", "a.b"},
		{"run", "--agent", "--name", long},
		{"agent", "create", "1agent"},
		{"agent", "create", "a.b"},
		{"agent", "create", long},
		{"agent", "rename", "main", "1agent"},
		{"agent", "rename", "main", "a.b"},
		{"agent", "rename", "main", long},
	}
	for _, args := range cases {
		c.take()
		out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if got := c.take(); len(got) != 0 {
			t.Errorf("ks %s sent %d request(s) before refusing the name: %v", strings.Join(args, " "), len(got), got)
		}
		if code != exitUsage {
			t.Errorf("ks %s: exit %d, want the usage error %d\n%s\n%s", strings.Join(args, " "), code, exitUsage, out, errOut)
		}
		if !strings.Contains(errOut+out, "starting with a letter") {
			t.Errorf("ks %s: the refusal does not state the rule:\n%s\n%s", strings.Join(args, " "), out, errOut)
		}
	}
}

func TestTheAppRefusesANewAgentNameTheServiceRefusesBeforeAnyRequest(t *testing.T) {
	c := &nameCtl{}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	t.Setenv("KS_HTTP_TIMEOUT_MS", "400")
	for _, name := range []string{"1agent", "a.b", strings.Repeat("a", 49)} {
		c.take()
		var b bytes.Buffer
		scr := newScreen(&b)
		scr.width = func() int { return 120 }
		keys := make(chan keyEvent, len(name)+2)
		for _, r := range name {
			keys <- keyEvent{kind: keyRune, r: r}
		}
		keys <- keyEvent{kind: keyEnter}
		close(keys)
		row, ok := appNewSession(hostedCreds{CTL: srv.URL, Token: "guard-test-token"}, scr, keys)
		if ok || row != nil {
			t.Errorf("the app started a session for the name %q", name)
		}
		if got := c.take(); len(got) != 0 {
			t.Errorf("the app sent %d request(s) for the name %q before refusing it: %v", len(got), name, got)
		}
		// the card wraps its text to the terminal: read it as words
		shown := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╰", " ", "─", " ").Replace(regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`).ReplaceAllString(b.String(), ""))), " ")
		if !strings.Contains(shown, "starting with a letter") {
			t.Errorf("the app's refusal of %q does not state the rule: %q", name, tail(shown, 600))
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
