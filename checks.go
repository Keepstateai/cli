// checks.go: project checks and setup operations through approved controls
// (KS-059).
//
//	ks check define <name> -- ARGV...   store a definition; runs nothing
//	ks check list                        an agent's definitions and what was trusted
//	ks check run <check>                 request a run
//	ks check show <run>                  a run: state, exit status, output, artifacts
//	ks check trust <run>                 trust the exact action a run stopped at
//
// A check is an argument vector with no shell, the workspace files its
// behaviour depends on, its artifacts and its bounds; the service hashes all
// of it. A run executes only when a person has trusted THAT definition over
// exactly the inputs the worker observes, so a changed test script stops the
// run at approval_required until it is trusted again. Trusting is confirmed
// by the action hash the person is shown (typed, or --confirm); --yes never
// trusts. A check's result is an ordinary check, never a verification.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type checkDefinition struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	SessionID  string `json:"session_id"`
	Name       string `json:"name"`
	Definition struct {
		Kind           string   `json:"kind"`
		Argv           []string `json:"argv"`
		Workdir        string   `json:"workdir"`
		Inputs         []string `json:"inputs"`
		Artifacts      []string `json:"artifacts"`
		TimeoutSeconds int      `json:"timeout_seconds"`
		MaxOutputBytes int      `json:"max_output_bytes"`
		Profile        string   `json:"profile"`
	} `json:"definition"`
	DefinitionHash string `json:"definition_hash"`
	ExactAction    string `json:"exact_action"`
	CreatedBy      string `json:"created_by"`
	CreatedAt      string `json:"created_at"`
	Trusted        []struct {
		InputsDigest string `json:"inputs_digest"`
		ActionHash   string `json:"action_hash"`
		TrustedBy    string `json:"trusted_by"`
		TrustedAt    string `json:"trusted_at"`
	} `json:"trusted"`
}

type checkRun struct {
	ID                   string `json:"id"`
	CheckID              string `json:"check_id"`
	AgentID              string `json:"agent_id"`
	Role                 string `json:"role"`
	DefinitionHash       string `json:"definition_hash"`
	State                string `json:"state"`
	RequestedBy          string `json:"requested_by"`
	RequestedAt          string `json:"requested_at"`
	ObservedInputsDigest string `json:"observed_inputs_digest,omitempty"`
	ActionHash           string `json:"action_hash,omitempty"`
	ExactAction          string `json:"exact_action,omitempty"`
	StartedAt            string `json:"started_at,omitempty"`
	DeadlineAt           string `json:"deadline_at,omitempty"`
	FinishedAt           string `json:"finished_at,omitempty"`
	ExitCode             *int   `json:"exit_code"`
	TimedOut             bool   `json:"timed_out"`
	Output               string `json:"output,omitempty"`
	OutputBytes          int64  `json:"output_bytes"`
	OutputTruncated      bool   `json:"output_truncated"`
	Artifacts            []struct {
		Path    string `json:"path"`
		Present bool   `json:"present"`
		SHA256  string `json:"sha256,omitempty"`
		Bytes   int64  `json:"bytes,omitempty"`
	} `json:"artifacts"`
	GroupClear *bool  `json:"process_group_clear"`
	Reason     string `json:"reason,omitempty"`
}

func checkAgent(cr hostedCreds, inv *Invocation) (inventoryRow, agentRow) {
	sess := agentSession(cr, inv)
	name := inv.Str("agent")
	if name == "" {
		name = "main"
	}
	a, err := resolveAgent(cr, sess, name)
	if err != nil {
		die(err)
	}
	return sess, a
}

