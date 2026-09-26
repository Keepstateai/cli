// agent.go: the agent window on the command line. A session holds one or
// more agents; this group lists them, opens one, reports one without
// following anything, decides the permission requests one is waiting on,
// queues an instruction for one, and saves and parks the session one
// lives in.
//
// Four properties shape every verb here. Opening an agent CREATES
// NOTHING: the control plane answers the same agent for the same name, so
// opening twice is one agent and two windows. Control is a lease, not a
// mode: a window either holds it or is watching, a window that is
// watching says so rather than pretending it can steer, and an
// instruction typed into a window carries that lease so the service can
// refuse it the moment it is no longer the current one — a displaced
// window submits nothing at all rather than quietly submitting as a
// stranger. A decision is bound to the exact action a PERSON WAS SHOWN:
// the client remembers what it displayed, reads the request again before
// deciding, and COMPARES; an action that changed is displayed and asked
// again, never decided, so nobody ever approves a request that changed
// under them and nothing is ever decided that the person did not name.
// And detaching is local: Ctrl-C closes this window, prints that it did,
// and leaves the agent working — nothing here ever cancels remote work.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------
// what the control plane reports
// ---------------------------------------------------------------------

type agentRow struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	Name          string `json:"name"`
	IsPrimary     bool   `json:"is_primary"`
	Activity      string `json:"activity"`
	ObservedAt    string `json:"observed_at"`
	ActiveTaskID  string `json:"active_task_id"`
	QueueRevision int64  `json:"queue_revision"`
	Revision      int64  `json:"revision"`
	CreatedAt     string `json:"created_at"`
	// KS-038: whether a window holds control ("held" or "none") and whether
	// the agent could take a consultation now, as the service reads them
	Controller   string `json:"controller,omitempty"`
	Consultation string `json:"consultation,omitempty"`
}

// agentLease is the control lease of one window: the authority to steer
// this agent, at this fence, until this moment. Its token authorises
// steering, so it is held in memory for the requests that need it and is
// never printed, never emitted in a document, and never written to the
// operations journal.
type agentLease struct {
	ID        string `json:"lease_id"`
	Fence     int64  `json:"fence"`
	Token     string `json:"lease_token"`
	ExpiresAt string `json:"expires_at"`
	Mode      string `json:"mode"`
}

type agentWindow struct {
	Agent       agentRow    `json:"agent"`
	Reconnected bool        `json:"reconnected"`
	Lease       *agentLease `json:"lease"`
	Resume      struct {
		AfterSeq int64  `json:"after_seq"`
		Cursor   string `json:"cursor"`
	} `json:"resume"`
	QueueDepth int64 `json:"queue_depth"`
	// KS-032: the machine behind the agent, and the recovery view when the
	// agent's own last report says it failed
	Runtime  *openRuntime  `json:"runtime,omitempty"`
	Recovery *openRecovery `json:"recovery,omitempty"`
}

type taskRow struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	SubmissionID string `json:"submission_id"`
	State        string `json:"state"`
	QueueSeq     int64  `json:"queue_seq"`
	Origin       string `json:"origin"`
	ContentRef   string `json:"content_ref"`
	CreatedAt    string `json:"created_at"`
	// Every field below is one the service already returns on this row, so
	// it is READ rather than derived. A client that recomputed an
	// instruction's standing from the fields it happened to know would be
	// stating its own opinion in the service's voice.
	UpdatedAt  string `json:"updated_at"`
	Revision   int64  `json:"revision"`
	AuthorType string `json:"author_type"`
	AuthorID   string `json:"author_id"`
	// HeldReason is the service's own word for why this instruction is not
	// moving. It is empty when the service recorded none, and an empty one
	// is reported as not recorded, never filled in from the state.
	HeldReason string `json:"held_reason"`
	// Verification is an INDEPENDENT verifier's finding. A runner may not
	// write it and this client never infers it: an instruction that
	// finished without one reads Finished, never Verified.
	Verification string `json:"verification_state"`
	// CurrentAttempt is the attempt identity the service holds for this
	// instruction, or empty when it holds none. Empty is stated as "the
	// service records none" and is never rendered as attempt zero.
	CurrentAttempt string `json:"current_attempt_id"`
	ContentHash    string `json:"content_hash"`
	// CancelRecovery is present on an instruction that reads cancelling:
	// where the stop stands against C04's interrupt wait and, once that has
	// passed, the explicit recovery actions (KS-044). Read, never inferred.
	CancelRecovery *cancelRecovery `json:"cancel_recovery,omitempty"`
}

// submittedTask is a task as the submission route answers it: the task,
// plus whether this submission id had already been accepted, in which case
// the original task comes back and nothing was queued twice.
type submittedTask struct {
	taskRow
	Replayed bool `json:"replayed"`
	// Runtime is present only when the session is not running (KS-041): the
	// instruction is accepted and HELD, the session was not woken, and
	// NextAction names the explicit action that would run it.
	Runtime *submissionRuntime `json:"runtime,omitempty"`
}

type submissionRuntime struct {
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	Woken      bool   `json:"woken"`
	NextAction string `json:"next_action,omitempty"`
	Note       string `json:"note"`
}

type approvalRow struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	Kind            string          `json:"kind"`
	Summary         string          `json:"summary"`
	ArgumentsJSON   json.RawMessage `json:"arguments_json,omitempty"`
	ExpiresAt       string          `json:"expires_at"`
	State           string          `json:"state"`
	Revision        int64           `json:"revision"`
	ExactActionHash string          `json:"exact_action_hash"`
	// What a person deciding needs on the approval itself (KS-047), as the
	// service states it: the instruction that was running when it was asked,
	// what the exact action touches, and what deciding costs.
	RequestedForTask string   `json:"requested_for_task,omitempty"`
	Affects          []string `json:"affects,omitempty"`
	CostImplication  string   `json:"cost_implication,omitempty"`
	ActionableUntil  string   `json:"actionable_until,omitempty"`
	DecidedBy        string   `json:"decided_by,omitempty"`
	DecidedAt        string   `json:"decided_at,omitempty"`
}

// UnmarshalJSON reads the approval as the control plane's route table
// serves it (action_kind, human_scope, arguments, decision) as well as the
// shorter names this client used first (kind, summary, arguments_json,
// state). Where both are present the service's own field wins; neither is
// ever invented.
func (a *approvalRow) UnmarshalJSON(b []byte) error {
	type plain approvalRow
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var svc struct {
		ActionKind string          `json:"action_kind"`
		HumanScope string          `json:"human_scope"`
		Arguments  json.RawMessage `json:"arguments"`
		Decision   string          `json:"decision"`
	}
	_ = json.Unmarshal(b, &svc)
	if svc.ActionKind != "" {
		p.Kind = svc.ActionKind
	}
	if svc.HumanScope != "" {
		p.Summary = svc.HumanScope
	}
	if len(svc.Arguments) > 0 && string(svc.Arguments) != "null" {
		p.ArgumentsJSON = svc.Arguments
	}
	if svc.Decision != "" {
		p.State = svc.Decision
	}
	*a = approvalRow(p)
	return nil
}

// journalEvent is one row of the session's journal, as the stream serves it.
// The correlation fields are read rather than re-derived: which instruction,
// which attempt and which execution generation an event belongs to are facts
// the service records, and a client that inferred them from ordering would
// be guessing at exactly the joins this journal exists to make possible.
type journalEvent struct {
	EventID     string          `json:"event_id"`
	StreamSeq   int64           `json:"stream_seq"`
	SubjectType string          `json:"subject_type"`
	SubjectID   string          `json:"subject_id"`
	TaskID      string          `json:"task_id"`
	AttemptID   string          `json:"attempt_id"`
	Epoch       int64           `json:"epoch"`
	Source      string          `json:"source"`
	ObservedAt  string          `json:"observed_at"`
	RecordedAt  string          `json:"recorded_at"`
	Payload     json.RawMessage `json:"payload"`
}

func (e journalEvent) kind() string {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	return p.Type
}

// ---------------------------------------------------------------------
// resolution: which session, which agent
// ---------------------------------------------------------------------

// agentSessionID is the id the agent routes are keyed by: the session's
// workspace record where the inventory reports one, and the session's own
// id otherwise, so an account whose inventory predates the record still
// reaches its agents.
func agentSessionID(r inventoryRow) string {
	if r.RecordID != "" {
		return r.RecordID
	}
	return r.ID
}

// agentSession answers the one session a command means, in the KS-022
// order (binding.go): --session through the same resolution the session
// verbs use (a full id, or a prefix unique among the account's sessions);
// else this project's binding; else a choice at a terminal; else an error.
// A target that --session did not name is shown before anything acts.
func agentSession(cr hostedCreds, inv *Invocation) inventoryRow {
	r, how, explicit := targetSession(cr, inv.Str("session"))
	if !explicit {
		showTarget(r, how)
	}
	return r
}

