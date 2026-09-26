// tasks.go — `ks task list` and `ks task show`: reading the instructions in
// an agent's queue.
//
// Before these, the only way to see an instruction's standing was the
// recovery view, which answers a different question — why a queue is STUCK.
// An agent whose queue is moving normally had no readable queue at all, and
// the service's own advice named these two verbs on nine different refusals.
// Advice that names a command nobody can type is a route the product only
// imagines it has.
//
// Both are READS and nothing else. They send no mutation, take no lease,
// hold no window, decide nothing and start nothing. Every field shown comes
// back from a route the service publishes:
//
//	GET /api/v2/tasks?agent_id=...   the queue and history of one agent
//	GET /api/v2/tasks/{id}           one instruction
//	GET /api/v2/tasks/{id}/content   its exact bytes, from the content store
//	GET /api/v2/queue-holds?agent_id the hold, when one stands
//
// Three things this deliberately does NOT do.
//
// It does not invent attempt history. C03 asks for it; this service records
// a `current_attempt_id` and nothing else, and where it holds no attempt
// this says so in those words rather than rendering a plausible one. A
// fabricated attempt list is exactly what `ks task resume` refuses to act
// on, and a reader who saw one here would reasonably expect that verb to
// work.
//
// It does not promote Finished to Verified. `verification_state` is an
// INDEPENDENT verifier's finding; the service refuses to let a runner write
// it, and this client refuses to derive it. An instruction that ended
// without one reads finished, never verified.
//
// It does not fill in a blank. Where the service recorded no reason, no
// author or no content, this prints "not recorded" — because a gap a client
// papers over is a gap nobody ever fixes.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// taskContent is the exact instruction bytes, as the content route answers
// them. Read separately because the task row carries a REFERENCE, not the
// text: the words live in the content store and are quoted from there.
type taskContent struct {
	TaskID    string `json:"task_id"`
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
	Text      string `json:"text"`
}

