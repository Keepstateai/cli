// recovery_screen.go: KS-031's recovery screen. When an agent's runner stops
// in the middle of an instruction, the service records it (QA-031-2): the
// agent reads recovery_required, the instruction reads
// reconciliation_required with the adapter's own summary, the execution
// closes "ambiguous", the queue is held behind it, and the journal carries
// the close's outcome (the runner's turn, the work, the acceptance, and each
// tool use that started and was never seen to finish).
//
// This client shows that as a screen, on every surface a person would look
// at, and never as Ready or Finished:
//
//	the live window   a task.finished event that closed reconciliation_required,
//	                  or an agent.activity event reading recovery_required,
//	                  prints the screen; the window does not fall through to
//	                  a shell or carry on as if the agent were working
//	ks agent status   a recovery_required agent, or a held queue, is said
//	                  beside the activity; a returning supervisor reading
//	                  "ready" does not hide a queue held over an unknown
//	ks agent queue show
//	                  for an instruction the runner stopped in, the screen's
//	                  facts: the outcome, the execution, and continuity
//
// CONTINUITY (VER-031-2) is stated from the records, never inferred: whether
// the execution started from a named saved point (the exact saved session
// state was restored, and it opened a conversation of its own) or not (no
// runtime state was restored; the runner runs again against the workspace as
// it was left); the conversation the execution ran in; the conversation the
// agent's runner last announced, from the journal's certification entries.
// Whether a dispatch told the runner to START or CONTINUE a conversation is
// on no customer route, so it is said to be not shown.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// crashOutcome is the adapter's close outcome, as the journal carries it.
type crashOutcome struct {
	RunnerTurn string `json:"runner_turn"`
	Work       string `json:"work"`
	Acceptance string `json:"acceptance"`
	Unsettled  []struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	} `json:"unsettled"`
}

// conversationMove is one agent.conversation_certified journal entry: the
// conversation a runner itself reported being in, and the one before it.
type conversationMove struct {
	At  string `json:"at"`
	Was string `json:"was"`
	Now string `json:"now"`
}

// crashFacts is everything the screen shows, each part with its own
// unreadable reason: a part that could not be read is said, never empty.
type crashFacts struct {
	TaskID          string             `json:"task_id"`
	State           string             `json:"state"`
	Summary         string             `json:"summary"`
	Outcome         *crashOutcome      `json:"outcome,omitempty"`
	ClosedAt        string             `json:"closed_at,omitempty"`
	AttemptID       string             `json:"attempt_id,omitempty"`
	Attempt         *attemptRowClient  `json:"attempt,omitempty"`
	Conversations   []conversationMove `json:"conversation_certifications"`
	JournalProblem  string             `json:"journal_unreadable,omitempty"`
	AttemptsProblem string             `json:"attempts_unreadable,omitempty"`
	ModeNotShown    string             `json:"conversation_mode"`
}

const modeNotShown = "not shown: whether the dispatch told the runner to start or to continue a conversation is on no customer route"

// runnerStopped: an instruction the runner stopped in, as the service
// recorded it.
func runnerStopped(state string) bool { return state == "reconciliation_required" }