func fetchAgents(cr hostedCreds, sessionID string) ([]agentRow, error) {
	var env struct {
		Data struct {
			Items []agentRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"session_id": {sessionID}}
	if err := hostedCall(cr, "GET", "/api/v2/agents?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// resolveAgent answers the one agent an argument names inside a session:
// its id, or its name when exactly one agent carries it. Two agents of one
// name is an error that names them; nothing is chosen for the caller.
func resolveAgent(cr hostedCreds, sess inventoryRow, arg string) (agentRow, error) {
	if arg == "" {
		return agentRow{}, &cliError{Code: exitUsage, Kind: "usage", Message: "an agent name is required", NextAction: "ks agent list --session " + sess.ShortID}
	}
	agents, err := fetchAgents(cr, agentSessionID(sess))
	if err != nil {
		return agentRow{}, err
	}
	return pickAgent(sess, agents, arg)
}

// pickAgent is resolveAgent's decision over a list ALREADY READ, so a
// caller that has the session's agents in hand resolves a name against the
// same view it is about to show rather than against a second read that may
// have moved underneath it.
func pickAgent(sess inventoryRow, agents []agentRow, arg string) (agentRow, error) {
	if arg == "" {
		return agentRow{}, &cliError{Code: exitUsage, Kind: "usage", Message: "an agent name is required", NextAction: "ks agent list --session " + sess.ShortID}
	}
	var byName []agentRow
	for _, a := range agents {
		if a.ID == arg {
			return a, nil
		}
		if a.Name != "" && a.Name == arg {
			byName = append(byName, a)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return agentRow{}, &cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("session %s has no agent named %q", sess.ShortID, arg),
			NextAction: "ks agent list --session " + sess.ShortID}
	}
	sort.Slice(byName, func(i, j int) bool { return byName[i].ID < byName[j].ID })
	var ids []string
	for _, a := range byName {
		ids = append(ids, a.ID)
	}
	return agentRow{}, &cliError{Code: exitUsage, Kind: "ambiguous",
		Message:    fmt.Sprintf("%d agents in session %s are named %q: %s; give the id", len(byName), sess.ShortID, arg, strings.Join(ids, ", ")),
		NextAction: "ks agent list --session " + sess.ShortID}
}

func fetchTasks(cr hostedCreds, agentID string) ([]taskRow, error) {
	var env struct {
		Data struct {
			Items []taskRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"agent_id": {agentID}}
	if err := hostedCall(cr, "GET", "/api/v2/tasks?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func fetchPendingApprovals(cr hostedCreds, sessionID string) ([]approvalRow, error) {
	var env struct {
		Data struct {
			Items []approvalRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"session_id": {sessionID}, "state": {"pending"}}
	if err := hostedCall(cr, "GET", "/api/v2/approvals?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// finishedTaskStates are the states the service reports for work that is
// over. A state this client does not know counts as waiting, because
// reporting a queue as shorter than it is would be the wrong mistake.
var finishedTaskStates = map[string]bool{
	"done": true, "completed": true, "succeeded": true, "failed": true,
	"cancelled": true, "canceled": true, "rejected": true, "expired": true,
}

// queueState reads the task list into the two facts a person asks for:
// what the agent is working on, and how much is waiting behind it.
func queueState(a agentRow, tasks []taskRow) (waiting int, current *taskRow) {
	for i := range tasks {
		if a.ActiveTaskID != "" && tasks[i].ID == a.ActiveTaskID {
			current = &tasks[i]
			continue
		}
		if !finishedTaskStates[strings.ToLower(tasks[i].State)] {
			waiting++
		}
	}
	return waiting, current
}

// ---------------------------------------------------------------------
// ks agent list
// ---------------------------------------------------------------------

func agentRole(a agentRow) string {
	if a.IsPrimary {
		return "primary"
	}
	return "-"
}

func agentLine(a agentRow) string {
	task := a.ActiveTaskID
	if task == "" {
		task = "none"
	}
	line := fmt.Sprintf("%-16s %-16s %-11s %-8s %s", clip(a.Name, 16), clip(a.ID, 16), clip(stateCell("agent_activity", a.Activity), 11), agentRole(a), task)
	if a.Controller != "" || a.Consultation != "" {
		line += fmt.Sprintf("  control %s · advice %s", figure(a.Controller), figure(a.Consultation))
	}
	return line
}

func hostedAgentList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	agents, err := fetchAgents(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	emit(map[string]any{"session": sess.ID, "agents": agents, "count": len(agents)}, func() {
		if len(agents) == 0 {
			fmt.Printf("No agents in session %s.\n", sess.ShortID)
			return
		}
		fmt.Printf("%-16s %-16s %-11s %-8s %s\n", "AGENT", "ID", "ACTIVITY", "ROLE", "CURRENT TASK")
		for _, a := range agents {
			fmt.Println(agentLine(a))
		}
		fmt.Printf("%d agent(s); open one: ks agent open <name> --session %s\n", len(agents), sess.ShortID)
	})
}

// ---------------------------------------------------------------------
// ks agent status
// ---------------------------------------------------------------------

func hostedAgentStatus(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	show := func(a agentRow) {
		tasks, err := fetchTasks(cr, a.ID)
		if err != nil {
			die(err)
		}
		waiting, current := queueState(a, tasks)
		age, stale := observedAge(a.ObservedAt, time.Now())
		emit(map[string]any{"session": sess.ID, "agent": a, "queue_depth": waiting, "current_task": current, "tasks": len(tasks), "observed_age": age, "stale": stale}, func() {
			fmt.Printf("agent %s (%s) in session %s\n", a.Name, a.ID, sess.ShortID)
			fmt.Printf("  activity       %s (%s)\n", stateLabel("agent_activity", a.Activity), age)
			if stale {
				fmt.Printf("  STALE          no observation for more than %s: this is the last known state, not the current one\n", staleAfter)
			}
			fmt.Printf("  role           %s\n", agentRole(a))
			fmt.Printf("  queue          %d waiting\n", waiting)
			if current != nil {
				fmt.Printf("  current task   %s (%s) at queue position %d\n", current.ID, stateLabel("task_state", current.State), current.QueueSeq)
			} else {
				fmt.Printf("  current task   none\n")
			}
			fmt.Printf("  revision       %d (queue %d)\n", a.Revision, a.QueueRevision)
			fmt.Printf("  observed       %s\n", figure(a.ObservedAt))
		})
	}
	show(a)
	if !inv.Bool("watch") {
		return
	}
	// --watch: read again every 2 s; Ctrl-C stops watching and nothing
	// remote changes (VER-037-2)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	cadence := pollCadence(boundedWait())
	for {
		select {
		case <-stop:
			fmt.Fprintln(os.Stderr, "stopped watching; nothing on the agent changed")
			return
		case <-time.After(cadence):
		}
		now, err := resolveAgent(cr, sess, a.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "the agent could not be read (%s); the last state above is kept\n", errText(err))
			continue
		}
		show(now)
	}
}

// staleAfter is C04's stale threshold for a live status.
const staleAfter = 15 * time.Second

// observedAge says how old an observation is, and whether it is stale.
func observedAge(observed string, now time.Time) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return "observed at an unknown time", true
	}
	age := now.Sub(t).Round(time.Second)
	if age < 0 {
		age = 0
	}
	return "observed " + age.String() + " ago", age > staleAfter
}

// ---------------------------------------------------------------------
// ks agent open
// ---------------------------------------------------------------------

// openAgent asks for the window. The route never creates an agent, so a
// second open of the same name is the same agent; what it settles is
// whether this window holds the control lease, and where in the journal
// the window should resume from.
func openAgent(cr hostedCreds, sess inventoryRow, a agentRow, take bool) (*agentWindow, error) {
	var env struct {
		Data agentWindow `json:"data"`
	}
	err := hostedCall(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/open", map[string]any{"take_control": take}, &env)
	if err == nil {
		return &env.Data, nil
	}
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_controller_held":
			// another window is steering this agent. Taking control from it
			// is a decision, never a retry the client makes for you.
			return nil, &cliError{Code: exitConflict, Kind: "controller_held",
				Message: fmt.Sprintf("another window holds control of agent %s: %s", a.Name, sanitize(he.Message)),
				NextAction: fmt.Sprintf("ks agent open %s --session %s --view (to watch beside it), or --take-control (to steer)",
					a.Name, sess.ShortID)}
		case "ks_not_found":
			return nil, &cliError{Code: exitUsage, Kind: "not_found",
				Message:    fmt.Sprintf("agent %s is no longer in session %s", a.Name, sess.ShortID),
				NextAction: "ks agent list --session " + sess.ShortID}
		}
	}
	return nil, err
}

// openAgentView opens a WATCHING window: the service answers the agent and
// the journal position and grants no lease, so this window steers nothing and
// can open beside the window that holds control (C02 OpenView: two views on
// one agent). Closing it touches the runner not at all.
func openAgentView(cr hostedCreds, sess inventoryRow, a agentRow) (*agentWindow, error) {
	var env struct {
		Data agentWindow `json:"data"`
	}
	err := hostedCall(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/open", map[string]any{"view_only": true}, &env)
	if err == nil {
		env.Data.Lease = nil // a watching window holds nothing, whatever it is told
		return &env.Data, nil
	}
	var he *hostedErr
	if errors.As(err, &he) && he.Type == "ks_not_found" {
		return nil, &cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("agent %s is no longer in session %s", a.Name, sess.ShortID),
			NextAction: "ks agent list --session " + sess.ShortID}
	}
	return nil, err
}

// controlLine says, in one phrase, whether this window may steer.
func controlLine(w *agentWindow) string {
	if w.Lease == nil {
		return "watching (another window holds control)"
	}
	mode := w.Lease.Mode
	if mode == "" {
		mode = "control"
	}
	return fmt.Sprintf("you hold control (lease %s, %s)", w.Lease.ID, mode)
}

// controlFacts is the lease as a script may read it: everything except the
// token, which authorises steering and therefore never leaves the process.
func controlFacts(w *agentWindow) map[string]any {
	if w.Lease == nil {
		return map[string]any{"held": false}
	}
	return map[string]any{"held": true, "lease_id": w.Lease.ID, "fence": w.Lease.Fence, "expires_at": w.Lease.ExpiresAt, "mode": w.Lease.Mode}
}

// actionShown is the action itself, as a person reads it: what kind of
// action it is, what it says it will do, and the exact arguments it
// carries. Everything that makes one action a DIFFERENT action is in
// here, because this is both what is displayed and what a decision is
// compared against. The expiry is deliberately not part of it: a clock
// moving on is not a different action.
func actionShown(ap approvalRow) string {
	line := fmt.Sprintf("%s — %s", figure(ap.Kind), figure(ap.Summary))
	if args := compactPayload(json.RawMessage(ap.ArgumentsJSON)); args != "" {
		line += " " + args
	}
	return sanitize(line)
}

// approvalHuman renders one pending permission request and how to decide
// it from where the reader is standing: inside a live window that is a
// keystroke, outside one it is a verb.
func approvalHuman(ap approvalRow, decideWith string) string {
	return sanitize(fmt.Sprintf("!! permission request %s: %s (expires %s); the agent waits until it is decided: %s",
		ap.ID, actionShown(ap), figure(ap.ExpiresAt), decideWith))
}

// ---------------------------------------------------------------------
// what was SHOWN: the only thing a decision may be compared against
// ---------------------------------------------------------------------

// shownAction is one permission request as this client PUT IT IN FRONT OF
// A PERSON: the service's hash of the exact action, and a hash of the
// action as it was rendered on the terminal. Only the hashes are kept —
// the action's own words stay where the service holds them, the way the
// operations journal keeps a hash of an instruction and never its body.
//
// A decision is compared against THIS. Reading the request again and
// sending back whatever the service says now would make the comparison
// vacuous: it is exactly how an action that changed after it was read
// gets approved by someone who never saw it.
type shownAction struct {
	CTL       string `json:"ctl"`
	ID        string `json:"approval_id"`
	Hash      string `json:"action_hash"`
	ActionSHA string `json:"action_sha256"`
	Revision  int64  `json:"revision"`
	ShownAt   string `json:"shown_at"`
}

func shownActionsPath() string { return filepath.Join(configDir(), "approvals-shown.jsonl") }

func shownFrom(cr hostedCreds, ap approvalRow) shownAction {
	return shownAction{CTL: cr.CTL, ID: ap.ID, Hash: ap.ExactActionHash,
		ActionSHA: sha256Hex([]byte(actionShown(ap))), Revision: ap.Revision,
		ShownAt: time.Now().UTC().Format(time.RFC3339)}
}

// recordShownAction appends one display to the local record, so a request
// read in one terminal is still compared against what was read when it is
// decided in another.
//
// A failure to write is not fatal and is not reported: the guarantee does
// not rest on this file. Where nothing was recorded, the decision path
// displays the request itself and compares against that display, so an
// unwritable configuration directory makes the client more careful, never
// less.
func recordShownAction(s shownAction) {
	if s.ID == "" {
		return
	}
	if os.MkdirAll(configDir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(shownActionsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(s)
	_, _ = f.Write(append(b, '\n'))
}

// lastShownAction answers the most recent display of one request on this
// control plane. A line this client cannot read is skipped: the record is
// something to read, never a lock to hold.
func lastShownAction(cr hostedCreds, id string) (shownAction, bool) {
	b, err := os.ReadFile(shownActionsPath())
	if err != nil {
		return shownAction{}, false
	}
	var found shownAction
	ok := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s shownAction
		if json.Unmarshal([]byte(line), &s) == nil && s.ID != "" && s.ID == id && s.CTL == cr.CTL {
			found, ok = s, true
		}
	}
	return found, ok
}

// sameAction reports whether the request as it reads NOW is the action
// that was shown. Both halves must agree: the service's own hash of the
// exact action, and the rendering the person actually read. Either one
// differing is a different action.
func sameAction(s shownAction, ap approvalRow) bool {
	return s.Hash == ap.ExactActionHash && s.ActionSHA == sha256Hex([]byte(actionShown(ap)))
}

// rememberShown records every pending request a window has just put on
// the screen, so deciding one later compares against what was on it.
func rememberShown(cr hostedCreds, pending []approvalRow) {
	for _, ap := range pending {
		recordShownAction(shownFrom(cr, ap))
	}
}

// inWindowDecision is the line that tells a person in a live window what
// to type to decide one request, and decideVerbs is the same thing for a
// terminal that is not following anything.
func inWindowDecision(ap approvalRow) string {
	return fmt.Sprintf("type \"a %s\" to approve or \"d %s\" to deny", ap.ID, ap.ID)
}

func decideVerbs(sess inventoryRow, ap approvalRow) string {
	return fmt.Sprintf("ks agent approve %s --session %s (or ks agent deny %s --session %s)", ap.ID, sess.ShortID, ap.ID, sess.ShortID)
}

func hostedAgentOpen(cr hostedCreds, inv *Invocation) {
	var sess inventoryRow
	var a agentRow
	var err error
	bound, _, _ := currentBinding(cr)
	if inv.Str("session") == "" && bound == nil {
		// no session named and none bound: the service resolves the name
		// across what you may see, and chooses nothing (KS-032)
		sess, a = resolveOpenTarget(cr, inv.Arg(0), inv.Str("project"))
		showTarget(sess, "the name "+inv.Arg(0))
	} else {
		if inv.Str("project") != "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--project narrows a search by name; with --session (or a project binding) the session is already named"})
		}
		sess = agentSession(cr, inv)
		if a, err = resolveAgent(cr, sess, inv.Arg(0)); err != nil {
			die(err)
		}
	}
	if inv.Bool("view") && inv.Bool("take-control") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--view watches and --take-control steers; a window is one or the other",
			NextAction: fmt.Sprintf("ks agent open %s --session %s --view", a.Name, sess.ShortID)})
	}
	var w *agentWindow
	if inv.Bool("view") {
		w, err = openAgentView(cr, sess, a)
	} else {
		w, err = openAgent(cr, sess, a, inv.Bool("take-control"))
	}
	if err != nil {
		die(err)
	}
	// OPENING NEVER WAKES A SESSION (KS-032). A session that is not running
	// is shown with the actions the service names, and only --resume asks
	// for the resume route.
	if w.Runtime != nil && !w.Runtime.Running {
		resumable := false
		for _, ac := range w.Runtime.Actions {
			if ac.Action == "resume_session" {
				resumable = true
			}
		}
		if inv.Bool("resume") && resumable {
			for _, ac := range w.Runtime.Actions {
				if ac.Action == "resume_session" && ac.Discloses != "" {
					progress("%s", sanitize(ac.Discloses))
				}
			}
			r, rerr := postResume(cr, agentSessionID(sess))
			if rerr != nil {
				die(rerr)
			}
			progress("session %s is resuming because you asked with --resume (state: %s)", sess.ShortID, figure(r["runtime_state"]))
		} else {
			if inv.Bool("resume") {
				fail(&cliError{Code: exitConflict, Kind: "not_resumable", Message: fmt.Sprintf("session %s is %s and cannot be resumed: %s", sess.ShortID, stateLabel("session_runtime", w.Runtime.State), sanitize(w.Runtime.Note))})
			}
			emit(map[string]any{"session": sess.ID, "agent": w.Agent, "runtime": w.Runtime, "recovery": w.Recovery, "woken": false}, func() {
				fmt.Printf("agent %s (%s) in session %s: the session is %s and was NOT woken\n", a.Name, a.ID, sess.ShortID, stateLabel("session_runtime", w.Runtime.State))
				fmt.Printf("  %s\n", sanitize(w.Runtime.Note))
				if w.Runtime.LastSavedAt != "" {
					fmt.Printf("  last saved %s (%s)\n", w.Runtime.LastSavedAt, figure(w.Runtime.CheckpointID))
				}
				printOpenActions(w.Runtime.Actions, a.Name, sess)
			})
			return
		}
	}
	if w.Recovery != nil {
		emit(map[string]any{"session": sess.ID, "agent": w.Agent, "runtime": w.Runtime, "recovery": w.Recovery}, func() {
			fmt.Printf("agent %s (%s) in session %s reads %s: %s\n", a.Name, a.ID, sess.ShortID, stateLabel("agent_activity", w.Recovery.Activity), sanitize(w.Recovery.Note))
			printOpenActions(w.Recovery.Actions, a.Name, sess)
		})
		return
	}
	joined := "opened"
	if w.Reconnected {
		joined = "rejoined"
	}
	// the header is a fact about the window, not a result: stderr, so a
	// piped stdout carries the agent's events and nothing else
	age, stale := observedAge(w.Agent.ObservedAt, time.Now())
	progress("agent %s (%s) in session %s · %s (%s) · %s · %s · %d queued",
		a.Name, a.ID, sess.ShortID, stateLabel("agent_activity", w.Agent.Activity), age, joined, controlLine(w), w.QueueDepth)
	if stale {
		progress("STALE: no observation of this agent for more than %s; the status above is the last known one", staleAfter)
	}

	// WHETHER THE QUEUE IS MOVING. A window showing "3 queued" beside an
	// agent that cannot start any of them is telling a person the number
	// and withholding the fact. A hold that cannot be READ is said to be
	// unreadable and is never rendered as "nothing is holding it".
	held, heldErr := heldSummary(cr, a.ID, sess)
	if heldErr != "" {
		progress("whether this queue is held could not be read, so it is not stated: %s", heldErr)
	} else if held != "" {
		progress("%s", held)
	}

	pending, perr := fetchPendingApprovals(cr, agentSessionID(sess))
	if perr != nil {
		progress("the pending permission requests could not be read: %s", sanitize(perr.Error()))
	}
	// both shapes below put these requests in front of a person, so both
	// are a display: what was displayed is what a later decision on one of
	// them has to match.
	rememberShown(cr, pending)

	if inv.Bool("no-follow") {
		emit(map[string]any{
			"session": sess.ID, "agent": w.Agent, "reconnected": w.Reconnected,
			"control": controlFacts(w), "queue_depth": w.QueueDepth,
			"resume":            map[string]any{"after_seq": w.Resume.AfterSeq, "cursor": w.Resume.Cursor},
			"approvals_pending": pending,
		}, func() {
			fmt.Printf("agent %s (%s) in session %s\n", a.Name, a.ID, sess.ShortID)
			fmt.Printf("  activity       %s\n", figure(w.Agent.Activity))
			fmt.Printf("  control        %s\n", controlLine(w))
			fmt.Printf("  queue          %d queued\n", w.QueueDepth)
			switch {
			case heldErr != "":
				fmt.Printf("  held           could not be read, so it is not stated: %s\n", heldErr)
			case held != "":
				fmt.Printf("  held           %s\n", held)
			default:
				fmt.Printf("  held           no hold stands on this queue\n")
			}
			fmt.Printf("  events from    %d\n", w.Resume.AfterSeq)
			for _, ap := range pending {
				fmt.Println(approvalHuman(ap, decideVerbs(sess, ap)))
			}
			fmt.Printf("follow it: ks agent open %s --session %s\n", a.Name, sess.ShortID)
		})
		return
	}
	// WHAT ALREADY HAPPENED, before what happens next. A window that began
	// at the live edge showed a person an empty screen and called it an
	// agent: the conversation they opened the window to read had already
	// been written to the journal. This renders the tail of it, marked as
	// history so nothing on the screen pretends to be arriving now.
	showBacklog(cr, sess, w.Resume.AfterSeq)
	for _, ap := range pending {
		emitLine(map[string]any{"type": "approval_pending", "approval": ap}, approvalHuman(ap, inWindowDecision(ap)))
	}
	win := &liveWindow{sess: sess, agent: a, lease: w.Lease}
	followAgent(cr, win, w)
}

