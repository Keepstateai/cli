// agent_view.go: the live window's model, rendered (KS-034).
//
//	ks agent view <name>               header, runner, footer counts and the
//	                                   C11 KS menu, in the service's order
//	                                   and with its labels, within 80x24
//	ks agent view <name> --choose N    perform menu item N through the
//	                                   request the service names for it
//
// Everything shown comes from GET /api/v2/agents/{id}/view. A footer count
// the service could not read is null and reads "unknown", never 0. Each menu
// item names the direct service request that performs it; none goes through
// the model. A destructive item is never preselected: its dialog is shown
// with the service's title, body and choices and "no default", and it runs
// only when the person names the confirming choice with --confirm.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"
)

type viewRequestRow struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type viewPaletteItem struct {
	Label       string           `json:"label"`
	Effect      string           `json:"effect"`
	Requests    []viewRequestRow `json:"requests"`
	Local       bool             `json:"local,omitempty"`
	Inference   bool             `json:"inference"`
	Destructive bool             `json:"destructive"`
	Confirm     *struct {
		Title   string   `json:"title"`
		Body    []string `json:"body"`
		Choices []string `json:"choices"`
		Default string   `json:"default"`
	} `json:"confirm,omitempty"`
}

type liveViewDoc struct {
	Header struct {
		AccountID    string   `json:"account_id"`
		ProjectName  string   `json:"project_name,omitempty"`
		SessionID    string   `json:"session_id"`
		SessionName  string   `json:"session_name"`
		AgentID      string   `json:"agent_id"`
		AgentName    string   `json:"agent_name"`
		Controller   string   `json:"controller"`
		RuntimeState string   `json:"runtime_state"`
		Activity     string   `json:"activity"`
		ObservedAt   string   `json:"observed_at"`
		KeyRoutes    []string `json:"key_routes"`
		LastSavedAt  string   `json:"last_saved_at,omitempty"`
	} `json:"header"`
	Runner struct {
		Label         string `json:"label"`
		Certification string `json:"certification"`
		Reported      bool   `json:"reported"`
	} `json:"runner"`
	Conversation viewRequestRow `json:"conversation"`
	ConvNote     string         `json:"conversation_note"`
	Footer       struct {
		Queued           *int `json:"queued"`
		Held             *int `json:"held"`
		PendingApprovals *int `json:"pending_approvals"`
		Advisers         *int `json:"advisers"`
	} `json:"footer"`
	Palette []viewPaletteItem `json:"palette"`
	// KS-037: the status as the service labels it, with its age; the task in
	// flight and the last one; model spend (null: unavailable); one action
	Status *struct {
		Label       string `json:"label"`
		ObservedAt  string `json:"observed_at"`
		AgeSeconds  *int64 `json:"age_seconds"`
		Stale       bool   `json:"stale"`
		StaleAfterS int    `json:"stale_after_seconds"`
	} `json:"status"`
	CurrentTask *viewTaskRow `json:"current_task"`
	LastTask    *viewTaskRow `json:"last_task"`
	Usage       *struct {
		ModelSpend *int64 `json:"model_microusd"`
		Standing   string `json:"model_standing"`
	} `json:"usage"`
	NextAction *openAction `json:"next_action"`
}

type viewTaskRow struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// fit80 cuts one line to the 80-column window.
func fit80(s string) string {
	s = sanitize(s)
	if utf8.RuneCountInString(s) <= 80 {
		return s
	}
	r := []rune(s)
	return string(r[:79]) + "…"
}

