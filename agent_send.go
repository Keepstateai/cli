// agent_send.go: ks agent send, an instruction from one agent (or a person)
// to another.
//
//	ks agent send <name> <text> [--if idle|working] [--wait] [--timeout S]
//
// Unlike ks agent tell, every send is a new instruction: a fresh submission
// id each call, so sending the same words twice queues them twice, which is
// what a calling agent that repeats itself means. The text carries who sent
// it, as <ks from="<caller>">...</ks>, so the receiving agent knows the
// words came from another program and not from its person.
//
// --if refuses with exit 5 and sends nothing when the agent's state does not
// match. A waiting permission request does not stop a send: the instruction
// queues behind it, and the answer says so. Nothing here ever decides a
// permission request.
package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// taggedText is the instruction as the receiving agent reads it.
func taggedText(c callerIdentity, text string) string {
	return fmt.Sprintf("<ks from=%q>%s</ks>", c.Label, text)
}

// stateMatches is the --if test: idle is a resting agent (idle or done),
// working is a busy one. Blocked and error match neither.
func stateMatches(cond string, d agentStatusDoc) bool {
	switch cond {
	case "idle":
		return d.State == "idle" || d.State == "done"
	case "working":
		return d.State == "working"
	}
	return true
}

func hostedAgentSend(cr hostedCreds, inv *Invocation) {
	name, text := inv.Arg(0), inv.Arg(1)
	cond := inv.Str("if")
	if inv.Set("if") && cond != "idle" && cond != "working" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--if is idle or working, not %q; nothing was sent", sanitize(cond))})
	}
	if strings.TrimSpace(text) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the instruction is empty; nothing was sent",
			NextAction: fmt.Sprintf("ks agent send %s \"check the tests\"", name)})
	}
	sess, a := childTarget(cr, inv, name)
	sessionID := agentSessionID(sess)
	st, serr := readAgentStatus(cr, sessionID, a.ID)
	if cond != "" {
		if serr != nil {
			ce := classify(serr)
			ce.Message = fmt.Sprintf("the state of %s could not be read, so --if %s cannot be checked; nothing was sent: %s", a.Name, cond, ce.Message)
			die(ce)
		}
		if !stateMatches(cond, st) {
			fail(&cliError{Code: exitConflict, Kind: "state_mismatch", Detail: st,
				Message:    fmt.Sprintf("%s is %s, not %s; nothing was sent", sanitize(firstNonEmpty(a.Name, name)), figure(st.State), cond),
				NextAction: fmt.Sprintf("ks agent wait %s", firstNonEmpty(a.Name, name))})
		}
	}
	if st.State == "clear" {
		fail(&cliError{Code: exitConflict, Kind: "agent_gone", Message: fmt.Sprintf("%s is gone (%s); nothing was sent", sanitize(firstNonEmpty(a.Name, name)), figure(st.Reason))})
	}
	caller := resolveCaller(cr)
	sid, err := newSubmissionID()
	if err != nil {
		die(err)
	}
	t, resolved, err := sendInstruction(cr, a.ID, sid, map[string]any{"submission_id": sid, "text": taggedText(caller, text)})
	if resolved {
		progress("accepted: the service holds submission %s (resolved after a lost acknowledgement)", sid)
	}
	if err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_queue_full" {
			die(&cliError{Code: exitConflict, Kind: "queue_full",
				Message:    fmt.Sprintf("the queue of %s is full: %s; nothing was sent", a.Name, sanitize(he.Message)),
				NextAction: fmt.Sprintf("ks agent wait %s", a.Name)})
		}
		die(err)
	}
	behind := st.State == "blocked" && st.Kind == "permission"
	doc := map[string]any{"agent": a.ID, "name": firstNonEmpty(a.Name, name), "session": sessionID, "submission_id": sid, "task": t.taskRow,
		"from": caller.Label, "caller": caller.ID, "held": t.Runtime != nil, "runtime": t.Runtime, "waits_behind_permission": behind}
	if serr == nil {
		doc["status_before"] = st
	}
	var waited *waitResult
	if inv.Bool("wait") {
		w := waitForTask(cr, sessionID, a.ID, t.ID, waitTimeoutFlag(inv, defaultAgentWait))
		w.Agent = firstNonEmpty(a.Name, name)
		waited = &w
		doc["wait"] = w
	}
	emit(doc, func() {
		fmt.Printf("sent: task %s for %s at queue position %d (%s)\n", t.ID, sanitize(firstNonEmpty(a.Name, name)), t.QueueSeq, figure(t.State))
		if behind {
			fmt.Println("  it waits behind the permission request: " + sanitize(st.Msg))
			fmt.Println("  a person decides that request; this send decided nothing")
		}
		if t.Runtime != nil {
			fmt.Printf("  the session is %s and was NOT woken: %s\n", figure(t.Runtime.State), sanitize(t.Runtime.Note))
		}
		if waited != nil {
			printWaitResult(*waited, waitTimeoutFlag(inv, defaultAgentWait))
		} else {
			fmt.Printf("next: ks agent wait %s, then ks agent read %s\n", firstNonEmpty(a.Name, name), firstNonEmpty(a.Name, name))
		}
	})
}

// waitForTask waits until the instruction just sent has finished, or the
// agent needs a person, or the timeout. Reading the instruction itself
// keeps an agent that was idle before the send from reading as finished
// before it began.
func waitForTask(cr hostedCreds, sessionID, agentID, taskID string, timeout time.Duration) waitResult {
	deadline := time.Now().Add(timeout)
	res := waitResult{AgentID: agentID, Session: sessionID}
	for {
		st, err := readAgentStatus(cr, sessionID, agentID)
		if err == nil {
			res.Status = &st
			if st.State == "clear" {
				res.Reason = "exited"
				return res
			}
			if st.State == "blocked" || st.State == "error" {
				res.Reason = "status"
				return res
			}
		}
		if tk, terr := fetchTask(cr, taskID); terr == nil && finishedTaskStates[strings.ToLower(tk.State)] {
			if st2, err2 := readAgentStatus(cr, sessionID, agentID); err2 == nil {
				res.Status = &st2
			}
			res.Reason = "status"
			return res
		}
		if time.Now().After(deadline) {
			res.Reason = "timeout"
			return res
		}
		time.Sleep(minDuration(waitPoll(), time.Until(deadline)+time.Millisecond))
	}
}
