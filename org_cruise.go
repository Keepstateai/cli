// org_cruise.go: Cruise in an organization's context. A job started there
// is the organization's: it runs on the organization's own keys, never
// yours, and is paid from the organization's credit within its budgets. The
// approval binds the organization's keys, read through the preflight (which
// every member who may start work can call; the key list itself is for the
// organization's owners and admins). In your personal context every Cruise
// verb is exactly what it was.
package main

import (
	"errors"
	"fmt"
)

// cruiseKeys are the keys an approval may bind in the chosen context: yours
// in the personal context; the organization's in its context.
func cruiseKeys(c hostedCreds) ([]customerKey, error) {
	org := currentContext(c)
	if org == "" {
		return fetchKeys(c)
	}
	var env struct {
		Data struct {
			Keys []customerKey `json:"keys"`
		} `json:"data"`
	}
	if err := hostedCall(c, "POST", "/api/v2/preflight", map[string]any{"mode": "cruise", "org_id": org}, &env); err != nil {
		return nil, cruiseCtxErr(c, orgErr(err, "nothing was done"))
	}
	return env.Data.Keys, nil
}

// cruiseCtxErr says an organization's refusal of a Cruise verb in the
// service's words, with the one step that fixes it; any other error, and
// every error in the personal context, passes through unchanged.
func cruiseCtxErr(c hostedCreds, err error) error {
	org := currentContext(c)
	var he *hostedErr
	if org == "" || !errors.As(err, &he) {
		return err
	}
	next, code := "", exitConflict
	switch he.Type {
	case "ks_organization_no_credit", "ks_tier_fence":
		next = "ks organization buy (an owner or billing member)"
	case "ks_budget_reached":
		next = "ks budget ask"
	case "ks_no_inference_key":
		next = "ks key add (an owner or admin, in the organization's context)"
	case "ks_seat_required":
		next, code = "ks organization show "+org, exitFailed
	case "ks_forbidden":
		next, code = "ks organization spend", exitFailed
	default:
		return err
	}
	return &cliError{Code: code, Kind: he.Type, HTTPStatus: he.Status, WorkStarted: workNo, Message: sanitize(he.Message), NextAction: next}
}

// cruiseContextNote is the line that says whose job this is, in an
// organization's context; nothing in the personal context.
func cruiseContextNote(c hostedCreds) string {
	org := currentContext(c)
	if org == "" {
		return ""
	}
	return fmt.Sprintf("Context: organization %s. The job runs on the organization's own keys and is paid from its credit, within its budgets; never from yours.", org)
}