func hostedCheckDefine(cr hostedCreds, inv *Invocation) {
	name := inv.Arg(0)
	if !c04Name.MatchString(name) {
		fail(&cliError{Code: exitUsage, Kind: "name_invalid", Message: fmt.Sprintf("%q is not a check name: 1 to 48 ASCII letters, digits, hyphens or underscores, starting with a letter; nothing was defined", sanitize(name))})
	}
	if len(inv.List("input")) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--input names the workspace files the check's behaviour depends on (the test script, its config); a check with none cannot be trusted over anything. Nothing was defined"})
	}
	for _, p := range append(append([]string{}, inv.List("input")...), inv.List("artifact")...) {
		workspacePath(p)
	}
	sess, a := checkAgent(cr, inv)
	body := map[string]any{"name": name, "argv": inv.Rest, "inputs": inv.List("input")}
	if k := inv.Str("kind"); k != "" {
		body["kind"] = k
	}
	if w := inv.Str("workdir"); w != "" {
		body["workdir"] = workspacePath(w)
	}
	if as := inv.List("artifact"); len(as) > 0 {
		body["artifacts"] = as
	}
	if inv.Set("timeout") {
		body["timeout_seconds"] = inv.Int("timeout")
	}
	if inv.Set("max-output") {
		body["max_output_bytes"] = inv.Int("max-output")
	}
	var env struct {
		Data checkDefinition `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/checks", body, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("defined check %s (%s) for agent %s in session %s; nothing ran\n", d.ID, d.Name, a.Name, sess.ShortID)
		fmt.Printf("  action      %s\n", visible(sanitize(d.ExactAction)))
		fmt.Printf("  definition  %s\n", d.DefinitionHash)
		fmt.Printf("  profile     %s\n", sanitize(d.Definition.Profile))
		fmt.Printf("run it: ks check run %s (it stops for your trust the first time, and whenever its inputs change)\n", d.ID)
	})
}

func hostedCheckList(cr hostedCreds, inv *Invocation) {
	sess, a := checkAgent(cr, inv)
	var env struct {
		Data struct {
			Items []checkDefinition `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(a.ID)+"/checks", nil, &env); err != nil {
		die(err)
	}
	ds := env.Data.Items
	emit(map[string]any{"agent": a.ID, "checks": ds, "count": len(ds)}, func() {
		if len(ds) == 0 {
			fmt.Printf("agent %s in session %s has no checks; define one: ks check define <name> --input <file> -- <argv...>\n", a.Name, sess.ShortID)
			return
		}
		for _, d := range ds {
			fmt.Printf("%s  %s  %s\n", d.ID, d.Name, visible(sanitize(d.ExactAction)))
			fmt.Printf("    definition %s; trusted over %d input set(s)\n", short(strings.TrimPrefix(d.DefinitionHash, "sha256:")), len(d.Trusted))
		}
	})
}

