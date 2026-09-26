// KS-092 client half: parser, state and alias regressions.
//
//	QA-092-1  the v0.1.8 budget, help and quote bugs, reintroduced one at a
//	          time into a copy of this source, each make the guard tests
//	          FAIL (a mutation check; KS_MUTATION=1, run by the regressions
//	          workflow), so a guard that silently stopped guarding is caught
//	QA-092-2  an unknown task, agent or session state is shown as unknown,
//	          never as a successful default
//	QA-092-3  every alias resolves to the same command, with the same
//	          parsing, safety checks and help, as its canonical spelling
package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mutation is one reintroduced defect: an exact edit, and the guard tests
// that must fail because of it.
type mutation struct {
	name, file, from, to, guards string
}

var ks092Mutations = []mutation{
	{"budget: an unparseable value silently becomes zero (v0.1.8 F1)", "command.go",
		"\t\tn, err := parseWhole(value)\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"Option --%s: %v.\", f.Name, err)\n\t\t}\n",
		"\t\tn, _ := parseWhole(value)\n",
		"TestStrictParsingRefusesBeforeAnyRequest|TestIntegerFormsFailWithoutTruncation"},
	{"help: a help word is skipped and the command runs (v0.1.8 D1, `ks run --help` started a session)", "command.go",
		"\t\tif isHelpWord(t) {\n\t\t\tinv.Help = true\n\t\t\treturn inv, nil\n\t\t}\n",
		"\t\tif isHelpWord(t) {\n\t\t\ti++\n\t\t\tcontinue\n\t\t}\n",
		"TestHelpHasNoSideEffect"},
	{"quote: exec joins the vector with spaces, losing the quoting (v0.1.8 F3)", "hosted.go",
		"\treturn strings.Join(words, \" \")\n",
		"\treturn strings.Join(args, \" \")\n",
		"TestShellJoinRoundTripsThroughSh|TestExecArgumentContract"},
	{"state: an unknown state is shown as a successful default (QA-092-2)", "states.go",
		"\treturn \"unknown (\" + sanitize(s) + \")\"\n",
		"\treturn \"ready\"\n",
		"TestKS092UnknownStatesAreNeverASuccessfulDefault"},
}

