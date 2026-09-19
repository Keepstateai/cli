// command.go: the one typed command schema (KS-002). Every verb the binary
// dispatches is a row here: its path (ks <resource> <action>), its aliases,
// its positional arguments, its flags with their types, and its handler. The
// parser below is the only place an argument is read, so a malformed command
// is refused before login, before any request, before any local write, and
// before anything billable: exit 2, the offending token, and at most one
// correction suggestion, which is never applied.
//
// The registry is also what commands.json is checked against (both
// directions, manifest_test.go), what the global usage is rendered from, and
// what KS-003 renders per-command help from and KS-008 completes from. One
// schema, several readers, so they cannot disagree.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type flagKind int

const (
	flagBool   flagKind = iota
	flagString          // any text; a value is required
	flagInt             // a whole number in decimal, exactly: no sign, no exponent, no suffix, no overflow
	flagUSD             // a dollar amount parsed exactly into micro-USD (parseUSD)
	flagList            // a repeatable text flag; also consumes following non-flag tokens when Variadic
)

// Flag is one option of a command. Name is canonical and dash-free
// ("budget-tokens"); Aliases are older spellings that map onto it
// ("budget"); Short is a one-letter form ("n"). Positive makes an integer
// refuse zero and negative values. Omitted and supplied are distinct: an
// invocation reports whether a flag was set at all.
type Flag struct {
	Name     string
	Aliases  []string
	Short    string
	Kind     flagKind
	Value    string // the placeholder shown in usage: N, URL, FILE
	Summary  string
	Positive bool
	Variadic bool
}

// Arg is one positional argument.
type Arg struct {
	Name     string
	Required bool
}

// Command is one row of the schema.
type Command struct {
	Path    []string   // {"run"} or {"cruise", "init"}
	Aliases [][]string // {{"save"}} for checkpoint; each alias is a full path
	Summary string
	Args    []Arg
	Rest    string // when set, every token after the positionals is literal and belongs to the named thing ("command")
	Flags   []Flag
	Surface string // client | hosted
	Nothing string // the closing line of a refusal: what did NOT happen ("No session was started.")
	Group   bool   // a head with subcommands and no handler of its own
	Run     func(inv *Invocation)
}

func (c *Command) Name() string { return strings.Join(c.Path, " ") }

// Usage renders the command's synopsis from the schema, so the line the
// docs page shows is the line the parser enforces.
func (c *Command) Usage() string {
	var b strings.Builder
	b.WriteString("ks " + c.Name())
	for _, a := range c.Args {
		if a.Required {
			b.WriteString(" <" + a.Name + ">")
		} else {
			b.WriteString(" [" + a.Name + "]")
		}
	}
	for _, f := range c.Flags {
		b.WriteString(" [" + f.render() + "]")
	}
	if c.Rest != "" {
		b.WriteString(" [--] <" + c.Rest + "...>")
	}
	return b.String()
}

func (f Flag) render() string {
	name := "--" + f.Name
	if f.Short != "" {
		name = "-" + f.Short
	}
	switch f.Kind {
	case flagBool:
		return name
	case flagList:
		if f.Variadic {
			return name + " " + f.Value + "..."
		}
		return name + " " + f.Value
	}
	return name + " " + f.Value
}

// Invocation is a parsed command line: the command, its positionals, the
// literal rest, and each flag's typed value with whether it was supplied.
type Invocation struct {
	Cmd   *Command
	Args  []string
	Rest  []string
	set   map[string]bool
	strs  map[string]string
	ints  map[string]int64
	lists map[string][]string
	Help  bool
}

func (inv *Invocation) Set(name string) bool      { return inv.set[name] }
func (inv *Invocation) Bool(name string) bool     { return inv.set[name] }
func (inv *Invocation) Str(name string) string    { return inv.strs[name] }
func (inv *Invocation) Int(name string) int64     { return inv.ints[name] }
func (inv *Invocation) List(name string) []string { return inv.lists[name] }

// Arg returns the i-th positional or "" when it was not given.
func (inv *Invocation) Arg(i int) string {
	if i < len(inv.Args) {
		return inv.Args[i]
	}
	return ""
}