func fetchTaskContent(cr hostedCreds, id string) (taskContent, error) {
	var env struct {
		Data taskContent `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/tasks/"+url.PathEscape(id)+"/content", nil, &env); err != nil {
		return taskContent{}, err
	}
	return env.Data, nil
}

// notRecorded is the one sentence every empty field gets. It is deliberately
// the same words everywhere: a reader learns once that this client does not
// guess, instead of decoding a different euphemism per field.
func notRecorded(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not recorded"
	}
	return s
}

// taskAuthor says who submitted an instruction, in the service's own two
// fields. A row with neither is not attributed to the reader.
func taskAuthor(t taskRow) string {
	switch {
	case t.AuthorType != "" && t.AuthorID != "":
		return t.AuthorType + " " + t.AuthorID
	case t.AuthorType != "":
		return t.AuthorType
	case t.AuthorID != "":
		return t.AuthorID
	}
	return "not recorded"
}

// verificationLine renders C05's fourth column WITHOUT ever inventing it.
func verificationLine(t taskRow) string {
	if strings.TrimSpace(t.Verification) == "" {
		return "none: this instruction has no independent verification, so it reads finished and never verified"
	}
	return stateLabel("verification_state", t.Verification)
}

// attemptLine states the attempt identity the service holds for this
// instruction NOW, or the fact that it holds none. It says nothing about
// history: the history is read from the attempts route and printed below
// it, and a sentence here claiming there is none would be false the moment
// the service began keeping it — which it now does.
func attemptLine(t taskRow) string {
	if strings.TrimSpace(t.CurrentAttempt) == "" {
		return "the service records no attempt under this instruction"
	}
	return t.CurrentAttempt + " (the one running or last dispatched; earlier ones are listed below)"
}

func taskListLine(t taskRow) string {
	return fmt.Sprintf("%-26s %4d %-12s %-9s %-10s %s",
		clip(t.ID, 26), t.QueueSeq, clip(stateCell("task_state", t.State), 12), clip(figure(t.Origin), 9),
		clip(taskAuthor(t), 10), figure(t.CreatedAt))
}

// sortTasks puts the queue in the order it was COMMITTED, which is the only
// order a queue has. Ties fall back to the id so two readings of one queue
// never differ.
func sortTasks(ts []taskRow) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].QueueSeq != ts[j].QueueSeq {
			return ts[i].QueueSeq < ts[j].QueueSeq
		}
		return ts[i].ID < ts[j].ID
	})
}

// ---------------------------------------------------------------------
// ks task list
// ---------------------------------------------------------------------

// hostedTaskList shows the instructions of one agent, or of every agent in
// the session when none is named. The service's list route is per agent by
// design (it answers ks_agent_required otherwise), so a session-wide listing
// is that route asked once per agent — never a wider route this client
// wished existed.
//
// A failed read of one agent's queue is NOT an empty queue. It is named,
// the exit is a failure, and the agents that did answer are still shown:
// reporting a queue as empty because it could not be read is the mistake
// that let a release strand every instruction it was holding.
func hostedTaskList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	agents, err := fetchAgents(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	if name := strings.TrimSpace(inv.Str("agent")); name != "" {
		// resolved against the agents already read, so one listing is one
		// view of the session rather than two reads that may disagree
		a, aerr := pickAgent(sess, agents, name)
		if aerr != nil {
			die(aerr)
		}
		agents = []agentRow{a}
	}
	var out []taskQueue
	unreadable := 0
	total := 0
	for _, a := range agents {
		ts, terr := fetchTasks(cr, a.ID)
		if terr != nil {
			unreadable++
			out = append(out, taskQueue{Agent: a.Name, AgentID: a.ID, Tasks: []taskRow{},
				Unreadable: sanitize(terr.Error())})
			continue
		}
		sortTasks(ts)
		total += len(ts)
		out = append(out, taskQueue{Agent: a.Name, AgentID: a.ID, Tasks: ts})
	}
	// An incomplete listing is ONE answer, not a document followed by an
	// error: --json must never print two envelopes, and nobody must be shown
	// a queue listing that looks whole above an exit code saying it is not.
	if unreadable > 0 {
		fail(&cliError{Code: exitTemporary, Kind: "queue_unreadable",
			Detail: partialQueues{Session: sess.ID, Queues: out, Tasks: total, Unreadable: unreadable},
			Message: fmt.Sprintf("%d of %d queues could not be READ and are NOT reported empty; nothing here is a complete picture of this session's instructions",
				unreadable, len(out)),
			NextAction: fmt.Sprintf("ks task list --session %s", sess.ShortID)})
	}
	emit(map[string]any{"session": sess.ID, "queues": out, "tasks": total,
		"unreadable_queues": unreadable}, func() {
		if len(out) == 0 {
			fmt.Printf("No agents in session %s, so no instructions.\n", sess.ShortID)
			return
		}
		for _, q := range out {
			fmt.Printf("agent %s (%s)\n", q.Agent, q.AgentID)
			if q.Unreadable != "" {
				fmt.Printf("  this queue could not be READ, so it is not shown; it is not known to be empty: %s\n", q.Unreadable)
				continue
			}
			if len(q.Tasks) == 0 {
				fmt.Printf("  no instructions\n")
				continue
			}
			fmt.Printf("  %-26s %4s %-12s %-9s %-10s %s\n", "INSTRUCTION", "POS", "STATE", "ORIGIN", "AUTHOR", "SUBMITTED")
			for _, t := range q.Tasks {
				fmt.Printf("  %s\n", taskListLine(t))
			}
		}
		fmt.Printf("%d instruction(s) across %d agent(s); one in full: ks task show <instruction> --session %s\n",
			total, len(out), sess.ShortID)
	})
}

// taskQueue is one agent's queue as this verb reports it, including the
// case where it could not be read at all. An unreadable queue keeps its
// place in the answer with an empty task list AND a reason, so nothing
// downstream can mistake it for an agent with no instructions.
type taskQueue struct {
	Agent      string    `json:"agent"`
	AgentID    string    `json:"agent_id"`
	Tasks      []taskRow `json:"tasks"`
	Unreadable string    `json:"unreadable,omitempty"`
}

// partialQueues is what an incomplete listing carries as FIELDS: what was
// read, and what could not be.
type partialQueues struct {
	Session    string      `json:"session"`
	Queues     []taskQueue `json:"queues"`
	Tasks      int         `json:"tasks"`
	Unreadable int         `json:"unreadable_queues"`
}

func (p partialQueues) detailLines() []string {
	lines := []string{fmt.Sprintf("session            %s", p.Session),
		fmt.Sprintf("queues read        %d of %d", len(p.Queues)-p.Unreadable, len(p.Queues)),
		fmt.Sprintf("instructions read  %d", p.Tasks)}
	for _, q := range p.Queues {
		if q.Unreadable != "" {
			lines = append(lines, fmt.Sprintf("  agent %s (%s): this queue could not be READ, so it is not shown; it is not known to be empty: %s",
				q.Agent, q.AgentID, q.Unreadable))
			continue
		}
		lines = append(lines, fmt.Sprintf("  agent %s (%s): %d instruction(s)", q.Agent, q.AgentID, len(q.Tasks)))
		for _, t := range q.Tasks {
			lines = append(lines, "    "+taskListLine(t))
		}
	}
	return lines
}

// ---------------------------------------------------------------------
// ks task show
// ---------------------------------------------------------------------

// hostedTaskShow answers one instruction in full: where it sits, what state
// the service holds for it, who submitted it, its exact words, whether it is
// what is holding the queue, and what attempt identity exists.
//
// --session is optional and is a GUARD, not a lookup key: the instruction is
// read by id from the route that takes an id. When a session is named and
// the instruction belongs to another one, this refuses rather than showing
// it — naming one session and being answered about another's work is never
// what somebody meant, and `ks task resume` already refuses on the same
// ground.
func hostedTaskShow(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "the id of the instruction to show is required; nothing is shown by position",
			NextAction: "ks task list --session <session>"})
	}
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	agentName, sessionShort, sessionForEvents := "", "", ""
	if inv.Str("session") != "" {
		sess := agentSession(cr, inv)
		sessionShort = sess.ShortID
		sessionForEvents = agentSessionID(sess)
		a, aerr := resolveAgent(cr, sess, t.AgentID)
		if aerr != nil {
			die(aerr)
		}
		agentName = a.Name
	}
	// the exact words, quoted from the content store. A content read that
	// fails costs the display its text and says so; it never becomes an
	// empty instruction.
	content, cerr := fetchTaskContent(cr, t.ID)
	contentProblem := ""
	if cerr != nil {
		var he *hostedErr
		if errors.As(cerr, &he) {
			contentProblem = sanitize(he.Error())
		} else {
			contentProblem = sanitize(cerr.Error())
		}
	}
	// whether THIS instruction is the one holding its agent's queue. A hold
	// that cannot be read costs the display that line, and is never read as
	// "nothing is holding it".
	holdState, holdProblem := "", ""
	if holds, herr := fetchQueueHolds(cr, t.AgentID); herr != nil {
		holdProblem = sanitize(herr.Error())
	} else if h, aerr := activeHold(holds); aerr != nil {
		holdProblem = sanitize(aerr.Error())
	} else if h == nil {
		holdState = "no hold stands on this agent's queue"
	} else if h.BlockingTask == t.ID {
		holdState = fmt.Sprintf("THIS instruction is what holds the queue (hold %s, cause %s)", h.ID, figure(h.Cause))
	} else {
		holdState = fmt.Sprintf("the queue is held by a different instruction (%s, hold %s)", h.BlockingTask, h.ID)
	}

	// the attempt history, read back rather than derived. A retry is a new
	// attempt and never a rewritten old one, so this is additive.
	atts, attErr := fetchAttempts(cr, t.ID)
	// and the one fact no other surface carries: whether a close of this
	// instruction was REFUSED. A refused close records nothing against the
	// instruction and, until a second refusal raises a hold, appears on no
	// recovery surface — the instruction just reads running. Somebody who
	// cannot see that cannot act on it.
	var refusals []refusedClose
	refusalProblem := ""
	if sessionForEvents != "" {
		var rerr error
		refusals, rerr = fetchTaskRefusals(cr, sessionForEvents, t.ID)
		if rerr != nil {
			refusalProblem = sanitize(rerr.Error())
		}
	}
	emit(map[string]any{"task": t, "agent": t.AgentID, "agent_name": agentName,
		"content": content, "content_unreadable": contentProblem,
		"hold": holdState, "hold_unreadable": holdProblem,
		"attempts": atts, "attempts_unreadable": errText(attErr),
		"refused_closes": refusals, "refusals_unreadable": refusalProblem}, func() {
		fmt.Printf("instruction %s\n", t.ID)
		if agentName != "" {
			fmt.Printf("  agent          %s (%s) in session %s\n", agentName, t.AgentID, sessionShort)
		} else {
			fmt.Printf("  agent          %s\n", t.AgentID)
		}
		fmt.Printf("  state          %s\n", stateLabel("task_state", t.State))
		if t.CancelRecovery != nil {
			printCancelRecovery(t.CancelRecovery, cancelRecoveryHints(t.ID, agentName, sessionShort))
		}
		fmt.Printf("  queue position %d\n", t.QueueSeq)
		fmt.Printf("  author         %s\n", taskAuthor(t))
		fmt.Printf("  origin         %s\n", figure(t.Origin))
		fmt.Printf("  submission     %s\n", notRecorded(t.SubmissionID))
		fmt.Printf("  submitted      %s\n", figure(t.CreatedAt))
		fmt.Printf("  last change    %s\n", figure(t.UpdatedAt))
		fmt.Printf("  revision       %d\n", t.Revision)
		fmt.Printf("  held because   %s\n", notRecorded(t.HeldReason))
		fmt.Printf("  verification   %s\n", verificationLine(t))
		fmt.Printf("  attempt        %s\n", attemptLine(t))
		if attErr != nil {
			fmt.Printf("  attempts       the history could not be READ and is not shown; it is not known to be empty: %s\n", sanitize(attErr.Error()))
		} else if len(atts) == 0 {
			fmt.Printf("  attempts       none recorded\n")
		} else {
			fmt.Printf("  attempts       %d, oldest first:\n", len(atts))
			for _, at := range atts {
				closed := at.ClosedState
				if closed == "" {
					closed = "-"
				}
				fmt.Printf("    %-26s index %-3d generation %-3d %-11s closed %-24s worker %s\n",
					at.ID, at.AttemptIndex, at.ExecutionEpoch, stateLabel("attempt_state", at.State), closed, notRecorded(at.WorkerID))
				if at.RetryOf != "" || at.CheckpointID != "" || at.AuthorizedBy != "" {
					fmt.Printf("      follows %s, from saved point %s, authorized by %s\n",
						notRecorded(at.RetryOf), notRecorded(at.CheckpointID), notRecorded(at.AuthorizedBy))
				}
			}
		}
		switch {
		case sessionForEvents == "":
			fmt.Printf("  refused closes not read: name --session to read this instruction's journal\n")
		case refusalProblem != "":
			fmt.Printf("  refused closes the journal could not be READ, so they are not shown; that is not the same as none: %s\n", refusalProblem)
		case len(refusals) == 0:
			fmt.Printf("  refused closes none\n")
		default:
			fmt.Printf("  refused closes %d. The service REFUSED to record a close of this instruction. The attempt is\n", len(refusals))
			fmt.Printf("                 still running and nothing was recorded against it; it was not downgraded.\n")
			for _, rc := range refusals {
				fmt.Printf("    %s attempt %s\n", figure(rc.At), notRecorded(rc.AttemptID))
				for _, why := range rc.Contradictions {
					fmt.Printf("      %s\n", why)
				}
			}
			fmt.Printf("                 If nobody closes it honestly, an authorized person may record that the\n")
			fmt.Printf("                 outcome could NOT be established — never that it succeeded or failed:\n")
			fmt.Printf("                 ks task reconcile %s --session %s --finding \"what you checked\"\n",
				t.ID, sessionShort)
		}
		if holdProblem != "" {
			fmt.Printf("  queue hold     could not be read, so whether this instruction holds the queue is NOT stated: %s\n", holdProblem)
		} else {
			fmt.Printf("  queue hold     %s\n", holdState)
		}
		fmt.Printf("  instructions:\n")
		if contentProblem != "" {
			fmt.Printf("    the exact instructions could not be READ and are not shown; they are not known to be empty: %s\n", contentProblem)
		} else if strings.TrimSpace(content.Text) == "" {
			fmt.Printf("    not recorded\n")
		} else {
			for _, line := range strings.Split(strings.TrimRight(content.Text, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
			fmt.Printf("    (%d bytes, %s)\n", content.Bytes, notRecorded(content.Digest))
		}
	})
}

// ---------------------------------------------------------------------
// ks task reconcile
// ---------------------------------------------------------------------

// A REFUSED CLOSE IS A CONDITION A PERSON CAN END, and this is the verb that
// ends it.
//
// When the service refuses a runner's success — because the runner's own
// report contradicts it, or because this service's records show a permission
// still open — the attempt stays running and nothing is recorded. That is the
// safe state: a refusal is never a downgrade, and an attempt that is still
// running is an attempt whose successor cannot start. But safe is not the
// same as finished. If the runner never closes it honestly, the instruction
// and everything committed behind it wait for ever.
//
// So an authorized person may record what nobody could establish. It records
// `reconciliation_required` and it CANNOT record succeeded or failed: nobody
// measured those, and a person writing a verdict they did not measure is the
// thing the refusal existed to prevent. It holds the dependent queue in the
// same commit, exactly as a runner-reported unknown does.
//
// It is bound to the revision that was read and to the execution generation
// it was prepared under, so a decision prepared before a restore cannot land
// after one. The finding is required: a record about an outcome nobody
// measured is worth nothing without the evidence beside it.
func hostedTaskReconcile(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "the id of the instruction to reconcile is required",
			NextAction: "ks task list --session " + sess.ShortID})
	}
	finding := strings.TrimSpace(inv.Str("finding"))
	if finding == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "--finding is required: this records that an outcome could NOT be established, and that record is worth nothing without what you established beside it",
			NextAction: fmt.Sprintf("ks task reconcile %s --session %s --finding \"what you checked\"", id, sess.ShortID)})
	}
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	// the instruction must belong to the session that was named: acting on
	// another session's work after naming this one is never what anybody
	// meant
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	rec, err := fetchSessionRecord(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	var env struct {
		Data struct {
			Task       *taskRow `json:"task"`
			HeldTasks  []string `json:"held_tasks"`
			RecordedBy string   `json:"recorded_by"`
			Note       string   `json:"note"`
		} `json:"data"`
	}
	body := map[string]any{"expected_revision": t.Revision, "epoch": rec.ExecutionEpoch, "finding": finding}
	if err := hostedMutate(cr, "POST", "/api/v2/tasks/"+url.PathEscape(t.ID)+"/reconcile", body, &env); err != nil {
		die(err)
	}
	got := env.Data.Task
	emit(map[string]any{"task": got, "held_tasks": env.Data.HeldTasks,
		"recorded_by": env.Data.RecordedBy, "note": env.Data.Note,
		"agent": a.Name, "session": sess.ID}, func() {
		state := "reconciliation_required"
		if got != nil && got.State != "" {
			state = got.State
		}
		fmt.Printf("instruction %s now reads %s\n", t.ID, figure(state))
		fmt.Printf("  this records that the outcome could NOT be established. It is not a success and not a\n")
		fmt.Printf("  failure: nobody measured either, and nothing here claims one.\n")
		fmt.Printf("  finding        %s\n", finding)
		if env.Data.RecordedBy != "" {
			fmt.Printf("  recorded by    %s\n", env.Data.RecordedBy)
		}
		if n := len(env.Data.HeldTasks); n > 0 {
			fmt.Printf("  held behind it %d instruction(s): %s\n", n, strings.Join(env.Data.HeldTasks, ", "))
			fmt.Printf("  continuing them is a separate decision: ks agent queue resume %s --session %s --finding \"...\"\n",
				a.Name, sess.ShortID)
		} else {
			fmt.Printf("  held behind it nothing\n")
		}
		if env.Data.Note != "" {
			fmt.Printf("  note           %s\n", env.Data.Note)
		}
	})
}

// ---------------------------------------------------------------------
// ks task cancel — end one instruction, and say only what is established
// ---------------------------------------------------------------------

// hostedTaskCancel asks the service to cancel ONE instruction (KS-044).
//
// THE WHOLE DIFFICULTY IS THE WORDING, not the request. The service's own
// vocabulary separates two things this command must never blur:
//
//	cancelling  cancellation has been REQUESTED and the stopping outcome is
//	            not yet known
//	cancelled   terminal: it will not run, and nothing resurrects it
//
// So this prints `cancelling` as a request whose outcome is still open, and
// it NEVER says an external effect was undone. Work already done by a tool
// or a provider is not reversed by asking for cancellation, and a client
// that implies otherwise is lying about the blast radius.
//
// It also never claims a cancellation it did not win. If the instruction
// finished while the request was in flight, the service returns the real
// outcome of that race and this prints THAT, because a false "cancelled"
// over a genuine completion is the worst answer available.
//
// Cancelling is not permission to continue the rest of the queue: the
// instructions held behind this one stay held, and continuing them remains
// a separate decision with its own command.
func hostedTaskCancel(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "the id of the instruction to cancel is required",
			NextAction: "ks task list --session " + sess.ShortID})
	}
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	// the instruction must belong to the session that was named: acting on
	// another session's work after naming this one is never what anybody meant
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	rec, err := fetchSessionRecord(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	// EXACTLY the declared result. `requested` distinguishes "this call
	// recorded the intent" from "it was already recorded or already
	// settled"; `replayed` is how a repeat after a lost reply says it
	// changed nothing; `signal_deadline` is the DOCUMENTED bound for work a
	// worker is already executing, derived from the supervision lease rather
	// than a number anybody picked.
	var env struct {
		Data struct {
			Task           *taskRow `json:"task"`
			Requested      bool     `json:"requested"`
			Replayed       bool     `json:"replayed"`
			Note           string   `json:"note"`
			SignalDeadline string   `json:"signal_deadline"`
			// Recovery is where an unconfirmed stop stands against C04's
			// 10 s interrupt wait, and once that has passed, the explicit
			// actions a person may choose between. Printed as the service
			// wrote them: this client takes none of them on its own.
			Recovery *cancelRecovery `json:"recovery"`
		} `json:"data"`
	}
	// Bound to the revision that was read and the generation it was prepared
	// under. Both are optional in the contract on purpose -- a caller
	// repeating after a lost reply never saw the revision its own first call
	// produced -- so they are sent, and the service decides when to enforce
	// them. Sent through the ordinary operation mechanism, so a lost reply
	// leaves a recorded key to ask about rather than a second request.
	body := map[string]any{"expected_revision": t.Revision, "epoch": rec.ExecutionEpoch}
	if r := strings.TrimSpace(inv.Str("reason")); r != "" {
		body["reason"] = r
	}
	if err := hostedMutate(cr, "POST", "/api/v2/tasks/"+url.PathEscape(t.ID)+"/cancel", body, &env); err != nil {
		die(err)
	}
	got := env.Data.Task
	state := ""
	if got != nil {
		state = got.State
	}
	emit(map[string]any{"task": got, "requested": env.Data.Requested,
		"replayed": env.Data.Replayed, "note": env.Data.Note,
		"signal_deadline": env.Data.SignalDeadline, "recovery": env.Data.Recovery,
		"agent": a.Name, "session": sess.ID}, func() {
		switch state {
		case "cancelling":
			fmt.Printf("instruction %s now reads %s\n", t.ID, figure(state))
			fmt.Printf("  cancellation is REQUESTED. The stopping outcome is not established yet,\n")
			fmt.Printf("  and this does not say the work stopped.\n")
			// the negator stays on the SAME line as the claim it denies: a
			// disclaimer split across two lines can be quoted as the claim
			fmt.Printf("  effects already in flight stay unresolved.\n")
			fmt.Printf("  nothing outside this service is claimed to be reversed.\n")
			if env.Data.SignalDeadline != "" {
				fmt.Printf("  by             %s the worker has either been handed the request or no\n", env.Data.SignalDeadline)
				fmt.Printf("                 longer supervises this agent\n")
			}
			printCancelRecovery(env.Data.Recovery, cancelRecoveryHints(t.ID, a.Name, sess.ShortID))
		case "cancelled":
			fmt.Printf("instruction %s now reads %s\n", t.ID, figure(state))
			fmt.Printf("  it will not run. Its identity, content and history are kept.\n")
		case "":
			fmt.Printf("instruction %s: the service returned no state for it\n", t.ID)
			fmt.Printf("  nothing here claims it was cancelled.\n")
		default:
			// a completion that won the race keeps its outcome
			fmt.Printf("instruction %s reads %s\n", t.ID, figure(state))
			fmt.Printf("  this is the outcome that stands; the cancellation did not replace it.\n")
		}
		if env.Data.Replayed {
			fmt.Printf("  the same request was already recorded; this changed nothing.\n")
		}
		fmt.Printf("  instructions committed after it stay held: cancelling one is not a queue\n")
		fmt.Printf("  release, and continuing them is a separate decision.\n")
		fmt.Printf("    ks agent queue resume %s --session %s --finding \"...\"\n", a.Name, sess.ShortID)
		if env.Data.Note != "" {
			fmt.Printf("  note           %s\n", env.Data.Note)
		}
	})
}

// ---------------------------------------------------------------------
// attempts and saved points: read, never composed
// ---------------------------------------------------------------------

// attemptRowClient is one execution of one instruction, as the service
// records it. A retry is a NEW attempt and never a rewritten old one, so the
// history is additive and this client never collapses it.
type attemptRowClient struct {
	ID               string   `json:"id"`
	TaskID           string   `json:"task_id"`
	AttemptIndex     int64    `json:"attempt_index"`
	ExecutionEpoch   int64    `json:"execution_epoch"`
	WorkerID         string   `json:"worker_id"`
	State            string   `json:"state"`
	ClosedState      string   `json:"closed_state"`
	EvidenceRequired bool     `json:"evidence_required"`
	ReceiptIDs       []string `json:"receipt_ids"`
	InputDigest      string   `json:"input_digest"`
	CheckpointID     string   `json:"checkpoint_id"`
	RetryOf          string   `json:"retry_of"`
	AuthorizedBy     string   `json:"authorized_by"`
	StartedAt        string   `json:"started_at"`
	EndedAt          string   `json:"ended_at"`
	ErrorCode        string   `json:"error_code"`
}

func fetchAttempts(cr hostedCreds, taskID string) ([]attemptRowClient, error) {
	var env struct {
		Data struct {
			Items []attemptRowClient `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/tasks/"+url.PathEscape(taskID)+"/attempts", nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// checkpointRowClient is one saved point. Its Scope is the sentence that
// matters and is never abbreviated on a surface: restoring returns EVERY
// agent and EVERY task in the session to that moment.
type checkpointRowClient struct {
	ID                string   `json:"id"`
	SessionID         string   `json:"session_id"`
	FleetCheckpointID string   `json:"fleet_checkpoint_id"`
	ManifestHash      string   `json:"content_manifest_hash"`
	ManifestVersion   int64    `json:"manifest_version"`
	State             string   `json:"state"`
	Boundary          string   `json:"boundary"`
	AttemptID         string   `json:"attempt_id"`
	TaskID            string   `json:"task_id"`
	SourceEpoch       int64    `json:"source_epoch"`
	TaskWatermark     int64    `json:"task_watermark"`
	EventWatermark    int64    `json:"event_watermark"`
	PendingReceipts   []string `json:"pending_effect_receipts"`
	Scope             string   `json:"scope"`
	Reason            string   `json:"reason"`
	CreatedAt         string   `json:"created_at"`
	// KS-053: the lineage and the engine's chunk counts (never estimated)
	Parent string         `json:"parent_checkpoint_id,omitempty"`
	Chunks map[string]int `json:"chunks,omitempty"`
}

func fetchCheckpoints(cr hostedCreds, sessionID string) ([]checkpointRowClient, error) {
	var env struct {
		Data struct {
			Items []checkpointRowClient `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sessionID)+"/checkpoints", nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func checkpointLine(c checkpointRowClient) string {
	task := c.TaskID
	if task == "" {
		task = "-"
	}
	return fmt.Sprintf("%-26s %-11s %-19s %-26s %s",
		clip(c.ID, 26), clip(figure(c.State), 11), clip(figure(c.Boundary), 19),
		clip(task, 26), figure(c.CreatedAt))
}

// ks session checkpoints — the saved points a restore may be NAMED against.
// There is no "most recent" here and no default anywhere: a boundary is
// chosen by a person, by id, or it is not chosen.
func hostedSessionCheckpoints(cr hostedCreds, inv *Invocation) {
	r, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	rows, err := fetchCheckpoints(cr, agentSessionID(r))
	if err != nil {
		die(err)
	}
	emit(map[string]any{"session": r.ID, "checkpoints": rows, "count": len(rows)}, func() {
		if len(rows) == 0 {
			fmt.Printf("Session %s has no saved points recorded.\n", r.ShortID)
			return
		}
		fmt.Printf("%-26s %-11s %-19s %-26s %s\n", "SAVED POINT", "STATE", "BOUNDARY", "ATTEMPT'S INSTRUCTION", "TAKEN")
		for _, c := range rows {
			fmt.Println(checkpointLine(c))
			if c.State != "valid" {
				// a failed save stays visible, with why, and is never offered
				why := c.Reason
				if why == "" {
					why = "the service recorded no reason"
				}
				fmt.Printf("    not restorable: %s\n", sanitize(why))
			}
			if c.Parent != "" || len(c.Chunks) > 0 {
				var parts []string
				for _, k := range []string{"memory", "disk", "device"} {
					if n, ok := c.Chunks[k]; ok {
						parts = append(parts, fmt.Sprintf("%s %d", k, n))
					}
				}
				fmt.Printf("    follows %s; chunks %s\n", notRecorded(c.Parent), notRecorded(strings.Join(parts, ", ")))
			}
		}
		fmt.Printf("\nA saved point is a WHOLE-SESSION saved point. Restoring one returns EVERY agent and\n")
		fmt.Printf("EVERY instruction in this session to that moment; it is not a file-level or a\n")
		fmt.Printf("single-instruction rollback.\n")
		fmt.Printf("%d saved point(s). Name a valid one: ks task resume <instruction> --session %s --checkpoint <saved point>\n",
			len(rows), r.ShortID)
	})
}

// ---------------------------------------------------------------------
// the journal, filtered to one instruction
// ---------------------------------------------------------------------

// refusedClose is what a task.finish_refused event says, as fields.
type refusedClose struct {
	At             string   `json:"at"`
	AttemptID      string   `json:"attempt_id"`
	Digest         string   `json:"contradiction_digest"`
	Contradictions []string `json:"contradictions"`
}

// fetchTaskRefusals reads the session journal and answers the refused
// closes of ONE instruction. A journal that cannot be read costs the
// caller these facts and says so; it is never read as "none were refused".
func fetchTaskRefusals(cr hostedCreds, sessionID, taskID string) ([]refusedClose, error) {
	var env struct {
		Data struct {
			Items []journalEvent `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"limit": {"1000"}}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sessionID)+"/events?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	var out []refusedClose
	for _, e := range env.Data.Items {
		if e.kind() != "task.finish_refused" {
			continue
		}
		if e.TaskID != taskID && e.SubjectID != taskID {
			continue
		}
		var p struct {
			AttemptID      string   `json:"attempt_id"`
			Digest         string   `json:"contradiction_digest"`
			Contradictions []string `json:"contradictions"`
			Why            []string `json:"why"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		c := p.Contradictions
		if len(c) == 0 {
			c = p.Why
		}
		at := e.AttemptID
		if at == "" {
			at = p.AttemptID
		}
		out = append(out, refusedClose{At: e.RecordedAt, AttemptID: at,
			Digest: p.Digest, Contradictions: c})
	}
	return out, nil
}

// ---------------------------------------------------------------------
// ks task resume — a retry is a new attempt from a NAMED boundary
// ---------------------------------------------------------------------

// A RETRY AND A RELEASE ARE STILL DIFFERENT DECISIONS, and naming a
// boundary does not merge them.
//
// `ks agent queue resume` permits the work waiting BEHIND a blocked
// instruction and never restarts it. This verb is the other one: it runs
// THAT instruction again as a new attempt, from a saved point a person
// NAMED. It releases nothing; the restore path holds dispatch and leaves
// starting the next instruction to the ordinary boundary, which is why a
// retry here can never quietly become a release.
//
// Two things it will not do.
//
// It never chooses the boundary. Without --checkpoint it lists this
// session's saved points and stops. There is no "most recent", because a
// restore that landed somewhere nobody meant is indistinguishable, from
// the outside, from one that worked.
//
// It never hides the scope. A KS saved point is a WHOLE-SESSION saved
// point: restoring returns every agent and every instruction in the
// session to that moment. Where work committed after the saved point would
// be affected, the service refuses with the exact list and a digest of it,
// and only a caller citing that digest may proceed — consent bound to what
// was shown rather than to the word yes. This client passes the digest
// through and composes none of its own.
func hostedTaskResume(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "the id of the instruction to run again is required; nothing is retried by position",
			NextAction: fmt.Sprintf("ks task list --session %s", sess.ShortID)})
	}
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	if !retryableState(t.State) {
		// the hold is READ so the refusal can state what actually holds this
		// queue rather than implying this instruction does. A hold that
		// cannot be read costs the statement some fields and never turns
		// this into a success.
		var h *queueHold
		if holds, herr := fetchQueueHolds(cr, t.AgentID); herr == nil {
			if found, aerr := activeHold(holds); aerr == nil {
				h = found
			}
		}
		fail(&cliError{Code: exitConflict, Kind: "not_retryable",
			Detail:     statementFor(sess, a.Name, t, h),
			NextAction: fmt.Sprintf("ks agent queue show %s --session %s", a.Name, sess.ShortID),
			Message: fmt.Sprintf("instruction %s reads %s: there is no failed or unresolved attempt under it to run again, so nothing was retried and nothing was released. A finished instruction is refined by submitting a new one.",
				t.ID, figure(t.State))})
	}
	ckID := strings.TrimSpace(inv.Str("checkpoint"))
	if ckID == "" {
		refuseWithoutABoundary(cr, sess, a.Name, t)
	}
	finding := strings.TrimSpace(inv.Str("finding"))
	if finding == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message: "--finding is required: a restore is recorded with the reason somebody asked for it",
			NextAction: fmt.Sprintf("ks task resume %s --session %s --checkpoint %s --finding \"why\"",
				t.ID, sess.ShortID, ckID)})
	}
	// the boundary is validated HERE against the service's own list, so a
	// name that cannot be restored is refused before anything irreversible
	// is asked for
	cks, cerr := fetchCheckpoints(cr, agentSessionID(sess))
	if cerr != nil {
		die(cerr)
	}
	var chosen *checkpointRowClient
	for i := range cks {
		if cks[i].ID == ckID {
			chosen = &cks[i]
			break
		}
	}
	if chosen == nil {
		fail(&cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("session %s records no saved point %s", sess.ShortID, ckID),
			NextAction: fmt.Sprintf("ks session checkpoints %s", sess.ShortID)})
	}
	if chosen.State != "valid" {
		fail(&cliError{Code: exitConflict, Kind: "checkpoint_not_restorable",
			Message: fmt.Sprintf("saved point %s reads %s, so it is not restorable. Nothing was restored, nothing was released and no other saved point was chosen for you.",
				chosen.ID, figure(chosen.State)),
			NextAction: fmt.Sprintf("ks session checkpoints %s", sess.ShortID)})
	}
	rec, err := fetchSessionRecord(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	body := map[string]any{"checkpoint_id": chosen.ID, "reason": finding,
		"expected_revision": rec.Revision, "epoch": rec.ExecutionEpoch}
	if d := strings.TrimSpace(inv.Str("accept-affected")); d != "" {
		body["accept_affected_digest"] = d
	}
	var env struct {
		Data restoreAnswer `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess))+"/restore", body, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_restore_scope" {
			// the service listed exactly what a restore would affect and
			// minted the digest that binds consent to that list. This client
			// prints its words and composes no list of its own.
			fail(&cliError{Code: exitConflict, Kind: "restore_scope", Message: sanitize(he.Message),
				NextAction: fmt.Sprintf("ks task resume %s --session %s --checkpoint %s --finding \"...\" --accept-affected <the digest above>",
					t.ID, sess.ShortID, chosen.ID)})
		}
		die(err)
	}
	res := env.Data
	// the new attempt is READ BACK from the attempt history, so what is
	// reported is the record rather than this client's expectation of it
	atts, aerr := fetchAttempts(cr, t.ID)
	var newest *attemptRowClient
	if aerr == nil {
		for i := range atts {
			if atts[i].CheckpointID == chosen.ID {
				newest = &atts[i]
			}
		}
	}
	emit(map[string]any{"session": sess.ID, "task": t.ID, "checkpoint": chosen,
		"restore": res, "attempt": newest, "attempts": atts,
		"attempts_unreadable": errText(aerr)}, func() {
		fmt.Printf("restored session %s from saved point %s\n", sess.ShortID, chosen.ID)
		fmt.Printf("  scope          %s\n", figure(res.Scope))
		for _, p := range res.Phases {
			mark := "done"
			if !p.Done {
				mark = "STOPPED"
			}
			fmt.Printf("  %-14s %s %s\n", p.Phase, mark, p.Detail)
		}
		if res.StoppedAt != "" {
			fmt.Printf("  the restore STOPPED at %s; the guest may or may not have been replaced — the phases above say which\n", res.StoppedAt)
		}
		if res.NewEpoch != 0 {
			fmt.Printf("  generation     %d (every earlier worker is now refused)\n", res.NewEpoch)
		}
		if n := len(res.SupersededAtts); n > 0 {
			fmt.Printf("  superseded     %d attempt(s): %s\n", n, strings.Join(res.SupersededAtts, ", "))
		}
		if aerr != nil {
			fmt.Printf("  new attempt    the attempt history could not be READ, so it is not shown; it is not known to be absent: %s\n", sanitize(aerr.Error()))
		} else if newest == nil {
			fmt.Printf("  new attempt    the service linked none to this saved point, and this client invents none\n")
		} else {
			fmt.Printf("  new attempt    %s (index %d, generation %d)\n", newest.ID, newest.AttemptIndex, newest.ExecutionEpoch)
			fmt.Printf("                 follows %s, from saved point %s, authorized by %s\n",
				notRecorded(newest.RetryOf), notRecorded(newest.CheckpointID), notRecorded(newest.AuthorizedBy))
		}
		if res.HoldID != "" {
			fmt.Printf("  queue          HELD (%s). Nothing was released and nothing was started: continuing the\n", res.HoldID)
			fmt.Printf("                 queue is a separate decision — ks agent queue resume %s --session %s --finding \"...\"\n",
				a.Name, sess.ShortID)
		}
		continuationLines(res)
		if res.Note != "" {
			fmt.Printf("  note           %s\n", res.Note)
		}
	})
	continuationUnknown(res, sess.ShortID)
}