// heldSummary answers, in one sentence, whether this agent's queue is
// moving — and says so from the service's own hold record rather than from
// the queue depth, which cannot carry the difference.
func heldSummary(cr hostedCreds, agentID string, sess inventoryRow) (string, string) {
	holds, err := fetchQueueHolds(cr, agentID)
	if err != nil {
		return "", sanitize(err.Error())
	}
	h, aerr := activeHold(holds)
	if aerr != nil {
		return "", sanitize(aerr.Error())
	}
	if h == nil {
		return "", ""
	}
	what := h.BlockingTask
	if what == "" {
		what = "an instruction the service did not name"
	}
	n := len(h.HeldTasks)
	return fmt.Sprintf("this queue is HELD behind %s (%s); %d instruction(s) wait and no worker may start one. Read it: ks agent queue show %s --session %s",
		what, figure(h.Cause), n, "<name>", sess.ShortID), ""
}

// backlogMax bounds what a window prints before the live edge. A person
// opening a window wants the end of the conversation, not all of it; the
// whole of it is in the journal and in --json.
const backlogMax = 30

// showBacklog renders the tail of the conversation that already happened,
// marked as history. It is best effort in exactly one direction: a journal
// that cannot be read costs the screen its history and SAYS so, and is
// never rendered as an agent that has said nothing.
func showBacklog(cr hostedCreds, sess inventoryRow, upto int64) {
	if upto <= 0 {
		return
	}
	var env struct {
		Data struct {
			Items []journalEvent `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"limit": {"400"}}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess))+"/events?"+q.Encode(), nil, &env); err != nil {
		progress("what already happened could not be read, so none of it is shown; that is not the same as nothing having happened: %s", sanitize(err.Error()))
		return
	}
	var past []journalEvent
	for _, e := range env.Data.Items {
		if e.StreamSeq <= upto {
			past = append(past, e)
		}
	}
	if len(past) == 0 {
		return
	}
	if len(past) > backlogMax {
		progress("... %d earlier entries are in the journal and are not printed here", len(past)-backlogMax)
		past = past[len(past)-backlogMax:]
	}
	progress("--- what already happened (history; nothing below is arriving now) ---")
	for _, e := range past {
		emitLine(e, agentEventLine(e))
	}
	progress("--- live from here ---")
}

// followAgent renders the session journal from where the window resumes,
// one line per event, until the stream ends or the person detaches, while
// two more goroutines keep the window a window: one reads what is typed
// into it, one renews the control lease for as long as it is held.
//
// Ctrl-C (and SIGTERM) detach: they close THIS window. The agent keeps
// working, nothing is cancelled, and the exit is a success, because
// detaching is what was asked for. The signal is watched on its own
// goroutine because the reader blocks on the stream rather than polling,
// which is the one difference from the wait loop in operations.go.
func followAgent(cr hostedCreds, win *liveWindow, w *agentWindow) {
	sess := win.sess
	// the window reads whole lines in the terminal's ordinary mode; its
	// settings are saved anyway and put back on every way out (KS-033), and
	// the size of the window holding control is reported on the lease's
	// control channel at open and on each settled resize
	restoreTerm := saveTerminal()
	stopSizes := startSizeReports(cr, win)
	restore := func() { stopSizes(); restoreTerm() }
	// Ctrl-C (C11, QA-035-3) INTERRUPTS the current work: the same control
	// request as ks task cancel on the instruction in flight, never text sent
	// to the agent, and what it did is shown. Leaving the window is its own
	// key (q). A terminating signal (SIGTERM, a closed terminal) leaves.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		for s := range sig {
			if s == os.Interrupt {
				win.interrupt(cr)
				continue
			}
			restore()
			fmt.Fprintln(os.Stderr, "[left the window; the agent keeps working]")
			os.Exit(exitOK)
		}
	}()
	go win.renew(cr)
	go win.readInput(cr, restore)
	err := streamEvents(cr, agentSessionID(sess), w.Resume.AfterSeq, func(kind string, data []byte) bool {
		switch kind {
		case "hello":
			var hello struct {
				Epoch any `json:"epoch"`
			}
			_ = json.Unmarshal(data, &hello)
			progress("following from event %d (epoch %v); Ctrl-C interrupts the instruction in flight, q leaves the window and the agent keeps working", w.Resume.AfterSeq, figure(hello.Epoch))
			progress("%s", win.legend())
			progress("Ctrl-C interrupts the instruction in flight · q leaves this window (the agent keeps working) · stop the agent: ks agent stop %s · save and pause the session: ks agent pause %s", win.agent.Name, win.agent.Name)
		case "event":
			var e journalEvent
			if json.Unmarshal(data, &e) != nil {
				return false
			}
			emitLine(e, agentEventLine(e))
		case "end":
			progress("the stream ended; the agent keeps working")
			return true
		}
		return false
	})
	restore()
	if err != nil {
		die(err)
	}
}

// transcriptEntry is one thing the agent said or did, as the service
// recorded it on the journal a view reads.
//
// The words are INERT. They are the agent's, stored as data; this client
// renders them and reads nothing in them as an instruction, a command or a
// decision. A view is a bystander to the work.
type transcriptEntry struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	Failed    bool   `json:"failed"`
	Clipped   bool   `json:"clipped"`
	// KS-049: who the control plane attributes the entry to. An entry the
	// service attributes to nobody is shown as unattributed, never as the
	// person's.
	AuthorType     string `json:"author_type"`
	AuthorID       string `json:"author_id"`
	ConsultationID string `json:"consultation_id"`
}

// attribution renders the service's author of an entry, or says there is none.
func attribution(t transcriptEntry) (string, bool) {
	if t.AuthorType == "" || t.AuthorID == "" {
		return "unattributed: the service recorded no author", false
	}
	return t.AuthorType + " " + t.AuthorID, true
}

// transcriptOf answers the conversation entry in a journal row, or nil.
func transcriptOf(e journalEvent) *transcriptEntry {
	if e.kind() != "agent.transcript" {
		return nil
	}
	var t transcriptEntry
	if json.Unmarshal(e.Payload, &t) != nil {
		return nil
	}
	return &t
}

// transcriptLine renders one conversation entry as a person reads it, with
// a marker that says which kind of thing it was. A kind this client does
// not know is shown as itself rather than dropped: a line nobody rendered
// is a line a watcher never learns existed.
func transcriptLine(t transcriptEntry) string {
	mark, body := "·", firstLineOf(t.Text)
	switch t.Kind {
	case "assistant_text":
		mark = "agent"
	case "instruction":
		mark = "asked"
		who, _ := attribution(t)
		body = fmt.Sprintf("[%s, task %s] %s", who, notRecorded(t.TaskID), body)
	case "consultation":
		// another agent's question to this one: never an instruction
		mark = "asked?"
		who, ok := attribution(t)
		if ok {
			who = "question from " + who
		}
		body = fmt.Sprintf("[%s, consultation %s] %s", who, notRecorded(t.ConsultationID), body)
	case "advice":
		mark = "advice"
		who, ok := attribution(t)
		if ok {
			who = "from " + who
		}
		body = fmt.Sprintf("[%s, consultation %s] %s", who, notRecorded(t.ConsultationID), body)
	case "tool_started":
		mark = "tool"
		body = orUnnamedTool(t.ToolName) + " started"
		if s := firstLineOf(t.Text); s != "" {
			body += ": " + s
		}
	case "tool_finished":
		mark = "tool"
		outcome := "finished"
		if t.Failed {
			outcome = "FAILED"
		}
		body = orUnnamedTool(t.ToolName) + " " + outcome
		if s := firstLineOf(t.Text); s != "" {
			body += ": " + s
		}
	case "runner_lifecycle":
		mark = "runner"
	default:
		mark = figure(t.Kind)
	}
	if t.Clipped {
		body += " […clipped by the service]"
	}
	return fmt.Sprintf("%-6s %s", mark, body)
}

func orUnnamedTool(n string) string {
	if strings.TrimSpace(n) == "" {
		return "an unnamed tool"
	}
	return n
}

// firstLineOf keeps a live window readable: the rest of a paragraph is in
// the journal and in --json, which is where a reader goes for it.
func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// agentEventLine renders one journal row for a person: its position, the
// time of day, what it is about, and the payload the service sent,
// compact. A permission request is marked so it cannot be skimmed past.
//
// A CONVERSATION entry is rendered as conversation rather than as a JSON
// blob. The transcript is the view C02 asks for, and a view that showed it
// as a compacted payload was making a person parse the thing the view
// exists to display. Nothing in it is acted on: the agent's words are data.
func agentEventLine(e journalEvent) string {
	head := fmt.Sprintf("%-6d %s", e.StreamSeq, clock(e.ObservedAt))
	if t := transcriptOf(e); t != nil {
		return sanitize(head + " " + transcriptLine(*t))
	}
	line := fmt.Sprintf("%s %s %s", head, figure(e.SubjectType), figure(e.SubjectID))
	if d := compactPayload(e.Payload); d != "" {
		line += " " + d
	}
	if isApprovalEvent(e) {
		line = "!! " + line + " — the agent waits until it is decided: type \"a <id>\" to approve or \"d <id>\" to deny"
	}
	return sanitize(line)
}

func isApprovalEvent(e journalEvent) bool {
	s := strings.ToLower(e.SubjectType)
	return strings.Contains(s, "approval") || strings.Contains(s, "permission")
}

// clock renders an event's time for a live window: the time of day when
// the service sent its usual timestamp, the value itself otherwise.
func clock(ts string) string {
	if len(ts) >= 19 && ts[10] == 'T' {
		return ts[11:19]
	}
	if ts == "" {
		return "unavailable"
	}
	return ts
}

func compactPayload(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	s := b.String()
	if s == "{}" || s == "null" {
		return ""
	}
	return s
}

// streamEvents reads the session's event stream and hands each frame to
// the caller, which reports whether the stream is finished. The body is
// not time-bounded: a live window is as long as the person leaves it open.
func streamEvents(cr hostedCreds, sessionID string, afterSeq int64, onFrame func(kind string, data []byte) bool) error {
	q := url.Values{"after_seq": {strconv.FormatInt(afterSeq, 10)}}
	path := "/api/v2/sessions/" + url.PathEscape(sessionID) + "/events/stream?" + q.Encode()
	resp, err := hostedDo(cr, "GET", path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return hostedError("GET", path, resp, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	kind := "message"
	var data []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 && onFrame(kind, []byte(strings.Join(data, "\n"))) {
				return nil
			}
			kind, data = "message", nil
		case strings.HasPrefix(line, ":"): // a comment, which keep-alives are
		case strings.HasPrefix(line, "event:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// a stream that ended on a frame the blank line never followed: the
	// frame was still sent, so it is still rendered
	if len(data) > 0 {
		onFrame(kind, []byte(strings.Join(data, "\n")))
	}
	return nil
}

// ---------------------------------------------------------------------
// ks agent tell
// ---------------------------------------------------------------------

// newSubmissionID mints the id one instruction will carry for as long as
// it takes to land: 16 random bytes, like the operation keys.
func newSubmissionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// submissionID answers the id this instruction submits under. The id is
// written to the local operation journal BEFORE anything is sent, and an
// instruction retried against the same agent on the same control plane
// reuses the recorded id: the control plane then answers the retry with
// the task it already queued, rather than queueing a second one. This is
// the same discipline as the idempotency keys in operations.go, one level
// up: the key protects the HTTP request, the submission id protects the
// queue.
func submissionID(cr hostedCreds, path, text string) (string, error) {
	sum := sha256Hex([]byte(text))
	for _, o := range recordedOperations() {
		if o.Submission != "" && o.Method == "POST" && o.Path == path && o.CTL == cr.CTL && o.BodySHA == sum {
			return o.Submission, nil
		}
	}
	id, err := newSubmissionID()
	if err != nil {
		return "", err
	}
	op := localOp{Key: "kssub_" + id, Submission: id, Method: "POST", Path: path, BodySHA: sum, CTL: cr.CTL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := recordOperation(op); err != nil {
		return "", fmt.Errorf("could not record the submission locally before sending it (%v); nothing was sent", err)
	}
	return id, nil
}

func hostedAgentTell(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	text := inv.Arg(1)
	if strings.TrimSpace(text) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the instruction is empty",
			NextAction: fmt.Sprintf("ks agent tell %s \"run the tests\" --session %s", a.Name, sess.ShortID)})
	}
	path := "/api/v2/agents/" + url.PathEscape(a.ID) + "/tasks"
	sid, err := submissionID(cr, path, text)
	if err != nil {
		die(err)
	}
	// this process holds no control lease (a window's lease lives in the
	// window), so the instruction is submitted standalone: no origin, no
	// fence, no lease fields at all.
	var env struct {
		Data submittedTask `json:"data"`
	}
	var err2 error
	var resolved bool
	env.Data, resolved, err2 = sendInstruction(cr, a.ID, sid, map[string]any{"submission_id": sid, "text": text})
	if resolved {
		progress("accepted: the service holds submission %s (resolved after a lost acknowledgement)", sid)
	}
	if err := err2; err != nil {
		var he *hostedErr
		if errors.As(err, &he) {
			switch he.Type {
			case "ks_controller_stale":
				die(&cliError{Code: exitConflict, Kind: "controller_stale",
					Message:    fmt.Sprintf("the control of agent %s moved to another window: %s", a.Name, sanitize(he.Message)),
					NextAction: fmt.Sprintf("ks agent open %s --session %s --take-control", a.Name, sess.ShortID)})
			case "ks_queue_full":
				die(&cliError{Code: exitConflict, Kind: "queue_full",
					Message:    fmt.Sprintf("the queue of agent %s is full: %s", a.Name, sanitize(he.Message)),
					NextAction: fmt.Sprintf("ks agent status %s --session %s", a.Name, sess.ShortID)})
			}
		}
		die(err)
	}
	t := env.Data
	// KS-041: accepted work on a session that is not running is HELD, and
	// only an explicit --resume (or ks agent resume) starts the runtime.
	// Acceptance itself never woke anything.
	resumed := map[string]any(nil)
	if inv.Bool("resume") && t.Runtime != nil {
		if !strings.HasSuffix(t.Runtime.NextAction, "/resume") {
			fail(&cliError{Code: exitConflict, Kind: "not_resumable", WorkStarted: workYes,
				Message: fmt.Sprintf("task %s was ACCEPTED and is held (%s), but --resume cannot run it: the session is %s and needs %s, which is not a resume",
					t.ID, sanitize(t.Runtime.Note), figure(t.Runtime.State), figure(t.Runtime.NextAction)),
				NextAction: fmt.Sprintf("ks session show %s", sess.ShortID)})
		}
		progress("task %s accepted and held; resuming session %s because --resume was given", t.ID, sess.ShortID)
		r, rerr := postResume(cr, agentSessionID(sess))
		if rerr != nil {
			ce := classify(rerr)
			ce.WorkStarted = workYes
			ce.Message = fmt.Sprintf("task %s was ACCEPTED and is held, but the session was not resumed: %s", t.ID, ce.Message)
			ce.NextAction = fmt.Sprintf("ks agent resume %s --session %s", a.Name, sess.ShortID)
			die(ce)
		}
		resumed = r
	}
	emit(map[string]any{"session": sess.ID, "agent": a.ID, "submission_id": sid, "replayed": t.Replayed, "task": t.taskRow,
		"accepted": true, "runtime": t.Runtime, "resumed": resumed}, func() {
		word := "queued"
		if t.Replayed {
			word = "already queued"
		}
		if t.Runtime != nil {
			word = "accepted and HELD"
			if t.Replayed {
				word = "already accepted and HELD"
			}
		}
		fmt.Printf("%s: task %s for agent %s at queue position %d (%s)", word, t.ID, a.Name, t.QueueSeq, figure(t.State))
		if t.Replayed {
			fmt.Print("; the same instruction was submitted once")
		}
		fmt.Println()
		if t.Runtime == nil {
			if inv.Bool("resume") {
				fmt.Println("the session is running; --resume had nothing to resume")
			}
			return
		}
		fmt.Printf("  the session is %s and was NOT woken: %s\n", figure(t.Runtime.State), sanitize(t.Runtime.Note))
		switch {
		case resumed != nil:
			fmt.Printf("  resuming session %s (state: %s) because you asked with --resume; session time is metered again\n", sess.ShortID, figure(resumed["runtime_state"]))
		case strings.HasSuffix(t.Runtime.NextAction, "/resume"):
			fmt.Printf("  to run it: ks agent resume %s --session %s (or submit with --resume)\n", a.Name, sess.ShortID)
		default:
			fmt.Printf("  it runs after: %s\n", figure(t.Runtime.NextAction))
		}
	})
}

// ---------------------------------------------------------------------
// ks agent approve | deny: deciding one permission request
// ---------------------------------------------------------------------

// approvalOutcome is what the decision route answers: the request as it
// stands after the decision, and what was decided.
type approvalOutcome struct {
	Approval  approvalRow `json:"approval"`
	Decision  string      `json:"decision"`
	DecidedAt string      `json:"decided_at"`
}

// fetchApproval reads one permission request whole.
func fetchApproval(cr hostedCreds, sess inventoryRow, id string) (approvalRow, error) {
	var env struct {
		Data approvalRow `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/approvals/"+url.PathEscape(id), nil, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && (he.Status == 404 || he.Type == "ks_not_found") {
			return approvalRow{}, &cliError{Code: exitUsage, Kind: "not_found",
				Message:    fmt.Sprintf("no permission request %s in session %s", id, sess.ShortID),
				NextAction: fmt.Sprintf("ks agent open <name> --session %s --no-follow", sess.ShortID)}
		}
		return approvalRow{}, err
	}
	if env.Data.ID == "" {
		return approvalRow{}, fmt.Errorf("the permission request could not be read (unexpected shape)")
	}
	return env.Data, nil
}

