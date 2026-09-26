// open_target.go: which agent `ks agent open` means, and what it does when
// the session behind it is not running (KS-032).
//
// Without --session and without a project binding, the agent is resolved by
// the service across everything the caller may see (GET
// /api/v2/agent-targets): exactly one match opens; several are shown as a
// numbered choice at a terminal (no default, never the newest) and refused
// with the list otherwise; none says how to create one. Resolving never
// creates an agent or a task.
//
// Opening never wakes a session. When the service says the session is not
// running, the window shows the runtime actions it names (Resume session,
// with the charge it discloses; View saved transcript) and follows nothing;
// --resume asks for the resume route explicitly, and only then.
package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type agentTargetRow struct {
	AgentID      string   `json:"agent_id"`
	AgentName    string   `json:"agent_name"`
	IsPrimary    bool     `json:"is_primary"`
	Activity     string   `json:"activity"`
	SessionID    string   `json:"session_id"`
	SessionName  string   `json:"session_name"`
	RuntimeState string   `json:"runtime_state"`
	ProjectID    string   `json:"project_id,omitempty"`
	ProjectName  string   `json:"project_name,omitempty"`
	KeyRoutes    []string `json:"key_routes"`
	Controlled   string   `json:"controlled_elsewhere,omitempty"`
}

type openAction struct {
	Action    string `json:"action"`
	Label     string `json:"label"`
	Request   string `json:"request"`
	Discloses string `json:"discloses,omitempty"`
}

type openRuntime struct {
	SessionID    string       `json:"session_id"`
	State        string       `json:"state"`
	Running      bool         `json:"running"`
	Woken        bool         `json:"woken"`
	Note         string       `json:"note"`
	Actions      []openAction `json:"actions"`
	UsageRoute   string       `json:"usage_route"`
	LastSavedAt  string       `json:"last_saved_at,omitempty"`
	CheckpointID string       `json:"last_checkpoint_id,omitempty"`
}

type openRecovery struct {
	Activity string       `json:"activity"`
	Note     string       `json:"note"`
	Actions  []openAction `json:"actions"`
}

func targetLine(t agentTargetRow) string {
	s := fmt.Sprintf("%s (%s) in session %q (%s)", t.AgentName, t.AgentID, t.SessionName, stateLabel("session_runtime", t.RuntimeState))
	if t.ProjectName != "" || t.ProjectID != "" {
		s += ", project " + figure(firstNonEmpty(t.ProjectName, t.ProjectID))
	}
	if len(t.KeyRoutes) > 0 {
		s += ", keys " + strings.Join(t.KeyRoutes, ",")
	}
	if t.Controlled != "" {
		s += ", controlled in another window"
	}
	return sanitize(s)
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// resolveOpenTarget asks the service which agent a name means, and returns
// the session and agent to open. It chooses nothing on its own.
func resolveOpenTarget(cr hostedCreds, name, project string) (inventoryRow, agentRow) {
	q := url.Values{"name": {name}}
	if project != "" {
		q.Set("project", project)
	}
	var env struct {
		Data struct {
			Resolution string           `json:"resolution"`
			Target     *agentTargetRow  `json:"target"`
			Candidates []agentTargetRow `json:"candidates"`
			Note       string           `json:"note"`
			Create     string           `json:"create"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/agent-targets?"+q.Encode(), nil, &env); err != nil {
		die(err)
	}
	d := env.Data
	var t agentTargetRow
	switch d.Resolution {
	case "resolved":
		if d.Target == nil {
			fail(fmt.Errorf("the service resolved %q but named no agent; nothing was opened", name))
		}
		t = *d.Target
	case "ambiguous":
		if !stdinIsTerminal() || out.noInput || len(d.Candidates) == 0 {
			lines := []string{}
			for _, c := range d.Candidates {
				lines = append(lines, targetLine(c))
			}
			fail(&cliError{Code: exitUsage, Kind: "ambiguous",
				Message:    fmt.Sprintf("%d agents are named %q and none is chosen for you: %s", len(d.Candidates), name, strings.Join(lines, "; ")),
				NextAction: fmt.Sprintf("ks agent open %s --session <session>", name)})
		}
		fmt.Fprintf(os.Stderr, "%d agents are named %q; choose one (there is no default):\n", len(d.Candidates), name)
		for i, c := range d.Candidates {
			fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, targetLine(c))
		}
		fmt.Fprint(os.Stderr, "number: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		i, ok := choiceIndex(line, len(d.Candidates))
		if !ok {
			fail(&cliError{Code: exitUsage, Kind: "selection_declined", Message: "no agent was chosen; nothing was opened"})
		}
		t = d.Candidates[i]
	default:
		msg := fmt.Sprintf("no agent you can see is named %q; opening never creates one", name)
		fail(&cliError{Code: exitUsage, Kind: "not_found", Message: msg, NextAction: firstNonEmpty(sanitize(d.Create), "ks run --agent --agent-name "+name)})
	}
	sess := inventoryRow{ID: t.SessionID, RecordID: t.SessionID, ShortID: t.SessionID, Name: t.SessionName, RuntimeState: t.RuntimeState}
	if rows, err := fetchInventory(cr, "", true); err == nil {
		for _, r := range rows {
			if r.RecordID == t.SessionID || r.ID == t.SessionID {
				sess = r
			}
		}
	}
	return sess, agentRow{ID: t.AgentID, SessionID: t.SessionID, Name: t.AgentName, IsPrimary: t.IsPrimary, Activity: t.Activity}
}

func choiceIndex(answer string, n int) (int, bool) {
	var i int
	if _, err := fmt.Sscanf(strings.TrimSpace(answer), "%d", &i); err != nil || i < 1 || i > n || fmt.Sprint(i) != strings.TrimSpace(answer) {
		return 0, false
	}
	return i - 1, true
}

func printOpenActions(actions []openAction, name string, sess inventoryRow) {
	for _, a := range actions {
		fmt.Printf("  %-22s %s\n", a.Label, sanitize(a.Request))
		if a.Discloses != "" {
			fmt.Printf("  %-22s %s\n", "", sanitize(a.Discloses))
		}
		switch a.Action {
		case "resume_session":
			fmt.Printf("  %-22s ks: ks agent open %s --session %s --resume\n", "", name, sess.ShortID)
		case "view_saved_transcript":
			fmt.Printf("  %-22s ks: ks agent logs --session %s\n", "", sess.ShortID)
		case "open_recovery":
			fmt.Printf("  %-22s ks: ks agent queue show %s --session %s\n", "", name, sess.ShortID)
		}
	}
}
