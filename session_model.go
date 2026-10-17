// session_model.go: the model a session's agents call (model choice).
//
//	ks session model <session>            the recorded model, and whether it
//	                                      can change now
//	ks session model <session> <model>    change it, while the session is
//	                                      paused; in effect from its next resume
//
// A session carries runner_model: the model its agents are started with,
// chosen at creation (ks run --agent --model) or the service's first rung.
// It changes only while the session is paused or has no machine yet, through
// PATCH against the revision just read, and the change reaches the agents
// when the session resumes. The service accepts only a model the catalog
// offers (ks cruise models); nothing is substituted for any other.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// sessionModelRecord is what this verb reads of the session's record.
type sessionModelRecord struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Revision     int64  `json:"revision"`
	RuntimeState string `json:"runtime_state"`
	RunnerModel  string `json:"runner_model"`
	// KeyBindings is what the session calls each provider with; absent
	// from a service that predates it
	KeyBindings *sessionKeyBindings `json:"key_bindings,omitempty"`
}

func fetchSessionModelRecord(cr hostedCreds, id string) (sessionModelRecord, error) {
	var env struct {
		Data sessionModelRecord `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, &env); err != nil {
		return sessionModelRecord{}, err
	}
	return env.Data, nil
}

// modelChangeStanding says whether a session's model can change now, from
// its runtime state, as the service decides it: a paused session, or one
// with no machine yet, can; an ended one cannot; every other live state
// must be paused first. pauseHint is this client's command that pauses it.
func modelChangeStanding(state, pauseHint string) (can bool, text string) {
	switch state {
	case "parked":
		return true, "can change now: the session is paused, and the change takes effect when it resumes"
	case "provisioning":
		return true, "can change now if its machine has not been given yet (the first start carries it); once the machine runs, pause it first"
	case "dead", "deleting", "deleted":
		return false, "cannot change: the session has ended"
	case "":
		return false, "unavailable: the service did not report the session's state"
	}
	return false, "cannot change while the session is " + sanitize(state) + ": pause it first (" + pauseHint + "), then change it; the change takes effect when it resumes"
}

// primaryAgentName is the name of a session's primary agent, for the hints
// that name it; "<agent>" when it cannot be read.
func primaryAgentName(cr hostedCreds, rec string) string {
	agents, err := fetchAgents(cr, rec)
	if err != nil {
		return "<agent>"
	}
	for _, a := range agents {
		if a.IsPrimary && agentNameSafe.MatchString(a.Name) {
			return a.Name
		}
	}
	return "<agent>"
}

func hostedSessionModel(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record",
			Message:    fmt.Sprintf("session %s has no workspace record, so it carries no model: a plain session runs no agent. Nothing changed", sess.ShortID),
			NextAction: "ks run --agent --model <model>"})
	}
	rec, err := fetchSessionModelRecord(cr, sess.RecordID)
	if err != nil {
		die(err)
	}
	pauseHint := fmt.Sprintf("ks agent pause %s --session %s", primaryAgentName(cr, sess.RecordID), sess.ShortID)
	can, standing := modelChangeStanding(rec.RuntimeState, pauseHint)
	model := strings.TrimSpace(inv.Arg(1))
	if model == "" {
		emit(map[string]any{"session": sess.ID, "record": rec.ID, "runner_model": rec.RunnerModel, "runtime_state": rec.RuntimeState,
			"revision": rec.Revision, "can_change_now": can, "standing": standing}, func() {
			fmt.Printf("session %s runs %s\n", sess.ShortID, sanitize(notRecorded(rec.RunnerModel)))
			fmt.Printf("  state          %s\n", stateWords("session_runtime", rec.RuntimeState))
			fmt.Printf("  change         %s\n", standing)
			fmt.Printf("  offered        ks cruise models lists the models a session may run; change: ks session model %s <model>\n", sess.ShortID)
		})
		return
	}
	if model == rec.RunnerModel {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("session %s already runs %s; nothing changed", sess.ShortID, sanitize(model))})
	}
	progress("change the model of session %s from %s to %s, against revision %d; it takes effect when the session resumes",
		sess.ShortID, sanitize(notRecorded(rec.RunnerModel)), sanitize(model), rec.Revision)
	var env struct {
		Data sessionModelRecord `json:"data"`
	}
	body := map[string]any{"runner_model": model, "expected_revision": rec.Revision}
	if err := hostedMutate(cr, "PATCH", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), body, &env); err != nil {
		die(modelChangeRefusal(err, sess, pauseHint))
	}
	emit(map[string]any{"session": env.Data, "from": rec.RunnerModel, "in_effect": "from the next resume"}, func() {
		fmt.Printf("session %s will run %s from its next resume (was %s; revision %d)\n",
			sess.ShortID, sanitize(notRecorded(env.Data.RunnerModel)), sanitize(notRecorded(rec.RunnerModel)), env.Data.Revision)
	})
}

// modelChangeRefusal words the service's refusals of a model change: the
// session must be paused first, the model is not offered (the message names
// the offered ones), the session has ended, or the record moved since it
// was read. Nothing changed in any of them.
func modelChangeRefusal(err error, sess inventoryRow, pauseHint string) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_session_not_paused":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = pauseHint + ", then ks session model " + sess.ShortID + " <model>"
	case "ks_model_invalid":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks cruise models (the models offered to sessions)"
	case "ks_session_killed":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks session list"
	case "ks_revision_conflict":
		ce.Message = "the record changed since it was read, so nothing changed; read it again and retry: " + ce.Message
		ce.NextAction = "ks session model " + sess.ShortID
	default:
		return err
	}
	return ce
}
