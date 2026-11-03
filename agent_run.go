// agent_run.go: ks agent run, a child agent started by an agent or a
// person, in a session of its own.
//
//	ks agent run --name review --template reviewer "review the diff on main"
//	ks agent run --bg --name review "..."      come back at once, as JSON
//
// It is ks run --agent with a first instruction and a parent:
//
//  1. the cost line, before anything exists: a child is a new session, and
//     session time and model use are metered like any session
//  2. the parent, named before anything exists: this caller (caller.go),
//     a hosted agent (--parent), or none (--no-parent)
//  3. the session record, its primary agent and its key (run_agent.go)
//  4. the parent recorded on the agent, before a machine starts
//  5. the machine, then the keeper (the child ends with its parent unless
//     --outlive-parent), then the first instruction: the template's body
//     joined in front of the task
//  6. with --bg the answer comes back at once; without it, this follows
//     until the instruction finishes, the agent needs a person, or the
//     timeout (180 s), which prints "still running as <name>" and exits 4
//
// "Ends with its parent" means saved and paused, never deleted, and only
// when the parent's exit was observed.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// maxChildren is how many children one parent may run at once; the service
// enforces it, and this client says so before anything is created.
const maxChildren = 8

type childParent struct {
	Kind  string `json:"kind"` // caller | agent
	Ref   string `json:"ref"`
	Label string `json:"label,omitempty"`
}

const meteredLine = "This starts a new session for %s; session time and model use are metered."

