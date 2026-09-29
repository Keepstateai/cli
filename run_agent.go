package main

// ks run --agent (KS-030): one controlled operation that starts an agent
// session, instead of a sequence of machine commands.
//
//  1. the capability gate: agent mode is served only where the control plane
//     says it is available
//  2. preflight (KS-029): blockers stop the run BEFORE anything is created or
//     billed; preflight uploads nothing, provisions nothing, calls no model
//  3. the session record and its primary agent, one commit on the service
//  3a. the key the agent calls with, bound to the session before a machine
//     exists: --key, or the account's ONE enabled key for the runner's
//     provider; none or several is refused before anything is created
//  4. provisioning: a machine, the agent started in it, and supervised once
//     its supervisor reports (a new agent reports Ready only after its first
//     instruction); a setup that does not complete is cleaned up
//     by the service and said so here
//  5. optionally a first task (--task), and the agent's window (--open),
//     both once the agent is supervised
//
// A Ready line is printed only for an agent the service OBSERVED ready.

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type provisionPhaseRow struct {
	Phase  string `json:"phase"`
	Done   bool   `json:"done"`
	Detail string `json:"detail"`
}

type provisionAnswer struct {
	SessionID     string              `json:"session_id"`
	AgentID       string              `json:"agent_id"`
	Phases        []provisionPhaseRow `json:"phases"`
	Ready         bool                `json:"ready"`
	Supervised    bool                `json:"supervised"`
	AgentActivity string              `json:"agent_activity"`
	CleanedUp     bool                `json:"cleaned_up"`
	Note          string              `json:"note"`
}

// runnerProvider is the key family the agent's runner calls through.
const runnerProvider = "anthropic"

func hostedRunAgent(cr hostedCreds, inv *Invocation) {
	requireCapability(cr, &Command{Path: []string{"run", "--agent"}, Needs: "agent.workspace"})
	var budget *int64
	if inv.Set("budget-tokens") {
		v := int64(inv.Int("budget-tokens"))
		budget = &v
	}
	rec, p, key, err := startAgentSession(cr, inv.Str("name"), inv.Str("agent-name"), inv.Str("key"), budget)
	if err != nil {
		die(err)
	}
	agentName := inv.Str("agent-name")
	facts := map[string]any{"session": rec.ID, "agent": p.AgentID, "ready": p.Ready, "supervised": p.Supervised, "key": key.ID,
		"agent_activity": p.AgentActivity, "cleaned_up": p.CleanedUp, "phases": p.Phases, "note": p.Note, "control_plane": cr.CTL}
	agentRef := agentName
	if agentRef == "" {
		agentRef = "main"
	}
	if !p.Ready && !p.Supervised {
		// Not Ready is never printed as Ready. Two different facts: the
		// setup failed (and was cleaned up), or the supervisor has not
		// reported for the agent yet.
		if p.CleanedUp || !phaseDone(p.Phases, "start_agent") {
			fail(&cliError{Code: exitFailed, Kind: "run_setup_failed", Message: p.Note,
				NextAction: "ks run --agent (to try again), or ks doctor"})
		}
		fail(&cliError{Code: exitTemporary, Kind: "agent_not_ready", Message: p.Note,
			NextAction: fmt.Sprintf("ks agent status %s --session %s", agentRef, rec.ID)})
	}

	// ---- 5. an optional first task, then the window -------------------------
	if text := strings.TrimSpace(inv.Str("task")); text != "" {
		path := "/api/v2/agents/" + url.PathEscape(p.AgentID) + "/tasks"
		sid, err := submissionID(cr, path, text)
		if err != nil {
			die(err)
		}
		var env struct {
			Data submittedTask `json:"data"`
		}
		if err := hostedMutate(cr, "POST", path, map[string]any{"submission_id": sid, "text": text}, &env); err != nil {
			die(err)
		}
		facts["task"] = env.Data.ID
		progress("task %s submitted to %s", env.Data.ID, agentRef)
	}
	if inv.Bool("open") {
		progress("agent %s is %s in session %s; opening its window", agentRef, startedWord(p), rec.ID)
		sub := &Invocation{Args: []string{agentRef}, set: map[string]bool{"session": true}, strs: map[string]string{"session": rec.ID}}
		hostedAgentOpen(cr, sub)
		return
	}
	emit(facts, func() {
		fmt.Printf("agent %s is %s in session %s\n", agentRef, startedWord(p), rec.ID)
		fmt.Println(p.Note)
		fmt.Printf("next: ks agent open %s --session %s   (or: ks agent tell %s \"...\" --session %s)\n", agentRef, rec.ID, agentRef, rec.ID)
	})
}

// startedWord is what the agent is, as reported: Ready only when it said so.
func startedWord(p provisionAnswer) string {
	if p.Ready {
		return "Ready"
	}
	return "supervised and waiting for its first instruction"
}

