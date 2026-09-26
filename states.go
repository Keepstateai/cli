// states.go: the protocol this client speaks and the states it knows
// (KS-010).
//
// Every request declares the protocol in X-KS-Protocol. A service that
// finds an operation newer than it refuses by name (426 ks_client_too_old)
// instead of answering in a shape this client cannot read, and the client
// says so and names the update.
//
// The states below are the C05 vocabulary this client was written against.
// A state outside it is printed as "unknown (<word>)": shown as the service
// sent it, never mapped onto the nearest word this client does know. The
// service publishes its own lists in the capability document; ks doctor
// names any published state this client does not know.
package main

import (
	"sort"
	"strconv"
)

const (
	clientProtocol = 2
	protocolHeader = "X-KS-Protocol"
)

var clientProtocolText = strconv.Itoa(clientProtocol)

// knownStates is what this client can interpret, per kind. "unavailable" is
// the service's word for a value it could not read, and is known everywhere.
var knownStates = map[string][]string{
	"agent_activity":     {"starting", "ready", "working", "waiting_provider", "running_tool", "waiting_approval", "waiting_advice", "paused", "stopped", "failed", "unknown", "removed", "recovery_required"},
	"task_state":         {"queued", "held", "claimed", "running", "cancelling", "cancelled", "succeeded", "failed", "reconciliation_required"},
	"attempt_state":      {"intent", "dispatched", "acknowledged", "running", "finished", "failed", "cancelled", "ambiguous", "superseded"},
	"verification_state": {"not_requested", "pending", "passed", "failed", "inconclusive"},
	"session_runtime":    {"provisioning", "running", "checkpointing", "stopping", "parked", "resuming", "failed", "recovery_required", "deleting", "deleted", "dead"},
}

func knows(kind, s string) bool {
	if s == "unavailable" {
		return true
	}
	for _, k := range knownStates[kind] {
		if k == s {
			return true
		}
	}
	return false
}

// stateLabel renders a state word of a kind: as it is when this client knows
// it, "unavailable" when absent, and "unknown (<word>)" otherwise.
func stateLabel(kind, s string) string {
	switch {
	case s == "":
		return "unavailable"
	case knows(kind, s):
		return s
	}
	return "unknown (" + sanitize(s) + ")"
}

// unknownPublished lists the states the service publishes that this client
// does not know, per kind, for ks doctor.
func unknownPublished(set *capabilitySet) []string {
	var out []string
	for kind, states := range set.States {
		if _, ok := knownStates[kind]; !ok {
			continue // a kind this client displays nowhere
		}
		for _, s := range states {
			if !knows(kind, s) {
				out = append(out, kind+" "+s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// stateCell is stateLabel for a narrow table column: an unknown state reads
// "unknown" there, and its word is in --json and in the show verbs.
func stateCell(kind, s string) string {
	if s != "" && !knows(kind, s) {
		return "unknown"
	}
	return stateLabel(kind, s)
}