// UsageError is a refusal of the command line itself. Exit 2, always.
type UsageError struct {
	Cmd        *Command
	Message    string
	Suggestion string // "Did you mean --budget-tokens?"; at most one, never applied
}

func (e *UsageError) Error() string { return e.Message }

// print writes the refusal in the fixed shape of the error copy contract:
// what was wrong, one suggestion, what did not happen.
func (e *UsageError) print() {
	fmt.Fprintln(os.Stderr, e.Message)
	if e.Suggestion != "" {
		fmt.Fprintln(os.Stderr, e.Suggestion)
	}
	if e.Cmd != nil && e.Cmd.Nothing != "" {
		fmt.Fprintln(os.Stderr, e.Cmd.Nothing)
	} else {
		fmt.Fprintln(os.Stderr, "Nothing was done.")
	}
	if e.Cmd != nil {
		fmt.Fprintf(os.Stderr, "usage: %s\n", e.Cmd.Usage())
	}
}

// ---------------------------------------------------------------------
// lookup
// ---------------------------------------------------------------------

// lookup finds the longest command path that prefixes args, following
// aliases. It returns the command and the remaining tokens, or nil and a
// suggestion when no command matches.
func lookup(reg []*Command, args []string) (*Command, []string, string) {
	var best *Command
	bestLen := 0
	for _, c := range reg {
		paths := append([][]string{c.Path}, c.Aliases...)
		for _, p := range paths {
			if len(p) > len(args) || len(p) <= bestLen {
				continue
			}
			match := true
			for i, w := range p {
				if args[i] != w {
					match = false
					break
				}
			}
			if match {
				best, bestLen = c, len(p)
			}
		}
	}
	if best == nil {
		var names []string
		for _, c := range reg {
			if !c.Group {
				names = append(names, c.Name())
			}
		}
		return nil, args, suggest(strings.Join(args[:min(len(args), 2)], " "), names)
	}
	return best, args[bestLen:], ""
}

// ---------------------------------------------------------------------
// parse
// ---------------------------------------------------------------------

func (c *Command) findFlag(name string) *Flag {
	for i := range c.Flags {
		f := &c.Flags[i]
		if f.Name == name {
			return f
		}
		for _, a := range f.Aliases {
			if a == name {
				return f
			}
		}
	}
	return nil
}

func (c *Command) findShort(letter string) *Flag {
	for i := range c.Flags {
		if c.Flags[i].Short == letter {
			return &c.Flags[i]
		}
	}
	return nil
}

func (c *Command) flagNames() []string {
	var out []string
	for _, f := range c.Flags {
		out = append(out, "--"+f.Name)
		for _, a := range f.Aliases {
			out = append(out, "--"+a)
		}
	}
	return out
}

// suggestFlag names the closest option, spelled canonically: a typo of a
// legacy alias is answered with the flag it maps onto, so the suggestion
// teaches the current spelling rather than the old one.
func (c *Command) suggestFlag(got string) string {
	canonical := map[string]string{}
	for _, f := range c.Flags {
		canonical["--"+f.Name] = "--" + f.Name
		for _, a := range f.Aliases {
			canonical["--"+a] = "--" + f.Name
		}
	}
	s := suggest(got, c.flagNames())
	if s == "" {
		return ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(s, "Did you mean "), "?")
	return "Did you mean " + canonical[name] + "?"
}

func isHelpWord(t string) bool { return t == "--help" || t == "-h" || t == "help" }

// looksLikeFlag reports whether a token is an option rather than a value:
// it starts with a dash and is not a negative number. A negative number is
// taken as the value it was meant to be, so that "--budget-tokens -5" is
// refused for being negative rather than for missing a value.
func looksLikeFlag(t string) bool {
	if !strings.HasPrefix(t, "-") || t == "-" {
		return false
	}
	if len(t) > 1 && t[1] >= '0' && t[1] <= '9' {
		return false
	}
	return true
}

