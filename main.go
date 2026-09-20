// ks — the KeepState thin client. A key to a building only we operate: every
// verb terminates at the KeepState control plane over HTTPS. No engine code
// lives here. MIT licensed; built in public CI; releases carry SHA256SUMS and
// provenance attestations, and `ks update` verifies before trusting.
package main

import (
	"fmt"
	"os"
)

// version is stamped by CI: -ldflags "-X main.version=v0.1.0"
var version = "dev"

// registry is the schema (command.go) with its handlers: the one list the
// parser dispatches from, the usage renders from, commands.json is checked
// against, and help and completion are generated from.
var registry = []*Command{
	{Path: []string{"login"}, Summary: "sign in (browser device flow)", Surface: "client",
		Effects:  "stores a device token for this machine in your config directory (0600); nothing is billed",
		Examples: []string{"ks login"},
		Flags:    []Flag{{Name: "ctl", Kind: flagString, Value: "URL", Summary: "the control plane to sign in to; default https://ctl.keepstate.ai"}},
		Nothing:  "No sign-in was started.", Run: func(inv *Invocation) {
			if err := runLogin(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"run"}, Summary: "start a hosted session", Surface: "hosted",
		Effects:  "starts a session on the fleet: session time is metered from this moment until the session is killed or parked",
		Examples: []string{"ks run", "ks run --budget-tokens 750000"},
		Flags: []Flag{
			{Name: "image", Kind: flagString, Value: "I", Summary: "the agent image; default your onboarding choice"},
			{Name: "budget-tokens", Aliases: []string{"budget"}, Kind: flagInt, Positive: true, Value: "N", Summary: "the session's token budget; default per tier (500,000 free, 2,000,000 paid); zero and negative are refused"},
		},
		Nothing: "No session was started.", Run: hosted(hostedRun)},
	{Path: []string{"checkpoint"}, Aliases: [][]string{{"save"}}, Summary: "save the session (durably); --stop parks it", Surface: "hosted",
		Effects:  "writes a checkpoint: storage is metered from this point; with --stop the session parks and session time stops",
		Examples: []string{"ks checkpoint <session>", "ks checkpoint <session> --stop"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "stop", Kind: flagBool, Summary: "park the session after the save succeeds"}},
		Nothing:  "No checkpoint was taken.", Run: hosted(hostedCheckpoint)},
	{Path: []string{"wake"}, Aliases: [][]string{{"resume"}}, Summary: "resume a parked session (mid-sentence)", Surface: "hosted",
		Effects:  "restores the last checkpoint on the fleet: session time is metered again",
		Examples: []string{"ks wake <session>"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was resumed.", Run: hosted(hostedWake)},
	{Path: []string{"kill"}, Summary: "destroy a session (guarded: checkpoint first)", Surface: "hosted",
		Effects:  "destroys the running machine; refused without a saved checkpoint unless --force; session time stops",
		Examples: []string{"ks kill <session>", "ks kill <session> --force"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "force", Kind: flagBool, Summary: "destroy it even without a saved checkpoint"}},
		Nothing:  "Nothing was destroyed.", Run: hosted(hostedKill)},
	{Path: []string{"session"}, Group: true, Summary: "the session inventory", Surface: "hosted"},
	{Path: []string{"session", "list"}, Aliases: [][]string{{"ls"}}, Summary: "your sessions, newest activity first: state, agent, task, last activity, last save", Surface: "hosted",
		Flags:    []Flag{{Name: "state", Kind: flagString, Value: "STATE", Summary: "only running, parked or dead"}, {Name: "all", Kind: flagBool, Summary: "include dead sessions"}},
		Needs:    "session.list",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks session list", "ks ls --state running", "ks session list --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionList)},
	{Path: []string{"session", "show"}, Summary: "one session in full; a short id is accepted when it is unique among yours", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Needs:    "session.list",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks session show 3f2a1c", "ks session show <session> --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionShow)},
	{Path: []string{"session", "key", "set"}, Summary: "bind a session's provider to one of your keys by id; that key is used or nothing is", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "key", Kind: flagString, Value: "KEY", Summary: "the key's id or alias"}},
		Needs:    "keys.inventory",
		Effects:  "changes the session's key binding at its next safe boundary; no key is substituted",
		Examples: []string{"ks session key set 3f2a1c --key vlt_0123abcd"},
		Nothing:  "Nothing was bound.", Run: hosted(hostedSessionKeySet)},
	{Path: []string{"key"}, Group: true, Summary: "your provider keys: never the secret", Surface: "hosted"},
	{Path: []string{"key", "list"}, Summary: "your keys: id, provider, alias, last four characters, enabled or disabled", Surface: "hosted",
		Needs:    "keys.inventory",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks key list", "ks key list --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedKeyList)},
	{Path: []string{"key", "show"}, Summary: "one key's metadata", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks key show vlt_0123abcd"},
		Nothing:  "Nothing was read.", Run: hosted(hostedKeyShow)},
	{Path: []string{"key", "add"}, Summary: "store a provider key read from standard input; it is never an argument and never comes back", Surface: "hosted",
		Flags:    []Flag{{Name: "provider", Kind: flagString, Value: "PROVIDER", Summary: "anthropic, openai or openrouter"}, {Name: "alias", Kind: flagString, Value: "NAME", Summary: "a name for the key"}},
		Needs:    "keys.inventory",
		Effects:  "stores the key on the service, sealed; a key for a provider you already hold is rotated in place and keeps its id",
		Examples: []string{"ks key add --provider anthropic --alias prod < key.txt"},
		Nothing:  "Nothing was stored.", Run: hosted(hostedKeyAdd)},
	{Path: []string{"key", "enable"}, Summary: "allow new admissions with a key", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "changes the key's standing",
		Examples: []string{"ks key enable vlt_0123abcd"},
		Nothing:  "Nothing changed.", Run: hosted(hostedKeySetEnabled(true))},
	{Path: []string{"key", "disable"}, Summary: "refuse new admissions with a key; in-flight usage is reconciled", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "changes the key's standing; sessions bound to it admit nothing until rebound",
		Examples: []string{"ks key disable vlt_0123abcd"},
		Nothing:  "Nothing changed.", Run: hosted(hostedKeySetEnabled(false))},
	{Path: []string{"key", "delete"}, Summary: "plan a key deletion, then execute it with --yes", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "without --yes: a plan, nothing deleted; with --yes (the global flag): the secret is destroyed and the bindings that named it are dropped",
		Examples: []string{"ks key delete vlt_0123abcd", "ks key delete vlt_0123abcd --yes"},
		Nothing:  "Nothing was deleted.", Run: hosted(hostedKeyDelete)},
	{Path: []string{"preflight"}, Summary: "what a run needs and what is missing: identity, keys, credit, capabilities, and the local upload preview; starts nothing", Surface: "hosted",
		Flags:    []Flag{{Name: "provider", Kind: flagString, Value: "PROVIDER", Summary: "the provider the run will use (default anthropic)"}},
		Needs:    "preflight",
		Effects:  "reads the service's facts and the workspace selection; no provisioning, no model call",
		Examples: []string{"ks preflight", "ks preflight --json"},
		Nothing:  "Nothing was started.", Run: hosted(hostedPreflight)},
	{Path: []string{"meter"}, Summary: "spend, budget, key sources", Surface: "hosted",
		Effects:  "reads the meter; nothing changes",
		Examples: []string{"ks meter <session>", "ks meter <session> --json"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was read.", Run: hosted(hostedMeter)},
	{Path: []string{"exec"}, Summary: "run one command in the session", Surface: "hosted",
		Flags:    []Flag{{Name: "shell", Kind: flagBool, Summary: "send the one argument verbatim for the session's shell to interpret (pipes, globs, variables); without it every argument reaches the program exactly as typed"}},
		Effects:  "runs the command inside the session's guest as root; whatever it does, it does",
		Examples: []string{"ks exec <session> -- python --version", "ks exec <session> -- printf %s 'a b'", "ks exec --shell <session> 'ls | wc -l'"},
		Args:     []Arg{{Name: "session", Required: true}}, Rest: "command",
		Nothing: "No command was sent.", Run: hosted(hostedExec)},
	{Path: []string{"attach"}, Summary: "interactive terminal into a running session (tmux)", Surface: "hosted",
		Effects:  "opens a terminal into the guest tmux; closing it changes nothing in the session",
		Examples: []string{"ks attach <session>"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was attached.", Run: hosted(hostedAttachCmd)},
	{Path: []string{"fork"}, Summary: "branch a checkpoint into diverging children", Surface: "hosted",
		Effects:  "starts N more running sessions from the checkpoint (paid plans): each child is metered like a session",
		Examples: []string{"ks fork <session> -n 2"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "children", Short: "n", Kind: flagInt, Positive: true, Value: "N", Summary: "how many children to branch; default 1"},
			{Name: "steer", Kind: flagString, Value: "FILE", Summary: "a steer file applied to every child"},
		},
		Nothing: "Nothing was forked.", Run: hosted(hostedFork)},
	{Path: []string{"cruise"}, Group: true, Summary: "accepted work on the fleet", Surface: "hosted"},
	{Path: []string{"cruise", "init"}, Summary: "draft the acceptance manifest from the repository into .keepstate/cruise.json", Surface: "client",
		Effects:  "writes .keepstate/cruise.json and .keepstate/verifier.json in the current directory; makes no request and calls no model",
		Examples: []string{"ks cruise init --goal \"make the inventory tests pass\"", "ks cruise init --tests \"make check\" --spend 1.50"},
		Flags: []Flag{{Name: "allow", Kind: flagList, Value: "PATH", Variadic: true, Summary: "review a sensitive or ignored path into the upload (repeatable); see ks cruise preview"},
			{Name: "goal", Kind: flagString, Value: "TEXT", Summary: "the goal the agent is given; without it, the draft's previous goal, else \"make the check pass: CMD\""},
			{Name: "tests", Kind: flagString, Value: "CMD", Summary: "the command that decides done, when init should not detect it (pytest, npm test, go test)"},
			{Name: "paths", Kind: flagList, Variadic: true, Value: "GLOB", Summary: "the paths an attempt may change; default every tracked file except the tests and .keepstate"},
			{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs in order; default the model table's default ladder"},
			{Name: "spend", Kind: flagUSD, Value: "USD", Summary: "the spend ceiling in dollars; default 2"},
		},
		Nothing: "No draft was written.", Run: cruiseInit},
	{Path: []string{"cruise", "preview"}, Aliases: [][]string{{"workspace", "preview"}}, Summary: "what an upload would carry and what it would not, with reasons: names and sizes, never content", Surface: "client",
		Flags:    []Flag{{Name: "allow", Kind: flagList, Value: "PATH", Variadic: true, Summary: "review a sensitive or ignored path into the upload (repeatable); never a symlink or .git"}},
		Effects:  "reads the workspace; no request, no file written",
		Examples: []string{"ks cruise preview", "ks cruise preview --json", "ks cruise preview --allow config/.env.production"},
		Nothing:  "Nothing was uploaded or written.", Run: cruisePreview},
	{Path: []string{"cruise", "approve"}, Summary: "lock the draft: its digest and the time into .keepstate/cruise.lock", Surface: "client",
		Effects:  "writes .keepstate/cruise.lock; makes no request",
		Examples: []string{"ks cruise approve"},
		Nothing:  "Nothing was approved.", Run: cruiseApprove},
	{Path: []string{"cruise", "run"}, Summary: "upload the workspace and the locked manifest; start the job", Surface: "hosted",
		Effects:  "uploads the packed workspace and starts a job that bills as the sessions it runs, up to the approved spend ceiling",
		Examples: []string{"ks cruise run"},
		Nothing:  "No job was started.", Run: func(inv *Invocation) {
			if err := cruiseRun(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "status"}, Summary: "the job (or the newest): state, rung, each attempt, save points, spend, verdict, artifact", Surface: "hosted",
		Effects:  "reads the job; nothing changes",
		Examples: []string{"ks cruise status", "ks cruise status <job>"},
		Args:     []Arg{{Name: "job", Required: false}},
		Nothing:  "Nothing was read.", Run: cruiseStatus},
	{Path: []string{"cruise", "logs"}, Summary: "the event stream, one event per line: seq, ts, type, detail", Surface: "hosted",
		Effects:  "reads the events; nothing changes",
		Examples: []string{"ks cruise logs <job>"},
		Args:     []Arg{{Name: "job", Required: true}},
		Nothing:  "Nothing was read.", Run: cruiseLogs},
	{Path: []string{"cruise", "cancel"}, Summary: "cancel a job", Surface: "hosted",
		Effects:  "stops the job and releases its parent session; spend already incurred stands",
		Examples: []string{"ks cruise cancel <job>"},
		Args:     []Arg{{Name: "job", Required: true}},
		Nothing:  "Nothing was cancelled.", Run: cruiseCancel},
	{Path: []string{"cruise", "resume"}, Summary: "resume a job from review, on the same or a wider ladder", Surface: "hosted",
		Effects:  "starts new attempts from the parked parent, billed like the first ones; refused outside review",
		Examples: []string{"ks cruise resume <job> --ladder anthropic:claude-sonnet-5"},
		Args:     []Arg{{Name: "job", Required: true}},
		Flags:    []Flag{{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs to continue on, in order"}},
		Nothing:  "Nothing was resumed.", Run: cruiseResume},
	{Path: []string{"cruise", "artifact"}, Summary: "download the accepted tarball", Surface: "hosted",
		Effects:  "writes one file locally after its sha256 matches the job record; never extracts or runs it",
		Examples: []string{"ks cruise artifact <job> --out result.tar.gz"},
		Args:     []Arg{{Name: "job", Required: true}},
		Flags:    []Flag{{Name: "out", Kind: flagString, Value: "FILE", Summary: "where to write it; default JOB.tar.gz"}},
		Nothing:  "Nothing was written.", Run: func(inv *Invocation) {
			if err := cruiseArtifact(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "models"}, Summary: "the model table the control plane serves: families, rungs, default ladder", Surface: "hosted",
		Effects:  "reads the table; nothing changes",
		Examples: []string{"ks cruise models"},
		Nothing:  "Nothing was read.", Run: cruiseModels},
	{Path: []string{"operation"}, Group: true, Summary: "operation records", Surface: "hosted"},
	{Path: []string{"operation", "show"}, Summary: "read an operation back by its id or key: state, result, timestamps", Surface: "hosted",
		Args:     []Arg{{Name: "operation", Required: true}},
		Needs:    "operations.idempotent",
		Effects:  "reads the record; nothing changes",
		Examples: []string{"ks operation show ksop_0123456789abcdef0123456789abcdef"},
		Nothing:  "Nothing was read.", Run: hosted(hostedOperationShow)},
	{Path: []string{"operation", "wait"}, Summary: "wait, within the bound, for an operation to finish and print its result", Surface: "hosted",
		Args:     []Arg{{Name: "operation", Required: true}},
		Needs:    "operations.idempotent",
		Effects:  "reads the record until it finishes or the wait bound passes; nothing changes; Ctrl-C stops the local waiting only",
		Examples: []string{"ks operation wait ksop_0123456789abcdef0123456789abcdef"},
		Nothing:  "Nothing was read.", Run: hosted(hostedOperationWait)},
	{Path: []string{"completion"}, Summary: "print a shell completion script generated from the command schema", Surface: "client",
		Args:     []Arg{{Name: "shell", Required: true}},
		Effects:  "prints the script; installs nothing and edits no shell file (the script's first lines say where to put it)",
		Examples: []string{"ks completion bash", "ks completion zsh > \"${fpath[1]}/_ks\"", "ks completion fish > ~/.config/fish/completions/ks.fish"},
		Nothing:  "Nothing was printed.", Run: func(inv *Invocation) {
			script, err := completionScript(inv.Arg(0))
			if err != nil {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: err.Error()})
			}
			fmt.Print(script)
		}},
	{Path: []string{"reference"}, Summary: "print the command reference as a Markdown table generated from the command schema", Surface: "client",
		Effects:  "prints the table; nothing changes",
		Examples: []string{"ks reference"},
		Nothing:  "Nothing was printed.", Run: func(*Invocation) { fmt.Print(referenceTable(reg)) }},
	{Path: []string{"doctor"}, Summary: "connectivity, token, version", Surface: "client",
		Effects:  "reads: the control plane health, your token, the latest release; nothing changes",
		Examples: []string{"ks doctor"},
		Run:      func(*Invocation) { os.Exit(runDoctor()) }},
	{Path: []string{"update"}, Summary: "self-update (checksum-verified)", Surface: "client",
		Effects:  "replaces this binary with the verified release; a checksum mismatch replaces nothing",
		Examples: []string{"ks update"},
		Nothing:  "Nothing was updated.", Run: func(*Invocation) {
			if err := runUpdate(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"logout"}, Summary: "remove the stored token", Surface: "client",
		Effects:  "deletes the stored token on this machine; revoke it server-side from your account page",
		Examples: []string{"ks logout"},
		Run: func(*Invocation) {
			if err := runLogout(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"uninstall"}, Summary: "how to remove ks completely", Surface: "client",
		Effects:  "prints the removal command; removes nothing itself",
		Examples: []string{"ks uninstall"},
		Run:      func(*Invocation) { runUninstall() }},
	{Path: []string{"version"}, Summary: "print the version", Surface: "client",
		Effects:  "nothing",
		Examples: []string{"ks version"},
		Run:      func(*Invocation) { emit(map[string]any{"version": version}, func() { fmt.Println("ks", version) }) }},
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "--version", "-v":
		fmt.Println("ks", version)
		return
	case "--help", "-h", "help":
		// ks help <command>: that command's help; a group's usage for a group
		if len(args) > 1 {
			if c, _, _ := lookup(registry, args[1:]); c != nil {
				if c.Group {
					groupUsage(c)
				} else {
					commandHelp(c)
				}
				return
			}
		}
		usage()
		return
	}
	if looksLikeFlag(args[0]) {
		fmt.Fprintf(os.Stderr, "Unknown option: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}

	c, rest, sugg := lookup(registry, args)
	if c == nil {
		fmt.Fprintf(os.Stderr, "unknown verb %q\n", args[0])
		if sugg != "" {
			fmt.Fprintln(os.Stderr, sugg)
		}
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}
	if c.Group {
		// a head with no subcommand, or a help word: the group's usage
		if len(rest) == 0 {
			groupUsage(c)
			os.Exit(2)
		}
		if isHelpWord(rest[0]) {
			groupUsage(c)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown %s verb %q\n", c.Name(), rest[0])
		if s := suggest(c.Name()+" "+rest[0], sortedNames(registry)); s != "" {
			fmt.Fprintln(os.Stderr, s)
		}
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		groupUsage(c)
		os.Exit(2)
	}

	// The option region is parsed BEFORE the handler exists to the process:
	// a malformed command line, or a help word inside the option region,
	// stops here, and nothing below it has run. This is the general form of
	// the founder ruling of 2026-09-09 (`ks run --help` started a billable
	// session): help never acts, and neither does a typo.
	inv, uerr := c.parse(rest)
	if uerr != nil {
		uerr.print()
		os.Exit(2)
	}
	if err := applyGlobals(inv); err != nil {
		(&UsageError{Cmd: c, Message: err.Error()}).print()
		os.Exit(2)
	}
	if inv.Help {
		if c.Path[0] == "cruise" {
			sabotageHelpHook() // test-only, KS_CLI_SABOTAGE_HELP=1: gate CR-7's sabotage
		}
		commandHelp(c)
		return
	}
	c.Run(inv)
}

func usage() { fmt.Print(registryUsage(registry)) }

// groupUsage prints a group's subcommands from the registry; cruise keeps
// its authored text, which the gates and tests read.
func groupUsage(g *Command) {
	if g.Name() == "cruise" {
		cruiseUsage()
		return
	}
	fmt.Printf("ks %s: %s\n\nusage:\n", g.Name(), g.Summary)
	for _, c := range registry {
		if !c.Group && len(c.Path) > 1 && c.Path[0] == g.Path[0] {
			fmt.Printf("  %-44s %s\n", c.Usage(), c.Summary)
		}
	}
	fmt.Println("\nHelp makes no request and changes nothing.")
}

// die reports a failure in the mode's shape and exits by the table.
func die(err error) { fail(err) }

// hosted wraps a handler that needs the stored sign-in. A signed-out client
// stops here with exit 2 and no request.
func hosted(run func(cr hostedCreds, inv *Invocation)) func(*Invocation) {
	return func(inv *Invocation) {
		cr, ok := hostedToken()
		if !ok {
			fail(&cliError{Code: exitAuth, Kind: "not_signed_in", Message: "Not signed in. Run: ks login", NextAction: "ks login"})
		}
		requireCapability(cr, inv.Cmd)
		run(cr, inv)
	}
}
