// session_harness.go: the runner a session's agents work inside.
//
//	ks session harness <session>                     the harness, and whether
//	                                                 it can switch now
//	ks session harness <session> <harness> [--model] switch it, while the
//	                                                 session is paused
//
// A switch keeps the session's files. Its agents' next instruction starts a
// new conversation in the other harness, which does not see the one so far;
// that one stays readable (ks agent history). The service accepts a harness
// the catalog offers and the session's own image carries: a session created
// before its image carried a harness cannot switch to it. The model moves
// with the harness, to its default unless --model names one of its models.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type sessionHarnessRecord struct {
	ID             string   `json:"id"`
	Revision       int64    `json:"revision"`
	RuntimeState   string   `json:"runtime_state"`
	RunnerModel    string   `json:"runner_model"`
	Harness        string   `json:"harness"`
	ImageHarnesses []string `json:"image_harnesses"`
}

// harnessSwitchStanding says whether a session's harness can switch now, as
// the service decides it: only while paused, or before it has a machine.
func harnessSwitchStanding(state, pauseHint string) (bool, string) {
	switch state {
	case "parked":
		return true, "can switch now: the session is paused, and the switch takes effect when it resumes"
	case "provisioning":
		return true, "can switch now if its machine has not been given yet; once the machine runs, pause it first"
	case "dead", "deleting", "deleted":
		return false, "cannot switch: the session has ended"
	case "":
		return false, "unavailable: the service did not report the session's state"
	}
	return false, "cannot switch while the session is " + sanitize(state) + ": pause it first (" + pauseHint + ")"
}

func hostedSessionHarness(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record",
			Message:    fmt.Sprintf("session %s has no workspace record, so it runs no harness: a plain session runs no agent. Nothing changed", sess.ShortID),
			NextAction: "ks run --agent --harness <harness>"})
	}
	var env struct {
		Data sessionHarnessRecord `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), nil, &env); err != nil {
		die(err)
	}
	rec := env.Data
	current := firstNonEmptyStr(rec.Harness, defaultHarness)
	pauseHint := fmt.Sprintf("ks agent pause %s --session %s", primaryAgentName(cr, sess.RecordID), sess.ShortID)
	can, standing := harnessSwitchStanding(rec.RuntimeState, pauseHint)
	to := strings.TrimSpace(inv.Arg(1))
	if to == "" {
		if inv.Set("model") {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--model goes with a harness to switch to; to change only the model: ks session model " + sess.ShortID + " <model>"})
		}
		image := rec.ImageHarnesses
		if len(image) == 0 {
			image = []string{defaultHarness}
		}
		emit(map[string]any{"session": sess.ID, "record": rec.ID, "harness": current, "runner_model": rec.RunnerModel, "image_harnesses": image,
			"runtime_state": rec.RuntimeState, "revision": rec.Revision, "can_switch_now": can, "standing": standing}, func() {
			fmt.Printf("session %s runs %s on %s\n", sess.ShortID, sanitize(current), sanitize(notRecorded(rec.RunnerModel)))
			fmt.Printf("  state          %s\n", stateWords("session_runtime", rec.RuntimeState))
			fmt.Printf("  switch         %s\n", standing)
			fmt.Printf("  its image      carries %s; a session cannot switch to a harness its image does not carry\n", sanitize(strings.Join(image, ", ")))
			fmt.Printf("  offered        ks cruise models lists the harnesses and their models; switch: ks session harness %s <harness>\n", sess.ShortID)
		})
		return
	}
	if to == current {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("session %s already runs %s; nothing changed", sess.ShortID, sanitize(to))})
	}
	body := map[string]any{"harness": to, "expected_revision": rec.Revision}
	if m := strings.TrimSpace(inv.Str("model")); m != "" {
		body["runner_model"] = m
	}
	progress("switch session %s from %s to %s, against revision %d; it takes effect when the session resumes, and the next instruction starts a new conversation",
		sess.ShortID, sanitize(current), sanitize(to), rec.Revision)
	var out struct {
		Data sessionHarnessRecord `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), body, &out); err != nil {
		die(harnessSwitchRefusal(err, sess, pauseHint, to))
	}
	emit(map[string]any{"session": out.Data, "from": current, "in_effect": "from the next resume; the next instruction starts a new conversation"}, func() {
		fmt.Printf("session %s will run %s on %s from its next resume (was %s; revision %d)\n",
			sess.ShortID, sanitize(firstNonEmptyStr(out.Data.Harness, to)), sanitize(notRecorded(out.Data.RunnerModel)), sanitize(current), out.Data.Revision)
		fmt.Printf("  its files stay; the next instruction starts a new conversation, and the one so far stays readable: ks agent history %s --session %s\n",
			primaryAgentName(cr, sess.RecordID), sess.ShortID)
		fmt.Printf("  the new agent is handed KeepState's handoff brief of the work with that instruction; read it after the resume: ks session brief %s\n", sess.ShortID)
	})
}

// harnessSwitchRefusal words the service's refusals of a switch, each with
// the next action. Nothing changed in any of them.
func harnessSwitchRefusal(err error, sess inventoryRow, pauseHint, to string) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	ce.Message = sanitize(he.Message) + "; nothing changed"
	switch he.Type {
	case "ks_session_not_paused":
		ce.NextAction = pauseHint + ", then ks session harness " + sess.ShortID + " " + sanitize(to)
	case "ks_harness_not_in_image":
		ce.NextAction = "ks run --agent --harness " + sanitize(to) + " (a new session)"
	case "ks_harness_unavailable", "ks_harness_invalid", "ks_model_harness_mismatch", "ks_model_invalid":
		ce.NextAction = "ks cruise models (the harnesses and the models each runs)"
	case "ks_no_inference_key":
		ce.NextAction = "ks key add (a key for the harness's provider)"
	case "ks_session_killed":
		ce.NextAction = "ks session list"
	case "ks_harness_same":
		ce.NextAction = "ks session harness " + sess.ShortID
	case "ks_revision_conflict":
		ce.Message = "the record changed since it was read, so nothing changed; read it again and retry: " + sanitize(he.Message)
		ce.NextAction = "ks session harness " + sess.ShortID
	default:
		return err
	}
	return ce
}
