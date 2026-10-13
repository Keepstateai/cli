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
	"encoding/json"
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
	words = commandCase(words)
	if exp, ok := slashShortcuts[words[0]]; ok {
		words = append(append([]string{}, exp...), words[1:]...)
	}
	c, rest, _ := lookup(registry, words)
	if c == nil {
		msg := fmt.Sprintf("/%s is not a ks command.", words[0])
		if near := nearestSlash(strings.Join(words[:min(len(words), 2)], " ")); near != "" {
			msg += " Did you mean /" + near + "?"
		}
		return nil, nil, fmt.Errorf("%s /help lists them", msg)
	}
	if hasHelp(rest) {
		// help is only read: a group lists its commands, and a command the
		// window does not run still explains itself
		return append(append([]string{}, c.Path...), "--help"), c, nil
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
	// filled when the command is missing a required argument: /agent tell
	// "run the tests" names this agent, the instruction being the one given.
	// An agent's name is filled only where it names an EXISTING agent: in
	// /agent create and /check define the first argument is a new name.
	required := 0
	for _, a := range c.Args {
		if a.Required {
			required++
		}
	}
	if positional < required && len(c.Args) > 0 && c.Args[0].Required && !hasHelp(rest) {
		switch c.Args[0].Name {
		case "name", "agent":
			if ctx.agentName != "" && c.Path[0] == "agent" && c.Name() != "agent create" {
				argv = append(append(append([]string{}, c.Path...), ctx.agentName), rest...)
			}
		case "session":
			if ctx.sessionID != "" {
				argv = append(append(append([]string{}, c.Path...), ctx.sessionID), rest...)
			}
		}
	}
	// the window's agent, for a command that names an agent by --agent NAME
	// (task list, result list, adviser connect/list, check define/list); a
	// switch that only happens to be called --agent (ks run --agent) is not one
	// -- and only for this window's session: another session named by --session
	// may have no agent of that name
	if f := c.flagNamed("agent"); f != nil && f.Kind != flagBool && !hasFlag(rest, "agent") && !hasFlag(rest, "session") && ctx.agentName != "" && !hasHelp(rest) {
		argv = append(argv, "--agent", ctx.agentName)
	}
	if c.flagNamed("session") != nil && !hasFlag(rest, "session") && ctx.sessionID != "" && !hasHelp(rest) &&
		!(len(c.Args) > 0 && c.Args[0].Name == "session") {
		argv = append(argv, "--session", ctx.sessionID)
	}
	return argv, c, nil
}

// commandCase lowers the words that name a command (/Stop is /stop, /Agent
// List is /agent list); its arguments keep their case.
func commandCase(words []string) []string {
	out := append([]string{}, words...)
	for n := min(3, len(out)); n > 0; n-- {
		low := append([]string{}, out...)
		for i := 0; i < n; i++ {
			if strings.HasPrefix(low[i], "-") {
				return out
			}
			low[i] = strings.ToLower(low[i])
		}
		if _, ok := slashShortcuts[low[0]]; ok && n == 1 {
			return low
		}
		if windowCommands[low[0]] && n == 1 {
			return low
		}
		if c, _, _ := lookup(registry, low[:n]); c != nil && len(c.Path) == n {
			return low
		}
	}
	return out
}