// restoreAnswer is what the restore route reports: the phases it completed,
// where it stopped if it did, and what it changed.
type restoreAnswer struct {
	SessionID    string `json:"session_id"`
	CheckpointID string `json:"checkpoint_id"`
	Phases       []struct {
		Phase  string `json:"phase"`
		Done   bool   `json:"done"`
		Detail string `json:"detail"`
	} `json:"phases"`
	StoppedAt      string   `json:"stopped_at"`
	NewEpoch       int64    `json:"new_epoch"`
	SupersededAtts []string `json:"superseded_attempts"`
	HoldID         string   `json:"hold_id"`
	Scope          string   `json:"scope"`
	Note           string   `json:"note"`
	// Continuation is what continues from the saved point, as the engine
	// established it (KS-052): exact_runtime, none or unknown, with the
	// service's note. Printed exactly as given; unknown is never success.
	Continuation     string `json:"continuation"`
	ContinuationNote string `json:"continuation_note"`
}

// continuationLines prints the continuation exactly as the service gave it.
func continuationLines(res restoreAnswer) {
	c := res.Continuation
	if c == "" {
		c = "not stated by the service"
	}
	fmt.Printf("  continuation   %s\n", sanitize(c))
	if res.ContinuationNote != "" {
		fmt.Printf("                 %s\n", sanitize(res.ContinuationNote))
	}
}

