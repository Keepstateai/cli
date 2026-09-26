// legacy_refusals.go: the named refusals the legacy per-session verbs can
// now receive (BACKLOG-170). The legacy routes (/api/sessions/{id}...)
// resolve a workspace session id, so `ks exec`, `ks attach` and the other
// legacy verbs addressed by a session_... id may be refused by name:
//
//	409 ks_agent_session_no_shell  an AGENT session has no shell: exec is
//	                               refused (submit an instruction instead)
//	                               and attach is refused (open the agent)
//	409 ks_session_no_engine       the workspace session has no engine
//	                               session behind it: nothing to act on
//
// Each is shown by its name, with the service's message and this client's
// next action. Any other named legacy refusal that carries a next_action
// shows it as the service gave it, so a pointer the service adds later
// (a lifecycle verb on an agent session pointing to a v2 route) is shown
// rather than lost.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func legacyRefusal(err error, id, verb string) error {
	var he *hostedErr
	if !errors.As(err, &he) || he.Type == "" {
		return err
	}
	var body struct {
		NextAction string `json:"next_action"`
	}
	_ = json.Unmarshal(he.Raw, &body)
	ce := classify(err)
	ce.Kind = he.Type
	switch he.Type {
	case "ks_agent_session_no_shell":
		if verb == "attach" || body.NextAction == "ks agent open" {
			ce.NextAction = fmt.Sprintf("ks agent open <agent> --session %s (the agent's window: its conversation, no shell)", id)
		} else {
			ce.NextAction = fmt.Sprintf("ks agent tell <agent> \"<the work>\" --session %s (an instruction, run under the service's constraints)", id)
		}
		ce.Message = sanitize(he.Message) + " [ks_agent_session_no_shell]"
	case "ks_session_no_engine":
		ce.Message = sanitize(he.Message) + " [ks_session_no_engine]"
		ce.NextAction = "the session's record: " + sanitize(figure(body.NextAction)) + "; it has no runtime to " + verb + ", and ks session list shows the sessions that do"
	default:
		ce.Message = sanitize(he.Message) + " [" + sanitize(he.Type) + "]"
		if body.NextAction != "" {
			ce.NextAction = sanitize(body.NextAction) + " (as the service names it)"
		}
	}
	return ce
}

// ---------------------------------------------------------------------
// BACKLOG-171: a legacy lifecycle verb on an AGENT session
// ---------------------------------------------------------------------

// useV2Pointer answers the workspace session id and the v2 request the
// service named, when err is ks_agent_session_use_v2.
func useV2Pointer(err error) (ws, next, message string, ok bool) {
	var he *hostedErr
	if !errors.As(err, &he) || he.Type != "ks_agent_session_use_v2" {
		return "", "", "", false
	}
	var body struct {
		NextAction string `json:"next_action"`
	}
	_ = json.Unmarshal(he.Raw, &body)
	next = body.NextAction
	const marker = "/api/v2/sessions/"
	i := strings.Index(next, marker)
	if i < 0 {
		return "", next, he.Message, true
	}
	ws = strings.SplitN(next[i+len(marker):], "/", 2)[0]
	return ws, next, he.Message, true
}

// followUseV2 is what kill, wake and fork do on an agent session: the
// service's refusal first, by name, then the workspace verb that does the
// same thing through the workspace lifecycle. A kill is NEVER turned into a
// deletion: it becomes a deletion PLAN, which changes nothing, and the
// deletion stays the plan-plus-confirm the v2 verb requires.
func followUseV2(cr hostedCreds, err error, verb string, inv *Invocation) bool {
	ws, next, msg, ok := useV2Pointer(err)
	if !ok {
		return false
	}
	progress("refused: %s [ks_agent_session_use_v2]", sanitize(msg))
	if ws == "" {
		fail(&cliError{Code: exitConflict, Kind: "ks_agent_session_use_v2", Message: "the service named no workspace session to act on instead; nothing was changed",
			NextAction: sanitize(figure(next)) + " (as the service names it)"})
	}
	switch verb {
	case "wake":
		progress("resuming it through the workspace route instead (%s): the same resume, keeping its supervision and records in step", sanitize(next))
		r, rerr := postResume(cr, ws)
		if rerr != nil {
			die(rerr)
		}
		emit(map[string]any{"session_id": ws, "state": r["runtime_state"], "refused": "ks_agent_session_use_v2", "via": next}, func() {
			progress("session %s resuming through the workspace route (state %s)", ws, figure(r["runtime_state"]))
			fmt.Println(ws)
		})
	case "kill":
		progress("a kill of an agent session is a DELETION through a plan you confirm; planning it instead (this changes nothing)")
		var env struct {
			Data struct {
				ID        string         `json:"id"`
				ExpiresAt string         `json:"expires_at"`
				Plan      map[string]any `json:"plan"`
			} `json:"data"`
		}
		if perr := hostedCall(cr, "POST", "/api/v2/sessions/"+url.PathEscape(ws)+"/deletion-plan", map[string]any{}, &env); perr != nil {
			die(perr)
		}
		d := env.Data
		if !out.json {
			fmt.Fprintf(os.Stderr, "deletion plan %s for session %s: NOTHING was deleted (plan expires %s)\n", d.ID, ws, figure(d.ExpiresAt))
			b, _ := json.Marshal(d.Plan)
			fmt.Fprintf(os.Stderr, "  plan %s\n", sanitize(string(b)))
		}
		fail(&cliError{Code: exitConflict, Kind: "ks_agent_session_use_v2", Detail: d,
			Message:    fmt.Sprintf("session %s was NOT killed: an agent session is deleted only by a plan you confirm, and the plan above changed nothing", ws),
			NextAction: fmt.Sprintf("ks session delete %s --execute %s --confirm %s", ws, d.ID, ws)})
	case "fork":
		extra := ""
		if inv.Set("steer") {
			extra = "; a steer file has no place in the workspace fork, so it is not carried"
		}
		n := int64(1)
		if inv.Set("children") {
			n = inv.Int("children")
		}
		fail(&cliError{Code: exitConflict, Kind: "ks_agent_session_use_v2",
			Message:    fmt.Sprintf("session %s was NOT forked: an agent session forks through a plan of a saved point you name, never one chosen for you%s", ws, extra),
			NextAction: fmt.Sprintf("ks session checkpoints %s, then ks session fork %s --plan --checkpoint <saved point> --children %d", ws, ws, n)})
	default:
		return false
	}
	return true
}
