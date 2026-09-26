// session.go: the session inventory (KS-021). ks session list (ks ls)
// reads the account's fleet sessions from the control plane, every page,
// newest activity first, and shows identity and state without guessing:
// a field the service did not report reads "unavailable". ks session show
// accepts a full id or a short id that is unique inside the account; an
// ambiguous prefix names its candidates and does nothing.
package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type inventoryRow struct {
	ID             string `json:"id"`
	ShortID        string `json:"short_id"`
	Name           string `json:"name"`
	RuntimeState   string `json:"runtime_state"`
	FleetState     string `json:"fleet_state,omitempty"` // no longer sent by the service; shown only when present
	Image          string `json:"image"`
	Parent         string `json:"parent,omitempty"`
	BudgetTokens   int64  `json:"budget_tokens"`
	ExecutionEpoch int64  `json:"execution_epoch"`
	// Revision is the record's concurrency token. A decision that changes a
	// session is bound to the revision it was prepared against, so one
	// prepared before something moved cannot land after it.
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"created_at"`
	LastActivityAt   string `json:"last_activity_at"`
	LastCheckpointID string `json:"last_checkpoint_id,omitempty"`
	LastCheckpointAt string `json:"last_checkpoint_at,omitempty"`
	RecordID         string `json:"record_id,omitempty"`
	PrimaryAgentID   string `json:"primary_agent_id,omitempty"`
	AgentActivity    string `json:"agent_activity"`
	TaskState        string `json:"task_state"`
	KeyAlias         string `json:"key_alias"`
	ObservedAt       string `json:"observed_at"`
}

// fetchInventory reads every page: a filter answers the complete
// authorized result, never the first page of it.
func fetchInventory(cr hostedCreds, state string, all bool) ([]inventoryRow, error) {
	var out []inventoryRow
	cursor := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"source": {"fleet"}, "limit": {"200"}}
		if state != "" {
			q.Set("state", state)
		}
		if all {
			q.Set("all", "1")
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var env struct {
			Data struct {
				Items      []inventoryRow `json:"items"`
				NextCursor string         `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", "/api/v2/sessions?"+q.Encode(), nil, &env); err != nil {
			return nil, err
		}
		out = append(out, env.Data.Items...)
		cursor = env.Data.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// termColumns is the terminal's width for tables: COLUMNS, then stty on
// the real stdin, then 80.
func termColumns() int {
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v >= 20 {
		return v
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		var rows, cols int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &rows, &cols); err == nil && cols >= 20 {
			return cols
		}
	}
	return 80
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func ago(ts string) string {
	if ts == "" {
		return "unavailable"
	}
	// the service's timestamps are RFC 3339 UTC; the date part is enough for a table
	if len(ts) >= 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}
	return ts
}

// renderInventory lays the rows out for a width: 80 columns keep identity,
// state, activity, task and last activity; 120 add name, key and the last
// save. No column is ever wider than its slot, so fields do not overlap.
func renderInventory(rows []inventoryRow, width int) string {
	var b strings.Builder
	wide := width >= 110
	if wide {
		fmt.Fprintf(&b, "%-14s %-16s %-8s %-10s %-11s %-16s %-16s %-16s\n", "SESSION", "NAME", "STATE", "AGENT", "TASK", "LAST ACTIVITY", "LAST SAVE", "KEY")
		for _, r := range rows {
			fmt.Fprintf(&b, "%-14s %-16s %-8s %-10s %-11s %-16s %-16s %-16s\n", clip(r.ShortID, 14), clip(r.Name, 16), clip(stateCell("session_runtime", r.RuntimeState), 8), clip(stateCell("agent_activity", r.AgentActivity), 10), clip(r.TaskState, 11), ago(r.LastActivityAt), ago(r.LastCheckpointAt), clip(r.KeyAlias, 16))
		}
	} else {
		fmt.Fprintf(&b, "%-14s %-8s %-11s %-11s %-16s %s\n", "SESSION", "STATE", "AGENT", "TASK", "LAST ACTIVITY", "SAVED")
		for _, r := range rows {
			saved := "no"
			if r.LastCheckpointID != "" {
				saved = "yes"
			}
			fmt.Fprintf(&b, "%-14s %-8s %-11s %-11s %-16s %s\n", clip(r.ShortID, 14), clip(stateCell("session_runtime", r.RuntimeState), 8), clip(stateCell("agent_activity", r.AgentActivity), 11), clip(r.TaskState, 11), ago(r.LastActivityAt), saved)
		}
	}
	return b.String()
}