func fetchCheckRun(cr hostedCreds, id string) (checkRun, error) {
	var env struct {
		Data checkRun `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/check-runs/"+url.PathEscape(id), nil, &env)
	return env.Data, err
}

func printCheckRun(r checkRun) {
	fmt.Printf("check run %s (check %s)  %s\n", r.ID, r.CheckID, strings.ToUpper(figure(r.State)))
	fmt.Printf("  role          %s (an ordinary check's result, never a verification)\n", figure(r.Role))
	fmt.Printf("  requested     by %s at %s\n", figure(r.RequestedBy), figure(r.RequestedAt))
	if r.ObservedInputsDigest != "" {
		fmt.Printf("  inputs seen   %s\n", r.ObservedInputsDigest)
	}
	switch r.State {
	case "approval_required":
		fmt.Printf("  NOT RUN: this definition has not been trusted over these inputs\n")
		fmt.Printf("  action        %s\n", visible(sanitize(r.ExactAction)))
		fmt.Printf("  action hash   %s\n", r.ActionHash)
		fmt.Printf("  trust it: ks check trust %s --confirm %s\n", r.ID, short(strings.TrimPrefix(r.ActionHash, "sha256:")))
	case "succeeded", "failed":
		exit := "not recorded"
		if r.ExitCode != nil {
			exit = fmt.Sprint(*r.ExitCode)
		}
		if r.TimedOut {
			exit += " (killed at the deadline)"
		}
		fmt.Printf("  exit status   %s\n", exit)
		if r.GroupClear != nil {
			fmt.Printf("  process group %s\n", map[bool]string{true: "confirmed empty", false: "NOT confirmed empty"}[*r.GroupClear])
		}
		for _, a := range r.Artifacts {
			if a.Present {
				fmt.Printf("  artifact      %s  %s bytes  sha256 %s\n", visible(sanitize(a.Path)), commas(a.Bytes), a.SHA256)
			} else {
				fmt.Printf("  artifact      %s  absent\n", visible(sanitize(a.Path)))
			}
		}
		if r.Output != "" {
			fmt.Printf("  output (%s bytes%s):\n", commas(r.OutputBytes), map[bool]string{true: ", cut at the bound", false: ""}[r.OutputTruncated])
			for _, l := range strings.Split(strings.TrimRight(visible(sanitize(r.Output)), "\n"), "\n") {
				fmt.Printf("    %s\n", l)
			}
		}
	}
	if r.Reason != "" {
		fmt.Printf("  reason        %s\n", sanitize(r.Reason))
	}
}

func hostedCheckRun(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data checkRun `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/checks/"+url.PathEscape(inv.Arg(0))+"/runs", map[string]any{}, &env); err != nil {
		die(err)
	}
	r := env.Data
	emit(r, func() {
		fmt.Printf("requested run %s of check %s (%s): the agent's worker takes it between instructions, and it executes only over inputs you trusted\n", r.ID, r.CheckID, figure(r.State))
		fmt.Printf("read it: ks check show %s\n", r.ID)
	})
}

func hostedCheckShow(cr hostedCreds, inv *Invocation) {
	r, err := fetchCheckRun(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	emit(r, func() { printCheckRun(r) })
}

func hostedCheckTrust(cr hostedCreds, inv *Invocation) {
	r, err := fetchCheckRun(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if r.State != "approval_required" || r.ActionHash == "" {
		fail(&cliError{Code: exitConflict, Kind: "check_not_awaiting_trust", Message: fmt.Sprintf("run %s is %s, not waiting for trust; nothing was trusted", r.ID, figure(r.State))})
	}
	// the exact action, shown before anything is trusted
	fmt.Fprintf(os.Stderr, "trust this action for check %s:\n  %s\n  over inputs %s\n  action hash %s\n", r.CheckID, visible(sanitize(r.ExactAction)), r.ObservedInputsDigest, r.ActionHash)
	hexHash := strings.TrimPrefix(strings.ToLower(r.ActionHash), "sha256:")
	matches := func(s string) bool {
		s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "sha256:")
		return s != "" && (s == hexHash || (len(s) == 12 && s == short(hexHash)))
	}
	given := inv.Str("confirm")
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not this action's hash (%s); nothing was trusted", given, short(hexHash))})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
			Message:    "trusting a check is confirmed by its action hash and none was given; --yes does not trust anything, and nothing was trusted",
			NextAction: fmt.Sprintf("ks check trust %s --confirm %s", r.ID, short(hexHash))})
	default:
		fmt.Fprintf(os.Stderr, "type the action hash (%s) to trust it, or anything else to stop: ", short(hexHash))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !matches(line) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the action was not confirmed; nothing was trusted"})
		}
	}
	var env struct {
		Data checkRun `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/check-runs/"+url.PathEscape(r.ID)+"/trust", map[string]any{"expected_action_hash": r.ActionHash}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_action_hash_mismatch" {
			ce := classify(err)
			ce.Message = "the action changed after it was shown, so nothing was trusted: " + ce.Message
			ce.NextAction = "ks check show " + r.ID
			die(ce)
		}
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("trusted: run %s is queued again (%s) and re-reads its inputs before it executes; a change to them stops it again\n", d.ID, figure(d.State))
	})
}