// continuationUnknown refuses to call a restore whose outcome the service
// could not establish a success.
func continuationUnknown(res restoreAnswer, sessShort string) {
	if res.Continuation == "unknown" {
		fail(&cliError{Code: exitTemporary, Kind: "continuation_unknown", WorkStarted: workUnknown,
			Message:    "the restore's outcome is UNKNOWN: " + sanitize(res.ContinuationNote),
			NextAction: "ks session show " + sessShort + " (the queue is held and the intent is on the record; do not rerun blindly)"})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error())
}

// refuseWithoutABoundary is the answer when nobody named a saved point. It
// lists what exists and stops. Choosing the newest here would be the exact
// defect this verb was rewritten to remove.
func refuseWithoutABoundary(cr hostedCreds, sess inventoryRow, agentName string, t taskRow) {
	cks, cerr := fetchCheckpoints(cr, agentSessionID(sess))
	var valid []checkpointRowClient
	for _, c := range cks {
		if c.State == "valid" {
			valid = append(valid, c)
		}
	}
	msg := fmt.Sprintf("instruction %s was NOT run again, and nothing was released. Running it again is a NEW attempt started from a saved point, and a saved point is NEVER chosen for you: name one with --checkpoint.",
		t.ID)
	if cerr != nil {
		msg += " This session's saved points could not be READ, so none are listed below; that is not the same as there being none."
	} else if len(valid) == 0 {
		msg += fmt.Sprintf(" This session records no restorable saved point, so there is no boundary a new attempt could start from. Nothing here is unavailable because of a permission, and no other boundary exists to offer.")
	}
	det := boundaryChoices{Task: t.ID, Agent: agentName, Session: sess.ShortID,
		Checkpoints: valid, Unreadable: errText(cerr)}
	next := fmt.Sprintf("ks session checkpoints %s", sess.ShortID)
	// where the hold on this queue OFFERS the retry, the service has named
	// the save point it would resume from: say so, and still choose nothing
	if holds, herr := fetchQueueHolds(cr, t.AgentID); herr == nil {
		if h, aerr := activeHold(holds); aerr == nil && h != nil && h.BlockingTask == t.ID {
			if c := retryChoice(h); c != nil && c.Available && c.CheckpointID != "" {
				det.Offered, det.Via = c.CheckpointID, c.Via
				msg += fmt.Sprintf(" The service offers this retry from saved point %s, through %s.", c.CheckpointID, figure(c.Via))
				next = fmt.Sprintf("ks task resume %s --session %s --checkpoint %s --finding \"...\"", t.ID, sess.ShortID, c.CheckpointID)
			}
		}
	}
	fail(&cliError{Code: exitUsage, Kind: "boundary_required", Detail: det, Message: msg, NextAction: next})
}

