// The roles: three built in, your own beside them, the front matter read,
// a command line ignored and a secret refused.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTemplate(t *testing.T) {
	tm, err := parseTemplate("lint", "f", "---\ndescription: Lints things\nharness: codex\nmodel: gpt-6-luna\nkey: vlt_0123abcd\ncommand: rm -rf /\n---\nYou lint.\n")
	if err != nil || tm.Description != "Lints things" || tm.Harness != "codex" || tm.Model != "gpt-6-luna" || tm.Key != "vlt_0123abcd" || tm.Body != "You lint." || strings.Join(tm.Ignored, ",") != "command" {
		t.Fatalf("parse: %+v %v", tm, err)
	}
	if _, err := parseTemplate("bad", "f", "---\nkey: sk-ant-abcdefghijklmnop\n---\nbody"); err == nil || !strings.Contains(err.Error(), "looks like a secret") {
		t.Fatalf("a secret in a template was accepted: %v", err)
	}
	for _, bad := range []string{"---\ndescription: x\n", "---\ndescription: x\n---\n", "---\nnot a pair\n---\nbody"} {
		if _, err := parseTemplate("bad", "f", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got := withTemplate(tm, "the task"); got != "You lint.\n\nthe task" {
		t.Fatalf("withTemplate: %q", got)
	}
}

func TestAgentTemplatesListsBuiltInsAndYourOwn(t *testing.T) {
	c := newInteropCtl()
	_, cfg, run := interopEnv(t, c)
	dir := filepath.Join(cfg, "keepstate", "templates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(dir, "docs.md"), "---\ndescription: Writes the docs\ncommand: make docs\n---\nYou write docs.\n")
	writeFileT(t, filepath.Join(dir, "reviewer.md"), "---\ndescription: My reviewer\n---\nYou review my way.\n")
	out, errs, code := run(nil, "agent", "templates")
	if code != 0 || !strings.Contains(out, "implementer") || !strings.Contains(out, "tester") || !strings.Contains(out, "docs           your file  Writes the docs") ||
		!strings.Contains(out, "replaces the built-in") || !strings.Contains(out, "ignored: command") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	if len(c.reqs) != 0 {
		t.Fatalf("listing templates made requests: %v", c.reqs)
	}
	// a child started with your role carries your body
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "r", "--template", "reviewer", "look"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	if task := c.first("POST /api/v2/agents/agent_1/tasks"); !strings.Contains(task, "You review my way.") || strings.Contains(task, "You are a reviewer.") {
		t.Fatalf("your file did not replace the built-in: %s", task)
	}
}

func TestBuiltInRoles(t *testing.T) {
	names := []string{}
	for _, b := range builtinTemplates {
		names = append(names, b.Name)
		if b.Body == "" || b.Description == "" {
			t.Errorf("%s is incomplete", b.Name)
		}
	}
	if strings.Join(names, ",") != "implementer,reviewer,tester" {
		t.Fatalf("built-in roles: %v", names)
	}
}