func viewCount(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

// viewLines is the whole screen: at most 24 lines of at most 80 columns.
func viewLines(v liveViewDoc) []string {
	h := v.Header
	proj := ""
	if h.ProjectName != "" {
		proj = " · project " + h.ProjectName
	}
	lines := []string{
		fmt.Sprintf("%s/%s%s · %s", h.SessionName, h.AgentName, proj, h.Controller),
		statusLine(v),
		fmt.Sprintf("keys %s · last save %s", figure(strings.Join(h.KeyRoutes, ",")), figure(h.LastSavedAt)),
		fmt.Sprintf("runner %s (%s)", figure(v.Runner.Label), figure(v.Runner.Certification)),
		taskLine(v),
		"",
		"KS menu (no default):",
	}
	for i, it := range v.Palette {
		mark := ""
		if it.Destructive {
			mark = " !"
		}
		lines = append(lines, fmt.Sprintf(" %2d %s%s — %s", i+1, it.Label, mark, it.Effect))
	}
	spend := "model spend unavailable"
	if v.Usage != nil && v.Usage.ModelSpend != nil {
		spend = "model spend " + money(v.Usage.ModelSpend, "USD", v.Usage.Standing)
	}
	lines = append(lines, "",
		fmt.Sprintf("queued %s · held %s · approvals %s · advisers %s",
			viewCount(v.Footer.Queued), viewCount(v.Footer.Held), viewCount(v.Footer.PendingApprovals), viewCount(v.Footer.Advisers)), spend)
	if v.NextAction != nil && v.NextAction.Label != "" {
		lines = append(lines, "next: "+v.NextAction.Label+" — "+v.NextAction.Request)
	}
	if len(lines) > 24 {
		lines = append(lines[:23], fmt.Sprintf("(%d more lines; --json shows everything)", len(lines)-23))
	}
	for i := range lines {
		lines[i] = fit80(lines[i])
	}
	return lines
}

func fetchLiveView(cr hostedCreds, agentID string) (liveViewDoc, error) {
	var env struct {
		Data liveViewDoc `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(agentID)+"/view", nil, &env)
	return env.Data, err
}

func hostedAgentView(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	v, err := fetchLiveView(cr, a.ID)
	if err != nil {
		die(err)
	}
	if !inv.Set("choose") {
		emit(v, func() {
			for _, l := range viewLines(v) {
				fmt.Println(l)
			}
		})
		return
	}
	n := int(inv.Int("choose"))
	if n < 1 || n > len(v.Palette) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--choose is 1 to %d, the menu's items; nothing was done", len(v.Palette))})
	}
	performMenuItem(cr, sess, a, v.Palette[n-1], inv.Str("confirm"))
}

// performMenuItem calls exactly the requests the service named for the
// item. A request this client cannot make as named (a placeholder path, or
// a method it does not know for that route) is refused, not approximated.
func performMenuItem(cr hostedCreds, sess inventoryRow, a agentRow, it viewPaletteItem, confirm string) {
	if it.Inference {
		fail(&cliError{Code: exitIntegrity, Kind: "menu_inference", Message: fmt.Sprintf("the service says %q goes through the model; a management control never does, so nothing was done", it.Label)})
	}
	if it.Local || len(it.Requests) == 0 {
		fmt.Println(fit80(it.Label + ": " + it.Effect))
		fmt.Println("commands: ks agent list, ks agent queue list, ks approval list, ks result list, ks session usage, ks agent pause, ks agent open --take-control")
		fmt.Println("Ready means the agent reported it is ready; Paused means a save and a stop were both proved; Verified only ever comes from an independent check")
		return
	}
	if it.Confirm != nil {
		fmt.Fprintln(os.Stderr, fit80(it.Confirm.Title))
		for _, b := range it.Confirm.Body {
			fmt.Fprintln(os.Stderr, "  "+fit80(b))
		}
		fmt.Fprintf(os.Stderr, "  choices: %s (no default)\n", strings.Join(it.Confirm.Choices, " / "))
		proceed := ""
		if len(it.Confirm.Choices) > 0 {
			proceed = it.Confirm.Choices[0]
		}
		if confirm != proceed || proceed == "" || proceed == it.Confirm.Default {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
				Message:    fmt.Sprintf("%q changes the session and is never preselected; nothing was done", it.Label),
				NextAction: fmt.Sprintf("--confirm %q", proceed)})
		}
	}
	for _, rq := range it.Requests {
		if strings.Contains(rq.Path, "<") {
			fmt.Printf("%s: %s needs a value you choose (%s); nothing was sent for it\n", it.Label, rq.Pattern, rq.Path)
			continue
		}
		switch {
		case rq.Method == "GET":
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := hostedCall(cr, "GET", rq.Path, nil, &env); err != nil {
				die(err)
			}
			emit(map[string]any{"label": it.Label, "request": rq, "data": env.Data}, func() {
				fmt.Printf("%s — GET %s\n", it.Label, rq.Path)
				var pretty bytes.Buffer
				_ = json.Indent(&pretty, env.Data, "", "  ")
				lines := strings.Split(pretty.String(), "\n")
				for i, l := range lines {
					if i >= 20 {
						fmt.Printf("… %d more lines (--json shows everything)\n", len(lines)-20)
						break
					}
					fmt.Println(fit80(visible(l)))
				}
			})
		case rq.Method == "POST" && rq.Pattern == "/api/v2/sessions/{id}/pause":
			var env struct {
				Data map[string]any `json:"data"`
			}
			if err := hostedMutate(cr, "POST", rq.Path, map[string]any{}, &env); err != nil {
				die(saveFailure(err, a.Name, sess))
			}
			emit(env.Data, func() {
				fmt.Printf("%s: the session reads %s (%s)\n", it.Label, figure(env.Data["runtime_state"]), sanitize(fmt.Sprint(env.Data["note"])))
			})
		case rq.Method == "POST" && rq.Pattern == "/api/v2/agents/{id}/open":
			w, err := openAgent(cr, sess, a, true)
			if err != nil {
				die(err)
			}
			emit(map[string]any{"control": controlFacts(w)}, func() { fmt.Println(it.Label + ": " + controlLine(w)) })
		case rq.Method == "DELETE" && rq.Pattern == "/api/v2/control-leases/{id}":
			fmt.Printf("%s: this command holds no window and so no control to release; the agent keeps working (a window leaves with Ctrl-C)\n", it.Label)
		default:
			fail(&cliError{Code: exitFailed, Kind: "menu_request_unknown", Message: fmt.Sprintf("the service names %s %s for %q, which this client does not know how to make; nothing was sent (ks update)", rq.Method, rq.Pattern, it.Label)})
		}
	}
}

// statusLine is the header's status: the service's label and its age, and a
// stale notice past the threshold; the runtime beside it.
func statusLine(v liveViewDoc) string {
	rt := "runtime " + stateLabel("session_runtime", v.Header.RuntimeState)
	if v.Status == nil {
		return rt + " · " + stateLabel("agent_activity", v.Header.Activity) + " (observed " + figure(v.Header.ObservedAt) + ")"
	}
	age := "age unknown"
	if v.Status.AgeSeconds != nil {
		age = fmt.Sprintf("%ds ago", *v.Status.AgeSeconds)
	}
	s := fmt.Sprintf("%s · %s (%s)", rt, figure(v.Status.Label), age)
	if v.Status.Stale {
		s += fmt.Sprintf(" STALE: last known, older than %ds", v.Status.StaleAfterS)
	}
	return s
}

func taskLine(v liveViewDoc) string {
	cur, last := "none", "none"
	if v.CurrentTask != nil {
		cur = v.CurrentTask.ID + " " + v.CurrentTask.Label
	}
	if v.LastTask != nil {
		last = v.LastTask.ID + " " + v.LastTask.Label
	}
	return "task " + cur + " · last " + last
}
