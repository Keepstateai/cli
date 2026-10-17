package main

// ks run --agent (KS-030): one controlled operation that starts an agent
// session, instead of a sequence of machine commands.
//
//  1. the capability gate: agent mode is served only where the control plane
//     says it is available
//  2. preflight (KS-029): blockers stop the run BEFORE anything is created or
//     billed; preflight uploads nothing, provisions nothing, calls no model
//  3. the session record and its primary agent, one commit on the service
//  3a. the key the agent calls with, bound to the session before a machine
//     exists: --key, or the account's ONE enabled key for the runner's
//     provider; none or several is refused before anything is created
//  4. provisioning: a machine, the agent started in it, and supervised once
//     its supervisor reports (a new agent reports Ready only after its first
//     instruction); a setup that does not complete is cleaned up
//     by the service and said so here
//  5. optionally a first task (--task), and the agent's window (--open),
//     both once the agent is supervised
//
// A Ready line is printed only for an agent the service OBSERVED ready.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type provisionPhaseRow struct {
	Phase  string `json:"phase"`
	Done   bool   `json:"done"`
	Detail string `json:"detail"`
}

type provisionAnswer struct {
	SessionID     string              `json:"session_id"`
	AgentID       string              `json:"agent_id"`
	Phases        []provisionPhaseRow `json:"phases"`
	Ready         bool                `json:"ready"`
	Supervised    bool                `json:"supervised"`
	AgentActivity string              `json:"agent_activity"`
	CleanedUp     bool                `json:"cleaned_up"`
	Note          string              `json:"note"`
}

// runnerProvider is the key family the agent's runner calls through.
const runnerProvider = "anthropic"

func hostedRunAgent(cr hostedCreds, inv *Invocation) {
	// the names first: a name the service would refuse is refused here,
	// before the capability check, the preflight or the key lookup
	if err := runAgentNameRefusal(inv); err != nil {
		fail(err)
	}
	requireCapability(cr, &Command{Path: []string{"run", "--agent"}, Needs: "agent.workspace"})
	var budget *int64
	if inv.Set("budget-tokens") {
		v := int64(inv.Int("budget-tokens"))
		budget = &v
	}
	// the harness first: one the catalog withholds is refused before anything exists
	harness, err := chooseHarness(cr, inv.Str("harness"))
	if err != nil {
		die(err)
	}
	var spend *int64
	if inv.Set("spend-limit") {
		v, perr := parseDollars("spend-limit", inv.Str("spend-limit"))
		if perr != nil {
			fail(perr)
		}
		spend = &v
	}
	rec, p, key, err := startAgentSession(cr, inv.Str("name"), inv.Str("agent-name"), inv.Str("key"), budget, inv.Str("model"), harness, spend)
	if err != nil {
		die(err)
	}
	agentName := inv.Str("agent-name")
	facts := map[string]any{"session": rec.ID, "agent": p.AgentID, "ready": p.Ready, "supervised": p.Supervised, "key": key.ID,
		"agent_activity": p.AgentActivity, "cleaned_up": p.CleanedUp, "phases": p.Phases, "note": p.Note, "control_plane": cr.CTL,
		"runner_model": rec.RunnerModel, "harness": firstNonEmptyStr(rec.Harness, harness.ID)}
	if rec.Managed != nil {
		facts["managed"] = rec.Managed
	}
	if rec.spendLimit > 0 {
		facts["spend_limit_microusd"] = rec.spendLimit
	}
	agentRef := agentName
	if agentRef == "" {
		agentRef = "main"
	}
	if !p.Ready && !p.Supervised {
		// Not Ready is never printed as Ready. Two different facts: the
		// setup failed (and was cleaned up), or the supervisor has not
		// reported for the agent yet.
		if p.CleanedUp || !phaseDone(p.Phases, "start_agent") {
			fail(&cliError{Code: exitFailed, Kind: "run_setup_failed", Message: p.Note,
				NextAction: "ks run --agent (to try again), or ks doctor"})
		}
		fail(&cliError{Code: exitTemporary, Kind: "agent_not_ready", Message: p.Note,
			NextAction: fmt.Sprintf("ks agent status %s --session %s", agentRef, rec.ID)})
	}

	// ---- 5. an optional first task, then the window -------------------------
	if text := strings.TrimSpace(inv.Str("task")); text != "" {
		path := "/api/v2/agents/" + url.PathEscape(p.AgentID) + "/tasks"
		sid, err := submissionID(cr, path, text)
		if err != nil {
			die(err)
		}
		var env struct {
			Data submittedTask `json:"data"`
		}
		if err := hostedMutate(cr, "POST", path, map[string]any{"submission_id": sid, "text": text}, &env); err != nil {
			die(err)
		}
		facts["task"] = env.Data.ID
		progress("task %s submitted to %s", env.Data.ID, agentRef)
	}
	if inv.Bool("open") {
		progress("agent %s is %s in session %s%s; opening its window", agentRef, startedWord(p), rec.ID, runningModel(rec))
		sub := &Invocation{Args: []string{agentRef}, set: map[string]bool{"session": true}, strs: map[string]string{"session": rec.ID}}
		hostedAgentOpen(cr, sub)
		return
	}
	emit(facts, func() {
		fmt.Printf("agent %s is %s in session %s%s\n", agentRef, startedWord(p), rec.ID, runningModel(rec))
		fmt.Println(p.Note)
		fmt.Printf("next: ks agent open %s --session %s   (or: ks agent tell %s \"...\" --session %s)\n", agentRef, rec.ID, agentRef, rec.ID)
	})
}

