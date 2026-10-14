// agent_history.go: an agent's conversation so far, read from the session
// journal in any session state, and the one reader of that journal every
// backlog goes through.
//
//	ks agent history <name> --session <s> [--last N]
//
// The journal route pages FORWARD only (after_seq, then the cursor each
// page answers, at most 1000 rows a page). A reader that asked for one page
// with a limit got the OLDEST rows, so a window on a long conversation
// showed the wrong tail. readSessionJournal pages to the end (or to the sequence a
// window resumes from) and keeps the last rows; the whole text of every
// entry is shown, because a history is read, not skimmed.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// readSessionJournal reads a session's journal from its start, page after page,
// and answers the events with a sequence at or below upto (every event
// when upto is 0), keeping only the last keepLast of them (all when keepLast
// is 0), and how many such events the journal holds. Events the service no
// longer retains are skipped from the earliest cursor it names.
func readSessionJournal(cr hostedCreds, sessionID string, upto int64, keepLast int) ([]journalEvent, int, error) {
	var kept []journalEvent
	total := 0
	q := url.Values{"limit": {"1000"}}
	var lastSeq int64
	for page := 0; page < 10000; page++ {
		var env struct {
			Data struct {
				Items      []journalEvent `json:"items"`
				NextCursor string         `json:"next_cursor"`
			} `json:"data"`
		}
		path := "/api/v2/sessions/" + url.PathEscape(sessionID) + "/events?" + q.Encode()
		if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
			var he *hostedErr
			if errors.As(err, &he) && he.Type == "ks_cursor_expired" {
				// the oldest events are gone: carry on from the earliest the
				// service still holds, once
				var body struct {
					Error struct {
						EarliestCursor string `json:"earliest_cursor"`
					} `json:"error"`
				}
				_ = json.Unmarshal(he.Raw, &body)
				if c := body.Error.EarliestCursor; c != "" && q.Get("cursor") != c {
					q = url.Values{"limit": {"1000"}, "cursor": {c}}
					continue
				}
			}
			return nil, 0, err
		}
		items := env.Data.Items
		for _, e := range items {
			if upto > 0 && e.StreamSeq > upto {
				continue
			}
			total++
			kept = append(kept, e)
			if keepLast > 0 && len(kept) > 2*keepLast {
				kept = append([]journalEvent(nil), kept[len(kept)-keepLast:]...)
			}
		}
		if len(items) == 0 || env.Data.NextCursor == "" || items[len(items)-1].StreamSeq <= lastSeq {
			break
		}
		lastSeq = items[len(items)-1].StreamSeq
		if upto > 0 && lastSeq >= upto {
			break
		}
		q = url.Values{"limit": {"1000"}, "cursor": {env.Data.NextCursor}}
	}
	if keepLast > 0 && len(kept) > keepLast {
		kept = append([]journalEvent(nil), kept[len(kept)-keepLast:]...)
	}
	return kept, total, nil
}

// fullTextKind: the conversation entries whose whole text a history shows.
func fullTextKind(kind string) bool {
	switch kind {
	case "assistant_text", "instruction", "consultation", "advice":
		return true
	}
	return false
}

// agentEventFull is agentEventLine with the whole of a conversation entry's
// text: the first line where agentEventLine puts it, the rest on lines
// under it.
func agentEventFull(e journalEvent) string {
	t := transcriptOf(e)
	if t == nil || !fullTextKind(t.Kind) {
		return agentEventLine(e)
	}
	head := fmt.Sprintf("%-6d %s", e.StreamSeq, clock(e.ObservedAt))
	lines := strings.Split(transcriptText(*t, true), "\n")
	out := []string{sanitize(head + " " + lines[0])}
	pad := strings.Repeat(" ", len(head)+1+7)
	for _, l := range lines[1:] {
		out = append(out, sanitize(pad+l))
	}
	return strings.Join(out, "\n")
}

