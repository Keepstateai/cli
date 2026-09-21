// recovery.go: what a person does when an agent's queue stops moving.
//
// An agent's queue is held when the instruction in front of it FAILED, or
// when nobody could establish what that instruction did. Nothing behind it
// starts until somebody decides that it should. That is the point: the
// instruction after "publish the release" is "announce the release", and
// announcing something that failed to publish — or that may or may not have
// published — is exactly the work a machine should not start on its own.
//
// So there are four verbs here and they divide the way the decision does.
// `ks agent queue show` is the recovery view: which instruction is blocking,
// what it last reported, what waits behind it, and what each choice would do.
// `ks agent queue resume` continues the rest of the queue. `ks agent queue
// hold` records that it stays held, and why — a queue that waits because
// somebody decided it should is a different thing from a queue nobody has
// looked at. `ks task resume` is the same continuation named from the other
// end, by the instruction you were looking at rather than by the agent.
//
// Two properties shape all of them.
//
// NONE OF THEM RESTARTS THE BLOCKING INSTRUCTION. Continuing the rest of the
// queue and running the failed thing again are different actions with
// different consequences, and no verb here does both. The service does not
// offer the second one at all, for a reason it states and this client prints
// rather than paraphrases.
//
// AND A DECISION IS BOUND TO WHAT WAS SHOWN. The client remembers what it put
// on the screen, reads the hold again immediately before deciding, and
// COMPARES. A hold whose blocking instruction has since resolved, whose held
// work has changed, or whose session has been restored underneath it is
// DISPLAYED AND REFUSED — nothing is sent, and the person is asked again now
// that they can see what they would be deciding. This is the same discipline
// the permission verbs use, for the same reason: releasing work over an
// outcome that changed after you read it is deciding something you never saw.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// what the control plane reports
// ---------------------------------------------------------------------

// queueHold is one hold on an agent's queue: what blocks it, what waits
// behind it, and what may be done about it.
type queueHold struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	State     string `json:"state"`
	Cause     string `json:"cause"`
	Revision  int64  `json:"revision"`

	BlockingTask    string `json:"blocking_task"`
	BlockingState   string `json:"blocking_task_state"`
	BlockingSummary string `json:"blocking_task_summary"`
	Reason          string `json:"reason"`

	// Epoch is the execution generation the hold was recorded under;
	// SessionEpoch is the one the session runs now, and a decision names that
	// one. A session that has been restored since reads a higher
	// SessionEpoch, and this client will not decide without one at all.
	Epoch        int64 `json:"epoch"`
	SessionEpoch int64 `json:"session_epoch"`

	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
	DecidedAt string `json:"decided_at"`
	Finding   string `json:"finding"`

	HeldTasks     []string     `json:"held_tasks"`
	ReleasedTasks []string     `json:"released_tasks"`
	Consequence   string       `json:"consequence"`
	Choices       []holdChoice `json:"choices"`
}

// holdChoice is one option the SERVICE states: what it would do, what the
// records would read afterwards, what it costs, and — for an option the
// service does not offer — why it does not. This client renders these; it
// composes none of its own, because an option a client imagined is a promise
// nobody made.
type holdChoice struct {
	Decision    string `json:"decision"`
	Available   bool   `json:"available"`
	Effect      string `json:"effect"`
	StateAfter  string `json:"state_after"`
	Cost        string `json:"cost"`
	Unavailable string `json:"unavailable_reason"`
}

// the decisions this client may send, and the one word that separates a
// known failure from an outcome nobody could establish. Restarting the
// blocking instruction is not among them and is not a flag on them.
const (
	decisionResume = "release_successors"
	decisionKeep   = "keep_held"
	holdIsActive   = "active"
	causeFailed    = "task_failed"
)

