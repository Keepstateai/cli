// fork_restore.go: planning and executing a fork (KS-054), and restoring a
// session from a named saved point (KS-052), on the session record.
//
//	ks session fork <session> --plan --checkpoint CK --children N
//	ks session fork <session> --execute <plan digest> --plan-id ID
//	ks session restore <session> --checkpoint CK --reason TEXT [--accept-affected D]
//
// A fork is planned first and creates nothing: the saved point it branches,
// what each child costs, which key bindings survive re-authorization, how
// many pending instructions become held templates, and what is not
// inherited. Execution names the plan's digest; a plan whose session or
// saved point moved is refused with the reason, never re-planned silently.
//
// A restore prints the service's continuation (exact_runtime | none |
// unknown) and its note exactly as given; an unknown continuation is never
// reported as success.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

func sessionRecordOf(cr hostedCreds, inv *Invocation) inventoryRow {
	r, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if r.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: fmt.Sprintf("session %s has no workspace record; nothing was done", r.ShortID)})
	}
	return r
}

func printPlanSection(plan map[string]any, key, label string) {
	v, ok := plan[key]
	if !ok {
		return
	}
	switch x := v.(type) {
	case string:
		fmt.Printf("  %-10s %s\n", label, sanitize(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if k != "effect" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			b, _ := json.Marshal(x[k])
			parts = append(parts, k+" "+string(b))
		}
		fmt.Printf("  %-10s %s\n", label, sanitize(strings.Join(parts, ", ")))
		if e, _ := x["effect"].(string); e != "" {
			fmt.Printf("  %-10s %s\n", "", sanitize(e))
		}
	}
}

func hostedSessionFork(cr hostedCreds, inv *Invocation) {
	plan, exec := inv.Bool("plan"), inv.Str("execute")
	if plan == (exec != "") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "a fork is planned first (--plan) and executed by naming that plan's digest (--execute DIGEST --plan-id ID): one of the two. Nothing was forked"})
	}
	sess := sessionRecordOf(cr, inv)
	base := "/api/v2/sessions/" + url.PathEscape(sess.RecordID)
	if plan {
		ck := strings.TrimSpace(inv.Str("checkpoint"))
		if ck == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--checkpoint names the saved point to branch; none is chosen for you", NextAction: "ks session checkpoints " + sess.ShortID})
		}
		n := int64(1)
		if inv.Set("children") {
			n = inv.Int("children")
		}
		var env struct {
			Data struct {
				ID           string         `json:"id"`
				CheckpointID string         `json:"checkpoint_id"`
				Children     int            `json:"children"`
				Plan         map[string]any `json:"plan"`
				PlanDigest   string         `json:"plan_digest"`
				State        string         `json:"state"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "POST", base+"/fork-plan", map[string]any{"checkpoint_id": ck, "children": n}, &env); err != nil {
			die(err)
		}
		d := env.Data
		emit(d, func() {
			fmt.Printf("fork plan %s for session %s: NOTHING was created\n", d.ID, sess.ShortID)
			printPlanSection(d.Plan, "source", "from")
			printPlanSection(d.Plan, "children", "children")
			printPlanSection(d.Plan, "runtime", "cost")
			printPlanSection(d.Plan, "keys", "keys")
			printPlanSection(d.Plan, "pending", "held")
			printPlanSection(d.Plan, "approvals", "approvals")
			printPlanSection(d.Plan, "advisers", "advisers")
			printPlanSection(d.Plan, "selection", "results")
			fmt.Printf("  digest     %s\n", d.PlanDigest)
			fmt.Printf("execute exactly this plan: ks session fork %s --execute %s --plan-id %s\n", sess.ShortID, d.PlanDigest, d.ID)
		})
		return
	}
	planID := strings.TrimSpace(inv.Str("plan-id"))
	if planID == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--plan-id names the plan the digest belongs to (ks session fork --plan prints both); nothing was forked"})
	}
	var env struct {
		Data struct {
			PlanID   string `json:"plan_id"`
			Children []struct {
				SessionID    string         `json:"session_id"`
				Name         string         `json:"name"`
				RuntimeState string         `json:"runtime_state"`
				KeyBindings  map[string]any `json:"key_bindings"`
				Agents       []struct {
					AgentID   string   `json:"agent_id"`
					Templates []string `json:"held_templates"`
					HoldID    string   `json:"hold_id"`
				} `json:"agents"`
			} `json:"children"`
			Note string `json:"note"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", base+"/fork", map[string]any{"plan_id": planID, "plan_digest": exec}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) {
			switch he.Type {
			case "ks_plan_stale", "ks_plan_digest_mismatch", "ks_checkpoint_mismatch", "ks_checkpoint_not_restorable":
				ce := classify(err)
				ce.Message = "nothing was forked: " + ce.Message
				ce.NextAction = "plan again: ks session fork " + sess.ShortID + " --plan --checkpoint <saved point>"
				die(ce)
			}
		}
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("forked session %s by plan %s into %d child(ren); each is metered as its own session\n", sess.ShortID, d.PlanID, len(d.Children))
		for _, c := range d.Children {
			fmt.Printf("  %s %q (%s)\n", c.SessionID, c.Name, stateLabel("session_runtime", c.RuntimeState))
			for _, a := range c.Agents {
				if len(a.Templates) > 0 {
					fmt.Printf("    agent %s: %d held template(s) under hold %s; nothing starts until you release it\n", a.AgentID, len(a.Templates), figure(a.HoldID))
				}
			}
		}
		if d.Note != "" {
			fmt.Println("note: " + sanitize(d.Note))
		}
	})
}

