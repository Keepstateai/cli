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
		Flags:   []Flag{{Name: "ctl", Kind: flagString, Value: "URL", Summary: "the control plane to sign in to; default https://ctl.keepstate.ai"}},
		Nothing: "No sign-in was started.", Run: func(inv *Invocation) {
			if err := runLogin(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"run"}, Summary: "start a hosted session", Surface: "hosted",
		Flags: []Flag{
			{Name: "image", Kind: flagString, Value: "I", Summary: "the agent image; default your onboarding choice"},
			{Name: "budget-tokens", Aliases: []string{"budget"}, Kind: flagInt, Positive: true, Value: "N", Summary: "the session's token budget; default per tier (500,000 free, 2,000,000 paid); zero and negative are refused"},
		},
		Nothing: "No session was started.", Run: hosted(hostedRun)},
	{Path: []string{"checkpoint"}, Aliases: [][]string{{"save"}}, Summary: "save the session (durably); --stop parks it", Surface: "hosted",
		Args:    []Arg{{Name: "session", Required: true}},
		Flags:   []Flag{{Name: "stop", Kind: flagBool, Summary: "park the session after the save succeeds"}},
		Nothing: "No checkpoint was taken.", Run: hosted(hostedCheckpoint)},
	{Path: []string{"wake"}, Aliases: [][]string{{"resume"}}, Summary: "resume a parked session (mid-sentence)", Surface: "hosted",
		Args:    []Arg{{Name: "session", Required: true}},
		Nothing: "Nothing was resumed.", Run: hosted(hostedWake)},
	{Path: []string{"kill"}, Summary: "destroy a session (guarded: checkpoint first)", Surface: "hosted",
		Args:    []Arg{{Name: "session", Required: true}},
		Flags:   []Flag{{Name: "force", Kind: flagBool, Summary: "destroy it even without a saved checkpoint"}},
		Nothing: "Nothing was destroyed.", Run: hosted(hostedKill)},
	{Path: []string{"meter"}, Summary: "spend, budget, key sources", Surface: "hosted",
		Args:    []Arg{{Name: "session", Required: true}},
		Flags:   []Flag{{Name: "json", Kind: flagBool, Summary: "print the meter as JSON"}},
		Nothing: "Nothing was read.", Run: hosted(hostedMeter)},
	{Path: []string{"exec"}, Summary: "run one command in the session", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}}, Rest: "command",
		Nothing: "No command was sent.", Run: hosted(hostedExec)},
	{Path: []string{"attach"}, Summary: "interactive terminal into a running session (tmux)", Surface: "hosted",
		Args:    []Arg{{Name: "session", Required: true}},
		Nothing: "Nothing was attached.", Run: hosted(hostedAttachCmd)},
	{Path: []string{"fork"}, Summary: "branch a checkpoint into diverging children", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "children", Short: "n", Kind: flagInt, Positive: true, Value: "N", Summary: "how many children to branch; default 1"},
			{Name: "steer", Kind: flagString, Value: "FILE", Summary: "a steer file applied to every child"},
		},
		Nothing: "Nothing was forked.", Run: hosted(hostedFork)},
	{Path: []string{"cruise"}, Group: true, Summary: "accepted work on the fleet", Surface: "hosted"},
	{Path: []string{"cruise", "init"}, Summary: "draft the acceptance manifest from the repository into .keepstate/cruise.json", Surface: "client",
		Flags: []Flag{
			{Name: "goal", Kind: flagString, Value: "TEXT", Summary: "the goal the agent is given; without it, the draft's previous goal, else \"make the check pass: CMD\""},
			{Name: "tests", Kind: flagString, Value: "CMD", Summary: "the command that decides done, when init should not detect it (pytest, npm test, go test)"},
			{Name: "paths", Kind: flagList, Variadic: true, Value: "GLOB", Summary: "the paths an attempt may change; default every tracked file except the tests and .keepstate"},
			{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs in order; default the model table's default ladder"},
			{Name: "spend", Kind: flagUSD, Value: "USD", Summary: "the spend ceiling in dollars; default 2"},
		},
		Nothing: "No draft was written.", Run: cruiseInit},
	{Path: []string{"cruise", "approve"}, Summary: "lock the draft: its digest and the time into .keepstate/cruise.lock", Surface: "client",
		Nothing: "Nothing was approved.", Run: cruiseApprove},
	{Path: []string{"cruise", "run"}, Summary: "upload the workspace and the locked manifest; start the job", Surface: "hosted",
		Nothing: "No job was started.", Run: func(inv *Invocation) {
			if err := cruiseRun(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "status"}, Summary: "the job (or the newest): state, rung, each attempt, save points, spend, verdict, artifact", Surface: "hosted",
		Args:    []Arg{{Name: "job", Required: false}},
		Nothing: "Nothing was read.", Run: cruiseStatus},
	{Path: []string{"cruise", "logs"}, Summary: "the event stream, one event per line: seq, ts, type, detail", Surface: "hosted",
		Args:    []Arg{{Name: "job", Required: true}},
		Nothing: "Nothing was read.", Run: cruiseLogs},
	{Path: []string{"cruise", "cancel"}, Summary: "cancel a job", Surface: "hosted",
		Args:    []Arg{{Name: "job", Required: true}},
		Nothing: "Nothing was cancelled.", Run: cruiseCancel},
	{Path: []string{"cruise", "resume"}, Summary: "resume a job from review, on the same or a wider ladder", Surface: "hosted",
		Args:    []Arg{{Name: "job", Required: true}},
		Flags:   []Flag{{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs to continue on, in order"}},
		Nothing: "Nothing was resumed.", Run: cruiseResume},
	{Path: []string{"cruise", "artifact"}, Summary: "download the accepted tarball", Surface: "hosted",
		Args:    []Arg{{Name: "job", Required: true}},
		Flags:   []Flag{{Name: "out", Kind: flagString, Value: "FILE", Summary: "where to write it; default JOB.tar.gz"}},
		Nothing: "Nothing was written.", Run: func(inv *Invocation) {
			if err := cruiseArtifact(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "models"}, Summary: "the model table the control plane serves: families, rungs, default ladder", Surface: "hosted",
		Nothing: "Nothing was read.", Run: cruiseModels},
	{Path: []string{"doctor"}, Summary: "connectivity, token, version", Surface: "client",
		Run: func(*Invocation) { os.Exit(runDoctor()) }},
	{Path: []string{"update"}, Summary: "self-update (checksum-verified)", Surface: "client",
		Nothing: "Nothing was updated.", Run: func(*Invocation) {
			if err := runUpdate(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"logout"}, Summary: "remove the stored token", Surface: "client",
		Run: func(*Invocation) {
			if err := runLogout(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"uninstall"}, Summary: "how to remove ks completely", Surface: "client",
		Run: func(*Invocation) { runUninstall() }},
	{Path: []string{"version"}, Summary: "print the version", Surface: "client",
		Run: func(*Invocation) { fmt.Println("ks", version) }},
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
					cruiseUsage()
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
			cruiseUsage()
			os.Exit(2)
		}
		if isHelpWord(rest[0]) {
			cruiseUsage()
			return
		}
		fmt.Fprintf(os.Stderr, "unknown cruise verb %q\n", rest[0])
		if s := suggest("cruise "+rest[0], sortedNames(registry)); s != "" {
			fmt.Fprintln(os.Stderr, s)
		}
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		cruiseUsage()
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

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// hosted wraps a handler that needs the stored sign-in. A signed-out client
// stops here with exit 2 and no request.
func hosted(run func(cr hostedCreds, inv *Invocation)) func(*Invocation) {
	return func(inv *Invocation) {
		cr, ok := hostedToken()
		if !ok {
			fmt.Fprintln(os.Stderr, "Not signed in. Run: ks login")
			os.Exit(2)
		}
		run(cr, inv)
	}
}
