// agent_manage.go: creating and removing an agent (KS-038).
//
//	ks agent create <name> --session S        the session's primary agent,
//	                                          for a session that has none
//	ks agent remove <name> --plan             what removal would touch;
//	                                          changes nothing
//	ks agent remove <name> --execute PLAN     remove it, by that plan
//
// Removal goes through a plan: the plan says what is cancelled, what stays
// readable and that the session is preserved, and the removal names it. An
// agent with work in flight is not removed (409 ks_agent_busy): the refusal
// names the work, and the way on is ks agent stop, then a new plan.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

func agentBusy(err error, a agentRow, sess inventoryRow) error {
	var he *hostedErr
	if errors.As(err, &he) && he.Type == "ks_agent_busy" {
		ce := classify(err)
		ce.Code = exitConflict
		ce.Message = "nothing was removed: " + ce.Message
		ce.NextAction = fmt.Sprintf("ks agent stop %s --session %s, then plan the removal again", a.Name, sess.ShortID)
		return ce
	}
	return err
}

func hostedAgentRemove(cr hostedCreds, inv *Invocation) {
	plan, exec := inv.Bool("plan"), strings.TrimSpace(inv.Str("execute"))
	if plan == (exec != "") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "removal is planned first (--plan) and executed by naming that plan (--execute PLAN): one of the two. Nothing was removed"})
	}
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	base := "/api/v2/agents/" + url.PathEscape(a.ID)
	if plan {
		var env struct {
			Data struct {
				ID        string         `json:"id"`
				ExpiresAt string         `json:"expires_at"`
				Plan      map[string]any `json:"plan"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "POST", base+"/removal-plan", map[string]any{}, &env); err != nil {
			die(agentBusy(err, a, sess))
		}
		d := env.Data
		emit(d, func() {
			fmt.Printf("removal plan %s for agent %s (%s) in session %s: NOTHING was removed (plan expires %s)\n", d.ID, a.Name, a.ID, sess.ShortID, figure(d.ExpiresAt))
			keys := make([]string, 0, len(d.Plan))
			for k := range d.Plan {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				printPlanSection(d.Plan, k, k)
			}
			fmt.Printf("remove exactly this: ks agent remove %s --session %s --execute %s\n", a.Name, sess.ShortID, d.ID)
		})
		return
	}
	var env struct {
		Data struct {
			Removed          bool   `json:"removed"`
			SessionPreserved bool   `json:"session_preserved"`
			TasksCancelled   int64  `json:"tasks_cancelled"`
			TasksCancelling  int64  `json:"tasks_cancelling"`
			Controllers      int64  `json:"controllers_released"`
			ExecutedAt       string `json:"executed_at"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", base+"?"+url.Values{"plan_id": {exec}}.Encode(), nil, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && (he.Type == "ks_plan_stale" || he.Type == "ks_plan_expired" || he.Type == "ks_plan_required") {
			ce := classify(err)
			ce.Message = "nothing was removed: " + ce.Message
			ce.NextAction = fmt.Sprintf("ks agent remove %s --session %s --plan", a.Name, sess.ShortID)
			die(ce)
		}
		die(agentBusy(err, a, sess))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("removed agent %s (%s): %d pending instruction(s) cancelled, %d asked to stop, %d control lease(s) released; the session and its history are preserved\n",
			a.Name, a.ID, d.TasksCancelled, d.TasksCancelling, d.Controllers)
	})
}

func hostedAgentCreate(cr hostedCreds, inv *Invocation) {
	name := inv.Arg(0)
	checkName(name)
	sess := agentSession(cr, inv)
	fmt.Fprintf(os.Stderr, "create agent %q as the primary agent of session %s %q\n", name, sess.ShortID, sess.Name)
	var env struct {
		Data agentRow `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/agents", map[string]any{"session_id": agentSessionID(sess), "name": name}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_conflict" {
			ce := classify(err)
			ce.Message = "nothing was created: " + ce.Message
			ce.NextAction = "ks agent list --session " + sess.ShortID + " (an agent lives in its own session: ks run --agent --agent-name " + name + ")"
			die(ce)
		}
		die(err)
	}
	emit(env.Data, func() {
		fmt.Printf("created agent %s (%s) in session %s\n", env.Data.Name, env.Data.ID, sess.ShortID)
	})
}
