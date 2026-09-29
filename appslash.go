package main

// appslash.go: every ks command as a / command in the app (phase 2).
//
// A / command RUNS THE SAME ks BINARY with the same arguments, as a child
// process whose output streams into the conversation. Its options, checks,
// refusals and output are therefore the command's own, by construction:
// there is no second copy of any command to drift. The window fills in its
// own context -- the session, and the agent's name -- when the command takes
// them and they were not given, and the child runs with --no-input, so a
// step that needs a confirmation refuses and names the flag instead of
// prompting inside the app.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// slashShortcuts are short names for what a window asks most.
var slashShortcuts = map[string][]string{
	"stop":      {"agent", "stop"},
	"pause":     {"agent", "pause"},
	"resume":    {"agent", "resume"},
	"queue":     {"agent", "queue", "show"},
	"tasks":     {"task", "list"},
	"results":   {"result", "list"},
	"approvals": {"approval", "list"},
	"advisers":  {"adviser", "list"},
	"usage":     {"session", "usage"},
	"keys":      {"key", "list"},
	"logs":      {"agent", "logs"},
	"agents":    {"agent", "list"},
	// the timeline and the results (app phase 3)
	"checkpoints": {"session", "checkpoints"},
	"restore":     {"session", "restore"},
	"diff":        {"result", "diff"},
	"apply":       {"result", "apply"},
}

// slashExcluded are the commands a window does not run, each with why.
var slashExcluded = map[string]string{
	"agent open": "you are in the agent's window already; /home, then Enter, opens another",
	"attach":     "it takes over the terminal itself: run ks attach outside the app",
	"login":      "signing in opens a browser: run ks login outside the app",
	"logout":     "it removes the sign-in the app is using: run ks logout outside the app",
}

// windowCommands are handled by the window itself (app.go), not run.
var windowCommands = map[string]bool{"help": true, "status": true, "home": true, "back": true, "quit": true, "exit": true, "take": true, "switch": true}

// splitWords splits a line like a shell would for quoting: "a b" and 'a b'
// are one word; nothing is expanded.
func splitWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("a quote is not closed")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// slashContext is what a window knows that a command may need.
type slashContext struct {
	sessionID string // the id a command's --session or <session> takes
	agentName string
}

// slashArgv turns a / line into the ks arguments it runs, or an error that
// says why it does not run. It never runs anything.
func slashArgv(line string, ctx slashContext) ([]string, *Command, error) {
	words, err := splitWords(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if err != nil || len(words) == 0 {
		return nil, nil, fmt.Errorf("an empty or unclosed command; /help lists them")
	}
	if exp, ok := slashShortcuts[words[0]]; ok {
		words = append(append([]string{}, exp...), words[1:]...)
	}
	c, rest, sugg := lookup(registry, words)
	if c == nil {
		msg := fmt.Sprintf("/%s is not a ks command", words[0])
		if sugg != "" {
			msg += "; " + sugg
		}
		return nil, nil, fmt.Errorf("%s (/help lists them)", msg)
	}
	if c.Group {
		return nil, c, fmt.Errorf("/%s is a group of commands; /%s --help lists them", c.Name(), c.Name())
	}
	if why, ok := slashExcluded[c.Name()]; ok {
		return nil, c, fmt.Errorf("/%s: %s", c.Name(), why)
	}
	argv := append(append([]string{}, c.Path...), rest...)
	// positional context: an agent name or a session the command requires
	// and was not given (flags are not counted as positionals)
	positional := 0
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			if f := c.flagNamed(strings.TrimLeft(rest[i], "-")); f != nil && f.Kind != flagBool && !strings.Contains(rest[i], "=") {
				i++ // its value
			}
			continue
		}
		positional++
	}
	if positional == 0 && len(c.Args) > 0 && c.Args[0].Required && !hasHelp(rest) {
		switch c.Args[0].Name {
		case "name", "agent":
			if ctx.agentName != "" {
				argv = append(append(append([]string{}, c.Path...), ctx.agentName), rest...)
			}
		case "session":
			if ctx.sessionID != "" {
				argv = append(append(append([]string{}, c.Path...), ctx.sessionID), rest...)
			}
		}
	}
	if c.flagNamed("session") != nil && !hasFlag(rest, "session") && ctx.sessionID != "" && !hasHelp(rest) &&
		!(len(c.Args) > 0 && c.Args[0].Name == "session") {
		argv = append(argv, "--session", ctx.sessionID)
	}
	return argv, c, nil
}

func (c *Command) flagNamed(name string) *Flag {
	for i := range c.Flags {
		if c.Flags[i].Name == name {
			return &c.Flags[i]
		}
		for _, a := range c.Flags[i].Aliases {
			if a == name {
				return &c.Flags[i]
			}
		}
	}
	return nil
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || strings.HasPrefix(a, "--"+name+"=") {
			return true
		}
	}
	return false
}

func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// runSlash runs the command as a child of this binary, streaming its output
// into the conversation; cancel (Ctrl-C) stops it. It answers the exit code.
func runSlash(scr *screen, argv []string, cancel <-chan struct{}) int {
	self, err := os.Executable()
	if err != nil {
		scr.Print("this binary could not be found to run the command: " + err.Error())
		return 1
	}
	scr.Print("$ ks " + strings.Join(quoteWords(argv), " "))
	cmd := exec.Command(self, append(argv, "--no-input")...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("COLUMNS=%d", appTerminalWidth()))
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		scr.Print("the command could not start: " + err.Error())
		return 1
	}
	var wg sync.WaitGroup
	for _, r := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
			for sc.Scan() {
				scr.Print("  " + sc.Text())
			}
		}(r)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-cancel:
			_ = cmd.Process.Signal(os.Interrupt)
		case <-done:
		}
	}()
	wg.Wait()
	err = cmd.Wait()
	close(done)
	if ee, ok := err.(*exec.ExitError); ok {
		code := ee.ExitCode()
		scr.Print(fmt.Sprintf("  (exit %d)", code))
		return code
	}
	return 0
}

func quoteWords(ws []string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		if strings.ContainsAny(w, " \t\"'") {
			w = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
		out[i] = w
	}
	return out
}

// slashNames is every / name a window accepts, for completion and help.
func slashNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for n := range windowCommands {
		add(n)
	}
	for n := range slashShortcuts {
		add(n)
	}
	for _, c := range registry {
		if c.Group {
			continue
		}
		if _, ex := slashExcluded[c.Name()]; ex {
			continue
		}
		add(c.Name())
	}
	sort.Strings(out)
	return out
}

// completeSlash answers what Tab makes of the input: the one completion, or
// the candidates when several fit (and the longest common beginning).
func completeSlash(input string) (string, []string) {
	if !strings.HasPrefix(input, "/") {
		return input, nil
	}
	typed := strings.TrimPrefix(input, "/")
	var hits []string
	for _, n := range slashNames() {
		if strings.HasPrefix(n, typed) {
			hits = append(hits, n)
		}
	}
	switch len(hits) {
	case 0:
		return input, nil
	case 1:
		return "/" + hits[0] + " ", nil
	}
	common := hits[0]
	for _, h := range hits[1:] {
		for !strings.HasPrefix(h, common) {
			common = common[:len(common)-1]
		}
	}
	return "/" + common, hits
}