// runAgentNameRefusal checks the new names `ks run --agent` sends: the
// session's (--name) and its primary agent's (--agent-name), each by the
// service's rule. An omitted name is the client's own default, which passes.
func runAgentNameRefusal(inv *Invocation) *cliError {
	if !inv.Bool("agent") {
		return nil
	}
	for _, f := range []string{"name", "agent-name"} {
		if !inv.Set(f) {
			continue
		}
		if err := newNameRefusal(inv.Str(f), "nothing was created"); err != nil {
			err.Message = "--" + f + ": " + err.Message
			return err
		}
	}
	return nil
}

// startedWord is what the agent is, as reported: Ready only when it said so.
func startedWord(p provisionAnswer) string {
	if p.Ready {
		return "Ready"
	}
	return "supervised and waiting for its first instruction"
}

// runKey is the key an agent session calls with: the one named, or the
// account's single enabled key for the runner's provider. None or several is
// refused before anything is created; a key is never guessed.
func runKey(cr hostedCreds, arg, runnerProvider string) (customerKey, error) {
	if arg != "" {
		k, err := resolveKey(cr, arg)
		if err != nil {
			return k, err
		}
		if k.Provider != runnerProvider || !k.Enabled {
			return k, &cliError{Code: exitUsage, Kind: "key_unusable",
				Message: fmt.Sprintf("key %s is a %s key (enabled: %t); the agent calls %s with an enabled %s key. Nothing was created", k.ID, k.Provider, k.Enabled, runnerProvider, runnerProvider), NextAction: "ks key list"}
		}
		return k, nil
	}
	keys, err := fetchKeys(cr)
	if err != nil {
		return customerKey{}, err
	}
	var usable []customerKey
	for _, k := range keys {
		if k.Provider == runnerProvider && k.Enabled {
			usable = append(usable, k)
		}
	}
	switch len(usable) {
	case 1:
		return usable[0], nil
	case 0:
		return customerKey{}, &cliError{Code: exitConflict, Kind: "no_key",
			Message: "you have no enabled " + runnerProvider + " key, which the agent calls with. Nothing was created", NextAction: "ks key add --provider " + runnerProvider}
	}
	var ids []string
	for _, k := range usable {
		ids = append(ids, k.ID+" (…"+k.Last4+")")
	}
	return customerKey{}, &cliError{Code: exitUsage, Kind: "key_ambiguous",
		Message:    fmt.Sprintf("you have %d enabled %s keys (%s); name the one this agent calls with. Nothing was created", len(usable), runnerProvider, strings.Join(ids, ", ")),
		NextAction: "ks run --agent --key <id>"}
}

