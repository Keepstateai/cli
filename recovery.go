// recovery.go: what a person does when an agent's queue stops moving.
//
// An agent's queue is held when the instruction in front of it FAILED, or
// when nobody could establish what that instruction did. Nothing behind it
// starts until somebody decides that it should. That is the point: the
// instruction after "publish the release" is "announce the release", and
// announcing something that failed to publish — or that may or may not have
// published — is exactly the work a machine should not start on its own.
//
// THREE CHOICES, and they are not interchangeable:
//
//	continue the rest     `ks agent queue resume NAME` releases the
//	                      instructions held behind the blocked one. The
//	                      blocked one is not repeated and its recorded
//	                      outcome is not touched.
//	try that one again    `ks task resume TASK_ID` is a NEW ATTEMPT at the
//	                      blocked instruction itself, started from a recorded
//	                      boundary. It releases nothing, and it never rewrites
//	                      the attempt that already ran.
//	leave everything      `ks agent queue hold NAME` records that the queue
//	                      stays held, and what was established — a queue that
//	                      waits because somebody decided it should is a
//	                      different record from a queue nobody has looked at.
//
// `ks agent queue show` is the recovery view all three are chosen from: which
// instruction is blocking, what it last reported, what waits behind it, and
// what each choice would do, cost and leave behind.
//
// NO VERB HERE STANDS IN FOR ANOTHER. This matters most where one of them
// cannot be performed. The service records no attempt under an instruction
// and no boundary for one to resume from, so the second choice cannot be
// carried out today — and an unsupported operation that quietly performs a
// DIFFERENT one is worse than one that refuses, because the person walks away
// believing they got what they asked for. So `ks task resume` states the
// problem as fields, names exactly what is missing, and stops. It never
// releases the queue as a consolation: somebody who asked to run one
// instruction again did not ask to start everything committed after it.
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
// known failure from an outcome nobody could establish.
//
// decisionRetry is listed here and NEVER SENT. It is the third choice — a new
// attempt at the blocked instruction — and this client recognises the word
// only so that it can find that option among the ones the service states,
// read back the service's own reason for not offering it, and refuse in the
// same terms. A client that did not know the word would have to invent a
// reason, and an invented reason about somebody else's boundary is a guess
// wearing a service's voice.
const (
	decisionResume = "release_successors"
	decisionKeep   = "keep_held"
	decisionRetry  = "retry_from_safe_point"
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

// nextAction is one option a person actually has, as a FIELD RECORD rather
// than a sentence: the decision, whether it is offered, the exact command
// that records it, and the service's own words for what it would do, what
// the records would read afterwards and what it costs.
//
// Nothing here is composed. An effect, a state or a cost the service did not
// state is ABSENT — never "none", never "$0", never a figure derived from a
// rate this client does not have. The ratified price book carries no
// per-token rate, so a client that printed a dollar amount here would have
// made it up.
type nextAction struct {
	Decision   string `json:"decision"`
	Available  bool   `json:"available"`
	Command    string `json:"command,omitempty"`
	Does       string `json:"does,omitempty"`
	Leaves     string `json:"leaves,omitempty"`
	Costs      string `json:"costs,omitempty"`
	NotOffered string `json:"not_offered_because,omitempty"`
}

// actionFrom binds one option the service stated to the command that records
// it here. An option the service does not offer carries no command: printing
// the verb for something that cannot be done invites running it.
func actionFrom(c holdChoice, command string) nextAction {
	a := nextAction{Decision: c.Decision, Available: c.Available,
		Does: c.Effect, Leaves: c.StateAfter, Costs: c.Cost, NotOffered: c.Unavailable}
	if c.Available {
		a.Command = command
	}
	return a
}

// lines renders one option for a reader. This is the single renderer: the
// recovery view and every refusal that lists what may be done instead print
// the same shape, so two surfaces cannot describe one option differently.
func (a nextAction) lines() []string {
	head := "  " + a.Decision
	switch {
	case !a.Available:
		head += "  —  NOT OFFERED"
	case a.Command != "":
		head += "  —  " + a.Command
	}
	out := []string{sanitize(head)}
	add := func(label, text string) {
		if strings.TrimSpace(text) != "" {
			out = append(out, sanitize(fmt.Sprintf("      %-9s %s", label, text)))
		}
	}
	if !a.Available {
		add("why not", a.NotOffered)
	}
	add("does", a.Does)
	add("leaves", a.Leaves)
	add("costs", a.Costs)
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
			for _, line := range actionFrom(c, verbFor(c.Decision, a.Name, sess)).lines() {
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

func heldCount(h queueHold) string { return countWord(len(h.HeldTasks)) }

// countWord says how much work a number of instructions is, including when
// it is none. "nothing" is a real answer here and never an empty line.
func countWord(n int) string {
	switch n {
	case 0:
		return "nothing"
	case 1:
		return "1 instruction"
	}
	return fmt.Sprintf("%d instructions", n)
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
		// what the service says it released, never what this client assumed
		// it would. Work that was cancelled while the queue was held is not
		// released by a decision to continue, and a client that counted the
		// instructions it last saw held would report it as running again.
		if len(after.ReleasedTasks) == 0 {
			fmt.Printf("resumed the queue of agent %s: the service listed nothing that it released, so what is running again is not reported here rather than assumed\n", agentName)
			fmt.Printf("  %s was held behind it when this decision was prepared\n", figure(heldCount(before)))
		} else {
			fmt.Printf("resumed the queue of agent %s: %d instruction(s) may run again, in the order they were committed\n", agentName, len(after.ReleasedTasks))
			for _, id := range after.ReleasedTasks {
				fmt.Printf("  %s\n", sanitize(id))
			}
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
// the problem, as fields
// ---------------------------------------------------------------------

// blockedStatement is what is wrong, as FIELDS. Which instruction is
// blocked, what it is known to have done, what nobody established, what is
// waiting behind it, what a next step would need that does not exist, and
// what may actually be done instead — each one readable on its own, in
// --json as a document and for a reader as an aligned block.
//
// A refusal that hides those facts inside one sentence makes somebody parse
// prose to find out what is stopping their work, and they will parse it
// wrongly under pressure. So this is the shape of the answer, and the
// message beside it is a summary of this rather than the only copy of it.
//
// Nothing in here is composed. Every value is a field the service recorded
// or the plain absence of one, and an absence is printed as an absence
// ("nothing was recorded") rather than filled in with something plausible.
type blockedStatement struct {
	Instruction  string `json:"instruction"`
	KnownOutcome string `json:"known_outcome"`
	// Reported is what the runner last said about the blocked instruction.
	// It is present only when the hold is about THIS instruction: the
	// service records a report against the instruction that stopped the
	// queue, and claiming "nothing was recorded" about some other one would
	// state an absence this client never looked for.
	Reported    string `json:"it_reported,omitempty"`
	Hold        string `json:"hold,omitempty"`
	Blocking    string `json:"blocking_instruction,omitempty"`
	HeldBecause string `json:"held_because,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
	// NotEstablished is what nobody measured about the work that already ran.
	// Missing is what a next step would need and this service does not keep.
	// They are different kinds of fact and are never merged: the first is
	// about the past, the second is about what the service can do at all.
	NotEstablished []string `json:"not_established"`
	Missing        []string `json:"what_is_missing"`
	// attemptReason is the first entry of Missing WITHOUT its label, for the
	// one-sentence summary beside the block. Unexported and unserialised: the
	// document already carries the fact, labelled, in what_is_missing, and a
	// second copy under a second name is a second thing to keep in step.
	attemptReason  string       `json:"-"`
	HeldSuccessors []string     `json:"held_successors"`
	NextActions    []nextAction `json:"next_actions"`
}

// detailLines states the same facts to a person, in the field order a person
// reads them in: which record, what it did, what waits, what is unknown,
// what is missing, and only then what may be done.
func (b blockedStatement) detailLines() []string {
	field := func(label, value string) string { return sanitize(fmt.Sprintf("  %-16s %s", label, value)) }
	out := []string{"what is blocked"}
	out = append(out, field("instruction", b.Instruction))
	out = append(out, field("known outcome", figure(b.KnownOutcome)))
	if b.Blocking != "" && b.Blocking == b.Instruction {
		if strings.TrimSpace(b.Reported) == "" {
			out = append(out, field("it reported", "nothing was recorded"))
		} else {
			out = append(out, field("it reported", b.Reported))
		}
	}
	if strings.TrimSpace(b.HeldBecause) != "" {
		out = append(out, field("held because", b.HeldBecause))
	}
	if b.Blocking != "" && b.Blocking != b.Instruction {
		out = append(out, field("queue held behind", b.Blocking))
	}
	switch {
	case b.Hold == "":
		out = append(out, field("held behind it", "nothing: this queue is not held"))
	case len(b.HeldSuccessors) == 0:
		out = append(out, field("held behind it", "nothing"))
	default:
		out = append(out, field("held behind it",
			fmt.Sprintf("%s (%s)", countWord(len(b.HeldSuccessors)), strings.Join(b.HeldSuccessors, ", "))))
	}
	out = append(out, labelledList(field, "not established", b.NotEstablished)...)
	out = append(out, labelledList(field, "what is missing", b.Missing)...)
	if len(b.NextActions) > 0 {
		out = append(out, "what you may do instead")
		for _, a := range b.NextActions {
			out = append(out, a.lines()...)
		}
	}
	return out
}

// labelledList prints one label over several entries, and the label once: a
// heading repeated down the left margin reads as several separate problems.
func labelledList(field func(string, string) string, label string, values []string) []string {
	out := make([]string, 0, len(values))
	for i, v := range values {
		if i > 0 {
			label = ""
		}
		out = append(out, field(label, v))
	}
	return out
}

// retryChoice finds the option for running the blocked instruction again
// among the ones the SERVICE states, or nil where it states none.
func retryChoice(h *queueHold) *holdChoice {
	if h == nil {
		return nil
	}
	for i := range h.Choices {
		if h.Choices[i].Decision == decisionRetry {
			return &h.Choices[i]
		}
	}
	return nil
}

// whyNoNewAttempt says why there will be no new attempt, and takes care to
// say something TRUE in each of the three worlds it can be asked in.
//
// The service states a reason: that reason is what a person is shown, in the
// service's words, so one boundary is described the same way wherever it is
// met and this client paraphrases nobody.
//
// The service states nothing at all: this client says what it knows — there
// is no attempt record under an instruction and no boundary for one to
// resume from — and that is the situation today.
//
// The service OFFERS it: then the sentence above would be a lie, and the
// honest answer is the narrower one. The verb still refuses, because this
// client has no way to name a boundary or bind a request to one; what it must
// not do is explain that refusal by asserting something about the service
// that has stopped being true. A client that reported its own missing
// half as the service's is how a stale surface outlives the thing it
// described.
func whyNoNewAttempt(h *queueHold) (reason string, offered bool) {
	c := retryChoice(h)
	switch {
	case c == nil:
		return retryMissingHere, false
	case c.Available:
		return retryOfferedNotHere, true
	case strings.TrimSpace(c.Unavailable) != "":
		return strings.TrimSpace(c.Unavailable), false
	}
	return retryMissingHere, false
}

// what a new attempt needs and this service does not keep. The first is
// stated only where the service states nothing itself; the second while the
// service offers no boundary at all, because it is the mistake a person is
// most likely to make in that world.
//
// `ks checkpoint` saves a session, and the capability registry reports that
// as available — so "a saved point exists" is an easy thing to believe. It
// is a different object: a whole machine at a moment, naming no instruction
// and marking no place in the queue. Resuming one would rewind work this
// command never named, which is why it is not the boundary a retry needs and
// why this client will not quietly treat it as one.
const (
	retryMissingHere = "this service records no attempt under an instruction, and no boundary for one to resume from, so a new attempt could neither be created nor say where it would start"
	// and the other direction: a service that has since grown the records,
	// read by a client that has not grown the command
	retryOfferedNotHere = "this service now offers a new attempt from a recorded boundary and this version of the client cannot ask for one: it would have to name the boundary and bind the request to it, and it can do neither. Update the client, or continue the rest of the queue instead"
	savedSessionNote    = "a boundary this instruction could resume from: saving a session stores the whole machine at that moment — it names no instruction and marks no place in the queue — so restoring one would rewind work nobody named, and it is not such a boundary"
)

// retryableState says whether there is anything to run again at all. A
// finished instruction is refined by submitting a new one; a queued or held
// one has not run yet; and a queue held behind somebody else's failure does
// not make its own successors retryable.
func retryableState(s string) bool {
	switch s {
	case "failed", "reconciliation_required", "unknown", "cancelled":
		return true
	}
	return false
}

// statementFor reads the queue's trouble into fields. Everything comes from
// the service's record: the state it reports, the words it wrote, the
// options it states and its own reason for the one it does not offer.
func statementFor(sess inventoryRow, agentName string, t taskRow, h *queueHold) blockedStatement {
	b := blockedStatement{Instruction: t.ID, KnownOutcome: t.State,
		NotEstablished: []string{}, Missing: []string{}, HeldSuccessors: []string{}, NextActions: []nextAction{}}
	if h != nil {
		b.Hold, b.Blocking, b.HeldBecause, b.Generation = h.ID, h.BlockingTask, h.Reason, h.SessionEpoch
		b.HeldSuccessors = append(b.HeldSuccessors, h.HeldTasks...)
		if h.BlockingTask == t.ID {
			// the hold carries the blocked instruction's state and last
			// report; where it does, it is the fresher of the two reads
			if h.BlockingState != "" {
				b.KnownOutcome = h.BlockingState
			}
			b.Reported = h.BlockingSummary
			if strings.TrimSpace(h.BlockingSummary) == "" {
				b.NotEstablished = append(b.NotEstablished, "what this instruction did: nothing was recorded")
			}
			if h.Cause != causeFailed {
				// C05's ambiguity contract: an unknown is not a failure, and
				// the difference decides what may safely be done next
				b.NotEstablished = append(b.NotEstablished,
					"its outcome: the queue stopped on a result nobody could establish, not on a measured failure")
			}
		}
		for _, c := range h.Choices {
			b.NextActions = append(b.NextActions, actionFrom(c, verbFor(c.Decision, agentName, sess)))
		}
	}
	reason, offered := whyNoNewAttempt(h)
	b.attemptReason = reason
	b.Missing = append(b.Missing, "a new attempt under this instruction: "+reason)
	if !offered {
		// the likely misunderstanding, stated only while it IS one: where the
		// service does record a boundary, saying that none exists would be
		// the client inventing an absence
		b.Missing = append(b.Missing, savedSessionNote)
	}
	return b
}

// ---------------------------------------------------------------------
// the retry refusal, kept for the cases that still have no boundary
// ---------------------------------------------------------------------

// taskRetryRefusal is the answer this verb gives, with the facts attached.
//
// Two different refusals, because they are two different facts. An
// instruction that did not fail has nothing to run again — a state conflict,
// and the first thing its reader needs to know. An instruction that did fail
// cannot be run again HERE, because the records a new attempt is made of do
// not exist: a known operation failure by C03's table, and not an integrity
// rejection, a syntax mistake or something a permission would unlock.
//
// Both say, in the same words every time, that nothing was released. That
// sentence is the whole point of this verb's existence in this form.
func taskRetryRefusal(sess inventoryRow, agentName string, t taskRow, h *queueHold) error {
	b := statementFor(sess, agentName, t, h)
	reread := fmt.Sprintf("ks agent queue show %s --session %s", agentName, sess.ShortID)
	if !retryableState(t.State) {
		return &cliError{Code: exitConflict, Kind: "not_retryable", Detail: b, NextAction: reread,
			Message: fmt.Sprintf("instruction %s reads %s: there is no failed or unresolved attempt under it to run again, so nothing was retried and nothing was released. A finished instruction is refined by submitting a new one.",
				t.ID, figure(t.State))}
	}
	// the message carries the one reason a new attempt cannot be made; the
	// block above it carries every missing thing as its own field. A message
	// that recited the whole list would be read by nobody, and the list is
	// not the summary's job.
	return &cliError{Code: exitFailed, Kind: "retry_unavailable", Detail: b, NextAction: reread,
		Message: sanitize(fmt.Sprintf("instruction %s was NOT run again, nothing was released and no decision was sent. Running it again means a NEW attempt under it, started from a recorded boundary, and this service records neither — so there is nothing here to authorize, and no permission would change the answer. What is missing: %s",
			t.ID, b.attemptReason))}
}