func hostedAgentRun(cr hostedCreds, inv *Invocation) {
	name := inv.Str("name")
	task := inv.Arg(0)
	switch {
	case name == "":
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--name is required: the child's name, which ks agent send, wait and read use; nothing was created",
			NextAction: "ks agent run --name review \"<task>\""})
	case strings.TrimSpace(task) == "":
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the task is empty; nothing was created", NextAction: "ks agent run --name " + name + " \"<task>\""})
	case inv.Set("parent") && inv.Bool("no-parent"):
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--parent names a parent and --no-parent says there is none; give one of them; nothing was created"})
	case inv.Bool("outlive-parent") && inv.Bool("no-parent"):
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--outlive-parent is about a parent, and --no-parent says there is none; nothing was created"})
	}
	var tmpl agentTemplate
	if inv.Set("template") {
		t, err := findTemplate(inv.Str("template"))
		if err != nil {
			die(err)
		}
		tmpl = t
	}
	keyArg := firstNonEmpty(inv.Str("key"), tmpl.Key)
	var spend *int64
	if inv.Set("spend-limit") {
		v, perr := parseDollars("spend-limit", inv.Str("spend-limit"))
		if perr != nil {
			fail(perr)
		}
		spend = &v
	}
	effort, _ := effortFlag(inv) // a word that is no level creates nothing
	harness, err := chooseHarness(cr, firstNonEmpty(inv.Str("harness"), tmpl.Harness))
	if err != nil {
		die(err)
	}
	timeout := waitTimeoutFlag(inv, defaultAgentWait)
	outlive, notify := inv.Bool("outlive-parent"), !inv.Bool("no-notify")

	// ---- 2. the parent, before anything exists -----------------------------
	var parent *childParent
	var caller callerIdentity
	retry := "ks agent run --no-parent --name " + name + " \"<task>\""
	switch {
	case inv.Bool("no-parent"):
	case inv.Set("parent"):
		id := resolveParentAgent(cr, inv.Str("parent"))
		parent = &childParent{Kind: "agent", Ref: id}
	default:
		caller = resolveCaller(cr)
		if err := registerCaller(cr, caller); err != nil {
			die(parentsUnsupported(err, retry))
		}
		parent = &childParent{Kind: "caller", Ref: caller.ID, Label: caller.Label}
		refuseTakenOrFull(cr, caller, name)
	}

	// ---- 1. the cost line, before anything exists --------------------------
	fmt.Fprintf(os.Stderr, meteredLine+"\n", name)

	// ---- 3. the record, its agent and its key ------------------------------
	rec, _, err := createAgentRecord(cr, name, name, keyArg, firstNonEmpty(inv.Str("model"), tmpl.Model), effort, harness, spend)
	if err != nil {
		die(err)
	}
	empty := func(ce *cliError, why string) {
		ce.WorkStarted = workYes
		ce.Message = fmt.Sprintf("session %s was created for %s with no machine, so nothing runs or is metered in it, but %s: %s", rec.ID, name, why, ce.Message)
		if ce.NextAction == "" {
			ce.NextAction = "ks session delete " + rec.ID + " (it removes the empty record)"
		}
		die(ce)
	}

	// ---- 4. the parent, recorded before a machine starts -------------------
	if parent != nil {
		if err := setParent(cr, rec, *parent, outlive, notify); err != nil {
			var he *hostedErr
			if errors.As(err, &he) && he.Type == "ks_children_limit" {
				empty(&cliError{Code: exitConflict, Kind: "children_limit", Message: sanitize(he.Message),
					NextAction: "ks agent wait --children all, then end a child, or ks session delete " + rec.ID}, fmt.Sprintf("its parent already runs %d children, the most one parent may run", maxChildren))
			}
			empty(classify(parentsUnsupported(err, "ks session delete "+rec.ID+", then "+retry)), "its parent could not be recorded")
		}
	}

	// ---- 5. the machine, the keeper, the first instruction -----------------
	p, err := provisionAgentSession(cr, rec, nil)
	if err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_free_tier_one_session" {
			empty(&cliError{Code: exitConflict, Kind: "free_tier_one_session", Message: "the free plan runs one session at a time and one is running",
				NextAction: "add credit in the console's Billing page, then run this again (ks session delete " + rec.ID + " removes the empty record)"}, "it was not started")
		}
		die(err)
	}
	if !p.Ready && !p.Supervised {
		if p.CleanedUp || !phaseDone(p.Phases, "start_agent") {
			fail(failedStart(cr, p, rec.ID, "ks agent run again, or ks doctor", func(n string) string {
				return "ks agent run --name " + n + " \"<task>\""
			}))
		}
		fail(&cliError{Code: exitTemporary, Kind: "agent_not_ready", WorkStarted: workYes, Message: p.Note,
			NextAction: fmt.Sprintf("ks agent status %s --session %s", name, rec.ID)})
	}
	agentID := firstNonEmpty(p.AgentID, rec.PrimaryAgentID)
	if parent != nil && parent.Kind == "caller" {
		ch := childRecord{Name: name, SessionID: rec.ID, AgentID: agentID, Outlive: outlive, Notify: notify, StartedAt: nowStamp()}
		if err := recordChild(caller, cr.CTL, ch); err != nil {
			progress("the child was not written to this machine's record (%v); name it with --session %s", err, rec.ID)
		}
		if !outlive {
			if err := ensureKeeper(caller.ID); err != nil {
				progress("the keeper could not be started (%v), so %s will not end when %s does; it keeps running until you end it", err, name, caller.Label)
			}
		}
	}
	sid, err := newSubmissionID()
	if err != nil {
		die(err)
	}
	t, _, err := sendInstruction(cr, agentID, sid, map[string]any{"submission_id": sid, "text": withTemplate(tmpl, task)})
	if err != nil {
		ce := classify(err)
		ce.WorkStarted = workYes
		ce.Message = fmt.Sprintf("%s started in session %s, but its first instruction was not sent: %s", name, rec.ID, ce.Message)
		ce.NextAction = fmt.Sprintf("ks agent send %s \"<task>\"", name)
		die(ce)
	}
	doc := map[string]any{"agent": agentID, "name": name, "session": rec.ID, "parent": parent, "outlive_parent": outlive, "notify": notify,
		"first_message": map[string]any{"sent": true, "task_id": t.ID}, "template": tmpl.Name, "metered": fmt.Sprintf(meteredLine, name),
		"harness": firstNonEmptyStr(rec.Harness, harness.ID), "runner_model": rec.RunnerModel, "runner_effort": rec.RunnerEffort}

	// ---- 6. back at once, or follow ----------------------------------------
	if inv.Bool("bg") {
		var st *statusRecord
		if s, serr := readAgentStatus(cr, rec.ID, agentID); serr == nil {
			st = &s
		}
		doc["status"] = st
		emit(doc, func() {
			fmt.Printf("started %s (agent %s) in session %s%s; its first instruction is task %s\n", name, agentID, rec.ID, parentWords(parent), t.ID)
			fmt.Printf("next: ks agent wait %s, then ks agent read %s\n", name, name)
		})
		return
	}
	w := waitForTask(cr, rec.ID, agentID, t.ID, timeout)
	w.Agent = name
	doc["status"] = w.Status
	doc["wait"] = w
	if w.Reason == "timeout" {
		fail(&cliError{Code: exitTemporary, Kind: "still_running", WorkStarted: workYes, Detail: doc,
			Message:    fmt.Sprintf("still running as %s (session %s) after %s", name, rec.ID, timeout),
			NextAction: "ks agent wait " + name})
	}
	emit(doc, func() {
		fmt.Printf("%s (agent %s) in session %s%s\n", name, agentID, rec.ID, parentWords(parent))
		printWaitResult(w, timeout)
		fmt.Printf("its reply: ks agent read %s\n", name)
	})
}