// createdRecord is the session record ks run --agent created.
type createdRecord struct {
	ID             string `json:"id"`
	Revision       int64  `json:"revision"`
	PrimaryAgentID string `json:"primary_agent_id"`
	// RunnerModel is the model the record carries for its agents: the one
	// chosen with --model, or the service's first rung; absent from a
	// service that predates the choice
	RunnerModel string `json:"runner_model,omitempty"`
	// Harness is the runner its agents work inside; absent from a service
	// that predates harnesses (Claude Code)
	Harness string `json:"harness,omitempty"`
	// Managed is the providers whose calls go on KeepState's key; absent
	// from a service that does not offer it
	Managed []string `json:"managed,omitempty"`
	// harnessName is the harness's catalog name, for the run's own line
	harnessName string
	// spendLimit is the spend limit on KeepState's key the run asked for
	spendLimit int64
}

// runningModel names the model the record carries, on the run's own line;
// nothing when the service did not say.
func runningModel(rec createdRecord) string {
	if rec.RunnerModel == "" {
		return ""
	}
	on := ""
	if rec.Harness != "" && rec.Harness != defaultHarness {
		on = " on " + sanitize(firstNonEmptyStr(rec.harnessName, rec.Harness))
	}
	return ", running " + sanitize(rec.RunnerModel) + on
}