// decidable reports the two reasons a request cannot be decided at all:
// it is another session's, or it is not waiting any more.
func decidable(sess inventoryRow, ap approvalRow) error {
	if want := agentSessionID(sess); ap.SessionID != "" && ap.SessionID != want {
		return &cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("permission request %s belongs to another session, not %s; nothing was decided", ap.ID, sess.ShortID),
			NextAction: "ks session list"}
	}
	if state := strings.ToLower(ap.State); state != "" && state != "pending" {
		return &cliError{Code: exitConflict, Kind: "approval_not_pending",
			Message: fmt.Sprintf("permission request %s is already %s%s, so nothing was decided; read the agent's current requests again before deciding",
				ap.ID, state, decidedByAt(ap.DecidedBy, ap.DecidedAt)),
			Detail:     resolvedDecision{Decision: state, DecidedBy: ap.DecidedBy, DecidedAt: ap.DecidedAt, Revision: ap.Revision},
			NextAction: fmt.Sprintf("ks agent open <name> --session %s --no-follow", sess.ShortID)}
	}
	return nil
}

// aboutToDecide is the display a decision makes for itself when nothing
// has put this request in front of anyone yet: the action, in full,
// before it is decided.
func aboutToDecide(ap approvalRow, decision string) string {
	return sanitize(fmt.Sprintf("about to %s permission request %s: %s (expires %s, revision %d)",
		decision, ap.ID, actionShown(ap), figure(ap.ExpiresAt), ap.Revision))
}

