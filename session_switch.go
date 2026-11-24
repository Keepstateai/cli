// session_switch.go: the two switches a session carries, from a terminal.
//
//	ks session network [on|off] [session]   whether the session's machine can
//	                                        reach the public internet
//	ks session auto    [on|off] [session]   whether the agent runs everything
//	                                        without asking, except deleting files
//
// With no state, each shows the switch and what it means. With on or off, it
// changes the switch against the revision just read, in any state the session
// is not ended in: a running session's machine takes it now, a paused one when
// it next starts. The service gives a running machine the network setting
// before it records it, so a change that did not reach the machine says so and
// changes nothing. The session is the one named, else this project's binding
// (ks session use), the same order every session verb follows.
//
// The network is on by default, for every session. On means web and git:
// ports 80, 443 and 22 and DNS, at a speed limit and a daily data cap. The
// cloud's metadata, private and shared address ranges, other machines,
// KeepState's own services and IPv6 stay unreachable either way.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// sessionSwitch is what differs between the two verbs: the record's field, the
// words for each state, and the value the service takes for on and off.
type sessionSwitch struct {
	verb   string // "network" or "auto"
	field  string // the record's field, and the PATCH field
	on     string // the field's value when the switch is on
	off    string
	label  string // "network" / "Auto Mode"
	onText string
	offTxt string
	// effect: when a change reaches the agent or the machine, for a state
	effectRunning, effectLater string
}

var networkSwitch = sessionSwitch{
	verb: "network", field: "network_mode", on: "open", off: "off", label: "the network",
	onText:        "on: the session's machine can open websites and use git over the public internet (ports 80, 443 and 22, and DNS), at a speed limit and a daily data cap",
	offTxt:        "off: the session's machine reaches its model and KeepState and nothing else",
	effectRunning: "in effect now", effectLater: "in effect when the session next starts",
}

var autoSwitch = sessionSwitch{
	verb: "auto", field: "permission_mode", on: "auto", off: "manual", label: "Auto Mode",
	onText:        "on: the agent runs everything without asking, except deleting files, which still waits for you",
	offTxt:        "off: every request asks you first",
	effectRunning: "in effect from the next permission request; earlier decisions are unchanged", effectLater: "in effect from the next permission request",
}

// switchRecord is what these verbs read of a session's record.
type switchRecord struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Revision       int64  `json:"revision"`
	RuntimeState   string `json:"runtime_state"`
	Mode           string `json:"mode"`
	NetworkMode    string `json:"network_mode"`
	PermissionMode string `json:"permission_mode"`
}

func (r switchRecord) value(sw sessionSwitch) string {
	if sw.field == "network_mode" {
		return r.NetworkMode
	}
	return r.PermissionMode
}

// isOn reads the record's value as on. An absent value is the default: on for
// the network (a service that predates the field offered it open), off for
// Auto Mode only when the record says manual.
func (sw sessionSwitch) isOn(v string) bool {
	if sw.field == "network_mode" {
		return v != "off"
	}
	return v == "auto"
}

func (sw sessionSwitch) words(on bool) string {
	if on {
		return sw.onText
	}
	return sw.offTxt
}

