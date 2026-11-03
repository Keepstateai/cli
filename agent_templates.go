// agent_templates.go: the roles a child agent can start with. A template is
// a Markdown file with a short front matter and a body; the body is the
// opening instructions, joined in front of the task ks agent run gives the
// child. Three roles are built in (reviewer, implementer, tester), and your
// own files under configDir()/templates/<name>.md sit beside them; a file
// of the same name as a built-in replaces it.
//
//	---
//	description: Reviews a change and reports what is wrong
//	harness: claude-code
//	model: claude-sonnet-5-5
//	key: vlt_0123abcd
//	---
//	You are a reviewer. ...
//
// The front matter is the hc template format without its command field:
// KeepState never runs a local command from a template, so a command line is
// read, ignored and said so. key names a key by its id or alias (or own, or
// keepstate); a value shaped like a secret is refused, because a key is
// never written into a template or an argument.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type agentTemplate struct {
	Name        string `json:"name"`
	Source      string `json:"source"` // built-in, or the file's path
	Description string `json:"description"`
	Harness     string `json:"harness,omitempty"`
	Model       string `json:"model,omitempty"`
	Key         string `json:"key,omitempty"`
	Body        string `json:"body"`
	// Ignored names front matter lines that were read and not used
	Ignored []string `json:"ignored,omitempty"`
	// Replaces is true for a file of a built-in's name
	Replaces bool `json:"replaces_builtin,omitempty"`
}

const builtinSource = "built-in"

// builtinTemplates are the three roles every client has.
var builtinTemplates = []agentTemplate{
	{Name: "implementer", Source: builtinSource, Description: "Makes the change the task asks for and checks it",
		Body: "You are an implementer. Make the change the task asks for, in small steps, and keep to what it asks.\n" +
			"Run the tests that cover the change. If none do, say so and add the smallest test that would.\n" +
			"When you finish, report what you changed, in which files, and how you checked it.\n" +
			"If the task is unclear or would need something you cannot do, stop and say what is missing instead of guessing."},
	{Name: "reviewer", Source: builtinSource, Description: "Reviews code or a change and reports what is wrong, without editing",
		Body: "You are a reviewer. Read the code or the change the task names. Do not edit any file.\n" +
			"Report what is wrong, what is risky and what is missing, most important first, each with its file and line.\n" +
			"Say what you did not check. End with a one-line verdict: ready, ready with changes, or not ready."},
	{Name: "tester", Source: builtinSource, Description: "Writes and runs tests for what the task names, without changing the code under test",
		Body: "You are a tester. Write or run tests for what the task names. Do not change the code under test.\n" +
			"Report each test you ran and whether it passed, with the exact output of any failure.\n" +
			"If something cannot be tested as asked, say why and what would make it testable."},
}

func templatesDir() string { return filepath.Join(configDir(), "templates") }

// parseTemplate reads one template file's text. name is its file name
// without .md.
func parseTemplate(name, source, text string) (agentTemplate, error) {
	t := agentTemplate{Name: name, Source: source}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	body := text
	if strings.HasPrefix(text, "---\n") {
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return t, fmt.Errorf("template %s opens a front matter with --- and never closes it", name)
		}
		head := text[4 : 4+end]
		body = strings.TrimPrefix(text[4+end+4:], "\n")
		for _, line := range strings.Split(head, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				return t, fmt.Errorf("template %s: front matter line %q is not key: value", name, sanitize(line))
			}
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch k {
			case "description":
				t.Description = v
			case "harness":
				t.Harness = v
			case "model":
				t.Model = v
			case "key":
				if secretShapes.MatchString(v) {
					return t, fmt.Errorf("template %s: key holds what looks like a secret; name a key by its id or alias (ks key list), never by its value", name)
				}
				t.Key = v
			default:
				// command among them: KeepState never runs a local command
				t.Ignored = append(t.Ignored, k)
			}
		}
	}
	t.Body = strings.TrimSpace(body)
	if t.Body == "" {
		return t, fmt.Errorf("template %s has no body: the body is the opening instructions", name)
	}
	return t, nil
}

// loadTemplates answers every template: the built-ins and your own files,
// sorted by name. A file that cannot be read is an error naming it.
func loadTemplates() ([]agentTemplate, error) {
	byName := map[string]agentTemplate{}
	for _, t := range builtinTemplates {
		byName[t.Name] = t
	}
	files, err := filepath.Glob(filepath.Join(templatesDir(), "*.md"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		if newNameRefusal(name, "") != nil {
			return nil, fmt.Errorf("template file %s: %q is not a role name (1 to 48 letters, digits, hyphens or underscores, starting with a letter)", f, sanitize(name))
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		t, err := parseTemplate(name, f, string(b))
		if err != nil {
			return nil, err
		}
		if old, ok := byName[name]; ok && old.Source == builtinSource {
			t.Replaces = true
		}
		byName[name] = t
	}
	out := make([]agentTemplate, 0, len(byName))
	for _, t := range byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var errNoTemplate = errors.New("no such template")

// findTemplate answers the template a role names.
func findTemplate(name string) (agentTemplate, error) {
	all, err := loadTemplates()
	if err != nil {
		return agentTemplate{}, err
	}
	var names []string
	for _, t := range all {
		if t.Name == name {
			return t, nil
		}
		names = append(names, t.Name)
	}
	return agentTemplate{}, &cliError{Code: exitUsage, Kind: "template_unknown",
		Message:    fmt.Sprintf("no template is named %q (there are: %s); nothing was created", sanitize(name), strings.Join(names, ", ")),
		NextAction: "ks agent templates"}
}

// withTemplate joins a template's body in front of the task.
func withTemplate(t agentTemplate, task string) string {
	if t.Body == "" {
		return task
	}
	return t.Body + "\n\n" + task
}

func runAgentTemplates(inv *Invocation) {
	all, err := loadTemplates()
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "template_unreadable", Message: sanitize(err.Error()), NextAction: "fix or remove the file under " + templatesDir()})
	}
	emit(map[string]any{"templates": all, "dir": templatesDir()}, func() {
		fmt.Printf("%-14s %-10s %s\n", "ROLE", "FROM", "DESCRIPTION")
		for _, t := range all {
			from := "built-in"
			if t.Source != builtinSource {
				from = "your file"
			}
			fmt.Printf("%-14s %-10s %s\n", clip(t.Name, 14), from, sanitize(t.Description))
			if t.Replaces {
				fmt.Printf("%-14s %-10s replaces the built-in of this name: %s\n", "", "", sanitize(t.Source))
			}
			if len(t.Ignored) > 0 {
				fmt.Printf("%-14s %-10s ignored: %s (KeepState never runs a local command from a template)\n", "", "", strings.Join(t.Ignored, ", "))
			}
		}
		fmt.Printf("your own roles: %s/<role>.md, front matter description, harness, model and key, then the opening instructions\n", templatesDir())
		fmt.Println("use one: ks agent run --name <name> --template <role> \"<task>\"")
	})
}