// boundaryChoices is the refusal's facts as FIELDS: which instruction, and
// exactly which saved points exist to name.
type boundaryChoices struct {
	Task        string                `json:"task"`
	Agent       string                `json:"agent"`
	Session     string                `json:"session"`
	Checkpoints []checkpointRowClient `json:"checkpoints"`
	Unreadable  string                `json:"unreadable,omitempty"`
	// Offered is the save point the service's hold names for this retry,
	// with the route it goes through; empty where it names none.
	Offered string `json:"offered_checkpoint_id,omitempty"`
	Via     string `json:"offered_via,omitempty"`
}

func (b boundaryChoices) detailLines() []string {
	lines := []string{
		fmt.Sprintf("instruction        %s", b.Task),
		fmt.Sprintf("agent              %s (session %s)", b.Agent, b.Session),
	}
	if b.Unreadable != "" {
		lines = append(lines, "saved points       could not be READ, so none are listed; that is not the same as none existing: "+b.Unreadable)
		return lines
	}
	if b.Offered != "" {
		lines = append(lines, fmt.Sprintf("offered by service %s, through %s", b.Offered, b.Via))
	}
	if len(b.Checkpoints) == 0 {
		lines = append(lines, "saved points       none restorable")
		return lines
	}
	lines = append(lines, fmt.Sprintf("saved points       %d restorable; a restore is WHOLE-SESSION and returns every agent and every instruction in this session to that moment", len(b.Checkpoints)))
	for _, c := range b.Checkpoints {
		lines = append(lines, "  "+checkpointLine(c))
	}
	return lines
}