// changedSinceShown refuses a decision because the request is no longer
// the action that was shown. NOTHING IS SENT. The client does not replace
// what the person read with what the service says now — substituting the
// fresh hash and revision is precisely how a changed action gets approved
// by someone who never saw it. The new action is displayed here, and
// deciding it is a fresh ask: the same verb again, or the same keystrokes
// again in a window.
func changedSinceShown(sess inventoryRow, now approvalRow, decision string) error {
	word := "approved"
	if decision == "deny" {
		word = "denied"
	}
	return &cliError{Code: exitConflict, Kind: "approval_changed",
		Message: sanitize(fmt.Sprintf("permission request %s changed after it was shown, so nothing was %s and no decision was sent. It now reads: %s (expires %s, revision %d, %s). Read that action, and ask again to decide the one you can see.",
			now.ID, word, actionShown(now), figure(now.ExpiresAt), now.Revision, figure(now.State))),
		NextAction: fmt.Sprintf("ks agent %s %s --session %s (that decides the action shown above)", decision, now.ID, sess.ShortID)}
}

// decideApproval approves or denies the exact action a person was SHOWN.
//
// The client remembers what it displayed for a request, reads the request
// again immediately before deciding, and COMPARES the two. Only a request
// that still reads as the action on the screen is decided, and it is
// decided under the hash that was on the screen. An action that changed —
// a different exact-action hash, or a materially different rendering — is
// displayed and refused: the decision is not sent, and the person has to
// ask again now that they can see what they would be deciding. That
// refusal is the same in a script as at a keyboard; there is no mode in
// which a changed action is decided automatically.
//
// Where nothing has been displayed yet (a request named straight from a
// terminal that has not opened a window), this displays it first and that
// display is what the read below is compared against. Nothing here ever
// decides on its own, and nothing decides a request the person did not
// name.
func decideApproval(cr hostedCreds, sess inventoryRow, id, decision string) (approvalRow, approvalOutcome, error) {
	shown, remembered := lastShownAction(cr, id)
	if !remembered {
		ap, err := fetchApproval(cr, sess, id)
		if err != nil {
			return approvalRow{}, approvalOutcome{}, err
		}
		if err := decidable(sess, ap); err != nil {
			return ap, approvalOutcome{}, err
		}
		progress("%s", aboutToDecide(ap, decision))
		shown = shownFrom(cr, ap)
		recordShownAction(shown)
	}
	// read it again IMMEDIATELY before deciding, and compare it with what
	// was shown rather than adopting it
	now, err := fetchApproval(cr, sess, id)
	if err != nil {
		return approvalRow{}, approvalOutcome{}, err
	}
	if err := decidable(sess, now); err != nil {
		return now, approvalOutcome{}, err
	}
	if !sameAction(shown, now) {
		// the refusal below displays the new action, so the new action is
		// what a second ask will be compared against
		recordShownAction(shownFrom(cr, now))
		return now, approvalOutcome{}, changedSinceShown(sess, now, decision)
	}
	// the hash is the one that was on the screen (it is the one the read
	// just confirmed); the revision is the one the service holds now, so a
	// record that moved without the action changing is still refused by
	// the service rather than by guesswork here
	body := map[string]any{"decision": decision, "expected_revision": now.Revision, "action_hash": shown.Hash}
	var env struct {
		Data approvalOutcome `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/approvals/"+url.PathEscape(now.ID)+"/decision", body, &env); err != nil {
		return now, approvalOutcome{}, decisionRefusal(cr, sess, now, decision, err)
	}
	outcome := env.Data
	if outcome.Approval.ID == "" {
		outcome.Approval = now
	}
	if outcome.Decision == "" {
		outcome.Decision = decision
	}
	return now, outcome, nil
}

// decisionRefusal turns the four refusals that mean "this is not the
// request you read" into one clear sentence each. None of them is
// retried: a decision that missed its action is not a decision to send
// again automatically. Where the request still exists, it is read once
// more so the refusal can say what it says NOW, which is the thing the
// person has to read before deciding again.
func decisionRefusal(cr hostedCreds, sess inventoryRow, ap approvalRow, decision string, err error) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	word := "approved"
	if decision == "deny" {
		word = "denied"
	}
	reread := fmt.Sprintf("ks agent open <name> --session %s --no-follow", sess.ShortID)
	nowReads := func() string {
		cur, rerr := fetchApproval(cr, sess, ap.ID)
		if rerr != nil || cur.ID == "" {
			return ""
		}
		return fmt.Sprintf(" It now reads: %s — %s (revision %d, %s).", figure(cur.Kind), figure(cur.Summary), cur.Revision, figure(cur.State))
	}
	switch he.Type {
	case "ks_approval_hash_mismatch":
		return &cliError{Code: exitConflict, Kind: "approval_hash_mismatch",
			Message: fmt.Sprintf("permission request %s is no longer the action you read, so nothing was %s: the exact action changed before the decision arrived.%s Read it again and decide the version you can see.",
				ap.ID, word, nowReads()),
			NextAction: fmt.Sprintf("ks agent approve %s --session %s", ap.ID, sess.ShortID)}
	case "ks_revision_conflict":
		return &cliError{Code: exitConflict, Kind: "revision_conflict",
			Message: fmt.Sprintf("permission request %s moved on while it was being decided, so nothing was %s.%s Read it again and decide the version you can see.",
				ap.ID, word, nowReads()),
			NextAction: fmt.Sprintf("ks agent approve %s --session %s", ap.ID, sess.ShortID)}
	case "ks_approval_expired":
		return &cliError{Code: exitConflict, Kind: "approval_expired",
			Message: fmt.Sprintf("permission request %s expired before the decision arrived, so nothing was %s and the agent was not given this permission; read the agent's current requests again before deciding.",
				ap.ID, word),
			NextAction: reread}
	case "ks_approval_not_pending":
		// the service answers a lost race with what now stands; that is
		// what the person needs, so it is printed, not a guess about it
		var body struct {
			Error struct {
				Resolved resolvedDecision `json:"resolved"`
			} `json:"error"`
		}
		_ = json.Unmarshal(he.Raw, &body)
		r := body.Error.Resolved
		what := "was already decided elsewhere"
		if r.Decision != "" {
			what = "was already " + r.Decision + decidedByAt(r.DecidedBy, r.DecidedAt)
		}
		ce := &cliError{Code: exitConflict, Kind: "approval_not_pending",
			Message: fmt.Sprintf("permission request %s %s, so nothing was %s here and your decision was not recorded; read the agent's current requests again before deciding.",
				ap.ID, what, word),
			NextAction: reread}
		if r.Decision != "" {
			ce.Detail = r
		}
		return ce
	}
	return err
}

// resolvedDecision is what stands on an approval somebody already decided.
type resolvedDecision struct {
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
	DecidedAt string `json:"decided_at"`
	Revision  int64  `json:"revision"`
}

func (r resolvedDecision) detailLines() []string {
	return []string{
		"recorded decision  " + figure(r.Decision),
		"decided by         " + figure(r.DecidedBy),
		"decided at         " + figure(r.DecidedAt),
	}
}

func decidedByAt(by, at string) string {
	s := ""
	if by != "" {
		s += " by " + by
	}
	if at != "" {
		s += " at " + at
	}
	return s
}

func decisionLine(ap approvalRow, decision string) string {
	word := "approved"
	if decision == "deny" {
		word = "denied"
	}
	return sanitize(fmt.Sprintf("%s permission request %s: %s — %s; the agent was told, and nothing else was decided",
		word, ap.ID, figure(ap.Kind), figure(ap.Summary)))
}

// hostedAgentDecide is ks agent approve and ks agent deny: the same verb
// with the word it sends. Both work from a terminal that is following
// nothing, which is why they exist as verbs as well as keystrokes.
func hostedAgentDecide(decision string) func(hostedCreds, *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		sess := agentSession(cr, inv)
		id := strings.TrimSpace(inv.Arg(0))
		if id == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the id of the permission request is required; nothing is decided by position",
				NextAction: fmt.Sprintf("ks agent open <name> --session %s --no-follow", sess.ShortID)})
		}
		ap, outcome, err := decideApproval(cr, sess, id, decision)
		if err != nil {
			die(err)
		}
		emit(map[string]any{"session": sess.ID, "approval": outcome.Approval, "decision": outcome.Decision, "decided_at": outcome.DecidedAt},
			func() { fmt.Println(decisionLine(ap, outcome.Decision)) })
	}
}

// ---------------------------------------------------------------------
// the live window: what it holds, and what it does with what is typed
// ---------------------------------------------------------------------

// liveWindow is the state one open window keeps while it follows an
// agent: which agent, in which session, and the control lease it holds —
// or the reason it no longer holds one. Three goroutines touch it (the
// one that reads what is typed, the one that renews the lease, and the
// one that follows the stream), so every read and write goes through the
// mutex.
type liveWindow struct {
	mu    sync.Mutex
	sess  inventoryRow
	agent agentRow
	lease *agentLease
	lost  string // why control is no longer held; empty while it is
}

// hold answers the lease this window may steer with, and whether it has
// one at all. The token is copied out under the lock and used for exactly
// one request; it is never stored anywhere else.
func (win *liveWindow) hold() (agentLease, bool) {
	win.mu.Lock()
	defer win.mu.Unlock()
	if win.lease == nil || win.lost != "" {
		return agentLease{}, false
	}
	return *win.lease, true
}

// lose records that this window is no longer the controller. It is called
// once per displacement; the second caller sees that control was already
// lost and says nothing more.
func (win *liveWindow) lose(reason string) bool {
	win.mu.Lock()
	defer win.mu.Unlock()
	if win.lost != "" {
		return false
	}
	win.lost = reason
	win.lease = nil
	return true
}

func (win *liveWindow) lostReason() string {
	win.mu.Lock()
	defer win.mu.Unlock()
	return win.lost
}

func (win *liveWindow) renewed(l agentLease) {
	win.mu.Lock()
	defer win.mu.Unlock()
	if win.lost != "" {
		return
	}
	next := l
	if next.Token == "" && win.lease != nil { // a renewal that rotates nothing keeps the token it renewed
		next.Token = win.lease.Token
	}
	if next.ID == "" && win.lease != nil {
		next.ID = win.lease.ID
	}
	win.lease = &next
}

// takeControlLine is the one thing a displaced or watching window can
// suggest: reopening and asking for control, which is a decision the
// person makes rather than one this client makes for them.
func (win *liveWindow) takeControlLine() string {
	return fmt.Sprintf("ks agent open %s --session %s --take-control", win.agent.Name, win.sess.ShortID)
}

// legend is the one line that says what this window accepts. It is a
// progress line, because it is a fact about the window rather than a
// result of it.
func (win *liveWindow) legend() string {
	if out.noInput {
		return "this window reads nothing typed into it (--no-input); decide requests with ks agent approve <id> --session " + win.sess.ShortID
	}
	if _, held := win.hold(); !held {
		return "type \"a <id>\" to approve a request or \"d <id>\" to deny it; this window is watching, so it queues no instruction (" + win.takeControlLine() + ")"
	}
	return "type \"a <id>\" to approve a request, \"d <id>\" to deny it, anything else to send it to the agent as an instruction; Ctrl-C interrupts the instruction in flight; \"q\" leaves the window"
}

// refuse prints one refusal inside a window that stays open. A window is
// a place a person is standing, so one bad line does not close it.
func (win *liveWindow) refuse(err error) {
	ce := classify(err)
	progress("refused: %s", ce.Message)
	if ce.NextAction != "" {
		progress("Next: %s", ce.NextAction)
	}
}

// readInput reads whole lines from standard input for as long as the
// window is open. A line is a decision only when it is exactly the
// approve or deny word with at most one more token, which is the id it
// decides: everything else is an instruction, so an ordinary sentence
// that happens to start with a letter is never read as a decision.
func (win *liveWindow) readInput(cr hostedCreds, restore func()) {
	if out.noInput {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch word, arg, ok := readWindowLine(line); {
		case ok && word == "detach":
			restore()
			fmt.Fprintln(os.Stderr, "[left the window; the agent keeps working]")
			os.Exit(exitOK)
		case ok:
			win.decide(cr, word, arg)
		default:
			win.submit(cr, line)
		}
	}
}

// interrupt is Ctrl-C in a window: a stop request for the instruction in
// flight, through the cancel route, and a line saying what it did. A window
// that is only watching steers nothing, and interrupts nothing.
func (win *liveWindow) interrupt(cr hostedCreds) {
	say := func(kind, human string, extra map[string]any) {
		data := map[string]any{"type": kind}
		for k, v := range extra {
			data[k] = v
		}
		emitLine(data, "Ctrl-C: "+human)
	}
	if _, held := win.hold(); !held {
		say("interrupt_refused", "this window is watching, so it interrupts nothing; leave with q", nil)
		return
	}
	agents, err := fetchAgents(cr, agentSessionID(win.sess))
	if err != nil {
		say("interrupt_failed", "the agent could not be read, so nothing was interrupted: "+errText(err), nil)
		return
	}
	var active string
	for _, a := range agents {
		if a.ID == win.agent.ID {
			active = a.ActiveTaskID
		}
	}
	if active == "" {
		say("interrupt_nothing", "nothing is running, so nothing was interrupted; leave with q", nil)
		return
	}
	t, err := fetchTask(cr, active)
	if err != nil {
		say("interrupt_failed", "instruction "+active+" could not be read, so nothing was interrupted: "+errText(err), nil)
		return
	}
	rec, err := fetchSessionRecord(cr, agentSessionID(win.sess))
	if err != nil {
		say("interrupt_failed", "the session could not be read, so nothing was interrupted: "+errText(err), nil)
		return
	}
	body := map[string]any{"expected_revision": t.Revision, "epoch": rec.ExecutionEpoch, "reason": "interrupted from the agent window (Ctrl-C)"}
	var env struct {
		Data struct {
			Task *taskRow `json:"task"`
		} `json:"data"`
	}
	resp, rb, err := doBounded(cr, "POST", "/api/v2/tasks/"+url.PathEscape(t.ID)+"/cancel", map[string]string{"Idempotency-Key": newIdempotencyKey()}, mustJSON(body))
	switch {
	case err != nil:
		say("interrupt_unknown", "the interrupt of "+t.ID+" was sent and no answer came back ("+errText(err)+"); whether it arrived is unknown -- ks task show "+t.ID, nil)
		return
	case resp.StatusCode/100 != 2:
		say("interrupt_refused", "the interrupt of "+t.ID+" was refused: "+errText(hostedError("POST", "cancel", resp, rb)), nil)
		return
	}
	_ = json.Unmarshal(rb, &env)
	state := "cancelling"
	if env.Data.Task != nil && env.Data.Task.State != "" {
		state = env.Data.Task.State
	}
	human := fmt.Sprintf("interrupt requested for %s; it reads %s", t.ID, stateLabel("task_state", state))
	if state == "cancelling" {
		human += " (asked to stop at a safe boundary; not claimed stopped)"
	}
	say("interrupted", human+"; the window stays open (q leaves)", map[string]any{"task_id": t.ID, "state": state})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// readWindowLine reads one typed line as a window command. "a" and "d"
// (and their long forms) decide; "q" detaches; anything longer is not a
// command at all.
func readWindowLine(line string) (word, arg string, ok bool) {
	f := strings.Fields(line)
	if len(f) == 0 || len(f) > 2 {
		return "", "", false
	}
	switch strings.ToLower(f[0]) {
	case "a", "approve":
		word = "approve"
	case "d", "deny":
		word = "deny"
	case "q", "quit", "detach":
		if len(f) > 1 {
			return "", "", false
		}
		return "detach", "", true
	default:
		return "", "", false
	}
	if len(f) == 2 {
		arg = f[1]
	}
	return word, arg, true
}

// decide settles one permission request from inside the window. The id is
// named, or there is exactly one waiting and that is what "a" alone
// means; several waiting with no id is a refusal that lists them, because
// a window must never pick one for you.
func (win *liveWindow) decide(cr hostedCreds, decision, arg string) {
	pending, err := fetchPendingApprovals(cr, agentSessionID(win.sess))
	if err != nil {
		win.refuse(err)
		return
	}
	id, err := chooseApproval(pending, arg)
	if err != nil {
		win.refuse(err)
		return
	}
	ap, outcome, err := decideApproval(cr, win.sess, id, decision)
	if err != nil {
		win.refuseDecision(err, id)
		return
	}
	emitLine(map[string]any{"type": "approval_decided", "approval": outcome.Approval, "decision": outcome.Decision, "decided_at": outcome.DecidedAt},
		decisionLine(ap, outcome.Decision))
}

// refuseDecision prints a refused decision inside the window. A request
// whose action changed since it was shown is NOT decided here: the new
// action is now on the screen, and the window asks for the decision again
// rather than making it on the person's behalf.
func (win *liveWindow) refuseDecision(err error, id string) {
	var ce *cliError
	if errors.As(err, &ce) && ce.Kind == "approval_changed" && id != "" {
		ce.NextAction = fmt.Sprintf("type %q to approve the action above, or %q to deny it", "a "+id, "d "+id)
	}
	win.refuse(err)
}

// chooseApproval answers the one request a typed argument names among the
// ones waiting: its id, a unique beginning of its id, or — with no
// argument at all — the single request that is waiting.
func chooseApproval(pending []approvalRow, arg string) (string, error) {
	ids := make([]string, 0, len(pending))
	for _, ap := range pending {
		ids = append(ids, ap.ID)
	}
	sort.Strings(ids)
	if arg == "" {
		switch len(ids) {
		case 1:
			return ids[0], nil
		case 0:
			return "", &cliError{Code: exitUsage, Kind: "nothing_pending", Message: "nothing is waiting for a decision in this session right now"}
		default:
			return "", &cliError{Code: exitUsage, Kind: "ambiguous",
				Message: fmt.Sprintf("%d requests are waiting (%s); name the one you mean", len(ids), strings.Join(ids, ", "))}
		}
	}
	var hits []string
	for _, id := range ids {
		if id == arg {
			return id, nil
		}
		if len(arg) >= 3 && strings.HasPrefix(id, arg) {
			hits = append(hits, id)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", &cliError{Code: exitUsage, Kind: "not_found",
			Message: fmt.Sprintf("no request waiting under %q; the ones waiting are: %s", arg, waitingList(ids))}
	}
	return "", &cliError{Code: exitUsage, Kind: "ambiguous",
		Message: fmt.Sprintf("%q begins %d of the waiting requests (%s); give the whole id", arg, len(hits), strings.Join(hits, ", "))}
}

func waitingList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// submit sends one typed line to the agent as LIVE input: the instruction
// carries this window's control lease, so the control plane can tell it
// from an instruction typed anywhere else and refuse it the moment this
// window is no longer the controller.
//
// There is no fallback. A window that has been displaced submits NOTHING:
// falling back to a standalone submission would be this window steering
// an agent it was just told it does not control, under a different name,
// which is exactly the confusion the lease exists to prevent.
func (win *liveWindow) submit(cr hostedCreds, text string) {
	lease, held := win.hold()
	if !held {
		reason := win.lostReason()
		if reason == "" {
			reason = "this window is watching; another window holds control"
		}
		win.refuse(&cliError{Code: exitConflict, Kind: "not_controller",
			Message:    reason + ", so nothing was sent to the agent",
			NextAction: win.takeControlLine()})
		return
	}
	path := "/api/v2/agents/" + url.PathEscape(win.agent.ID) + "/tasks"
	sid, err := submissionID(cr, path, text)
	if err != nil {
		win.refuse(err)
		return
	}
	// the lease token is in the request and nowhere else: the journal keeps
	// the submission id and a hash of the instruction, never the body.
	body := map[string]any{
		"submission_id": sid, "text": text,
		"origin": "live", "fence": lease.Fence, "lease_id": lease.ID, "lease_token": lease.Token,
	}
	var env struct {
		Data submittedTask `json:"data"`
	}
	env.Data, _, err = sendInstruction(cr, win.agent.ID, sid, body)
	if err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_controller_stale" {
			win.displaced(sanitize(he.Message))
			return
		}
		win.refuse(err)
		return
	}
	t := env.Data
	human := fmt.Sprintf("sent: task %s at queue position %d (%s)", t.ID, t.QueueSeq, figure(t.State))
	if t.Replayed {
		human = fmt.Sprintf("already sent: task %s at queue position %d (%s); the same instruction was submitted once", t.ID, t.QueueSeq, figure(t.State))
	}
	emitLine(map[string]any{"type": "instruction_sent", "submission_id": sid, "replayed": t.Replayed, "task": t.taskRow}, human)
}

// displaced says, once and plainly, that control moved to another window.
// The window stays open and keeps showing the agent's events, because
// watching is a real thing to be doing; it simply steers nothing.
func (win *liveWindow) displaced(detail string) {
	if !win.lose("control of this agent moved to another window") {
		return
	}
	progress("control moved to another window: %s", detail)
	progress("nothing was sent, and this window will not submit as the controller again; it is watching now")
	progress("to steer from here again: %s", win.takeControlLine())
}

// renew keeps the control lease alive for as long as the window is open.
// The lease is short by design, so a window that stops renewing stops
// being the controller: this loop is the difference between holding
// control and having held it. A renewal that fails is not retried into
// oblivion — it is reported, and the window drops to watching.
func (win *liveWindow) renew(cr hostedCreds) {
	for {
		lease, held := win.hold()
		if !held {
			return
		}
		wait := leaseRenewAfter(lease, time.Now())
		if wait < 0 {
			return // no expiry was reported, so there is no clock to renew against
		}
		time.Sleep(wait)
		if _, held := win.hold(); !held {
			return
		}
		next, err := renewLease(cr, lease)
		if err != nil {
			var he *hostedErr
			detail := sanitize(err.Error())
			if errors.As(err, &he) && he.Type == "ks_controller_stale" {
				win.displaced(sanitize(he.Message))
				return
			}
			if win.lose("this window's control could not be renewed") {
				progress("the control of agent %s could not be renewed: %s", win.agent.Name, detail)
				progress("this window no longer holds control and will submit nothing as the controller; it is watching now")
				progress("to steer from here again: %s", win.takeControlLine())
			}
			return
		}
		win.renewed(*next)
	}
}

// leaseRenewAfter is how long a window waits before renewing: a third of
// what is left, never less than a second (a renewal loop is not a busy
// loop) and never more than half a minute. A lease with no readable
// expiry answers -1: there is nothing to renew against, and inventing a
// cadence for it would be inventing the contract.
func leaseRenewAfter(l agentLease, now time.Time) time.Duration {
	if l.ExpiresAt == "" {
		return -1
	}
	exp, err := time.Parse(time.RFC3339, l.ExpiresAt)
	if err != nil {
		return -1
	}
	ttl := exp.Sub(now)
	if ttl <= 0 {
		// already at or past its expiry: renew promptly, but a renewal loop
		// is never a busy loop, however short the lease the service hands out
		return 250 * time.Millisecond
	}
	d := ttl / 3
	if d < time.Second {
		d = time.Second
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// renewLease extends one control lease. It carries the lease it renews,
// so repeating it is the same renewal rather than a new claim, and it is
// deliberately NOT recorded in the operations journal: a heartbeat every
// few seconds does not belong in the record of what a person asked for.
func renewLease(cr hostedCreds, l agentLease) (*agentLease, error) {
	var env struct {
		Data agentLease `json:"data"`
	}
	body := map[string]any{"lease_token": l.Token, "fence": l.Fence}
	if err := hostedCall(cr, "PUT", "/api/v2/control-leases/"+url.PathEscape(l.ID), body, &env); err != nil {
		return nil, err
	}
	next := env.Data
	if next.ID == "" {
		next.ID = l.ID
	}
	if next.Token == "" {
		next.Token = l.Token
	}
	if next.Fence == 0 {
		next.Fence = l.Fence
	}
	if next.ExpiresAt == "" {
		return nil, fmt.Errorf("the renewed lease reports no expiry, so this window cannot tell how long it still holds control")
	}
	return &next, nil
}

// ---------------------------------------------------------------------
// ks agent pause | resume: saving the session, and waking it again
// ---------------------------------------------------------------------

// sessionPause is what the pause route answers: the state the session is
// in now, whether the save it just took is safe, and a sentence about it.
// "stopping" is not "parked": the save is already durable, and the
// machine is still winding down.
type sessionPause struct {
	RuntimeState   string `json:"runtime_state"`
	CheckpointSafe bool   `json:"checkpoint_safe"`
	Note           string `json:"note"`
}

func fetchSessionRecord(cr hostedCreds, id string) (inventoryRow, error) {
	var env struct {
		Data inventoryRow `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, &env); err != nil {
		return inventoryRow{}, err
	}
	return env.Data, nil
}