// runKey is the key an agent session calls with: the one named, or the
// account's single enabled key for the runner's provider. None or several is
// refused before anything is created; a key is never guessed.
func runKey(cr hostedCreds, arg string) (customerKey, error) {
	if arg != "" {
		k, err := resolveKey(cr, arg)
		if err != nil {
			return k, err
		}
		if k.Provider != runnerProvider || !k.Enabled {
			return k, &cliError{Code: exitUsage, Kind: "key_unusable",
				Message: fmt.Sprintf("key %s is a %s key (enabled: %t); the agent calls %s with an enabled %s key. Nothing was created", k.ID, k.Provider, k.Enabled, runnerProvider, runnerProvider), NextAction: "ks key list"}
		}
		return k, nil
	}
	keys, err := fetchKeys(cr)
	if err != nil {
		return customerKey{}, err
	}
	var usable []customerKey
	for _, k := range keys {
		if k.Provider == runnerProvider && k.Enabled {
			usable = append(usable, k)
		}
	}
	switch len(usable) {
	case 1:
		return usable[0], nil
	case 0:
		return customerKey{}, &cliError{Code: exitConflict, Kind: "no_key",
			Message: "you have no enabled " + runnerProvider + " key, which the agent calls with. Nothing was created", NextAction: "ks key add --provider " + runnerProvider}
	}
	var ids []string
	for _, k := range usable {
		ids = append(ids, k.ID+" (…"+k.Last4+")")
	}
	return customerKey{}, &cliError{Code: exitUsage, Kind: "key_ambiguous",
		Message:    fmt.Sprintf("you have %d enabled %s keys (%s); name the one this agent calls with. Nothing was created", len(usable), runnerProvider, strings.Join(ids, ", ")),
		NextAction: "ks run --agent --key <id>"}
}

// createdRecord is the session record ks run --agent created.
type createdRecord struct {
	ID             string `json:"id"`
	Revision       int64  `json:"revision"`
	PrimaryAgentID string `json:"primary_agent_id"`
}

// startAgentSession is steps 2 to 4 of ks run --agent, for the command and
// for the app alike: preflight, the key, the record, its binding and the
// machine. It returns errors rather than exiting; progress goes where
// progress goes (the terminal, or the app's screen).
func startAgentSession(cr hostedCreds, name, agentName, keyArg string, budget *int64) (createdRecord, provisionAnswer, customerKey, error) {
	var none createdRecord
	// ---- 2. preflight: stop before anything exists --------------------------
	var pf struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/preflight", map[string]any{"provider": runnerProvider, "mode": "agent"}, &pf); err != nil {
		return none, provisionAnswer{}, customerKey{}, err
	}
	if b, ok := pf.Data["blockers"].([]any); ok && len(b) > 0 {
		var lines []string
		for _, x := range b {
			lines = append(lines, fmt.Sprint(x))
		}
		return none, provisionAnswer{}, customerKey{}, &cliError{Code: exitConflict, Kind: "preflight_blocked",
			Message:    "preflight found blockers, so nothing was created: " + strings.Join(lines, "; "),
			NextAction: "ks preflight --provider " + runnerProvider}
	}

	// ---- 2a. the key, chosen before anything exists --------------------------
	key, err := runKey(cr, keyArg)
	if err != nil {
		return none, provisionAnswer{}, customerKey{}, err
	}

	// ---- 3. the record and its primary agent --------------------------------
	if name == "" {
		name = "run-" + time.Now().UTC().Format("20060102-150405")
	}
	body := map[string]any{"name": name, "mode": "agent"}
	if agentName != "" {
		body["primary_agent_name"] = agentName
	}
	var created struct {
		Data createdRecord `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions", body, &created); err != nil {
		return none, provisionAnswer{}, key, err
	}
	rec := created.Data
	progress("session %s created with its primary agent; starting a machine for it", rec.ID)

	// ---- 3a. the session's binding: that key is used or nothing is ---------
	var bound struct {
		Data struct {
			Revision int64 `json:"revision"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(rec.ID)+"/bindings",
		map[string]any{"keys": map[string]string{runnerProvider: key.ID}, "expected_revision": rec.Revision}, &bound); err != nil {
		return rec, provisionAnswer{}, key, err
	}
	rec.Revision = bound.Data.Revision
	progress("  %s calls go through your key %s (…%s)", runnerProvider, key.ID, key.Last4)

	// ---- 4. the machine and the agent in it --------------------------------
	req := map[string]any{"expected_revision": rec.Revision}
	if budget != nil {
		req["budget"] = *budget
	}
	var prov struct {
		Data provisionAnswer `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(rec.ID)+"/provision", req, &prov); err != nil {
		return rec, provisionAnswer{}, key, err
	}
	p := prov.Data
	for _, ph := range p.Phases {
		mark := "done"
		if !ph.Done {
			mark = "NOT DONE"
		}
		progress("  %-14s %s: %s", ph.Phase, mark, ph.Detail)
	}
	return rec, p, key, nil
}

func phaseDone(ph []provisionPhaseRow, name string) bool {
	for _, p := range ph {
		if p.Phase == name {
			return p.Done
		}
	}
	return false
}