// parse reads the tokens after the command path. The option region ends at
// "--" or, for a command with Rest, after its positionals; help words are
// help only inside the option region.
func (c *Command) parse(tokens []string) (*Invocation, *UsageError) {
	inv := &Invocation{Cmd: c, set: map[string]bool{}, strs: map[string]string{}, ints: map[string]int64{}, lists: map[string][]string{}}
	fail := func(msg, sugg string) (*Invocation, *UsageError) {
		return nil, &UsageError{Cmd: c, Message: msg, Suggestion: sugg}
	}
	var positional []string
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		// the rest: literal, untouched
		if c.Rest != "" && len(positional) == len(c.Args) {
			if t == "--" {
				i++
			}
			inv.Rest = append(inv.Rest, tokens[i:]...)
			break
		}
		if t == "--" {
			positional = append(positional, tokens[i+1:]...)
			break
		}
		if isHelpWord(t) {
			inv.Help = true
			return inv, nil
		}
		if looksLikeFlag(t) {
			name, value, hasValue := t, "", false
			if strings.HasPrefix(t, "--") {
				name = strings.TrimPrefix(t, "--")
				if eq := strings.IndexByte(name, '='); eq >= 0 {
					name, value, hasValue = name[:eq], name[eq+1:], true
				}
			} else {
				name = strings.TrimPrefix(t, "-")
				if len(name) != 1 {
					return fail(fmt.Sprintf("Unknown option: %s", t), c.suggestFlag(t))
				}
				f := c.findShort(name)
				if f == nil {
					return fail(fmt.Sprintf("Unknown option: %s", t), c.suggestFlag(t))
				}
				name = f.Name
			}
			f := c.findFlag(name)
			if f == nil {
				return fail(fmt.Sprintf("Unknown option: --%s", name), c.suggestFlag("--"+name))
			}
			if inv.set[f.Name] && f.Kind != flagList {
				return fail(fmt.Sprintf("Option --%s was given more than once.", f.Name), "")
			}
			switch f.Kind {
			case flagBool:
				if hasValue {
					return fail(fmt.Sprintf("Option --%s takes no value.", f.Name), "")
				}
				inv.set[f.Name] = true
				i++
				continue
			}
			// a value is required
			if !hasValue {
				if i+1 >= len(tokens) || looksLikeFlag(tokens[i+1]) || tokens[i+1] == "--" {
					return fail(fmt.Sprintf("Option --%s requires a value (%s).", f.Name, f.Value), "")
				}
				value = tokens[i+1]
				i++
			}
			i++
			if err := inv.setValue(f, value); err != nil {
				return fail(err.Error(), "")
			}
			if f.Kind == flagList && f.Variadic {
				for i < len(tokens) && !looksLikeFlag(tokens[i]) && tokens[i] != "--" && !isHelpWord(tokens[i]) {
					inv.lists[f.Name] = append(inv.lists[f.Name], tokens[i])
					i++
				}
			}
			continue
		}
		positional = append(positional, t)
		i++
	}
	if len(positional) > len(c.Args) && c.Rest == "" {
		extra := positional[len(c.Args)]
		sugg := ""
		if strings.HasPrefix(extra, "-") {
			sugg = c.suggestFlag(extra)
		}
		return fail(fmt.Sprintf("Unexpected argument: %q.", extra), sugg)
	}
	for k, a := range c.Args {
		if a.Required && k >= len(positional) {
			return fail(fmt.Sprintf("Missing argument: <%s>.", a.Name), "")
		}
	}
	if c.Rest != "" && len(inv.Rest) == 0 {
		return fail(fmt.Sprintf("Missing argument: <%s...>.", c.Rest), "")
	}
	inv.Args = positional
	return inv, nil
}