// cancelRecoveryHints names the ks command for each recovery action.
func cancelRecoveryHints(taskID, agentName, sessionShort string) map[string]string {
	if sessionShort == "" {
		return nil
	}
	return map[string]string{
		"wait":           fmt.Sprintf("ks task show %s --session %s", taskID, sessionShort),
		"stop_runtime":   fmt.Sprintf("ks agent pause %s --session %s", agentName, sessionShort),
		"record_unknown": fmt.Sprintf("ks task reconcile %s --session %s --finding \"what you established\"", taskID, sessionShort),
	}
}

// cancelRecovery is the service's account of an unconfirmed stop (KS-044
// QA-044-2): when it was requested, C04's interrupt wait, whether that has
// passed, and -- once it has -- the explicit actions on offer.
type cancelRecovery struct {
	RequestedAt          string `json:"requested_at"`
	InterruptWaitSeconds int    `json:"interrupt_wait_seconds"`
	OverdueAt            string `json:"overdue_at"`
	Overdue              bool   `json:"overdue"`
	Note                 string `json:"note"`
	Options              []struct {
		Action  string `json:"action"`
		Request string `json:"request"`
		Effect  string `json:"effect"`
		DoesNot string `json:"does_not"`
	} `json:"options"`
}

// printCancelRecovery says where an unconfirmed stop stands. Inside the
// interrupt wait it says the stop is still pending; after it, it lists the
// service's recovery actions verbatim, each with what it does and what it
// does not do, and takes none of them.
//
// hints maps an action to the ks command that takes it, where the caller
// knows the session and agent; each is printed beside the service's request.
func printCancelRecovery(r *cancelRecovery, hints map[string]string) {
	if r == nil {
		return
	}
	if !r.Overdue {
		if r.OverdueAt != "" {
			fmt.Printf("  interrupt wait %d s: if the stop is not confirmed by %s, recovery actions are offered\n", r.InterruptWaitSeconds, r.OverdueAt)
		} else if r.Note != "" {
			fmt.Printf("  interrupt wait %s\n", r.Note)
		}
		return
	}
	fmt.Printf("  NOT STOPPED: the stop was not confirmed within the %d s interrupt wait.\n", r.InterruptWaitSeconds)
	if r.Note != "" {
		fmt.Printf("  note           %s\n", r.Note)
	}
	if len(r.Options) == 0 {
		fmt.Printf("  the service named no recovery action; nothing was taken\n")
		return
	}
	fmt.Printf("  choose one; none is taken for you:\n")
	for _, o := range r.Options {
		fmt.Printf("    %-15s %s\n", o.Action, o.Request)
		fmt.Printf("    %-15s does: %s\n", "", o.Effect)
		fmt.Printf("    %-15s does not: %s\n", "", o.DoesNot)
		if h := hints[o.Action]; h != "" {
			fmt.Printf("    %-15s ks: %s\n", "", h)
		}
	}
}
