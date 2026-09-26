// approvals.go: reading permission requests outside a live window (KS-047).
//
//	ks approval list   a session's requests (pending by default, --all for
//	                   every one), each with the instruction it was asked
//	                   for, what it touches and what deciding costs
//	ks approval show   one request in full
//
// Deciding stays where it was: ks agent approve / ks agent deny (also
// reachable as ks approval approve / deny), bound to the request's revision
// and to the exact action that was shown. A read here IS such a display: the
// requests it prints are recorded as shown, so a decision made afterwards is
// compared against what this printed.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

func fetchApprovals(cr hostedCreds, sess inventoryRow, all bool) ([]approvalRow, error) {
	state := "pending"
	if all {
		state = "all"
	}
	var env struct {
		Data struct {
			Items []approvalRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"session_id": {agentSessionID(sess)}, "state": {state}}
	if err := hostedCall(cr, "GET", "/api/v2/approvals?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func approvalLines(ap approvalRow, sess inventoryRow) []string {
	lines := []string{
		fmt.Sprintf("permission request %s  %s", ap.ID, strings.ToUpper(figure(ap.State))),
		"  action          " + actionShown(ap),
	}
	task := ap.RequestedForTask
	if task == "" {
		task = "not recorded (no instruction was running when it was asked, or the service kept none)"
	}
	lines = append(lines, "  for instruction "+task)
	if len(ap.Affects) == 0 {
		lines = append(lines, "  affects         nothing the service could name from the action's arguments")
	} else {
		for i, a := range ap.Affects {
			label := "  affects         "
			if i > 0 {
				label = "                  "
			}
			lines = append(lines, label+visible(sanitize(a)))
		}
	}
	lines = append(lines, "  cost            "+notRecorded(ap.CostImplication))
	lines = append(lines, "  expires         "+figure(ap.ExpiresAt))
	if ap.ActionableUntil != "" {
		lines = append(lines, "  answerable till "+ap.ActionableUntil+" (after this the agent no longer waits for the answer)")
	}
	lines = append(lines, fmt.Sprintf("  revision        %d", ap.Revision))
	if st := strings.ToLower(ap.State); st != "" && st != "pending" {
		lines = append(lines, "  decided         "+figure(ap.State)+decidedByAt(ap.DecidedBy, ap.DecidedAt))
	} else {
		lines = append(lines, fmt.Sprintf("  decide          ks agent approve %s --session %s | ks agent deny %s --session %s", ap.ID, sess.ShortID, ap.ID, sess.ShortID))
	}
	for i := range lines {
		lines[i] = sanitize(lines[i])
	}
	return lines
}

func hostedApprovalList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	aps, err := fetchApprovals(cr, sess, inv.Bool("all"))
	if err != nil {
		die(err)
	}
	// this list is a display a decision will be compared against
	for _, ap := range aps {
		if strings.EqualFold(ap.State, "pending") || ap.State == "" {
			recordShownAction(shownFrom(cr, ap))
		}
	}
	emit(map[string]any{"session": agentSessionID(sess), "approvals": aps, "count": len(aps)}, func() {
		if len(aps) == 0 {
			if inv.Bool("all") {
				fmt.Printf("session %s has no permission requests\n", sess.ShortID)
			} else {
				fmt.Printf("session %s has no pending permission requests (--all lists decided ones too)\n", sess.ShortID)
			}
			return
		}
		for _, ap := range aps {
			for _, l := range approvalLines(ap, sess) {
				fmt.Println(l)
			}
		}
	})
}

func hostedApprovalShow(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	ap, err := fetchApproval(cr, sess, strings.TrimSpace(inv.Arg(0)))
	if err != nil {
		die(err)
	}
	if want := agentSessionID(sess); ap.SessionID != "" && ap.SessionID != want {
		fail(&cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("permission request %s belongs to another session, not %s", ap.ID, sess.ShortID), NextAction: "ks session list"})
	}
	if strings.EqualFold(ap.State, "pending") || ap.State == "" {
		recordShownAction(shownFrom(cr, ap))
	}
	emit(ap, func() {
		for _, l := range approvalLines(ap, sess) {
			fmt.Println(l)
		}
	})
}
