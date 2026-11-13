// A name is a display name: an agent's and a session's name may carry single
// spaces, as the shared testdata/vectors/name-rule-v2.json says (the service
// holds its own rule to the same file). A name is never an identifier: a name
// with a space is one argument, quoted in a shell, and a command this client
// suggests quotes it. A new session is named by the service after its agent
// ("<agent name> session", then "<agent name> session 2"), so ks run --agent
// sends no name unless --name is given.
package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type displayVectors struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Rule        string `json:"rule"`
	AgentMax    int    `json:"agent_max"`
	SessionMax  int    `json:"session_max"`
	Accepted    []string
	Normalized  []struct{ In, Out string }
	Refused     []string
	SessionOnly []string `json:"session_only_accepted"`
	SessionNo   []string `json:"session_refused"`
}

func loadDisplayVectors(t *testing.T) displayVectors {
	t.Helper()
	var v displayVectors
	b, err := os.ReadFile("testdata/vectors/name-rule-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Name != "ks-display-name-rule" || v.Version != 2 || len(v.Accepted) == 0 || len(v.Refused) == 0 {
		t.Fatalf("display name vectors: %v %q %d", err, v.Name, v.Version)
	}
	return v
}

func TestDisplayNamesFollowTheSharedVectors(t *testing.T) {
	v := loadDisplayVectors(t)
	if displayName.String() != v.Rule || agentNameMax != v.AgentMax || sessionNameMax != v.SessionMax {
		t.Errorf("the client holds %q, %d, %d; the shared vector records %q, %d, %d", displayName.String(), agentNameMax, sessionNameMax, v.Rule, v.AgentMax, v.SessionMax)
	}
	for _, n := range v.Accepted {
		if !displayNameOK(n, agentNameMax) || !displayNameOK(n, sessionNameMax) || tidyName(n) != n {
			t.Errorf("a name the service accepts is refused or changed here: %q", n)
		}
	}
	for _, n := range v.SessionOnly {
		if !displayNameOK(n, sessionNameMax) {
			t.Errorf("a session name the service accepts is refused here: %q", n)
		}
	}
	for _, c := range v.Normalized {
		if got := tidyName(c.In); got != c.Out || !displayNameOK(c.In, agentNameMax) {
			t.Errorf("%q reads %q, want %q", c.In, got, c.Out)
		}
	}
	for _, n := range v.Refused {
		if displayNameOK(n, agentNameMax) {
			t.Errorf("an agent name the service refuses is accepted here: %q", n)
		}
	}
	for _, n := range v.SessionNo {
		if displayNameOK(n, sessionNameMax) {
			t.Errorf("a session name the service refuses is accepted here: %q", n)
		}
	}
}

func TestACommandThatNamesAThingQuotesAName(t *testing.T) {
	for in, want := range map[string]string{
		"main": "main", "Senior_Analyst": "Senior_Analyst",
		"Senior Analyst": "'Senior Analyst'", "Senior  Analyst": "<name>", " main": "<name>", "a;b": "<name>", "a'b c": "<name>",
	} {
		if got := commandWord(in); got != want {
			t.Errorf("commandWord(%q) = %q, want %q", in, got, want)
		}
	}
	if argv := agentResumeArgv("Senior Analyst", "abc123"); len(argv) != 6 || argv[3] != "Senior Analyst" {
		t.Errorf("the command that reopens an agent with a space in its name: %v", argv)
	}
}

func TestANameWithSpacesIsNotRefusedBeforeARequest(t *testing.T) {
	c := &nameCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	for _, args := range [][]string{
		{"agent", "create", "Senior Analyst"},
		{"agent", "rename", "main", "Senior Analyst"},
		{"session", "rename", "main", "Senior Analyst session 2"},
		{"run", "--agent", "--agent-name", "Senior Analyst"},
		{"run", "--agent", "--name", "Senior Analyst session"},
	} {
		c.take()
		out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if strings.Contains(out+errOut, "is not a name") {
			t.Errorf("ks %s refused a name with spaces before any request:\n%s\n%s", strings.Join(args, " "), out, errOut)
		}
		if code == exitUsage && len(c.take()) == 0 {
			t.Errorf("ks %s exited %d without a request", strings.Join(args, " "), code)
		}
	}
	// the rule still holds
	for _, args := range [][]string{{"agent", "create", "1 agent"}, {"agent", "create", "a\tb"}, {"session", "rename", "main", strings.Repeat("a", 65)}} {
		c.take()
		_, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if code != exitUsage || len(c.take()) != 0 || !strings.Contains(errOut, "spaces") {
			t.Errorf("ks %s: exit %d, want a usage error naming spaces, before any request: %s", strings.Join(args, " "), code, errOut)
		}
	}
}

func TestRunAgentSendsNoSessionNameUnlessGiven(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		name  any
		agent string
	}{
		{[]string{"run", "--agent"}, nil, ""},
		{[]string{"run", "--agent", "--agent-name", "Senior  Analyst"}, nil, "Senior  Analyst"},
		{[]string{"run", "--agent", "--name", "  My   run "}, "My run", ""},
	} {
		c := &runCtl{workspace: "available", prov: readyAnswer()}
		syncFixture(t, &c.mu)
		bin, cfg := runAgentEnv(t, c)
		out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), tc.args...)
		if code != 0 || len(c.creations) != 1 {
			t.Fatalf("ks %s: exit %d, %d creations\n%s\n%s", strings.Join(tc.args, " "), code, len(c.creations), out, errOut)
		}
		got, has := c.creations[0]["name"]
		if tc.name == nil && has {
			t.Errorf("ks %s sent the session name %q: the service names it after the agent", strings.Join(tc.args, " "), got)
		}
		if tc.name != nil && got != tc.name {
			t.Errorf("ks %s sent the session name %v, want %v", strings.Join(tc.args, " "), got, tc.name)
		}
		if tc.agent != "" && c.creations[0]["primary_agent_name"] != tc.agent {
			t.Errorf("ks %s sent the agent name %v", strings.Join(tc.args, " "), c.creations[0]["primary_agent_name"])
		}
	}
}