func (inv *Invocation) setValue(f *Flag, value string) error {
	inv.set[f.Name] = true
	switch f.Kind {
	case flagString:
		inv.strs[f.Name] = value
	case flagList:
		inv.lists[f.Name] = append(inv.lists[f.Name], value)
	case flagInt:
		n, err := parseWhole(value)
		if err != nil {
			return fmt.Errorf("Option --%s: %v.", f.Name, err)
		}
		if f.Positive && n <= 0 {
			return fmt.Errorf("Option --%s must be above zero, got %d.", f.Name, n)
		}
		inv.ints[f.Name] = n
	case flagUSD:
		micro, err := parseUSD(value)
		if err != nil {
			return fmt.Errorf("Option --%s: %v.", f.Name, err)
		}
		inv.ints[f.Name] = micro
	}
	return nil
}

// parseWhole reads a whole number written in decimal and nothing else. A
// sign, an exponent, a suffix, a separator, whitespace or a value outside
// int64 is refused with its reason; nothing is truncated or rounded.
func parseWhole(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("a whole number is required")
	}
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return 0, fmt.Errorf("%q is not a whole number", s)
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a whole number (digits only, no sign, exponent or suffix)", s)
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is too large for a whole number", s)
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ---------------------------------------------------------------------
// suggestions: one, close, never applied
// ---------------------------------------------------------------------

func suggest(got string, candidates []string) string {
	best, bestD := "", 4
	for _, c := range candidates {
		d := editDistance(strings.ToLower(got), strings.ToLower(c))
		if d < bestD || (d == bestD && best != "" && c < best) {
			best, bestD = c, d
		}
	}
	if best == "" {
		return ""
	}
	return "Did you mean " + best + "?"
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(min(prev[j]+1, cur[j-1]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// ---------------------------------------------------------------------
// help rendering from the schema
// ---------------------------------------------------------------------

// commandHelp prints one command's purpose, arguments and options, from
// the schema and nothing else. KS-003 extends it with effects and examples.
func commandHelp(c *Command) {
	fmt.Printf("ks %s: %s\n\nusage:\n  %s\n", c.Name(), c.Summary, c.Usage())
	if len(c.Aliases) > 0 {
		var a []string
		for _, p := range c.Aliases {
			a = append(a, "ks "+strings.Join(p, " "))
		}
		fmt.Printf("\nalso: %s\n", strings.Join(a, ", "))
	}
	if len(c.Args) > 0 || c.Rest != "" {
		fmt.Println("\narguments:")
		for _, a := range c.Args {
			req := "optional"
			if a.Required {
				req = "required"
			}
			fmt.Printf("  %-22s %s\n", a.Name, req)
		}
		if c.Rest != "" {
			fmt.Printf("  %-22s literal; everything after it belongs to it, --help included\n", c.Rest+"...")
		}
	}
	if len(c.Flags) > 0 {
		fmt.Println("\noptions:")
		for _, f := range c.Flags {
			name := f.render()
			if len(f.Aliases) > 0 {
				name += " (also --" + strings.Join(f.Aliases, ", --") + ")"
			}
			fmt.Printf("  %-34s %s\n", name, f.Summary)
		}
	}
	fmt.Println("\nHelp makes no request and changes nothing.")
}

// registryUsage renders the global usage from the schema, in registry
// order, so a verb the binary has is a verb the usage shows.
func registryUsage(reg []*Command) string {
	var b strings.Builder
	b.WriteString("ks — durable agent sessions on KeepState\n\nusage:\n")
	for _, c := range reg {
		if c.Group {
			continue
		}
		line := c.Usage()
		if len(line) > 44 {
			fmt.Fprintf(&b, "  %s\n  %-44s %s\n", line, "", c.Summary)
			continue
		}
		fmt.Fprintf(&b, "  %-44s %s\n", line, c.Summary)
	}
	b.WriteString(`
Sessions survive kills: checkpoint, wake, and the agent resumes exactly
where it stopped — files, memory, and running processes intact.
Every --help makes no request and changes nothing.
`)
	return b.String()
}

// sortedNames is used by tests and completion: every command name and
// alias the registry knows, sorted.
func sortedNames(reg []*Command) []string {
	var out []string
	for _, c := range reg {
		if c.Group {
			continue
		}
		out = append(out, c.Name())
		for _, a := range c.Aliases {
			out = append(out, strings.Join(a, " "))
		}
	}
	sort.Strings(out)
	return out
}