func fetchQueueHolds(cr hostedCreds, agentID string) ([]queueHold, error) {
	var env struct {
		Data struct {
			Items []queueHold `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"agent_id": {agentID}}
	if err := hostedCall(cr, "GET", "/api/v2/queue-holds?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// activeHold answers the one hold that is stopping this queue now, or nil.
// The service keeps at most one active at a time; if one ever reported two,
// this refuses to choose between them rather than taking the first.
func activeHold(holds []queueHold) (*queueHold, error) {
	var found []queueHold
	for _, h := range holds {
		if h.State == holdIsActive {
			found = append(found, h)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	}
	ids := make([]string, 0, len(found))
	for _, h := range found {
		ids = append(ids, h.ID)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("the control plane reports %d holds on this queue at once (%s); this client will not choose between them",
		len(found), strings.Join(ids, ", "))
}

func fetchTask(cr hostedCreds, id string) (taskRow, error) {
	var env struct {
		Data taskRow `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/tasks/"+url.PathEscape(id), nil, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && (he.Status == 404 || he.Type == "ks_not_found") {
			return taskRow{}, &cliError{Code: exitUsage, Kind: "not_found",
				Message: fmt.Sprintf("no instruction %s", id), NextAction: "ks session list"}
		}
		return taskRow{}, err
	}
	if env.Data.ID == "" {
		return taskRow{}, fmt.Errorf("the instruction could not be read (unexpected shape)")
	}
	return env.Data, nil
}

// ---------------------------------------------------------------------
// what was SHOWN: the only thing a decision may be compared against
// ---------------------------------------------------------------------

// shownRecovery is one recovery state as this client PUT IT IN FRONT OF A
// PERSON: which hold, at which revision, and a hash of everything that was on
// the screen — the blocking instruction and what it reads, what it last
// reported, what waits behind it, which generation the session is running,
// and what each option would do. Only the hashes are kept; the words stay
// where the service holds them.
//
// A decision is compared against THIS. Reading the hold again and sending
// back whatever it says now would make the comparison vacuous: it is exactly
// how a queue gets released over an outcome that changed after somebody read
// it.
type shownRecovery struct {
	CTL      string `json:"ctl"`
	ID       string `json:"hold_id"`
	Revision int64  `json:"revision"`
	StateSHA string `json:"state_sha256"`
	ShownAt  string `json:"shown_at"`
}

func shownRecoveryPath() string { return filepath.Join(configDir(), "recovery-shown.jsonl") }

// recoveryShown is the hold as it was read: every fact the view puts on the
// screen, in a fixed order. Everything that makes one recovery decision a
// DIFFERENT decision is in here, because this is both what is displayed and
// what a decision is compared against.
func recoveryShown(h queueHold) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hold=%s state=%s cause=%s revision=%d generation=%d recorded_under=%d\n",
		h.ID, h.State, h.Cause, h.Revision, h.SessionEpoch, h.Epoch)
	fmt.Fprintf(&b, "blocking=%s reads=%s\n", h.BlockingTask, h.BlockingState)
	fmt.Fprintf(&b, "reported=%s\n", h.BlockingSummary)
	fmt.Fprintf(&b, "reason=%s\n", h.Reason)
	fmt.Fprintf(&b, "held=%s\n", strings.Join(h.HeldTasks, ","))
	for _, c := range h.Choices {
		fmt.Fprintf(&b, "choice=%s available=%t effect=%s after=%s cost=%s refused=%s\n",
			c.Decision, c.Available, c.Effect, c.StateAfter, c.Cost, c.Unavailable)
	}
	return b.String()
}

func shownRecoveryFrom(cr hostedCreds, h queueHold) shownRecovery {
	return shownRecovery{CTL: cr.CTL, ID: h.ID, Revision: h.Revision,
		StateSHA: sha256Hex([]byte(recoveryShown(h))), ShownAt: time.Now().UTC().Format(time.RFC3339)}
}