// nearestSlash is the / name closest to what was typed, when one is close.
func nearestSlash(typed string) string {
	typed = strings.ToLower(typed)
	best, bestD := "", 3
	for _, n := range slashNames() {
		d := editDistance(typed, n)
		if first := strings.Fields(typed); len(first) > 1 {
			if d2 := editDistance(first[0], n); d2 < d {
				d = d2
			}
		}
		if d < bestD || (d == bestD && best != "" && n < best) {
			best, bestD = n, d
		}
	}
	return best
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

// slashConfirm is a confirmation a / command asked for: what it said, and
// the ks command that confirms it.
type slashConfirm struct {
	message string
	next    string
}

// childEnv is the environment a / command runs in: told it runs in the app
// (its refusals read in the app's words), and two columns narrower than the
// terminal, because its lines are shown indented by two.
func childEnv() []string {
	return append(os.Environ(), "KS_IN_APP=1", fmt.Sprintf("COLUMNS=%d", max(20, appTerminalWidth()-2)))
}

// runSlash runs the command as a child of this binary, streaming its output
// into the conversation; cancel (Ctrl-C) stops it. It answers the exit code,
// and the confirmation the command asked for, if it asked for one (the app
// then asks the person itself).
func runSlash(scr *screen, argv []string, cancel <-chan struct{}) (int, *slashConfirm) {
	width := appTerminalWidth()
	self, err := os.Executable()
	if err != nil {
		scr.Print(errorLines("run the command", "this program could not find itself to run it: "+err.Error(), "", width))
		return 1, nil
	}
	scr.Print("\n" + strings.Join(wrapHang(th.accent("$")+" ", "  ", th.accent("ks "+strings.Join(quoteWords(argv), " ")), width), "\n"))
	cmd := exec.Command(self, append(argv, "--no-input")...)
	cmd.Env = childEnv()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		scr.Print(errorLines("start the command", err.Error(), "", width))
		return 1, nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var confirm *slashConfirm
	explained := false // the command said what went wrong itself
	for _, r := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
			for sc.Scan() {
				l := sc.Text()
				if strings.HasPrefix(l, appConfirmMarker) {
					var m struct{ Message, Next string }
					if json.Unmarshal([]byte(strings.TrimPrefix(l, appConfirmMarker)), &m) == nil {
						mu.Lock()
						confirm = &slashConfirm{message: m.Message, next: m.Next}
						mu.Unlock()
					}
					continue
				}
				if strings.HasPrefix(l, iconFailed+" ") || strings.HasPrefix(l, "error:") {
					mu.Lock()
					explained = true
					mu.Unlock()
				}
				scr.Print(slashOutputLine("  " + l))
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
		if confirm == nil && !explained {
			scr.Print(notice(iconFailed, toneBad, th.bad(fmt.Sprintf("The command stopped without finishing (exit code %d).", code)), width))
		}
		return code, confirm
	}
	return 0, nil
}

// runSlashJSON runs a command quietly for its JSON answer, for a step the app
// shows in its own words (an agent stop, a deletion). The answer is the
// command's own: data, or the error it refused with.
func runSlashJSON(argv []string) (json.RawMessage, *cliError) {
	self, err := os.Executable()
	if err != nil {
		return nil, &cliError{Message: "this program could not find itself to run the command: " + err.Error()}
	}
	cmd := exec.Command(self, append(argv, "--json", "--no-input", "--quiet")...)
	cmd.Env = childEnv()
	outb, _ := cmd.Output()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *cliError       `json:"error"`
	}
	if json.Unmarshal(outb, &env) != nil {
		return nil, &cliError{Message: "the command gave no answer that could be read", WorkStarted: workUnknown}
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Data, nil
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

// windowHelp explains each command the window handles itself.
var windowHelp = map[string]string{
	"help":   "/help lists what this window does. /help <command> shows a ks command's options, as /<command> --help does",
	"status": "/status shows the agent's full state: its activity, its current instruction, its queue and who holds control",
	"home":   "/home goes back to your agents. This agent keeps working",
	"back":   "/back goes back to your agents. This agent keeps working",
	"quit":   "/quit leaves the app. Every agent keeps working",
	"exit":   "/exit leaves the app. Every agent keeps working",
	"take":   "/take takes control of this agent from another window that holds it",
	"switch": "/switch <agent> [session] opens another of your agents without going home. /agents lists this session's",
}

func windowCommandHelp(name string) string {
	if h, ok := windowHelp[name]; ok {
		return h
	}
	return "/" + name + " is handled by this window; /help lists what it does"
}

// looksLikeKsCommand reports whether an instruction is really a ks command
// typed into the agent's input ("ks agent open"), and the / command that runs
// it here. Only a line whose first word is ks and whose next words name a
// real command counts; prose that starts with "ks" is an instruction.
func looksLikeKsCommand(text string) (string, bool) {
	words, err := splitWords(strings.TrimSpace(text))
	if err != nil || len(words) < 2 || words[0] != "ks" || strings.Contains(text, "\n") {
		return "", false
	}
	if c, _, _ := lookup(registry, words[1:]); c == nil {
		return "", false
	}
	return "/" + strings.Join(quoteWords(words[1:]), " "), true
}

// slashOutputLine colours a / command's output line by what it is: an error
// or refusal red, the next step to take amber, the rest as printed.
func slashOutputLine(l string) string {
	t := strings.TrimSpace(l)
	switch {
	case strings.HasPrefix(t, "error:") || strings.HasPrefix(t, "refused:") || strings.HasPrefix(t, iconFailed+" "):
		return th.bad(l)
	case strings.HasPrefix(t, "Next:"):
		return th.warn(l)
	case strings.HasPrefix(t, "Remote work started:"):
		return th.dim(l)
	}
	return l
}

// slashSuggestions is what the window lists under the input while a / command
// is being typed: each command that fits, with what it does, the first (the
// one Tab completes toward) in the accent; at most max rows.
func slashSuggestions(input string, limit int) []string {
	if !strings.HasPrefix(input, "/") || strings.Contains(input, "\n") {
		return nil
	}
	typed := strings.TrimPrefix(input, "/")
	var fits []string
	for _, n := range slashNames() {
		if strings.HasPrefix(n, typed) && n != typed {
			fits = append(fits, n)
		}
	}
	if len(fits) == 0 {
		return nil
	}
	var rows []string
	for i, n := range fits {
		if i == limit {
			rows = append(rows, th.dim(fmt.Sprintf("… %d more (Tab completes)", len(fits)-limit)))
			break
		}
		name := fmt.Sprintf("/%-24s", n)
		if i == 0 {
			name = th.accent(name)
		}
		rows = append(rows, name+" "+th.dim(slashSummary(n)))
	}
	return rows
}

// slashSummary is one line on what a / name does.
func slashSummary(n string) string {
	if h, ok := windowHelp[n]; ok {
		return strings.TrimSpace(strings.TrimPrefix(h, "/"+n))
	}
	words := strings.Fields(n)
	if exp, ok := slashShortcuts[n]; ok {
		words = exp
	}
	if c, _, _ := lookup(registry, words); c != nil {
		return c.Summary
	}
	return ""
}