// historyEntry renders one journal row as the conversation reads it: who,
// and the whole text. ok is false for a row that is not part of this
// agent's conversation (another agent's words, a row nothing is said for).
func historyEntry(e journalEvent, agentID, agentName, account string) (who, text string, ok bool) {
	if e.SubjectType == "agent" && e.SubjectID != "" && agentID != "" && e.SubjectID != agentID {
		return "", "", false
	}
	if t := transcriptOf(e); t != nil {
		body := strings.TrimRight(t.Text, "\n")
		clipped := ""
		if t.Clipped {
			clipped = " […clipped by the service]"
		}
		switch t.Kind {
		case "assistant_text":
			if strings.TrimSpace(body) == "" {
				return "", "", false
			}
			return agentName, body + clipped, true
		case "instruction":
			who, _ := attribution(*t)
			if t.AuthorType == "human" && (account == "" || t.AuthorID == account) {
				who = "you"
			}
			return who, body + clipped + " (task " + notRecorded(t.TaskID) + ")", true
		case "consultation":
			// a question another agent asked this one over a connection
			return notRecorded(t.AuthorID) + " asked", body + clipped + " (consultation " + notRecorded(t.ConsultationID) + ")", true
		case "advice":
			return "advice from " + notRecorded(t.AuthorID), body + clipped + " (consultation " + notRecorded(t.ConsultationID) + ", unverified)", true
		case "tool_started":
			s := orUnnamedTool(t.ToolName) + " started"
			if d := firstLineOf(t.Text); d != "" {
				s += ": " + d
			}
			return "tool", s, true
		case "tool_finished":
			outcome := "finished"
			if t.Failed {
				outcome = "FAILED"
			}
			s := orUnnamedTool(t.ToolName) + " " + outcome
			if d := firstLineOf(t.Text); d != "" {
				s += ": " + d
			}
			if hint := consultRefusalHint(t.Text); hint != "" {
				s += " -- " + hint
			}
			return "tool", s, true
		case "runner_lifecycle":
			return "runner", body, true
		}
		return figure(t.Kind), body + clipped, true
	}
	var p struct {
		Type     string `json:"type"`
		State    string `json:"state"`
		Summary  string `json:"summary"`
		Reason   string `json:"reason"`
		Runtime  string `json:"runtime_state"`
		From     string `json:"from"`
		To       string `json:"to"`
		Actor    string `json:"actor"`
		InEffect string `json:"in_effect"`
		Consult  string `json:"consultation_id"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	switch p.Type {
	case "task.finished":
		switch {
		case p.State == "succeeded":
			return "task", "finished (" + firstNonEmpty(e.TaskID, e.SubjectID) + ")", true
		case p.State == "failed":
			return "task", "FAILED (" + firstNonEmpty(e.TaskID, e.SubjectID) + "): " + notRecorded(firstLineOf(p.Summary)), true
		}
		return "task", notRecorded(p.State) + " (" + firstNonEmpty(e.TaskID, e.SubjectID) + ")", true
	case "session.parked":
		if fundsParked(firstNonEmpty(p.Runtime, "parked"), p.Reason) {
			return "session", fundsPausedLine, true
		}
		return "session", "paused; its work is saved and nothing runs until it is resumed", true
	case "session.resumed":
		return "session", "running again", true
	case "session.stopped":
		return "session", "stopped", true
	case "session.model-changed":
		return "session", fmt.Sprintf("model changed from %s to %s (%s; by %s)", notRecorded(p.From), notRecorded(p.To), notRecorded(p.InEffect), notRecorded(p.Actor)), true
	case "consultation.requested":
		return "session", "a question went to a connected adviser (consultation " + notRecorded(p.Consult) + "; ks advice show " + notRecorded(p.Consult) + ")", true
	case "consultation.answered":
		return "session", "a connected adviser answered (consultation " + notRecorded(p.Consult) + "; ks advice show " + notRecorded(p.Consult) + ")", true
	case "consultation.cancelled":
		return "session", "a question to a connected adviser was cancelled (consultation " + notRecorded(p.Consult) + ")", true
	}
	if isApprovalEvent(e) {
		return "approval", "permission request " + notRecorded(e.SubjectID) + " " + compactPayload(e.Payload), true
	}
	return "", "", false
}

// historyText lays one entry out: the time of day, who, and the text, its
// further lines aligned under the first.
func historyText(e journalEvent, who, text string) string {
	// a short label fills its column; a long one (who asked, whose advice)
	// is followed by a colon so the text is told from the name
	label := fmt.Sprintf("%-8s ", who)
	if len(who) > 8 {
		label = who + ": "
	}
	head := clock(firstNonEmpty(e.ObservedAt, e.RecordedAt)) + "  " + label
	lines := strings.Split(text, "\n")
	out := []string{sanitize(head + lines[0])}
	pad := strings.Repeat(" ", len(head))
	for _, l := range lines[1:] {
		out = append(out, sanitize(pad+l))
	}
	return strings.Join(out, "\n")
}

// hostedAgentHistory is ks agent history <name>: the conversation, oldest
// first and newest last, in any session state; nothing is woken.
func hostedAgentHistory(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	events, total, err := readSessionJournal(cr, agentSessionID(sess), 0, 0)
	if err != nil {
		ce := classify(err)
		ce.Message = "the conversation could not be read, so none of it is shown; that is not the same as nothing having happened: " + ce.Message
		die(ce)
	}
	type entry struct {
		e         journalEvent
		who, text string
	}
	var entries []entry
	for _, e := range events {
		if who, text, ok := historyEntry(e, a.ID, a.Name, cr.AccountID); ok {
			entries = append(entries, entry{e, who, text})
		}
	}
	if inv.Set("last") {
		if n := int(inv.Int("last")); len(entries) > n {
			entries = entries[len(entries)-n:]
		}
	}
	state := sess.RuntimeState
	if rec, rerr := fetchSessionRecord(cr, agentSessionID(sess)); rerr == nil && rec.RuntimeState != "" {
		state = rec.RuntimeState
	}
	progress("conversation of agent %s (%s) in session %s: %d entries of %d journal rows, newest last; the session is %s and was not woken",
		a.Name, a.ID, sess.ShortID, len(entries), total, stateLabel("session_runtime", state))
	if len(entries) == 0 {
		progress("nothing has been said yet")
	}
	for _, en := range entries {
		emitLine(en.e, historyText(en.e, en.who, en.text))
	}
}