func fetchSwitchRecord(cr hostedCreds, id string) (switchRecord, error) {
	var env struct {
		Data switchRecord `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, &env); err != nil {
		return switchRecord{}, err
	}
	return env.Data, nil
}

// switchState reads the verb's arguments: "on" or "off" first, then an
// optional session; or only a session (show). Anything else in the first
// place is a usage error that says what the verb takes.
func switchState(sw sessionSwitch, inv *Invocation) (state, sessionArg string, err error) {
	a0, a1 := strings.TrimSpace(inv.Arg(0)), strings.TrimSpace(inv.Arg(1))
	switch strings.ToLower(a0) {
	case "on", "off":
		return strings.ToLower(a0), a1, nil
	}
	if a1 != "" {
		return "", "", &cliError{Code: exitUsage, Kind: "usage",
			Message:    fmt.Sprintf("%q is not on or off. ks session %s takes on or off first, then the session if there is no binding. Nothing changed", sanitize(a0), sw.verb),
			NextAction: "ks session " + sw.verb + " on|off [session]"}
	}
	return "", a0, nil
}

// switchRefusal puts the service's refusals in words with the next action.
func switchRefusal(err error, sw sessionSwitch, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_revision_conflict":
		ce.Message = "the record changed since it was read, so nothing changed; read it again and retry: " + ce.Message
		ce.NextAction = "ks session " + sw.verb + " " + sess.ShortID
	case "ks_session_killed":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks session list"
	case "ks_engine_unavailable", "ks_engine_refused":
		ce.Message = sanitize(he.Message)
		ce.NextAction = "ks session " + sw.verb + " " + sess.ShortID
	case "ks_network_unavailable":
		ce.Message = sanitize(he.Message)
		ce.NextAction = ""
	case "ks_forbidden":
		ce.Message = "changing " + sw.label + " needs the operator role on this session; nothing changed: " + ce.Message
		ce.NextAction = ""
	default:
		return err
	}
	return ce
}

func hostedSessionSwitch(sw sessionSwitch) func(cr hostedCreds, inv *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		state, sessionArg, err := switchState(sw, inv)
		if err != nil {
			fail(err)
		}
		sess, how, explicit := targetSession(cr, sessionArg)
		if !explicit {
			showTarget(sess, how)
		}
		if sess.RecordID == "" {
			fail(&cliError{Code: exitFailed, Kind: "no_record",
				Message:    fmt.Sprintf("session %s has no workspace record, so it carries no %s setting: a plain session was made by an older client. Nothing changed", sess.ShortID, sw.label),
				NextAction: "ks run --agent"})
		}
		rec, err := fetchSwitchRecord(cr, sess.RecordID)
		if err != nil {
			die(err)
		}
		on := sw.isOn(rec.value(sw))
		show := func(isOn bool, extra string) map[string]any {
			return map[string]any{"session": sess.ID, "record": rec.ID, sw.field: map[bool]string{true: sw.on, false: sw.off}[isOn],
				"state": map[bool]string{true: "on", false: "off"}[isOn], "runtime_state": rec.RuntimeState, "revision": rec.Revision, "words": sw.words(isOn), "note": extra}
		}
		if state == "" {
			emit(show(on, ""), func() {
				outf("session %s: %s is %s\n", sess.ShortID, sw.label, sw.words(on))
				outf("  state          %s\n", stateWords("session_runtime", rec.RuntimeState))
				if sw.verb == "network" {
					outf("  always blocked in either position: the cloud's metadata, private and shared address ranges, other machines, KeepState's own services, IPv6; only web and git ports\n")
				}
				outf("  change         ks session %s %s %s\n", sw.verb, map[bool]string{true: "off", false: "on"}[on], sess.ShortID)
			})
			return
		}
		want := state == "on"
		if want == on {
			emit(show(on, "unchanged"), func() {
				outf("%s is already %s for session %s; nothing changed\n", capitalFirst(sw.label), state, sess.ShortID)
			})
			return
		}
		to := sw.off
		if want {
			to = sw.on
		}
		progress("turn %s %s for session %s %q, against revision %d", sw.label, state, sess.ShortID, sanitize(rec.Name), rec.Revision)
		var env struct {
			Data switchRecord `json:"data"`
		}
		body := map[string]any{sw.field: to, "expected_revision": rec.Revision}
		if err := hostedMutate(cr, "PATCH", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), body, &env); err != nil {
			die(switchRefusal(err, sw, sess))
		}
		after := sw.isOn(env.Data.value(sw))
		effect := sw.effectLater
		if sessionRunningWord(rec.RuntimeState) {
			effect = sw.effectRunning
		}
		emit(map[string]any{"session": env.Data, "from": map[bool]string{true: "on", false: "off"}[on], "state": map[bool]string{true: "on", false: "off"}[after], "in_effect": effect}, func() {
			outf("%s is now %s for session %s (revision %d); %s\n", capitalFirst(sw.label), state, sess.ShortID, env.Data.Revision, effect)
		})
	}
}

// sessionRunningWord: a machine that is running takes a change now.
func sessionRunningWord(state string) bool { return state == "running" }

var (
	hostedSessionNetwork = hostedSessionSwitch(networkSwitch)
	hostedSessionAuto    = hostedSessionSwitch(autoSwitch)
)