func firstNonEmptyStr(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// startObserver, when set (the app), is told each step of starting an agent
// session -- "session", "key" (its last four), "machine", "phase:<name>"
// ("true" when done) -- in place of the progress lines the command prints.
var startObserver func(step, detail string)

// startAgentSession is steps 2 to 4 of ks run --agent, for the command and
// for the app alike: preflight, the key, the record, its binding and the
// machine. It returns errors rather than exiting; progress goes where
// progress goes (the terminal, or the app's screen).
func startAgentSession(cr hostedCreds, name, agentName, keyArg string, budget *int64, model string, harness catalogHarness, spend *int64) (createdRecord, provisionAnswer, customerKey, error) {
	var none createdRecord
	// the provider the harness calls: Anthropic for Claude Code, the
	// catalog's for another (OpenAI for Codex)
	runnerProvider := firstNonEmptyStr(harness.Provider, runnerProvider)
	// ---- 2. your own key or KeepState's key, chosen before anything exists ----
	choice, err := chooseRunKey(cr, keyArg, runnerProvider, spend)
	if err != nil {
		return none, provisionAnswer{}, customerKey{}, err
	}
	// ---- 2a. preflight: stop before anything exists -------------------------
	pfBody := map[string]any{"provider": runnerProvider, "mode": "agent"}
	if choice.answered {
		// the service judges KeepState's key only for a run chosen on it
		pfBody["managed"] = []string{}
		if choice.managed {
			pfBody["managed"] = []string{runnerProvider}
		}
	}
	var pf struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/preflight", pfBody, &pf); err != nil {
		return none, provisionAnswer{}, customerKey{}, err
	}
	if b, ok := pf.Data["blockers"].([]any); ok && len(b) > 0 {
		var lines []string
		for _, x := range b {
			lines = append(lines, fmt.Sprint(x))
		}
		next := "ks preflight --provider " + runnerProvider
		if choice.answered {
			next += " --key " + map[bool]string{true: keepstateKeyArg, false: ownKeyArg}[choice.managed]
		}
		return none, provisionAnswer{}, customerKey{}, &cliError{Code: exitConflict, Kind: "preflight_blocked",
			Message:    "preflight found blockers, so nothing was created: " + strings.Join(lines, "; "),
			NextAction: next}
	}
	var key customerKey
	if choice.managed {
		key = customerKey{ID: keepstateKeyArg, Provider: runnerProvider}
	} else if key, err = runKey(cr, choice.keyArg, runnerProvider); err != nil {
		return none, provisionAnswer{}, customerKey{}, err
	}

	// ---- 3. the record and its primary agent --------------------------------
	if name == "" {
		name = "run-" + time.Now().UTC().Format("20060102-150405")
	}
	body := map[string]any{"name": name, "mode": "agent"}
	if agentName != "" {
		body["primary_agent_name"] = agentName
	}
	if model != "" {
		// the model its agents call: one the catalog offers (ks cruise
		// models); the service refuses any other and creates nothing
		body["runner_model"] = model
	}
	if harness.ID != "" && harness.ID != defaultHarness {
		// the runner its agents work inside; Claude Code is sent as nothing,
		// so a service that predates harnesses is asked nothing new
		body["harness"] = harness.ID
	}
	if choice.send != nil {
		// sent only to a service that offers KeepState's key: the providers
		// on it (none, over a KeepState default), and its spend limit
		body["managed"] = *choice.send
		if choice.managed && choice.spend > 0 {
			body["spend_limit_microusd"] = choice.spend
		}
	}
	var created struct {
		Data createdRecord `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions", body, &created); err != nil {
		return none, provisionAnswer{}, key, err
	}
	rec := created.Data
	rec.harnessName = harness.Name
	if choice.managed && !containsStr(rec.Managed, runnerProvider) {
		return rec, provisionAnswer{}, key, &cliError{Code: exitFailed, Kind: "managed_not_recorded",
			Message:    fmt.Sprintf("session %s was created, but the service did not record KeepState's key for its %s calls, so no machine was started for it", rec.ID, providerName(runnerProvider)),
			NextAction: "ks session key set " + rec.ID + " --key keepstate, or ks session key set " + rec.ID + " --key <your key>"}
	}
	if choice.managed {
		rec.spendLimit = choice.spend
	}
	if startObserver != nil {
		startObserver("session", "")
	} else {
		withModel := ""
		if rec.RunnerModel != "" {
			withModel = " (model " + sanitize(rec.RunnerModel) + ")"
		}
		if rec.Harness != "" && rec.Harness != defaultHarness {
			withModel = " (model " + sanitize(rec.RunnerModel) + " on " + harnessLabel(harness) + ")"
		}
		progress("session %s created with its primary agent%s; starting a machine for it", rec.ID, withModel)
	}

	// ---- 3a. the session's binding: that key is used or nothing is ---------
	// (a session on KeepState's key was bound by its creation)
	if !choice.managed {
		var bound struct {
			Data struct {
				Revision int64 `json:"revision"`
			} `json:"data"`
		}
		if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(rec.ID)+"/bindings",
			map[string]any{"keys": map[string]string{runnerProvider: key.ID}, "expected_revision": rec.Revision}, &bound); err != nil {
			return rec, provisionAnswer{}, key, err
		}
		rec.Revision = bound.Data.Revision
	}
	switch {
	case startObserver != nil && choice.managed:
		startObserver("key", keepstateKeyArg)
		startObserver("machine", "")
	case startObserver != nil:
		startObserver("key", key.Last4)
		startObserver("machine", "")
	case choice.managed:
		progress("  %s", choice.line(runnerProvider))
	default:
		progress("  %s calls go through your key %s (…%s)", runnerProvider, key.ID, key.Last4)
	}

	// ---- 4. the machine and the agent in it --------------------------------
	req := map[string]any{"expected_revision": rec.Revision}
	if budget != nil {
		req["budget"] = *budget
	}
	var prov struct {
		Data provisionAnswer `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(rec.ID)+"/provision", req, &prov); err != nil {
		return rec, provisionAnswer{}, key, err
	}
	p := prov.Data
	for _, ph := range p.Phases {
		if startObserver != nil {
			startObserver("phase:"+ph.Phase, fmt.Sprint(ph.Done))
			continue
		}
		mark := "done"
		if !ph.Done {
			mark = "NOT DONE"
		}
		progress("  %-14s %s: %s", ph.Phase, mark, ph.Detail)
	}
	return rec, p, key, nil
}

