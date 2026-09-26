// parked.go: BACKLOG-150. When the account's credits run out, the funds
// interlock saves the session and pauses it (it is never killed). The
// service now says so on every read: the session record's park_reason, the
// agent's and the task's session_runtime, and the live view's
// header.park_reason with a frozen-status note and the add_credits action.
//
// Before this, a funds-parked session looked like a silent hang: the agent's
// last word ("working") and its task's last state ("running") are frozen at
// the saved point and nothing moves. Every surface here therefore puts the
// pause FIRST and never shows such a session as running; what the agent and
// the task last reported is shown only as frozen history.
package main

import (
	"fmt"
	"net/url"
)

// parkReasonFunds is the service's reason for a park it made because the
// account's credits ran out (ctl/workspace_funds_park.go).
const parkReasonFunds = "funds_interlock"

// fundsPausedLine is what a funds-parked session reads, everywhere.
const fundsPausedLine = "paused: out of credit — add credit (console), then resume"

// sessionRuntimeDoc is the service's agentSessionRuntime: present on an
// agent (and on a task still to move) whenever its session is NOT running,
// absent while it runs.
type sessionRuntimeDoc struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Note       string `json:"note"`
	NextAction string `json:"next_action,omitempty"`
}

// fundsParked: the service says the funds interlock parked the session and
// the session is not running again.
func fundsParked(state, reason string) bool {
	return reason == parkReasonFunds && state != "running"
}

func (rt *sessionRuntimeDoc) funds() bool {
	return rt != nil && fundsParked(rt.State, rt.Reason)
}

// sessionStateCell is a session's runtime for a table cell. A funds park
// reads "paused", whatever the fleet's own word is, and a footnote under the
// table carries the full line.
func sessionStateCell(state, reason string) string {
	if fundsParked(state, reason) {
		return "paused"
	}
	return stateCell("session_runtime", state)
}

// frozenTask: a task state that would otherwise read as work in progress.
func frozenTask(state string) bool {
	switch state {
	case "claimed", "running", "cancelling":
		return true
	}
	return false
}

// runtimeLines are the detail lines for a session that is not running, as
// the service states it, with the pause put plainly first for a funds park.
// resumeHint is this client's command that resumes the session.
func runtimeLines(rt *sessionRuntimeDoc, resumeHint string) []string {
	if rt == nil {
		return nil
	}
	var out []string
	if rt.funds() {
		out = append(out,
			"  session        "+fundsPausedLine,
			"  why            "+sanitize(notRecorded(rt.Note)),
			"  add credit     in the console (the service's action: "+sanitize(figure(rt.NextAction))+"); adding credit does not resume the session by itself",
		)
		if resumeHint != "" {
			out = append(out, "  then resume    "+resumeHint)
		}
		return out
	}
	out = append(out, fmt.Sprintf("  session        %s: %s", sanitize(stateLabel("session_runtime", rt.State)), sanitize(notRecorded(rt.Note))))
	if rt.Reason != "" {
		out = append(out, "  park reason    "+sanitize(rt.Reason))
	}
	if rt.NextAction != "" {
		out = append(out, "  next           "+sanitize(rt.NextAction))
	}
	return out
}

// sessionRecordFacts is what the workspace session record adds to a fleet
// inventory row: its own runtime state and why it was parked.
type sessionRecordFacts struct {
	ID           string `json:"id"`
	RuntimeState string `json:"runtime_state"`
	ParkReason   string `json:"park_reason,omitempty"`
}

// fetchSessionRecords reads every page of the account's session records
// (GET /api/v2/sessions without source=fleet), keyed by record id. The
// fleet inventory does not carry park_reason; the record does.
func fetchSessionRecords(cr hostedCreds) (map[string]sessionRecordFacts, error) {
	out := map[string]sessionRecordFacts{}
	cursor := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var env struct {
			Data struct {
				Items      []sessionRecordFacts `json:"items"`
				NextCursor string               `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", "/api/v2/sessions?"+q.Encode(), nil, &env); err != nil {
			return nil, err
		}
		for _, it := range env.Data.Items {
			out[it.ID] = it
		}
		cursor = env.Data.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// withParkReasons fills each inventory row's park_reason from its record
// when the inventory did not carry one. A control plane from c0a64d1 on
// carries park_reason on the inventory row itself, so no second read is
// made when any row carries it. The field is omitted when empty, so an
// inventory with no reason on any row cannot say which kind of control
// plane sent it: the records are then read only when a row that names a
// record is not running, which is the only row a park reason can explain.
// A failed read of the records is returned as a sentence to show, never
// as "not parked".
func withParkReasons(cr hostedCreds, rows []inventoryRow) ([]inventoryRow, string) {
	need := false
	for _, r := range rows {
		if r.ParkReason != "" {
			return rows, "" // the inventory carries the field
		}
		if r.RecordID != "" && r.RuntimeState != "running" {
			need = true
		}
	}
	if !need {
		return rows, ""
	}
	recs, err := fetchSessionRecords(cr)
	if err != nil {
		return rows, "why a session is parked could not be read (" + sanitize(errText(err)) + "); a parked session may be out of credit"
	}
	for i := range rows {
		if rows[i].ParkReason != "" || rows[i].RecordID == "" {
			continue
		}
		if rec, ok := recs[rows[i].RecordID]; ok {
			rows[i].ParkReason = rec.ParkReason
			if rows[i].RecordState == "" {
				rows[i].RecordState = rec.RuntimeState
			}
		}
	}
	return rows, ""
}

// rowFundsParked: the record names a funds park and neither the record nor
// the fleet says the session runs again.
func rowFundsParked(r inventoryRow) bool {
	if r.ParkReason != parkReasonFunds {
		return false
	}
	st := r.RecordState
	if st == "" {
		st = r.RuntimeState
	}
	return fundsParked(st, r.ParkReason)
}