// copyTree copies the module (no .git) so a mutation never touches this one.
func copyTree(t *testing.T, dst string) {
	t.Helper()
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == ".git" || p == "node_modules") {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKS092MutationsAreCaughtByTheirGuards(t *testing.T) {
	// every mutation must still apply to the current source: a mutation
	// that no longer matches would silently stop testing its guard
	for _, m := range ks092Mutations {
		b, err := os.ReadFile(m.file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(b), m.from) != 1 {
			t.Errorf("%s: the edit no longer matches %s exactly once; update the mutation with the code", m.name, m.file)
		}
	}
	if os.Getenv("KS_MUTATION") == "" {
		t.Skip("KS_MUTATION not set: the mutation runs (a copy, rebuilt per defect) are in the regressions workflow")
	}
	for _, m := range ks092Mutations {
		m := m
		t.Run(m.file+": "+m.name, func(t *testing.T) {
			dst := t.TempDir()
			copyTree(t, dst)
			p := filepath.Join(dst, m.file)
			b, _ := os.ReadFile(p)
			if err := os.WriteFile(p, []byte(strings.Replace(string(b), m.from, m.to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "test", "-count=1", "-run", "^("+m.guards+")$", ".")
			cmd.Dir = dst
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the defect was reintroduced and the guards PASSED: %s is not guarded\n%s", m.name, out)
			}
			if !strings.Contains(string(out), "--- FAIL") {
				t.Fatalf("the guards did not fail as tests (a build error proves nothing):\n%s", out)
			}
		})
	}
}

// QA-092-2: states the client does not know read as unknown, everywhere.
func TestKS092UnknownStatesAreNeverASuccessfulDefault(t *testing.T) {
	for _, kind := range []string{"agent_activity", "task_state", "session_runtime", "attempt_state"} {
		if got := stateLabel(kind, "teleporting"); got != "unknown (teleporting)" {
			t.Errorf("%s: an unknown state reads %q", kind, got)
		}
		if got := stateLabel(kind, ""); got != "unavailable" {
			t.Errorf("%s: an absent state reads %q", kind, got)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		env := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			env(map[string]any{"registry_version": "t", "build": "b", "fetched_at": "x", "price_book": "v1.3", "limits": map[string]any{},
				"capabilities": []map[string]any{{"id": "session.list", "availability": "available", "summary": "s", "surface": "api"},
					{"id": "agent.workspace", "availability": "available", "summary": "s", "surface": "api"}}})
		case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
			env(map[string]any{"items": []map[string]any{{"id": "agentsession0000000000000000aaaa", "short_id": agentSessionShort, "name": "n",
				"runtime_state": "glowing", "record_id": agentSessionRecord, "agent_activity": "teleporting", "task_state": "1 queued",
				"observed_at": "x", "last_activity_at": "2026-09-26T10:00:00Z", "created_at": "x"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/sessions":
			env(map[string]any{"items": []any{}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/agents":
			env(map[string]any{"items": []map[string]any{{"id": "agt_main0001", "session_id": agentSessionRecord, "name": "main", "is_primary": true,
				"activity": "teleporting", "observed_at": "2026-09-26T10:00:00Z", "active_task_id": "tsk_q"}}})
		case r.URL.Path == "/api/v2/tasks":
			env(map[string]any{"items": []map[string]any{{"id": "tsk_q", "agent_id": "agt_main0001", "state": "quantum", "queue_seq": 1}}})
		case r.URL.Path == "/api/v2/tasks/tsk_q":
			env(map[string]any{"id": "tsk_q", "agent_id": "agt_main0001", "state": "quantum", "queue_seq": 1})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"type":"not_found","message":"no"}}`))
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	for _, args := range [][]string{
		{"session", "list"}, {"session", "show", agentSessionShort},
		{"agent", "status", "main", "--session", agentSessionShort}, {"agent", "list", "--session", agentSessionShort},
		{"task", "list", "--session", agentSessionShort}, {"task", "show", "tsk_q"},
	} {
		out, errs, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		all := out + errs
		if !strings.Contains(all, "unknown") { // "unknown (x)" in details, "unknown" in a table cell
			t.Errorf("%v does not say the state is unknown:\n%q", args, all)
		}
		for _, success := range []string{"ready", "succeeded", "Verified", "finished"} {
			for _, l := range strings.Split(all, "\n") {
				if strings.Contains(l, success) && !strings.Contains(l, "unknown") && !strings.Contains(l, "Finished, never") {
					if strings.Contains(l, "teleporting") || strings.Contains(l, "quantum") || strings.Contains(l, "glowing") ||
						strings.HasPrefix(strings.TrimSpace(l), "activity") || strings.HasPrefix(strings.TrimSpace(l), "state") {
						t.Errorf("%v reads %q for an unknown state: %q", args, success, l)
					}
				}
			}
		}
	}
}

// QA-092-3: an alias is the same command -- same object, so the same Run,
// capability gate and parser -- and says the same help and refuses the same
// mistakes as its canonical spelling; a flag alias sets the same value.
func TestKS092EveryAliasIsItsCanonicalCommand(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	n := 0
	for _, c := range registry {
		for _, alias := range c.Aliases {
			n++
			got, _, _ := lookup(registry, alias)
			if got != c {
				t.Errorf("alias %v resolves to %v, not %v", alias, got.Path, c.Path)
				continue
			}
			h1, _, c1 := auditExec(t, bin, cfg, t.TempDir(), nil, append(append([]string{}, c.Path...), "--help")...)
			h2, _, c2 := auditExec(t, bin, cfg, t.TempDir(), nil, append(append([]string{}, alias...), "--help")...)
			if c1 != 0 || c2 != 0 || h1 != h2 {
				t.Errorf("alias %v helps differently from %v (exit %d/%d)", alias, c.Path, c1, c2)
			}
			_, e1, x1 := auditExec(t, bin, cfg, t.TempDir(), nil, append(append([]string{}, c.Path...), "--no-such-option-ks092")...)
			_, e2, x2 := auditExec(t, bin, cfg, t.TempDir(), nil, append(append([]string{}, alias...), "--no-such-option-ks092")...)
			if x1 != 2 || x2 != 2 || e1 != e2 {
				t.Errorf("alias %v refuses a bad option differently from %v:\n%s\n%s", alias, c.Path, e1, e2)
			}
		}
		for _, f := range c.Flags {
			for _, fa := range f.Aliases {
				n++
				val := "7"
				switch f.Kind {
				case flagBool:
					continue // a bool alias carries no value to compare
				case flagUSD:
					val = "2"
				}
				args := []string{}
				for range c.Args {
					args = append(args, "x")
				}
				i1, e1 := c.parse(append(append([]string{}, args...), "--"+f.Name, val))
				i2, e2 := c.parse(append(append([]string{}, args...), "--"+fa, val))
				if (e1 == nil) != (e2 == nil) || (e1 == nil && (i1.Str(f.Name) != i2.Str(f.Name) || i1.Int(f.Name) != i2.Int(f.Name) || !i2.Set(f.Name))) {
					t.Errorf("%s: flag alias --%s does not set --%s the same way", c.Name(), fa, f.Name)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no alias was found to check")
	}
}