// recordShownRecovery appends one display to the local record, so a hold read
// in one terminal is still compared against what was read when it is decided
// in another.
//
// A failure to write is not fatal and is not reported: the guarantee does not
// rest on this file. Where nothing was recorded, the decision path displays
// the hold itself and compares against that display, so an unwritable
// configuration directory makes this client more careful, never less.
func recordShownRecovery(s shownRecovery) {
	if s.ID == "" {
		return
	}
	if os.MkdirAll(configDir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(shownRecoveryPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(s)
	_, _ = f.Write(append(b, '\n'))
}

// lastShownRecovery answers the most recent display of one hold on this
// control plane. A line this client cannot read is skipped: the record is
// something to read, never a lock to hold.
func lastShownRecovery(cr hostedCreds, id string) (shownRecovery, bool) {
	b, err := os.ReadFile(shownRecoveryPath())
	if err != nil {
		return shownRecovery{}, false
	}
	var found shownRecovery
	ok := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s shownRecovery
		if json.Unmarshal([]byte(line), &s) == nil && s.ID != "" && s.ID == id && s.CTL == cr.CTL {
			found, ok = s, true
		}
	}
	return found, ok
}

// sameRecovery reports whether the hold as it reads NOW is the one that was
// shown. Both halves must agree: the revision the service held then, and the
// state that was actually on the screen. Either one differing is a different
// decision.
//
// The second half is not redundant. A blocking instruction that has since
// been resolved, different work waiting behind the hold, a session restored
// underneath it, an option that now does something else: none of those has to
// move the hold's own revision, and every one of them changes what a person
// is deciding.
func sameRecovery(s shownRecovery, h queueHold) bool {
	return s.Revision == h.Revision && s.StateSHA == sha256Hex([]byte(recoveryShown(h)))
}

// ---------------------------------------------------------------------
// the recovery view
// ---------------------------------------------------------------------

// blockedWord says what the blocking instruction is, in the words its cause
// earns. A known failure is not an unknown and an unknown is not a failure,
// and this client never rounds one to the other.
func blockedWord(h queueHold) string {
	if h.Cause == causeFailed {
		return "failed instruction"
	}
	return "unresolved instruction"
}

// verbFor is the command that records one decision, from where the reader is
// standing.
func verbFor(decision, agent string, sess inventoryRow) string {
	switch decision {
	case decisionResume:
		return fmt.Sprintf("ks agent queue resume %s --session %s --finding \"...\"", agent, sess.ShortID)
	case decisionKeep:
		return fmt.Sprintf("ks agent queue hold %s --session %s --finding \"...\"", agent, sess.ShortID)
	}
	return ""
}

// choiceLines renders one option: what it would do, what the records would
// read afterwards, what it costs — or, for an option the service does not
// offer, that it is not offered and why. Every word of it comes from the
// service.
func choiceLines(c holdChoice, verb string) []string {
	head := "  " + c.Decision
	switch {
	case !c.Available:
		head += "  —  NOT OFFERED"
	case verb != "":
		head += "  —  " + verb
	}
	out := []string{sanitize(head)}
	add := func(label, text string) {
		if strings.TrimSpace(text) != "" {
			out = append(out, sanitize(fmt.Sprintf("      %-9s %s", label, text)))
		}
	}
	if !c.Available {
		add("why not", c.Unavailable)
	}
	add("does", c.Effect)
	add("leaves", c.StateAfter)
	add("costs", c.Cost)
	return out
}

// printQueue lists every instruction this agent has, in committed order, with
// the blocking one marked. Nothing is filtered out: a decision about what to
// continue is made against the whole queue, failures and skipped work
// included, and a queue that hides what went wrong is a queue nobody can
// reason about.
func printQueue(tasks []taskRow, blocking string) {
	if len(tasks) == 0 {
		fmt.Println("the queue is empty")
		return
	}
	fmt.Printf("the queue in full (%d instruction(s); nothing here is hidden by a decision)\n", len(tasks))
	fmt.Printf("  %-2s %-4s %-28s %s\n", "", "POS", "INSTRUCTION", "STATE")
	for _, t := range tasks {
		marker := "  "
		if t.ID == blocking {
			marker = "->"
		}
		fmt.Println(sanitize(fmt.Sprintf("  %-2s %-4d %-28s %s", marker, t.QueueSeq, clip(t.ID, 28), figure(t.State))))
	}
}

func hostedAgentQueueShow(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	holds, err := fetchQueueHolds(cr, a.ID)
	if err != nil {
		die(err)
	}
	h, err := activeHold(holds)
	if err != nil {
		die(err)
	}
	tasks, terr := fetchTasks(cr, a.ID)
	if terr != nil {
		progress("the queue itself could not be read: %s", sanitize(terr.Error()))
	}
	// this IS the display a later decision is compared against
	if h != nil {
		recordShownRecovery(shownRecoveryFrom(cr, *h))
	}

	facts := map[string]any{"session": sess.ID, "agent": a.ID, "held": h != nil, "tasks": tasks, "holds": holds}
	if h != nil {
		facts["hold"] = *h
	}
	emit(facts, func() {
		if h == nil {
			fmt.Printf("the queue of agent %s (%s) in session %s is not held; nothing is waiting on a decision\n", a.Name, a.ID, sess.ShortID)
			printQueue(tasks, "")
			return
		}
		fmt.Printf("the queue of agent %s (%s) in session %s is HELD\n", a.Name, a.ID, sess.ShortID)
		fmt.Printf("  %-15s %s (%s)\n", blockedWord(*h), h.BlockingTask, figure(h.BlockingState))
		if strings.TrimSpace(h.BlockingSummary) == "" {
			fmt.Printf("  %-15s nothing was recorded\n", "it reported")
		} else {
			fmt.Printf("  %-15s %s\n", "it reported", sanitize(h.BlockingSummary))
		}
		fmt.Printf("  %-15s %s\n", "held because", sanitize(figure(h.Reason)))
		fmt.Printf("  %-15s %s\n", "waiting", sanitize(figure(h.Consequence)))
		switch {
		case h.SessionEpoch == 0:
			fmt.Printf("  %-15s unavailable; nothing can be decided until it can be read\n", "generation")
		case h.Epoch != 0 && h.SessionEpoch != h.Epoch:
			fmt.Printf("  %-15s recorded under %d, and the session now runs %d: it was restored since\n", "generation", h.Epoch, h.SessionEpoch)
		default:
			fmt.Printf("  %-15s %d\n", "generation", h.SessionEpoch)
		}
		fmt.Println("your choices")
		for _, c := range h.Choices {
			for _, line := range choiceLines(c, verbFor(c.Decision, a.Name, sess)) {
				fmt.Println(line)
			}
		}
		printQueue(tasks, h.BlockingTask)
	})
}

// ---------------------------------------------------------------------
// the decision
// ---------------------------------------------------------------------

// decidableHold reports the reasons a hold cannot be decided at all: it is
// another session's, it has already been decided, or the generation a
// decision must name cannot be read.
func decidableHold(sess inventoryRow, agentName string, h queueHold) error {
	reread := fmt.Sprintf("ks agent queue show %s --session %s", agentName, sess.ShortID)
	if want := agentSessionID(sess); h.SessionID != "" && h.SessionID != want {
		return &cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("hold %s belongs to another session, not %s; nothing was decided", h.ID, sess.ShortID),
			NextAction: "ks session list"}
	}
	if h.State != holdIsActive {
		return &cliError{Code: exitConflict, Kind: "hold_not_active",
			Message: fmt.Sprintf("this queue was already decided elsewhere (%s, by %s), so nothing was decided here; read it again before deciding",
				figure(h.Decision), figure(h.DecidedBy)),
			NextAction: reread}
	}
	if h.SessionEpoch == 0 {
		return &cliError{Code: exitTemporary, Kind: "generation_unavailable",
			Message: fmt.Sprintf("the execution generation of session %s could not be read, so this decision cannot say which one it was prepared under; nothing was decided",
				sess.ShortID),
			NextAction: reread}
	}
	return nil
}