// ---- the automatic-save policy (KS-053) -------------------------------------------

func hostedCheckpointPolicy(cr hostedCreds, inv *Invocation) {
	sess := sessionRecordOf(cr, inv)
	on, off := inv.Bool("on"), inv.Bool("off")
	if on && off {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--on or --off, not both; nothing changed"})
	}
	if inv.Set("interval") && !on {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--interval goes with --on; nothing changed"})
	}
	if inv.Set("interval") && (inv.Int("interval") < 5 || inv.Int("interval") > 120) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--interval is 5 to 120 minutes; nothing changed"})
	}
	path := "/api/v2/sessions/" + url.PathEscape(sess.RecordID) + "/checkpoint-policy"
	type policy struct {
		Enabled         bool   `json:"enabled"`
		IntervalMinutes int    `json:"interval_minutes"`
		State           string `json:"state"`
		PendingSince    string `json:"pending_since"`
		DeferredReason  string `json:"deferred_reason"`
		LastAttemptAt   string `json:"last_attempt_at"`
		Certification   string `json:"certification"`
		StorageEffect   string `json:"storage_effect"`
		Revision        int64  `json:"session_revision"`
	}
	var env struct {
		Data policy `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		die(err)
	}
	p := env.Data
	if on || off {
		body := map[string]any{"enabled": on, "expected_revision": p.Revision}
		if inv.Set("interval") {
			body["interval_minutes"] = inv.Int("interval")
		}
		fmt.Fprintf(os.Stderr, "turn automatic saves %s for session %s (against revision %d); nothing is saved or erased by this\n", map[bool]string{true: "ON", false: "OFF"}[on], sess.ShortID, p.Revision)
		if err := hostedMutate(cr, "PUT", path, body, &env); err != nil {
			die(err)
		}
		p = env.Data
	}
	emit(p, func() {
		if !p.Enabled {
			fmt.Printf("automatic saves for session %s: OFF (state %s)\n", sess.ShortID, figure(p.State))
		} else {
			fmt.Printf("automatic saves for session %s: every %d min (state %s)\n", sess.ShortID, p.IntervalMinutes, figure(p.State))
		}
		if p.PendingSince != "" {
			fmt.Printf("  a save is pending since %s: %s\n", p.PendingSince, sanitize(p.DeferredReason))
		}
		if p.LastAttemptAt != "" {
			fmt.Printf("  last attempt %s\n", p.LastAttemptAt)
		}
		fmt.Printf("  %s\n", sanitize(p.Certification))
		fmt.Printf("  storage: %s\n", sanitize(p.StorageEffect))
	})
}
