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