func hostedSessionList(cr hostedCreds, inv *Invocation) {
	state := inv.Str("state")
	if state != "" && state != "running" && state != "parked" && state != "dead" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--state is running, parked or dead", NextAction: "ks session list --state running"})
	}
	rows, err := fetchInventory(cr, state, inv.Bool("all"))
	if err != nil {
		die(err)
	}
	emit(map[string]any{"sessions": rows, "count": len(rows), "state_filter": state, "all": inv.Bool("all")}, func() {
		if len(rows) == 0 {
			if state != "" {
				fmt.Printf("No %s sessions.\n", state)
			} else {
				fmt.Println("No sessions. Start one: ks run")
			}
			return
		}
		fmt.Print(renderInventory(rows, termColumns()))
		fmt.Printf("%d session(s); details: ks session show <session>\n", len(rows))
	})
}

// resolveSession answers the one session an argument names: a full id, or
// a prefix unique among the account's sessions. Ambiguity is an error
// that lists the candidates; nothing is chosen for the caller.
func resolveSession(cr hostedCreds, arg string) (inventoryRow, error) {
	if arg == "" {
		return inventoryRow{}, &cliError{Code: exitUsage, Kind: "usage", Message: "a session id is required", NextAction: "ks session list"}
	}
	rows, err := fetchInventory(cr, "", true)
	if err != nil {
		return inventoryRow{}, err
	}
	var hits []inventoryRow
	for _, r := range rows {
		if r.ID == arg || (r.RecordID != "" && r.RecordID == arg) {
			return r, nil
		}
		if len(arg) >= 4 && strings.HasPrefix(r.ID, arg) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return inventoryRow{}, &cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("no session of yours starts with %q", arg), NextAction: "ks session list"}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	var names []string
	for _, h := range hits {
		names = append(names, h.ShortID+" ("+h.RuntimeState+")")
	}
	return inventoryRow{}, &cliError{Code: exitUsage, Kind: "ambiguous", Message: fmt.Sprintf("%q matches %d sessions: %s; give more of the id", arg, len(hits), strings.Join(names, ", ")), NextAction: "ks session list"}
}

func hostedSessionShow(cr hostedCreds, inv *Invocation) {
	r, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	emit(r, func() {
		fmt.Printf("session %s (%s)\n", r.ID, r.ShortID)
		fmt.Printf("  name           %s\n", r.Name)
		if r.FleetState != "" {
			fmt.Printf("  state          %s (fleet: %s)\n", r.RuntimeState, r.FleetState)
		} else {
			fmt.Printf("  state          %s\n", stateLabel("session_runtime", r.RuntimeState))
		}
		fmt.Printf("  agent          %s\n", stateLabel("agent_activity", r.AgentActivity))
		fmt.Printf("  task           %s\n", r.TaskState)
		fmt.Printf("  key            %s\n", r.KeyAlias)
		fmt.Printf("  image          %s\n", r.Image)
		fmt.Printf("  budget         %d tokens\n", r.BudgetTokens)
		fmt.Printf("  epoch          %d\n", r.ExecutionEpoch)
		fmt.Printf("  created        %s\n", r.CreatedAt)
		fmt.Printf("  last activity  %s\n", r.LastActivityAt)
		if r.LastCheckpointID != "" {
			fmt.Printf("  last save      %s at %s\n", r.LastCheckpointID, r.LastCheckpointAt)
		} else {
			fmt.Printf("  last save      none\n")
		}
		if r.Parent != "" {
			fmt.Printf("  forked from    %s\n", r.Parent)
		}
		if r.RecordID != "" {
			fmt.Printf("  record         %s (primary agent %s)\n", r.RecordID, r.PrimaryAgentID)
		}
		fmt.Printf("  observed       %s\n", r.ObservedAt)
	})
}