func phaseDone(ph []provisionPhaseRow, name string) bool {
	for _, p := range ph {
		if p.Phase == name {
			return p.Done
		}
	}
	return false
}

// runKeyChoice is which key a new agent session's calls go on, decided
// before anything is created.
type runKeyChoice struct {
	managed     bool      // on KeepState's key
	fromDefault bool      // because the account's default for the provider says so
	keyArg      string    // your own key: its id or alias, or "" for your one enabled key
	send        *[]string // the managed field the creation sends; nil sends none
	spend       int64     // the spend limit on KeepState's key the creation sends
	offer       managedProvider
	answered    bool // the account's model keys were read
}

// line is what the run says about a session on KeepState's key.
func (c runKeyChoice) line(provider string) string {
	s := provider + " calls go on KeepState's key: " + spreadText(provider, c.offer) + ", paid from your purchased credit"
	if c.spend > 0 {
		s += "; this session's spend limit is " + receiptMoney(c.spend)
	} else {
		s += "; this session's spend limit is the service's default"
	}
	if c.fromDefault {
		s += " (your account's default for " + providerName(provider) + ")"
	}
	return s
}

// chooseRunKey decides the key from --key (an id or alias, own, keepstate or
// nothing) and the account's default for the provider. A service that does
// not list KeepState's key is asked nothing new and sent nothing new.
func chooseRunKey(cr hostedCreds, keyArg, provider string, spend *int64) (runKeyChoice, error) {
	c := runKeyChoice{keyArg: keyArg}
	if keyArg == ownKeyArg {
		c.keyArg = ""
	}
	row, listed := managedListed(cr)
	if !listed {
		if keyArg == keepstateKeyArg {
			return c, &cliError{Code: exitConflict, Kind: "managed_unavailable",
				Message:    "KeepState's key cannot be used with this service; " + managedRowWhy(row) + ". Nothing was created",
				NextAction: "ks run --agent --key own (your own key)"}
		}
		return c, nil
	}
	doc, err := fetchModelKeys(cr)
	if err != nil {
		if keyArg != keepstateKeyArg {
			return c, nil // not answered: your own key, as before, and nothing new is sent
		}
		if errors.Is(err, errModelKeysNotServed) {
			return c, &cliError{Code: exitConflict, Kind: "managed_unavailable",
				Message: "KeepState's key is not offered by this service: it does not serve the account's model keys. Nothing was created", NextAction: "ks run --agent --key own (your own key)"}
		}
		return c, err
	}
	c.answered = true
	want := keyArg == keepstateKeyArg
	if keyArg == "" && doc.defaultFor(provider) == "managed" {
		want, c.fromDefault = true, true
	}
	if !want {
		if doc.defaultFor(provider) == "managed" {
			none := []string{} // your own key over a KeepState default
			c.send = &none
		}
		return c, nil
	}
	mp, ok := doc.offered(provider)
	if !ok {
		msg := "KeepState's key is not offered for " + providerName(provider) + " now: " + doc.notOfferedWhy(provider) + ". Nothing was created"
		next := "ks run --agent --key own (your own key)"
		if c.fromDefault {
			msg = "your account's default for " + providerName(provider) + " is KeepState's key, which is not offered now: " + doc.notOfferedWhy(provider) + ". Nothing was created"
			next = "ks run --agent --key own, or ks key default " + provider + " own"
		}
		return c, &cliError{Code: exitConflict, Kind: "managed_unavailable", Message: msg, NextAction: next}
	}
	c.managed, c.offer = true, mp
	c.send = &[]string{provider}
	c.spend = doc.Managed.Limits.SessionDefault
	if spend != nil {
		c.spend = *spend
	}
	return c, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
