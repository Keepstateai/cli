package main

// ks run --agent (KS-030): one controlled operation that starts an agent
// session, instead of a sequence of machine commands.
//
//  1. the capability gate: agent mode is served only where the control plane
//     says it is available
//  2. preflight (KS-029): blockers stop the run BEFORE anything is created or
//     billed; preflight uploads nothing, provisions nothing, calls no model
//  3. the session record and its primary agent, one commit on the service
//  4. provisioning: a machine, the agent started in it, and Ready only when
//     the agent reports it; a setup that does not complete is cleaned up by
//     the service and said so here
//  5. optionally a first task (--task), and the agent's window (--open),
//     both only after Ready
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
	AgentActivity string              `json:"agent_activity"`
	CleanedUp     bool                `json:"cleaned_up"`
	Note          string              `json:"note"`
}

// runnerProvider is the key family the agent's runner calls through.
const runnerProvider = "anthropic"

func hostedRunAgent(cr hostedCreds, inv *Invocation) {
	requireCapability(cr, &Command{Path: []string{"run", "--agent"}, Needs: "agent.workspace"})

	// ---- 2. preflight: stop before anything exists --------------------------
	var pf struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/preflight", map[string]any{"provider": runnerProvider, "mode": "agent"}, &pf); err != nil {
		die(err)
	}
	if b, ok := pf.Data["blockers"].([]any); ok && len(b) > 0 {
		var lines []string
		for _, x := range b {
			lines = append(lines, fmt.Sprint(x))
		}
		fail(&cliError{Code: exitConflict, Kind: "preflight_blocked",
			Message:    "preflight found blockers, so nothing was created: " + strings.Join(lines, "; "),
			NextAction: "ks preflight --provider " + runnerProvider})
	}

	// ---- 3. the record and its primary agent --------------------------------
	name := inv.Str("name")
	if name == "" {
		name = "run-" + time.Now().UTC().Format("20060102-150405")
	}
	agentName := inv.Str("agent-name")
	body := map[string]any{"name": name, "mode": "agent"}
	if agentName != "" {
		body["primary_agent_name"] = agentName
	}
	var created struct {
		Data struct {
			ID             string `json:"id"`
			Revision       int64  `json:"revision"`
			PrimaryAgentID string `json:"primary_agent_id"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions", body, &created); err != nil {
		die(err)
	}
	rec := created.Data
	progress("session %s created with its primary agent; starting a machine for it", rec.ID)

	// ---- 4. the machine and the agent in it --------------------------------
	req := map[string]any{"expected_revision": rec.Revision}
	if inv.Set("budget-tokens") {
		req["budget"] = inv.Int("budget-tokens")
	}
	var prov struct {
		Data provisionAnswer `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(rec.ID)+"/provision", req, &prov); err != nil {
		die(err)
	}
	p := prov.Data
	for _, ph := range p.Phases {
		mark := "done"
		if !ph.Done {
			mark = "NOT DONE"
		}
		progress("  %-14s %s: %s", ph.Phase, mark, ph.Detail)
	}
	facts := map[string]any{"session": rec.ID, "agent": p.AgentID, "ready": p.Ready,
		"agent_activity": p.AgentActivity, "cleaned_up": p.CleanedUp, "phases": p.Phases, "note": p.Note, "control_plane": cr.CTL}
	agentRef := agentName
	if agentRef == "" {
		agentRef = "main"
	}
	if !p.Ready {
		// Not Ready is never printed as Ready. Two different facts: the
		// setup failed (and was cleaned up), or the agent is running and
		// has not reported Ready yet.
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
		progress("agent %s is Ready in session %s; opening its window", agentRef, rec.ID)
		sub := &Invocation{Args: []string{agentRef}, set: map[string]bool{"session": true}, strs: map[string]string{"session": rec.ID}}
		hostedAgentOpen(cr, sub)
		return
	}
	emit(facts, func() {
		fmt.Printf("agent %s is Ready in session %s\n", agentRef, rec.ID)
		fmt.Println(p.Note)
		fmt.Printf("next: ks agent open %s --session %s   (or: ks agent tell %s \"...\" --session %s)\n", agentRef, rec.ID, agentRef, rec.ID)
	})
}

func phaseDone(ph []provisionPhaseRow, name string) bool {
	for _, p := range ph {
		if p.Phase == name {
			return p.Done
		}
	}
	return false
}