// fetchJournal reads the session journal, every page, oldest first.
func fetchJournal(cr hostedCreds, sessionID string) ([]journalEvent, error) {
	var out []journalEvent
	cursor := ""
	for page := 0; page < 100; page++ {
		q := url.Values{"limit": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var env struct {
			Data struct {
				Items      []journalEvent `json:"items"`
				NextCursor string         `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sessionID)+"/events?"+q.Encode(), nil, &env); err != nil {
			return nil, err
		}
		out = append(out, env.Data.Items...)
		if env.Data.NextCursor == "" || len(env.Data.Items) == 0 {
			break
		}
		cursor = env.Data.NextCursor
	}
	return out, nil
}

// finishedFact reads a task.finished payload into the facts.
func finishedFact(f *crashFacts, e journalEvent) {
	var p struct {
		State     string          `json:"state"`
		Summary   string          `json:"summary"`
		AttemptID string          `json:"attempt_id"`
		Outcome   json.RawMessage `json:"outcome"`
	}
	if json.Unmarshal(e.Payload, &p) != nil {
		return
	}
	f.State, f.Summary, f.AttemptID, f.ClosedAt = p.State, p.Summary, p.AttemptID, e.ObservedAt
	f.Outcome = nil
	if len(p.Outcome) > 0 && string(p.Outcome) != "null" {
		var o crashOutcome
		if json.Unmarshal(p.Outcome, &o) == nil {
			f.Outcome = &o
		}
	}
}

// readCrashFacts gathers the screen for one instruction of one agent.
func readCrashFacts(cr hostedCreds, sessionID, agentID string, t taskRow) crashFacts {
	f := crashFacts{TaskID: t.ID, State: t.State, Conversations: []conversationMove{}, ModeNotShown: modeNotShown}
	if evs, err := fetchJournal(cr, sessionID); err != nil {
		f.JournalProblem = sanitize(errText(err))
	} else {
		for _, e := range evs {
			switch e.kind() {
			case "task.finished":
				if e.SubjectID == t.ID || e.TaskID == t.ID {
					finishedFact(&f, e)
				}
			case "agent.conversation_certified":
				var p conversationMove
				var who struct {
					AgentID string `json:"agent_id"`
				}
				_ = json.Unmarshal(e.Payload, &who)
				if json.Unmarshal(e.Payload, &p) == nil && (who.AgentID == agentID || e.SubjectID == agentID) {
					p.At = e.ObservedAt
					f.Conversations = append(f.Conversations, p)
				}
			}
		}
		// the record's own state is authoritative over the journal's word
		f.State = t.State
	}
	if atts, err := fetchAttempts(cr, t.ID); err != nil {
		f.AttemptsProblem = sanitize(errText(err))
	} else {
		for i := range atts {
			if f.AttemptID == "" || atts[i].ID == f.AttemptID {
				a := atts[i]
				f.Attempt = &a
			}
		}
	}
	if f.Summary == "" {
		f.Summary = t.HeldReason
	}
	return f
}

// crashScreenLines is the screen itself.
func crashScreenLines(f crashFacts, agentName, sessShort string, continuity bool) []string {
	out := []string{
		"!! RECOVERY: the runner stopped in the middle of an instruction. This agent is NOT ready and the instruction is NOT finished.",
		fmt.Sprintf("   instruction    %s reads %s: what it did is UNKNOWN", f.TaskID, stateLabel("task_state", f.State)),
	}
	if strings.TrimSpace(f.Summary) != "" {
		out = append(out, "   it reported    "+sanitize(f.Summary))
	} else {
		out = append(out, "   it reported    nothing was recorded")
	}
	switch {
	case f.JournalProblem != "":
		out = append(out, "   outcome        the journal could not be READ, so the close's outcome is not shown; that is not the same as none: "+f.JournalProblem)
	case f.Outcome == nil:
		out = append(out, "   outcome        the close recorded no outcome")
	default:
		o := f.Outcome
		out = append(out, fmt.Sprintf("   outcome        runner turn %s · work %s · acceptance %s", figure(o.RunnerTurn), figure(o.Work), figure(o.Acceptance)))
		if len(o.Unsettled) == 0 {
			out = append(out, "   unsettled      none recorded")
		}
		for _, u := range o.Unsettled {
			out = append(out, fmt.Sprintf("   unsettled      %s %s: %s", sanitize(figure(u.Kind)), sanitize(figure(u.ID)), sanitize(figure(u.Detail))))
		}
	}
	if continuity {
		out = append(out, continuityLines(f)...)
	}
	next := "ks agent queue show <agent> --session <session>"
	if agentName != "" && sessShort != "" {
		next = fmt.Sprintf("ks agent queue show %s --session %s", agentName, sessShort)
	}
	out = append(out,
		"   nothing runs   the instructions after it are held until a person decides; nothing is retried by itself",
		"   decide         "+next)
	return out
}

// continuityLines answer VER-031-2 from the records: was exact saved state
// restored for this execution, or not; which conversation it ran in; which
// one a runner last announced.
func continuityLines(f crashFacts) []string {
	var out []string
	switch {
	case f.AttemptsProblem != "":
		out = append(out, "   runtime state  the execution record could not be READ, so it is not shown: "+f.AttemptsProblem)
	case f.Attempt == nil:
		out = append(out, "   runtime state  the service records no execution for this instruction")
	default:
		a := f.Attempt
		out = append(out, fmt.Sprintf("   execution      %s reads %s (worker %s, generation %d)", a.ID, stateLabel("attempt_state", a.State), notRecorded(a.WorkerID), a.ExecutionEpoch))
		if a.CheckpointID != "" {
			out = append(out, "   runtime state  RESTORED: this execution started from saved point "+a.CheckpointID+" (the exact saved session state), in a conversation of its own")
		} else {
			out = append(out, "   runtime state  NOT restored: no saved point was applied; the runner runs again against the workspace as it was left")
		}
		out = append(out, "   ran in         conversation "+notRecorded(a.RunnerSessionID))
	}
	switch {
	case f.JournalProblem != "":
		out = append(out, "   certified      not shown: the journal could not be read")
	case len(f.Conversations) == 0:
		out = append(out, "   certified      no runner has announced a conversation for this agent, so none is continued")
	default:
		c := f.Conversations[len(f.Conversations)-1]
		line := fmt.Sprintf("   certified      conversation %s, announced by the runner at %s", sanitize(c.Now), figure(c.At))
		if f.Attempt != nil && f.Attempt.RunnerSessionID != "" {
			if f.Attempt.RunnerSessionID == c.Now {
				line += "; the execution ran in this one"
			} else {
				line += "; the execution ran in a DIFFERENT one"
			}
		}
		out = append(out, line)
	}
	out = append(out, "   start/continue "+modeNotShown)
	return out
}

// activityRecoveryLines is the screen for an agent.activity event reading
// recovery_required: the instruction's close may not have arrived yet.
func activityRecoveryLines(agentName, sessShort string) []string {
	return []string{
		"!! RECOVERY: this agent reads recovery_required. Its runner stopped and what it was doing is unknown; it is NOT ready.",
		fmt.Sprintf("   decide         ks agent queue show %s --session %s", agentName, sessShort),
	}
}

// recoveryScreenFor answers the screen an event calls for, or nil.
func recoveryScreenFor(e journalEvent, agentName, sessShort string) []string {
	switch e.kind() {
	case "agent.activity":
		var p struct {
			Activity string `json:"activity"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Activity == "recovery_required" {
			return activityRecoveryLines(agentName, sessShort)
		}
	case "task.finished":
		f := crashFacts{TaskID: e.SubjectID, ModeNotShown: modeNotShown}
		if e.TaskID != "" {
			f.TaskID = e.TaskID
		}
		finishedFact(&f, e)
		if runnerStopped(f.State) {
			// the window has the close in hand; the execution record and
			// the certifications are read by ks agent queue show
			return crashScreenLines(f, agentName, sessShort, false)
		}
	}
	return nil
}