func parentWords(p *childParent) string {
	switch {
	case p == nil:
		return ", with no parent"
	case p.Kind == "agent":
		return ", a child of agent " + p.Ref
	}
	return ", a child of " + p.Label
}

// resolveParentAgent answers the hosted agent --parent names: its id, or a
// name the service resolves to one agent.
func resolveParentAgent(cr hostedCreds, arg string) string {
	if strings.HasPrefix(arg, "agent_") {
		return arg
	}
	_, a := resolveOpenTarget(cr, arg, "")
	return a.ID
}

// refuseTakenOrFull refuses, before anything is created, a name this caller
// already uses for a child that still exists, and a ninth child.
func refuseTakenOrFull(cr hostedCreds, c callerIdentity, name string) {
	r, err := readCallerRecord(c.ID)
	if err != nil {
		die(err)
	}
	live := 0
	for _, ch := range r.Children {
		st, serr := readAgentStatus(cr, ch.SessionID, ch.AgentID)
		if serr == nil && st.State == "clear" {
			continue
		}
		if ch.Name == name {
			fail(&cliError{Code: exitConflict, Kind: "name_taken",
				Message:    fmt.Sprintf("%s already has a child named %s (session %s); nothing was created", c.Label, name, ch.SessionID),
				NextAction: fmt.Sprintf("ks agent send %s \"<text>\", or choose another --name", name)})
		}
		if !ch.Outlive {
			live++
		}
	}
	if live >= maxChildren {
		fail(&cliError{Code: exitConflict, Kind: "children_limit",
			Message:    fmt.Sprintf("%s already runs %d children, the most one parent may run; nothing was created", c.Label, live),
			NextAction: "ks agent wait --children all"})
	}
}

// setParent records the child's parent on its agent, against the agent's
// revision.
func setParent(cr hostedCreds, rec createdRecord, p childParent, outlive, notify bool) error {
	agents, err := fetchAgents(cr, rec.ID)
	if err != nil {
		return err
	}
	var rev int64
	found := false
	for _, a := range agents {
		if a.ID == rec.PrimaryAgentID || (rec.PrimaryAgentID == "" && a.IsPrimary) {
			rev, found = a.Revision, true
			rec.PrimaryAgentID = a.ID
		}
	}
	if !found {
		return fmt.Errorf("session %s lists no primary agent to record a parent on", rec.ID)
	}
	body := map[string]any{"outlive_parent": outlive, "notify": notify, "expected_revision": rev}
	if p.Kind == "agent" {
		body["parent_agent_id"] = p.Ref
	} else {
		body["caller_id"] = p.Ref
	}
	return hostedMutate(cr, "PUT", "/api/v2/agents/"+url.PathEscape(rec.PrimaryAgentID)+"/parent", body, nil)
}
