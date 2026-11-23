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
	// The funds park: why the session is parked when no person parked it
	// (funds_interlock: the account's credits ran out). The fleet inventory
	// does not carry it, so it is read from the session's record, whose own
	// runtime state is kept beside it.
	ParkReason  string `json:"park_reason,omitempty"`
	RecordState string `json:"record_runtime_state,omitempty"`
	// RunnerModel is the model the session's agents are started with, read
	// from the session's record (the fleet inventory does not carry it); a
	// record that could not be read says so here, never as no model.
	RunnerModel           string `json:"runner_model,omitempty"`
	RunnerModelUnreadable string `json:"runner_model_unreadable,omitempty"`
	// KeyBindings is what the session calls each provider with, read from
	// its record: one of your keys, or KeepState's key
	KeyBindings *sessionKeyBindings `json:"key_bindings,omitempty"`
}

// fetchInventory reads every page: a filter answers the complete
// authorized result, never the first page of it.
func fetchInventory(cr hostedCreds, state string, all bool) ([]inventoryRow, error) {
	// an organization's context: its session records (org_run.go)
	if org := currentContext(cr); org != "" {
		return orgInventory(cr, org, state)
	}
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

// inventoryCols and inventoryCells lay the rows out for a width: 120
// columns and more add name, key and the last save; under that identity,
// state (the console's word beside the raw state), activity, task and last
// activity. A table that still does not fit is stacked by the kit.
func inventoryCols(wide bool) []Col {
	if wide {
		return []Col{{Title: "Session"}, {Title: "Name", Flex: true}, {Title: "State"}, {Title: "Agent"}, {Title: "Task"}, {Title: "Last activity", Second: true}, {Title: "Last save", Second: true}, {Title: "Key", Second: true}}
	}
	return []Col{{Title: "Session"}, {Title: "State"}, {Title: "Agent"}, {Title: "Task"}, {Title: "Last activity", Second: true}, {Title: "Saved", Second: true}}
}

func inventoryCells(rows []inventoryRow, wide bool) [][]string {
	var out [][]string
	for _, r := range rows {
		if wide {
			out = append(out, []string{r.ShortID, r.Name, invState(r), invAgent(r), invTask(r), ago(r.LastActivityAt), ago(r.LastCheckpointAt), r.KeyAlias})
			continue
		}
		saved := "no"
		if r.LastCheckpointID != "" {
			saved = "yes"
		}
		out = append(out, []string{r.ShortID, invState(r), invAgent(r), invTask(r), ago(r.LastActivityAt), saved})
	}
	return out
}

// inventoryNotes says, for each session parked for want of credit, what that
// means.
func inventoryNotes(rows []inventoryRow) []string {
	var notes []string
	for _, r := range rows {
		if rowFundsParked(r) {
			notes = append(notes, fmt.Sprintf("%s %s; its agent's last word and task are frozen at the saved point; nothing runs or is charged.", r.ShortID, fundsPausedLine))
		}
	}
	return notes
}

// sessionEnded: the session's record or runtime is over (the console's
// Ended): dead, deleting or deleted.
func sessionEnded(state string) bool {
	switch state {
	case "dead", "deleting", "deleted":
		return true
	}
	return false
}

// invState, invAgent and invTask are a row's cells. The state is the
// console's word beside the raw state; a funds-parked session reads paused
// (its footnote says out of credit), its agent's frozen word reads frozen,
// and a task that last read running reads paused: none of them is moving
// (the funds park).
func invState(r inventoryRow) string {
	if rowFundsParked(r) {
		return "Paused (" + sanitize(r.RuntimeState) + ")"
	}
	return stateWords("session_runtime", r.RuntimeState)
}

func invAgent(r inventoryRow) string {
	if rowFundsParked(r) {
		return "frozen"
	}
	return stateCell("agent_activity", r.AgentActivity)
}

func invTask(r inventoryRow) string {
	if rowFundsParked(r) && frozenTask(r.TaskState) {
		return "paused"
	}
	return r.TaskState
}

func hostedSessionList(cr hostedCreds, inv *Invocation) {
	state := inv.Str("state")
	if state != "" && state != "running" && state != "parked" && state != "dead" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--state is running, parked or dead", NextAction: "ks session list --state running"})
	}
	// ended sessions are listed too, in a final group (--all is kept for
	// the scripts that pass it; it changes nothing now)
	rows, err := fetchInventory(cr, state, true)
	if err != nil {
		die(err)
	}
	rows, parkProblem := withParkReasons(cr, rows)
	doc := map[string]any{"sessions": rows, "count": len(rows), "state_filter": state, "all": true}
	if parkProblem != "" {
		doc["park_reasons_unreadable"] = parkProblem
	}
	if org := currentContext(cr); org != "" {
		doc["context"] = org
	}
	emit(doc, func() {
		o, _ := cmdUI()
		o.List(sessionListScreen(rows, state, currentContext(cr), parkProblem, o.cols))
	})
}

