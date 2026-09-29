package main

// help.go: the top-level and group help, written to be read. The top level
// shows where to start, then every top-level command and every group as ONE
// line in a named section; a group's help shows its commands with their
// arguments only. Flags live in `ks <command> --help`, the one place that
// explains them. Everything is rendered from the registry, so a command the
// binary has is a command the help shows, and a registry command missing from
// the sections below still appears under "Other".

import (
	"fmt"
	"strings"
)

// helpSections orders the top-level commands and groups by what a person is
// doing. A name absent here is listed under "Other" rather than hidden.
var helpSections = []struct {
	title string
	names []string
}{
	{"Agents", []string{"agent", "task", "approval", "adviser", "advice", "result", "file", "check"}},
	{"Sessions", []string{"run", "session", "checkpoint", "wake", "kill", "fork", "exec", "attach", "meter"}},
	{"Cruise", []string{"cruise"}},
	{"Keys and setup", []string{"key", "preflight", "project"}},
	{"Account and this client", []string{"login", "logout", "doctor", "update", "version", "operation", "completion", "reference", "uninstall"}},
}

// helpStartHere is the shortest path to a working agent.
var helpStartHere = [][2]string{
	{"ks", "open the app: your agents, and a window you stay in"},
	{"ks login", "sign in (opens your browser)"},
	{"ks run --agent", "start an agent session: a machine with an agent in it"},
	{"ks agent open main", "open that agent's window"},
	{"ks session list", "your sessions; a short id such as 3f2a1c stands for the full id"},
}

func helpWidth() int {
	w := termColumns()
	if w < 40 {
		w = 80
	}
	if w > 110 {
		w = 110
	}
	return w
}

// wrapLine lays text out after a left column of width `left`, wrapping at
// word boundaries within `width` and indenting continuation lines under the
// text, so a long description reads as a paragraph, not a spill.
func wrapLine(b *strings.Builder, label string, left int, text string, width int) {
	room := width - left
	if room < 20 {
		room = 20
	}
	pad := strings.Repeat(" ", left)
	first := label + strings.Repeat(" ", max(1, left-displayWidth(label)))
	if displayWidth(label) >= left {
		b.WriteString(label + "\n")
		first = pad
	}
	line, cur := first, 0
	for _, word := range strings.Fields(text) {
		ww := displayWidth(word)
		if cur > 0 && cur+1+ww > room {
			b.WriteString(line + "\n")
			line, cur = pad, 0
		}
		if cur > 0 {
			line += " "
			cur++
		}
		line += word
		cur += ww
	}
	b.WriteString(line + "\n")
}

// firstClause is a summary up to its first ": " or "; ", which is what one
// line in an overview needs; the whole text is one --help away.
func firstClause(s string) string {
	depth := 0
	for i := 0; i < len(s)-1; i++ {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ';', ':':
			if depth == 0 && s[i+1] == ' ' && i > 0 {
				return strings.TrimSuffix(strings.TrimSpace(s[:i]), ".")
			}
		}
	}
	return strings.TrimSuffix(strings.TrimSpace(s), ".")
}

// argsOnly is a command's synopsis without its options: the name and its
// positional arguments, which is what a list of commands needs.
func argsOnly(c *Command) string {
	s := "ks " + c.Name()
	for _, a := range c.Args {
		if a.Required {
			s += " <" + a.Name + ">"
		} else {
			s += " [" + a.Name + "]"
		}
	}
	if c.Rest != "" {
		s += " <" + c.Rest + "...>"
	}
	return s
}

func registryUsage(reg []*Command) string {
	width := helpWidth()
	var b strings.Builder
	b.WriteString("ks — durable agent sessions on KeepState\n\nStart here:\n")
	for _, s := range helpStartHere {
		wrapLine(&b, "  "+s[0], 24, s[1], width)
	}

	// what each top-level name is: a group (with its size) or one command
	top := map[string]*Command{}
	size := map[string]int{}
	var order []string
	for _, c := range reg {
		head := c.Path[0]
		if _, seen := top[head]; !seen {
			order = append(order, head)
		}
		if len(c.Path) == 1 {
			top[head] = c
		} else {
			if top[head] == nil {
				top[head] = nil
			}
			if !c.Group {
				size[head]++
			}
		}
	}
	line := func(name string) {
		c := top[name]
		summary := ""
		if c != nil {
			summary = c.Summary
		}
		summary = firstClause(summary)
		switch n := size[name]; {
		case n == 1:
			summary += " · 1 command"
		case n > 1:
			summary += fmt.Sprintf(" · %d commands", n)
		}
		wrapLine(&b, "  "+name, 14, fitWidth(summary, width-14), width)
	}
	listed := map[string]bool{}
	for _, sec := range helpSections {
		var names []string
		for _, n := range sec.names {
			if _, ok := top[n]; ok {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			continue
		}
		b.WriteString("\n" + sec.title + ":\n")
		for _, n := range names {
			line(n)
			listed[n] = true
		}
	}
	var other []string
	for _, n := range order {
		if !listed[n] {
			other = append(other, n)
		}
	}
	if len(other) > 0 {
		b.WriteString("\nOther:\n")
		for _, n := range other {
			line(n)
		}
	}
	b.WriteString(`
More:
  ks <command> --help   what a command does, its options and examples
  ks <group> --help     the commands in a group, e.g. ks agent --help

Every command also takes --json, --plain, --quiet, --no-input, --yes and
--wait-timeout. Exit codes: 0 ok, 1 failed, 2 usage, 3 sign-in, 4 temporary
or unknown outcome, 5 conflict or limit, 6 integrity, 130 interrupted.
Help makes no request and changes nothing.
`)
	return b.String()
}

// groupHelp is a group's commands, one per line with its arguments, and the
// pointer to each command's own help for options.
func groupHelp(g *Command, reg []*Command) string {
	width := helpWidth()
	var b strings.Builder
	fmt.Fprintf(&b, "ks %s — %s\n\nCommands:\n", g.Name(), g.Summary)
	var cmds []*Command
	left := 0
	for _, c := range reg {
		if !c.Group && len(c.Path) > 1 && c.Path[0] == g.Path[0] {
			cmds = append(cmds, c)
			if w := displayWidth(argsOnly(c)) + 4; w > left {
				left = w
			}
		}
	}
	if left > 38 {
		left = 38 // a longer synopsis gets its own line; the text starts below it
	}
	for _, c := range cmds {
		wrapLine(&b, "  "+argsOnly(c), left, firstClause(c.Summary), width)
	}
	fmt.Fprintf(&b, "\nRun ks %s <command> --help for a command's options and examples.\nHelp makes no request and changes nothing.\n", g.Name())
	return b.String()
}
