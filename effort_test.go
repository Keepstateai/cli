// The model's effort: a person chooses how hard the model thinks, with
// --effort on ks session runner, ks run --agent and ks agent run. The word is
// checked here only against the five levels and "default"; the service checks
// it against the model. The read says the level chosen or the default, or why
// the session cannot take one, beside what it costs.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEffortWordsAndParse(t *testing.T) {
	for in, want := range map[string]string{"low": "low", "Medium": "medium", " high ": "high", "xhigh": "xhigh", "max": "max", "default": ""} {
		got, err := parseEffortFlag(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ultra", "extra-high", "none", "5"} {
		if _, err := parseEffortFlag(bad); err == nil || err.Code != exitUsage || !strings.Contains(err.Message, "low, medium, high, xhigh or max") {
			t.Errorf("%q was not refused as usage: %+v", bad, err)
		}
	}
	if effortWords("xhigh") != "Extra high" || effortWords("max") != "Max" || effortWords("low") != "Low" {
		t.Fatal("the level names")
	}
	if !strings.Contains(effortMoney, "more output tokens") || !strings.Contains(effortMoney, "same per-token price") || !strings.Contains(effortMoney, "token budget and spend limit sooner") {
		t.Fatalf("the money words: %q", effortMoney)
	}
	// the read's one line: chosen, default, unavailable
	if l := effortLine(sessionRunnerRecord{RunnerEffort: "high", RunnerEffortDefault: "medium"}); !strings.Contains(l, "High") || strings.Contains(l, "default") {
		t.Errorf("chosen: %q", l)
	}
	if l := effortLine(sessionRunnerRecord{RunnerEffortDefault: "medium"}); !strings.Contains(l, "default (Medium)") {
		t.Errorf("default: %q", l)
	}
	if l := effortLine(sessionRunnerRecord{RunnerEffortUnavailable: "the Codex runner in this session's image cannot take one"}); !strings.Contains(l, "cannot take one") {
		t.Errorf("unavailable: %q", l)
	}
	if l := effortLine(sessionRunnerRecord{}); !strings.Contains(l, "default") {
		t.Errorf("a service that says nothing: %q", l)
	}
}

func TestCatalogEffortLines(t *testing.T) {
	var h catalogHarness
	if err := json.Unmarshal([]byte(`{"id":"claude-code","models":[
	 {"id":"claude-sonnet-5-5","efforts":["low","medium","high","xhigh","max"],"default_effort":"medium"},
	 {"id":"claude-haiku-4-5-20251001","efforts":[],"default_effort":""},
	 {"id":"claude-old"}]}`), &h); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(harnessEffortLines(h), "\n")
	for _, want := range []string{"claude-sonnet-5-5: Low, Medium, High, Extra high, Max (default Medium)", "claude-haiku-4-5-20251001: takes no effort setting"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "claude-old") {
		t.Errorf("a model the catalog says nothing about was described:\n%s", got)
	}
}

// effortRunnerCtl: runnerCtl whose session read carries the effort fields,
// and which refuses a level the model does not take.
type effortRunnerCtl struct {
	*runnerCtl
	effort map[string]any
}

func (c *effortRunnerCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_park" {
		cw := httptest.NewRecorder()
		c.runnerCtl.ServeHTTP(cw, r)
		var env map[string]any
		_ = json.Unmarshal(cw.Body.Bytes(), &env)
		d, _ := env["data"].(map[string]any)
		for k, v := range c.effort {
			d[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/api/v2/sessions/session_park/runner-change" {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["runner_effort"] == "max" {
			c.rmu.Lock()
			c.sent = append(c.sent, body)
			c.rmu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": "ks_effort_invalid", "type": "ks_effort_invalid",
				"message": "Claude Haiku 4.5 takes no effort setting; choose default", "work_started": "no"}})
			return
		}
		raw, _ := json.Marshal(body)
		r.Body = httpBody(raw)
	}
	c.runnerCtl.ServeHTTP(w, r)
}