// sessionListScreen is ks session list: the live sessions, then the ended
// ones under their own heading.
func sessionListScreen(rows []inventoryRow, state, org, parkProblem string, cols int) listScreen {
	wide := cols >= 120
	s := listScreen{Title: "Your sessions", Detail: listCount(len(rows), "session", "sessions"), Cols: inventoryCols(wide)}
	if org != "" {
		s.Title = "Sessions of the organization " + org
	}
	var live, ended []inventoryRow
	for _, r := range rows {
		if state == "" && sessionEnded(r.RuntimeState) {
			ended = append(ended, r)
		} else {
			live = append(live, r)
		}
	}
	if len(live) > 0 {
		s.Groups = append(s.Groups, listGroup{Rows: inventoryCells(live, wide)})
	}
	if len(ended) > 0 {
		s.Groups = append(s.Groups, listGroup{Heading: "Ended", Rows: inventoryCells(ended, wide)})
	}
	if len(rows) == 0 {
		if state != "" {
			s.Empty = "No " + state + " sessions."
		} else {
			s.Empty = "You have no sessions yet."
			s.Next = []string{"ks run"}
		}
		return s
	}
	s.Notes = append(s.Notes, inventoryNotes(rows)...)
	if parkProblem != "" {
		s.Notes = append(s.Notes, parkProblem)
	}
	s.Notes = append(s.Notes, "The session column is the start of its full id (ks run prints the full one); either works in every command.")
	s.Next = []string{"ks session show <session>"}
	return s
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
	rs, parkProblem := withParkReasons(cr, []inventoryRow{r})
	r = rs[0]
	// the model its agents call lives on the session's record; a record that
	// cannot be read is said, never shown as no model
	if r.RecordID != "" {
		if rec, err := fetchSessionModelRecord(cr, r.RecordID); err != nil {
			r.RunnerModelUnreadable = "could not be read: " + sanitize(errText(err))
		} else {
			r.RunnerModel = rec.RunnerModel
			r.KeyBindings = rec.KeyBindings
		}
	}
	var doc any = r
	if parkProblem != "" {
		doc = map[string]any{"session": r, "park_reasons_unreadable": parkProblem}
	}
	emit(doc, func() {
		outf("session %s (%s)\n", r.ID, r.ShortID)
		outf("  name           %s\n", r.Name)
		if rowFundsParked(r) {
			outf("  state          %s\n", fundsPausedLine)
			outf("  why            the account's credits ran out, so the funds interlock saved this session and paused it (it was never killed); nothing runs and nothing is charged while it is parked\n")
			outf("  fleet state    %s\n", stateWords("session_runtime", r.RuntimeState))
			outf("  agent          %s (frozen at the saved point; not current)\n", stateWords("agent_activity", r.AgentActivity))
			outf("  task           %s (frozen; nothing is moving)\n", r.TaskState)
			outf("  then resume    ks wake %s (after adding credit)\n", r.ShortID)
		} else if parkProblem != "" {
			outf("  park reason    %s\n", parkProblem)
		}
		if !rowFundsParked(r) {
			if r.FleetState != "" {
				outf("  state          %s (fleet: %s)\n", stateWords("session_runtime", r.RuntimeState), sanitize(r.FleetState))
			} else {
				outf("  state          %s\n", stateWords("session_runtime", r.RuntimeState))
			}
			if r.ParkReason != "" {
				outf("  park reason    %s\n", sanitize(r.ParkReason))
			}
			outf("  agent          %s\n", stateWords("agent_activity", r.AgentActivity))
			outf("  task           %s\n", r.TaskState)
		}
		outf("  key            %s\n", r.KeyAlias)
		if r.KeyBindings != nil {
			outf("  model keys     %s\n", r.KeyBindings.words())
		}
		outf("  image          %s\n", r.Image)
		outf("  budget         %d tokens\n", r.BudgetTokens)
		outf("  epoch          %d\n", r.ExecutionEpoch)
		outf("  created        %s\n", r.CreatedAt)
		outf("  last activity  %s\n", r.LastActivityAt)
		if r.LastCheckpointID != "" {
			outf("  last save      %s at %s\n", r.LastCheckpointID, r.LastCheckpointAt)
		} else {
			outf("  last save      none\n")
		}
		if r.Parent != "" {
			outf("  forked from    %s\n", r.Parent)
		}
		if r.RecordID != "" {
			outf("  record         %s (primary agent %s)\n", r.RecordID, r.PrimaryAgentID)
			if r.RunnerModelUnreadable != "" {
				outf("  runner model   %s\n", r.RunnerModelUnreadable)
			} else {
				outf("  runner model   %s (change it while paused: ks session model %s <model>)\n", sanitize(notRecorded(r.RunnerModel)), r.ShortID)
			}
		}
		outf("  observed       %s\n", r.ObservedAt)
	})
}
