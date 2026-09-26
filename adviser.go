package main

// adviser.go: connecting one of your agents to another as an adviser, and
// withdrawing it.
//
// A connection is a PERSON's act, and it is the only thing that lets an agent
// consult another: the service lists nothing wider to an agent than the live
// connections made from it. So the one decision here is made carefully.
// Every session's primary agent is called "main", which makes two agents easy
// to confuse, so connect shows BOTH ends by session and agent (and project,
// where there is one), the scope, what the adviser is shown, the limits and
// the expiry, and only then asks for a confirmation that names the adviser.
//
// The global --yes does NOT confirm a connection: a grant is on the list of
// decisions --yes never skips. The confirmation is typed at a terminal, or
// given as --confirm <session>/<agent> (or the adviser's id) matching what is
// displayed. Without either -- and always with --no-input -- nothing is
// connected, and the refusal names the flag to pass (exit 2).

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type grantLimits struct {
	MaxConsultations               int64 `json:"max_consultations"`
	MaxTokensPerConsultation       int64 `json:"max_tokens_per_consultation"`
	MaxOutputTokensPerConsultation int64 `json:"max_output_tokens_per_consultation"`
}

type adviserGrant struct {
	ID                string      `json:"id"`
	SourceAgentID     string      `json:"source_agent_id"`
	TargetAgentID     string      `json:"target_agent_id"`
	Scope             string      `json:"scope"`
	ExpiresAt         string      `json:"expires_at"`
	GrantRevision     int64       `json:"grant_revision"`
	ApprovedBy        string      `json:"approved_by"`
	RevokedAt         string      `json:"revoked_at,omitempty"`
	Live              bool        `json:"live"`
	Limits            grantLimits `json:"limits"`
	ContextPolicy     string      `json:"context_policy"`
	SourceAgentName   string      `json:"source_agent_name,omitempty"`
	SourceSessionID   string      `json:"source_session_id,omitempty"`
	SourceSessionName string      `json:"source_session_name,omitempty"`
	TargetAgentName   string      `json:"target_agent_name,omitempty"`
	TargetSessionID   string      `json:"target_session_id,omitempty"`
	TargetSessionName string      `json:"target_session_name,omitempty"`
}

// grantState is the one word a person reads: live, revoked or expired.
func grantState(g adviserGrant) string {
	switch {
	case g.RevokedAt != "":
		return "revoked"
	case g.Live:
		return "live"
	}
	return "expired"
}

// end is one side of a connection as a person must see it before granting.
type adviserEnd struct {
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	ProjectID   string `json:"project_id,omitempty"`
}

func (e adviserEnd) label() string { return e.SessionName + "/" + e.AgentName }

func (e adviserEnd) line() string {
	s := fmt.Sprintf("%s (agent %s, session %s", e.label(), e.AgentID, e.SessionID)
	if e.ProjectID != "" {
		s += ", project " + e.ProjectID
	}
	return s + ")"
}