// aboutToDecideHold is the display a decision makes for itself when nothing
// has put this hold in front of anyone yet.
func aboutToDecideHold(h queueHold, decision string) string {
	return sanitize(fmt.Sprintf("about to record %s on the queue held behind %s %s (%s): %s (revision %d, generation %d)",
		decision, blockedWord(h), h.BlockingTask, figure(h.BlockingState), figure(h.Consequence), h.Revision, h.SessionEpoch))
}

func heldCount(h queueHold) string {
	switch len(h.HeldTasks) {
	case 0:
		return "nothing"
	case 1:
		return "1 instruction"
	}
	return fmt.Sprintf("%d instructions", len(h.HeldTasks))
}

// recoveryChangedSinceShown refuses a decision because the queue is no longer
// the one that was shown. NOTHING IS SENT. The client does not replace what
// the person read with what the service says now — adopting the fresh
// revision is precisely how a queue gets released over an outcome somebody
// never saw. The new state is displayed here, and deciding it is a fresh ask.
func recoveryChangedSinceShown(sess inventoryRow, agentName string, now queueHold, decision string) error {
	what := "released"
	if decision == decisionKeep {
		what = "recorded"
	}
	blocking := fmt.Sprintf("%s now reads %s", now.BlockingTask, figure(now.BlockingState))
	if strings.TrimSpace(now.BlockingSummary) != "" {
		blocking += fmt.Sprintf(" (%s)", now.BlockingSummary)
	}
	return &cliError{Code: exitConflict, Kind: "recovery_changed",
		Message: sanitize(fmt.Sprintf("this queue changed after it was shown, so nothing was %s and no decision was sent. It now reads: %s, with %s waiting behind it; %s (revision %d, generation %d). Read that, and ask again to decide the queue you can see.",
			what, blocking, heldCount(now), figure(now.Consequence), now.Revision, now.SessionEpoch)),
		NextAction: fmt.Sprintf("ks agent queue show %s --session %s", agentName, sess.ShortID)}
}