// waitForParked polls the session until it reads parked, within the wait
// bound, saying where it has got to as it goes. It never prints a sleep
// for someone else to run and never asks anyone to go and look: the
// waiting is the client's job. It answers the last state it saw and
// whether that state was parked.
func waitForParked(cr hostedCreds, id string, short string) (string, time.Duration, bool) {
	bound := boundedWait()
	interval := pollCadence(bound)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	started := time.Now()
	deadline := started.Add(bound)
	state := "stopping"
	for {
		select {
		case <-sig:
			fail(&cliError{Code: exitInterrupt, Kind: "interrupted",
				Message:     fmt.Sprintf("interrupted locally after %s; session %s keeps stopping and its save stays safe", time.Since(started).Round(time.Second), short),
				WorkStarted: workYes, NextAction: "ks session show " + short})
		case <-time.After(interval):
		}
		r, err := fetchSessionRecord(cr, id)
		if err != nil {
			progress("reading session %s: %s (still waiting)", short, sanitize(err.Error()))
		} else {
			if s := strings.ToLower(r.RuntimeState); s != "" {
				state = s
			}
			if state == "parked" {
				return state, time.Since(started).Round(time.Second), true
			}
			progress("session %s is still %s after %s", short, state, time.Since(started).Round(time.Second))
		}
		if time.Now().After(deadline) {
			return state, time.Since(started).Round(time.Second), false
		}
	}
}