// sessionRecord reads a session record's name and project by its record id.
func sessionRecord(cr hostedCreds, id string) (name, project string, err error) {
	var env struct {
		Data struct {
			Name      string `json:"name"`
			ProjectID string `json:"project_id"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, &env); err != nil {
		return "", "", err
	}
	return env.Data.Name, env.Data.ProjectID, nil
}

func endOfAgent(cr hostedCreds, a agentRow) (adviserEnd, error) {
	name, project, err := sessionRecord(cr, a.SessionID)
	if err != nil {
		return adviserEnd{}, err
	}
	return adviserEnd{AgentID: a.ID, AgentName: a.Name, SessionID: a.SessionID, SessionName: name, ProjectID: project}, nil
}

func fetchAgentByID(cr hostedCreds, id string) (agentRow, error) {
	var env struct {
		Data agentRow `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(id), nil, &env); err != nil {
		return agentRow{}, err
	}
	return env.Data, nil
}

// resolveAdviserTarget answers the ONE agent the adviser argument names: its
// id, or <session>/<agent> where the session is named by its name, its id or
// a unique id prefix. Nothing is chosen for the caller: a name two sessions
// share is refused with both listed.
func resolveAdviserTarget(cr hostedCreds, arg string) (agentRow, error) {
	if strings.HasPrefix(arg, "agent_") && !strings.Contains(arg, "/") {
		return fetchAgentByID(cr, arg)
	}
	sessArg, agentArg, ok := strings.Cut(arg, "/")
	if !ok || sessArg == "" || agentArg == "" {
		return agentRow{}, &cliError{Code: exitUsage, Kind: "usage",
			Message:    fmt.Sprintf("the adviser %q is neither an agent id nor <session>/<agent>", arg),
			NextAction: "ks agent list --session <session>"}
	}
	rows, err := fetchInventory(cr, "", true)
	if err != nil {
		return agentRow{}, err
	}
	var byName []inventoryRow
	for _, r := range rows {
		if r.Name == sessArg {
			byName = append(byName, r)
		}
	}
	var sess inventoryRow
	switch len(byName) {
	case 1:
		sess = byName[0]
	case 0:
		if sess, err = resolveSession(cr, sessArg); err != nil {
			return agentRow{}, err
		}
	default:
		sort.Slice(byName, func(i, j int) bool { return byName[i].ID < byName[j].ID })
		var ids []string
		for _, r := range byName {
			ids = append(ids, r.ShortID)
		}
		return agentRow{}, &cliError{Code: exitUsage, Kind: "ambiguous",
			Message:    fmt.Sprintf("%d sessions are named %q (%s); name the session by its id", len(byName), sessArg, strings.Join(ids, ", ")),
			NextAction: "ks session list"}
	}
	return resolveAgent(cr, sess, agentArg)
}

func limitsLine(l grantLimits) string {
	return fmt.Sprintf("at most %d consultation(s); per consultation %s tokens, %s of them output",
		l.MaxConsultations, commas(l.MaxTokensPerConsultation), commas(l.MaxOutputTokensPerConsultation))
}

// confirmGrant decides whether the person confirmed THIS adviser. It returns
// only when they did.
func confirmGrant(target adviserEnd, given string) {
	want := []string{target.label(), target.AgentID}
	matches := func(s string) bool {
		s = strings.TrimSpace(s)
		for _, w := range want {
			if s == w {
				return true
			}
		}
		return false
	}
	if given != "" {
		if matches(given) {
			return
		}
		fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch",
			Message:    fmt.Sprintf("--confirm %q does not name the adviser shown (%s); nothing was connected", given, target.label()),
			NextAction: "ks adviser connect ... --confirm " + target.label()})
	}
	// a terminal is a character device that is not /dev/null: a closed or
	// redirected stdin must never read as a person who could answer
	tty := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		tty = true
		if dn, derr := os.Stat(os.DevNull); derr == nil && os.SameFile(fi, dn) {
			tty = false
		}
	}
	if out.noInput || !tty {
		msg := "a connection is confirmed by naming its adviser, and no confirmation was given; --yes does not confirm a connection"
		if out.noInput {
			msg = "--no-input was given and a connection needs your confirmation; --yes does not confirm a connection"
		}
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: msg + "; nothing was connected",
			NextAction: "pass --confirm " + target.label()})
	}
	fmt.Fprintf(os.Stderr, "type %s to connect it, or anything else to stop: ", target.label())
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !matches(line) {
		fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the adviser was not confirmed; nothing was connected"})
	}
}