// decideQueueHold records one decision about the exact queue a person was
// SHOWN.
//
// The client remembers what it displayed, reads the hold again immediately
// before deciding, and COMPARES the two. Only a queue that still reads the
// same is decided, and it is decided under the revision that was on the
// screen, so a record that moved between the comparison and the request is
// refused by the service rather than by guesswork here. A queue that changed
// — a blocking instruction that resolved, different work waiting behind it, a
// session restored underneath it, an option that now does something else — is
// displayed and refused: the decision is not sent, and the person has to ask
// again now that they can see what they would be deciding. That refusal is
// the same in a script as at a keyboard.
//
// Where nothing has been displayed yet (a hold decided straight from a
// terminal that has not looked at it), this displays it first and that
// display is what the read below is compared against.
func decideQueueHold(cr hostedCreds, sess inventoryRow, agentName string, h queueHold, decision, finding string) (queueHold, error) {
	shown, remembered := lastShownRecovery(cr, h.ID)
	if !remembered {
		if err := decidableHold(sess, agentName, h); err != nil {
			return h, err
		}
		progress("%s", aboutToDecideHold(h, decision))
		shown = shownRecoveryFrom(cr, h)
		recordShownRecovery(shown)
	}
	// read it again IMMEDIATELY before deciding, and compare it with what was
	// shown rather than adopting it
	holds, err := fetchQueueHolds(cr, h.AgentID)
	if err != nil {
		return h, err
	}
	var now *queueHold
	for i := range holds {
		if holds[i].ID == h.ID {
			now = &holds[i]
			break
		}
	}
	if now == nil {
		return h, &cliError{Code: exitConflict, Kind: "hold_gone",
			Message:    fmt.Sprintf("hold %s is no longer reported for this agent, so nothing was decided", h.ID),
			NextAction: fmt.Sprintf("ks agent queue show %s --session %s", agentName, sess.ShortID)}
	}
	if err := decidableHold(sess, agentName, *now); err != nil {
		return *now, err
	}
	if !sameRecovery(shown, *now) {
		// the refusal below displays the new state, so the new state is what
		// a second ask will be compared against
		recordShownRecovery(shownRecoveryFrom(cr, *now))
		return *now, recoveryChangedSinceShown(sess, agentName, *now, decision)
	}
	// the revision is the one that was on the screen; the generation is the
	// one the service reports for the session now, and the comparison above
	// has already established that they are the same generation that was read
	body := map[string]any{
		"decision":          decision,
		"finding":           finding,
		"epoch":             now.SessionEpoch,
		"expected_revision": shown.Revision,
	}
	var env struct {
		Data queueHold `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/queue-holds/"+url.PathEscape(now.ID)+"/decision", body, &env); err != nil {
		return *now, recoveryRefusal(sess, agentName, *now, decision, err)
	}
	out := env.Data
	if out.ID == "" {
		out = *now
	}
	return out, nil
}

// recoveryRefusal turns the refusals that mean "this is not the queue you
// read" into one clear sentence each. None of them is retried: a decision
// that missed its queue is not a decision to send again automatically.
func recoveryRefusal(sess inventoryRow, agentName string, h queueHold, decision string, err error) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	what := "released"
	if decision == decisionKeep {
		what = "recorded"
	}
	reread := fmt.Sprintf("ks agent queue show %s --session %s", agentName, sess.ShortID)
	switch he.Type {
	case "ks_revision_conflict":
		return &cliError{Code: exitConflict, Kind: "revision_conflict",
			Message: fmt.Sprintf("the queue held behind %s moved on while it was being decided, so nothing was %s. Read it again and decide the state you can see.",
				h.BlockingTask, what),
			NextAction: reread}
	case "ks_epoch_mismatch":
		return &cliError{Code: exitConflict, Kind: "generation_moved",
			Message: fmt.Sprintf("session %s was restored while this was being decided, so nothing was %s: a decision prepared before a restore is not applied after one. Read the queue again and decide the state you can see.",
				sess.ShortID, what),
			NextAction: reread}
	case "ks_hold_not_active":
		return &cliError{Code: exitConflict, Kind: "hold_not_active",
			Message:    fmt.Sprintf("this queue was already decided elsewhere, so nothing was %s here; read it again before deciding.", what),
			NextAction: reread}
	case "ks_retry_unavailable":
		// this client never sends that decision. If a control plane answers
		// with it anyway, the reason is the service's to state and not this
		// client's to soften.
		return &cliError{Code: exitIntegrity, Kind: "retry_unavailable",
			Message: sanitize(he.Message), NextAction: reread}
	}
	return err
}

// findingOf reads the one thing a decision over unsettled work must carry:
// what the person established. It is a flag rather than a prompt so that it
// reads the same in a script as at a keyboard, and it is REQUIRED, because a
// decision about an outcome nobody measured is worth little without the
// record of what was found out instead.
func findingOf(inv *Invocation, usage string) string {
	finding := strings.TrimSpace(inv.Str("finding"))
	if finding == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "--finding records what you established about the blocked instruction; a decision over an outcome nobody measured is not worth keeping without it",
			NextAction: usage})
	}
	return finding
}

// decisionReport prints what was decided and what it did, in the service's
// own terms, ending with the one sentence a person must not be able to
// misread: the blocking instruction was not run again.
func decisionReport(sess inventoryRow, agentName string, before, after queueHold, decision string) {
	emit(map[string]any{"session": sess.ID, "agent": after.AgentID, "hold": after,
		"decision": after.Decision, "decided_at": after.DecidedAt}, func() {
		if decision == decisionKeep {
			fmt.Printf("the queue of agent %s stays held behind %s %s (%s): %s\n",
				agentName, blockedWord(after), after.BlockingTask, figure(after.BlockingState), sanitize(figure(after.Finding)))
			fmt.Printf("%s, and the decision to leave it held is recorded\n", sanitize(figure(after.Consequence)))
			return
		}
		released := after.ReleasedTasks
		if len(released) == 0 {
			released = before.HeldTasks
		}
		fmt.Printf("resumed the queue of agent %s: %d instruction(s) may run again, in the order they were committed\n", agentName, len(released))
		for _, id := range released {
			fmt.Printf("  %s\n", id)
		}
		fmt.Printf("the %s %s still reads %s and was NOT run again\n",
			blockedWord(after), after.BlockingTask, figure(after.BlockingState))
	})
}

// ---------------------------------------------------------------------
// ks agent queue resume | ks agent queue hold
// ---------------------------------------------------------------------

func hostedQueueDecision(decision string) func(hostedCreds, *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		sess := agentSession(cr, inv)
		a, err := resolveAgent(cr, sess, inv.Arg(0))
		if err != nil {
			die(err)
		}
		verb := "resume"
		if decision == decisionKeep {
			verb = "hold"
		}
		finding := findingOf(inv, fmt.Sprintf("ks agent queue %s %s --session %s --finding \"what you established\"", verb, a.Name, sess.ShortID))
		holds, err := fetchQueueHolds(cr, a.ID)
		if err != nil {
			die(err)
		}
		h, err := activeHold(holds)
		if err != nil {
			die(err)
		}
		if h == nil {
			fail(&cliError{Code: exitConflict, Kind: "not_held",
				Message:    fmt.Sprintf("the queue of agent %s is not held, so there was nothing to decide and nothing was changed", a.Name),
				NextAction: fmt.Sprintf("ks agent queue show %s --session %s", a.Name, sess.ShortID)})
		}
		after, err := decideQueueHold(cr, sess, a.Name, *h, decision, finding)
		if err != nil {
			die(err)
		}
		decisionReport(sess, a.Name, *h, after, decision)
	}
}

// ---------------------------------------------------------------------
// ks task resume
// ---------------------------------------------------------------------

// hostedTaskResume continues the instructions held behind ONE named
// instruction. It is the same recorded decision as ks agent queue resume,
// reached from the other end: you name the instruction you were looking at
// rather than the agent whose queue stopped.
//
// It does NOT restart the instruction you name, and it cannot come to do so
// by accident: this client sends a release, a release leaves that record
// exactly as it stands, and the service offers no decision on this route that
// would restart it. Where a person means "run that again", the recovery view
// says in the service's own words that it is not offered, and why.
func hostedTaskResume(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "the id of the blocked instruction is required; nothing is resumed by position",
			NextAction: fmt.Sprintf("ks agent queue show <name> --session %s", sess.ShortID)})
	}
	finding := findingOf(inv, fmt.Sprintf("ks task resume %s --session %s --finding \"what you established\"", id, sess.ShortID))
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	// whose queue this is, resolved inside the session that was named. An
	// instruction belonging to an agent of some OTHER session is refused here,
	// before any hold is read: naming one session and deciding another's queue
	// is never what somebody meant.
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	name := a.Name
	holds, err := fetchQueueHolds(cr, t.AgentID)
	if err != nil {
		die(err)
	}
	h, err := activeHold(holds)
	if err != nil {
		die(err)
	}
	switch {
	case h == nil:
		fail(&cliError{Code: exitConflict, Kind: "not_held",
			Message: fmt.Sprintf("nothing is held behind instruction %s: its queue is not held, and this changed nothing (it reads %s)",
				t.ID, figure(t.State)),
			NextAction: fmt.Sprintf("ks agent queue show %s --session %s", name, sess.ShortID)})
	case h.BlockingTask != t.ID:
		fail(&cliError{Code: exitConflict, Kind: "not_the_blocking_instruction",
			Message: fmt.Sprintf("instruction %s is not what holds this queue: the queue is held behind %s (%s), and nothing was resumed. Resuming names the instruction the queue is waiting on.",
				t.ID, h.BlockingTask, figure(h.BlockingState)),
			NextAction: fmt.Sprintf("ks task resume %s --session %s --finding \"what you established\"", h.BlockingTask, sess.ShortID)})
	}
	after, err := decideQueueHold(cr, sess, name, *h, decisionResume, finding)
	if err != nil {
		die(err)
	}
	decisionReport(sess, name, *h, after, decisionResume)
}