// boundedWait is the wait bound of one command: the default, what
// --wait-timeout set, or the impatience a test asks for.
func boundedWait() time.Duration {
	bound := waitBound
	if v := os.Getenv("KS_WAIT_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			bound = time.Duration(ms) * time.Millisecond
		}
	}
	return bound
}

// pollCadence keeps a short bound from being one long silence.
func pollCadence(bound time.Duration) time.Duration {
	interval := pollInterval
	if bound < interval*4 {
		interval = bound / 4
	}
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	return interval
}

func hostedAgentPause(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	id := agentSessionID(sess)
	var env struct {
		Data sessionPause `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(id)+"/pause", map[string]any{}, &env); err != nil {
		die(saveFailure(err, a.Name, sess))
	}
	p := env.Data
	state := strings.ToLower(p.RuntimeState)
	report := func(final string, waited time.Duration) {
		emit(map[string]any{"session": sess.ID, "agent": a.ID, "runtime_state": final,
			"checkpoint_safe": p.CheckpointSafe, "note": p.Note, "waited_seconds": int64(waited.Seconds())}, func() {
			// what is said about the save is what the service said about it,
			// never what would be nicer to hear
			if p.CheckpointSafe {
				fmt.Printf("session %s is parked: the save is complete and safe, and session time has stopped", sess.ShortID)
			} else {
				fmt.Printf("session %s is parked and session time has stopped; the service did not confirm its save as safe", sess.ShortID)
			}
			if waited > 0 {
				fmt.Printf(" (stopping finished %s after the save)", waited)
			}
			fmt.Println()
		})
	}
	if state == "parked" {
		report("parked", 0)
		return
	}
	// stopping: the save is already durable and the machine is winding
	// down. Saying "saved" here and stopping there is the whole point.
	safe := "the save is safe"
	if !p.CheckpointSafe {
		safe = "the service did not confirm the save as safe"
	}
	progress("session %s: %s, and stopping is still finishing%s", sess.ShortID, safe, noteSuffix(p.Note))
	final, waited, parked := waitForParked(cr, id, sess.ShortID)
	if !parked {
		fail(&cliError{Code: exitTemporary, Kind: "still_stopping",
			Message: fmt.Sprintf("session %s is still %s after %s; the save it took is complete, and nothing else was started",
				sess.ShortID, final, waited),
			WorkStarted: workYes,
			NextAction:  fmt.Sprintf("ks agent pause %s --session %s --wait-timeout 5m (the same request, waiting longer)", a.Name, sess.ShortID)})
	}
	report(final, waited)
}

// saveFailed is what the service read back after a save that did not
// complete (KS-051): no new saved point, the previous one as it was, and the
// runtime state it observed (or unavailable).
type saveFailed struct {
	RuntimeState       string `json:"runtime_state"`
	CheckpointRecorded bool   `json:"checkpoint_recorded"`
	PreviousCheckpoint string `json:"previous_checkpoint_id"`
}

func (f saveFailed) detailLines() []string {
	prev := f.PreviousCheckpoint
	if prev == "" {
		prev = "none (there was no earlier saved point)"
	} else {
		prev += " (untouched)"
	}
	return []string{
		"new saved point    none recorded",
		"previous          " + prev,
		"session reads     " + figure(f.RuntimeState),
	}
}

// saveFailure states a failed save as the service stated it. It is not an
// unknown outcome: the service established that nothing was saved, and it
// read back whether the session still runs.
func saveFailure(err error, agent string, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) || (he.Type != "ks_save_failed" && he.Type != "ks_fleet_unavailable") {
		return err
	}
	var body struct {
		Error saveFailed `json:"error"`
	}
	_ = json.Unmarshal(he.Raw, &body)
	ce := classify(err)
	ce.Code, ce.WorkStarted, ce.Detail = exitFailed, workNo, body.Error
	if he.Type == "ks_fleet_unavailable" {
		ce.Code = exitTemporary
	}
	ce.Message = "the session was NOT saved and nothing new was recorded: " + sanitize(he.Message)
	if body.Error.RuntimeState == "running" {
		ce.NextAction = fmt.Sprintf("the session is still running and its agents carry on; try again: ks agent pause %s --session %s", agent, sess.ShortID)
	} else {
		ce.NextAction = "ks session show " + sess.ShortID
	}
	return ce
}

func noteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return ": " + sanitize(note)
}

func hostedAgentResume(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	id := agentSessionID(sess)
	res, err := postResume(cr, id)
	if err != nil {
		var he *hostedErr
		if !errors.As(err, &he) || he.Type != "ks_stopping" {
			die(err)
		}
		// the save from the last pause has not finished putting the machine
		// away yet. Nothing failed and nothing was resumed: this is a wait,
		// so the client waits it out rather than handing back a conflict
		// nobody can act on.
		progress("session %s cannot resume yet%s", sess.ShortID, noteSuffix(he.Message))
		progress("waiting for it to finish, then resuming; nothing has been resumed so far")
		final, waited, parked := waitForParked(cr, id, sess.ShortID)
		if !parked {
			fail(&cliError{Code: exitConflict, Kind: "stopping",
				Message: fmt.Sprintf("session %s is still %s after %s, so nothing was resumed; the previous save is still completing and this can be asked again",
					sess.ShortID, final, waited),
				WorkStarted: workNo,
				NextAction:  fmt.Sprintf("ks agent resume %s --session %s", a.Name, sess.ShortID)})
		}
		progress("session %s finished stopping after %s; resuming it now", sess.ShortID, waited)
		if res, err = postResume(cr, id); err != nil {
			die(err)
		}
	}
	emit(map[string]any{"session": sess.ID, "agent": a.ID, "runtime_state": res["runtime_state"], "note": res["note"]}, func() {
		fmt.Printf("session %s is resuming from its last save (state: %s); session time is metered again\n",
			sess.ShortID, figure(res["runtime_state"]))
	})
}

func postResume(cr hostedCreds, id string) (map[string]any, error) {
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(id)+"/resume", map[string]any{}, &env); err != nil {
		return nil, err
	}
	if env.Data == nil {
		env.Data = map[string]any{}
	}
	return env.Data, nil
}
