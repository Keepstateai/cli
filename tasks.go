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
	return figure(t.Verification)
}

// attemptLine states the attempt identity the service holds, or the fact
// that it holds none. C03 asks for attempt history; this service records a
// current attempt and no history, and that is what is reported.
func attemptLine(t taskRow) string {
	if strings.TrimSpace(t.CurrentAttempt) == "" {
		return "the service records no attempt under this instruction, and no history of earlier ones"
	}
	return t.CurrentAttempt + " (the service records this attempt; it keeps no history of earlier ones)"
}

func taskListLine(t taskRow) string {
	return fmt.Sprintf("%-26s %4d %-12s %-9s %-10s %s",
		clip(t.ID, 26), t.QueueSeq, clip(figure(t.State), 12), clip(figure(t.Origin), 9),
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
	agentName, sessionShort := "", ""
	if inv.Str("session") != "" {
		sess := agentSession(cr, inv)
		sessionShort = sess.ShortID
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

	emit(map[string]any{"task": t, "agent": t.AgentID, "agent_name": agentName,
		"content": content, "content_unreadable": contentProblem,
		"hold": holdState, "hold_unreadable": holdProblem}, func() {
		fmt.Printf("instruction %s\n", t.ID)
		if agentName != "" {
			fmt.Printf("  agent          %s (%s) in session %s\n", agentName, t.AgentID, sessionShort)
		} else {
			fmt.Printf("  agent          %s\n", t.AgentID)
		}
		fmt.Printf("  state          %s\n", figure(t.State))
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