func hostedAdviserConnect(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	srcName := inv.Str("agent")
	if srcName == "" {
		srcName = "main"
	}
	src, err := resolveAgent(cr, sess, srcName)
	if err != nil {
		die(err)
	}
	tgt, err := resolveAdviserTarget(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if tgt.ID == src.ID {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "an agent cannot be its own adviser; nothing was connected", NextAction: "ks agent list --session " + sess.ShortID})
	}
	from, err := endOfAgent(cr, src)
	if err != nil {
		die(err)
	}
	to, err := endOfAgent(cr, tgt)
	if err != nil {
		die(err)
	}
	expires := inv.Str("expires")
	if expires == "" {
		expires = "24h"
	}
	ttl, perr := time.ParseDuration(expires)
	if perr != nil || ttl <= 0 || ttl > 30*24*time.Hour {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--expires %q is not a duration between 1s and 720h", expires), NextAction: "--expires 24h"})
	}
	lim := grantLimits{MaxConsultations: 50, MaxTokensPerConsultation: 16384, MaxOutputTokensPerConsultation: 4096}
	if inv.Set("max-consultations") {
		lim.MaxConsultations = inv.Int("max-consultations")
	}
	if inv.Set("max-tokens") {
		lim.MaxTokensPerConsultation = inv.Int("max-tokens")
	}
	if inv.Set("max-output-tokens") {
		lim.MaxOutputTokensPerConsultation = inv.Int("max-output-tokens")
	}
	// WHAT IS ABOUT TO BE GRANTED, shown before anything is sent
	// shown on stderr even under --quiet: this is the decision's own display,
	// not progress, and a confirmation of something not shown is no
	// confirmation (stdout stays the --json envelope alone)
	shown := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	shown("connect an adviser:")
	shown("  from      %s", from.line())
	shown("  adviser   %s", to.line())
	shown("  scope     advice only, one round per consultation; a reply authorises nothing")
	shown("  shares    the question %s writes, and nothing of its transcript or files", from.label())
	shown("  limits    %s", limitsLine(lim))
	shown("  expires   in %s; revoke at any time with ks adviser disconnect <grant>", ttl)
	shown("  paid by   model use for advice is metered to the ADVISER's session (%s) under its own key", to.SessionName)
	confirmGrant(to, inv.Str("confirm"))

	body := map[string]any{"source_agent_id": src.ID, "target_agent_id": tgt.ID, "scope": "advice",
		"context_policy": "question_only", "expires_seconds": int64(ttl / time.Second),
		"limits": map[string]any{"max_consultations": lim.MaxConsultations, "max_tokens_per_consultation": lim.MaxTokensPerConsultation,
			"max_output_tokens_per_consultation": lim.MaxOutputTokensPerConsultation}}
	var env struct {
		Data adviserGrant `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/adviser-grants", body, &env); err != nil {
		die(err)
	}
	g := env.Data
	emit(map[string]any{"grant": g, "from": from, "adviser": to, "confirmed": true}, func() {
		fmt.Printf("connected: %s may consult %s (grant %s, revision %d, %s)\n", from.label(), to.label(), g.ID, g.GrantRevision, grantState(g))
		fmt.Printf("expires %s; withdraw with: ks adviser disconnect %s\n", g.ExpiresAt, g.ID)
	})
}

func grantLine(g adviserGrant, agentID string) string {
	dir, other := "consults", g.TargetSessionName+"/"+g.TargetAgentName
	if g.TargetAgentID == agentID {
		dir, other = "advises", g.SourceSessionName+"/"+g.SourceAgentName
	}
	return fmt.Sprintf("%-18s %-8s %-8s %-28s rev %-3d expires %s", g.ID, grantState(g), dir, other, g.GrantRevision, g.ExpiresAt)
}

func hostedAdviserList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	name := inv.Str("agent")
	if name == "" {
		name = "main"
	}
	a, err := resolveAgent(cr, sess, name)
	if err != nil {
		die(err)
	}
	var env struct {
		Data struct {
			Items []adviserGrant `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/adviser-grants?"+url.Values{"agent_id": {a.ID}}.Encode(), nil, &env); err != nil {
		die(err)
	}
	gs := env.Data.Items
	emit(map[string]any{"agent": a.ID, "grants": gs, "count": len(gs)}, func() {
		if len(gs) == 0 {
			fmt.Printf("agent %s has no adviser connections. Connect one: ks adviser connect <session>/<agent> --session %s\n", a.Name, sess.ShortID)
			return
		}
		fmt.Printf("%-18s %-8s %-8s %-28s\n", "GRANT", "STATE", "", "OTHER END")
		for _, g := range gs {
			fmt.Println(grantLine(g, a.ID))
		}
	})
}

func hostedAdviserDisconnect(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var env struct {
		Data adviserGrant `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/adviser-grants/"+url.PathEscape(id), nil, &env); err != nil {
		die(err)
	}
	g := env.Data
	emit(map[string]any{"grant": g, "stops": "every future consultation over this connection, at once",
		"not_recalled": "advice already delivered, and questions already answered, stay where they are"}, func() {
		fmt.Printf("disconnected %s: %s/%s may no longer consult %s/%s (revoked %s)\n", g.ID,
			g.SourceSessionName, g.SourceAgentName, g.TargetSessionName, g.TargetAgentName, g.RevokedAt)
		fmt.Println("stops: every future consultation over this connection, at once")
		fmt.Println("not recalled: advice already delivered stays where it is")
	})
}