func TestSessionRunnerEffort(t *testing.T) {
	rc := &runnerCtl{modelCtl: modelCtl{model: "claude-sonnet-5-5", rev: 5}, ops: map[string]map[string]any{}, reads: map[string]int{},
		harness: "claude-code", key: map[string]any{"provider": "anthropic", "route": "keepstate"}}
	c := &effortRunnerCtl{runnerCtl: rc, effort: map[string]any{"runner_effort": "", "runner_effort_default": "medium"}}
	syncFixture(t, &rc.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// the read: the default named, what effort costs, and how to change it
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark")
	if code != 0 || !strings.Contains(out, "effort         default (Medium)") || !strings.Contains(out, "more output tokens") || !strings.Contains(out, "--effort <level>") {
		t.Fatalf("the read: %d\n%s%s", code, out, errs)
	}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--json")
	if !strings.Contains(out, `"runner_effort":""`) || !strings.Contains(out, `"runner_effort_default":"medium"`) {
		t.Fatalf("the JSON read lacks the effort:\n%s", out)
	}

	// --effort alone is a change, and only it is sent
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--effort", "high")
	if code != 0 {
		t.Fatalf("--effort high: %d\n%s%s", code, out, errs)
	}
	b := rc.sent[0]
	if b["runner_effort"] != "high" || b["runner_model"] != nil || b["harness"] != nil || b["key"] != nil || b["expected_revision"] != float64(5) {
		t.Fatalf("the change sent: %v", b)
	}
	// default is sent as no choice
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--effort", "default", "--model", "claude-opus-5-5"); code != 0 {
		t.Fatalf("--effort default: %d\n%s", code, errs)
	}
	if v, sent := rc.sent[1]["runner_effort"]; !sent || v != "" || rc.sent[1]["runner_model"] != "claude-opus-5-5" {
		t.Fatalf("default was not sent as \"\": %v", rc.sent[1])
	}
	// a level the model does not take: the service's words, nothing changed, where the levels are listed
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--effort", "max")
	if code == 0 || !strings.Contains(errs, "takes no effort setting") || !strings.Contains(errs, "nothing changed") || !strings.Contains(errs, "ks cruise models") {
		t.Fatalf("a refused level: %d\n%s", code, errs)
	}
	// a word that is no level: usage, nothing sent
	n := len(rc.sent)
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark", "--effort", "ultra"); code != exitUsage || !strings.Contains(errs, "low, medium, high, xhigh or max") {
		t.Fatalf("--effort ultra: %d\n%s", code, errs)
	}
	if len(rc.sent) != n {
		t.Fatal("a usage error sent a change")
	}

	// a session that cannot take one says why
	c.effort = map[string]any{"runner_effort": "", "runner_effort_default": "medium", "runner_effort_unavailable": "the Codex runner in this session's image cannot take an effort setting"}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "session", "runner", "fleetpark")
	if !strings.Contains(out, "cannot take an effort setting") {
		t.Fatalf("unavailable is not said:\n%s", out)
	}
}

// ks run --agent --effort sends runner_effort on the record; default and no
// flag send nothing; a word that is no level creates nothing; --effort
// without --agent belongs to an agent session.
func TestRunAgentSendsTheChosenEffort(t *testing.T) {
	c := &runCtl{workspace: "available", prov: readyAnswer()}
	syncFixture(t, &c.mu)
	bin, cfg := runAgentEnv(t, c)
	out, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--model", "claude-sonnet-5-5", "--effort", "xhigh")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "at Extra high effort") {
		t.Fatalf("the run's line does not name the effort:\n%s", out)
	}
	for _, args := range [][]string{{"run", "--agent", "--effort", "default"}, {"run", "--agent"}} {
		if _, errOut, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...); code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, code, errOut)
		}
	}
	c.mu.Lock()
	created := append([]map[string]any(nil), c.creations...)
	c.mu.Unlock()
	if len(created) != 3 || created[0]["runner_effort"] != "xhigh" {
		t.Fatalf("the creation body does not carry the effort: %v", created)
	}
	for _, b := range created[1:] {
		if _, sent := b["runner_effort"]; sent {
			t.Fatalf("an effort nobody chose was sent: %v", b)
		}
	}
	if _, errOut, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--agent", "--effort", "hard"); code != exitUsage || !strings.Contains(errOut, "low, medium, high, xhigh or max") {
		t.Fatalf("--effort hard: exit %d\n%s", code, errOut)
	}
	if _, errOut, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--effort", "high"); code != exitUsage || !strings.Contains(errOut, "--effort belongs to an agent session") {
		t.Fatalf("--effort without --agent: exit %d\n%s", code, errOut)
	}
	if len(c.creations) != 3 {
		t.Fatalf("a refused run created a session: %v", c.creations)
	}
}

func TestAgentRunSendsTheChosenEffort(t *testing.T) {
	c := newInteropCtl()
	_, _, run := interopEnv(t, c)
	if out, errs, code := run(nil, "agent", "run", "--bg", "--no-parent", "--name", "review", "--effort", "low", "review the diff", "--json"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if created := c.first("POST /api/v2/sessions "); !strings.Contains(created, `"runner_effort":"low"`) {
		t.Fatalf("the creation does not carry the effort: %s", created)
	}
	c2 := newInteropCtl()
	_, _, run2 := interopEnv(t, c2)
	if _, errs, code := run2(nil, "agent", "run", "--no-parent", "--name", "x", "--effort", "lots", "t"); code != exitUsage || c2.mutations() != 0 {
		t.Fatalf("--effort lots: %d (%d mutations)\n%s", code, c2.mutations(), errs)
	}
}

func httpBody(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
