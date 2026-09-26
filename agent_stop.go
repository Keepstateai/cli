// agent_stop.go: stopping an agent (KS-035), which is none of the three
// other things it is easily confused with:
//
//	Leave window     q in ks agent open: the window closes, its control is
//	                 released, and the agent keeps working
//	Interrupt        ks task cancel <task>: one instruction is asked to stop
//	Agent stop       ks agent stop <name> (this file): the instruction in
//	                 flight is asked to stop at a safe boundary and the
//	                 agent's queue is held; the SESSION KEEPS RUNNING, so
//	                 runtime and storage may still be charged
//	Save and pause   ks agent pause <name>: the whole session is saved and
//	                 parked, and only a proven save and stop reads Paused
//
// Stop's dialog says that charges continue and offers Save and pause as a
// separate action; it never assumes it. Stop is confirmed by naming the
// agent (typed, or --confirm); --yes does not confirm it.
//
// Ctrl-C, by context (QA-035-3): in an agent window that holds control it
// INTERRUPTS the instruction in flight -- the same request as ks task cancel,
// never text sent to the agent -- says what it did, and the window stays
// open (a watching window interrupts nothing and says so); text being typed
// is discarded by the terminal and never sent. While a command waits (a
// pause, an operation, ks agent status --watch) it stops the local waiting
// only; while following logs it stops following. Leaving a window is q.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func hostedAgentStop(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	label := sess.Name + "/" + a.Name
	fmt.Fprintf(os.Stderr, "stop agent %s (%s) in session %s?\n", a.Name, a.ID, sess.ShortID)
	fmt.Fprintln(os.Stderr, "  the instruction in flight is ASKED to stop at a safe boundary, and nothing further in its queue starts until you release the hold")
	fmt.Fprintln(os.Stderr, "  the session keeps running: runtime and storage may still be charged")
	fmt.Fprintf(os.Stderr, "  to also stop those charges, Save and pause is a separate action: ks agent pause %s --session %s\n", a.Name, sess.ShortID)
	given := strings.TrimSpace(inv.Str("confirm"))
	ok := func(s string) bool {
		s = strings.TrimSpace(s)
		return s != "" && (s == a.Name || s == a.ID || s == label)
	}
	switch {
	case given != "":
		if !ok(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q does not name agent %s; nothing was stopped", given, a.Name)})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
			Message:    "stopping an agent is confirmed by naming it; --yes does not confirm it, and nothing was stopped",
			NextAction: fmt.Sprintf("ks agent stop %s --session %s --confirm %s", a.Name, sess.ShortID, a.Name)})
	default:
		fmt.Fprintf(os.Stderr, "type %s to stop it, or anything else to leave it working: ", a.Name)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !ok(line) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the agent was not named; nothing was stopped"})
		}
	}
	body := map[string]any{}
	if r := strings.TrimSpace(inv.Str("reason")); r != "" {
		body["reason"] = r
	}
	var env struct {
		Data struct {
			Interrupted  string     `json:"interrupt_requested_task"`
			TaskState    string     `json:"task_state"`
			HeldTasks    []string   `json:"held_tasks"`
			SessionState string     `json:"session_runtime_state"`
			Charges      string     `json:"charges"`
			Park         openAction `json:"park"`
			Resume       string     `json:"resume"`
			Note         string     `json:"note"`
			AlreadyHeld  bool       `json:"already_held"`
			Hold         *struct {
				ID string `json:"id"`
			} `json:"hold"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/stop", body, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_agent_removed" {
			die(&cliError{Code: exitConflict, Kind: he.Type, Message: sanitize(he.Message), NextAction: "ks agent list --session " + sess.ShortID})
		}
		die(err)
	}
	d := env.Data
	emit(d, func() {
		if d.Interrupted != "" {
			fmt.Printf("stop requested: instruction %s now reads %s (asked to stop at a safe boundary; not claimed stopped)\n", d.Interrupted, stateLabel("task_state", d.TaskState))
		} else {
			fmt.Println("stop requested: no instruction was in flight")
		}
		hold := "the queue is held"
		if d.Hold != nil && d.Hold.ID != "" {
			hold += " (" + d.Hold.ID + ")"
		}
		if d.AlreadyHeld {
			hold += "; it was already held"
		}
		fmt.Printf("  %s: %d instruction(s) wait and nothing further starts\n", hold, len(d.HeldTasks))
		fmt.Printf("  session %s is %s: %s\n", sess.ShortID, stateLabel("session_runtime", d.SessionState), sanitize(d.Charges))
		if d.Park.Label != "" {
			fmt.Printf("  %s (separate, not done): %s — ks agent pause %s --session %s\n", d.Park.Label, sanitize(d.Park.Request), a.Name, sess.ShortID)
		}
		if d.Resume != "" {
			fmt.Printf("  to continue: %s\n", sanitize(d.Resume))
		}
		if d.Note != "" {
			fmt.Println("  " + sanitize(d.Note))
		}
	})
}
