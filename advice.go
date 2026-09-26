// advice.go: attributed advice and honest disagreement (KS-066, the client
// half). The service answers two reads:
//
//	GET /api/v2/tasks/{id}/advice    every consultation an instruction
//	                                 raised, the advisers still pending and
//	                                 those unavailable NAMED, whether the set
//	                                 is complete, current or history
//	GET /api/v2/consultations/{id}   one consultation: who asked whom (names
//	                                 and scoped ids), when it was asked, taken
//	                                 up and answered, the context it was
//	                                 asked with, its state, and the adviser's
//	                                 own words, attributed and unverified
//
// Surfaces: ks advice list <instruction>, ks advice show <consultation>, a
// summary in ks task show, and the live window's drawer (type "advice").
//
// THIS CLIENT NEVER COMPUTES OR SHOWS A CONSENSUS, A VOTE, A SCORE OR A
// PERCENTAGE. Replies are shown as given, one per adviser, each with its
// own attribution; disagreement is left visible as two replies; a missing
// reply is a named gap. Advice is always labelled unverified. Advice on an
// instruction that has ended is shown as that instruction's history, never
// as current advice.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type adviceParty struct {
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
}

type adviceContext struct {
	Policy        string   `json:"policy"`
	Hashes        []string `json:"hashes"`
	GrantID       string   `json:"grant_id"`
	GrantRevision int64    `json:"grant_revision"`
}

type adviceResponse struct {
	Text         string `json:"text"`
	By           string `json:"by"`
	AttemptID    string `json:"attempt_id,omitempty"`
	Verification string `json:"verification"`
}

type consultationDoc struct {
	ID             string          `json:"id"`
	State          string          `json:"state"`
	Label          string          `json:"label"`
	Asker          adviceParty     `json:"asker"`
	Adviser        adviceParty     `json:"adviser"`
	SourceTaskID   string          `json:"source_task_id,omitempty"`
	TaskStanding   string          `json:"task_standing"`
	RequestedAt    string          `json:"requested_at"`
	DeliveredAt    string          `json:"delivered_at,omitempty"`
	ReceivedAt     string          `json:"received_at,omitempty"`
	Deadline       string          `json:"deadline"`
	Context        adviceContext   `json:"context"`
	Question       string          `json:"question"`
	Response       *adviceResponse `json:"response"`
	DeclinedReason string          `json:"declined_reason,omitempty"`
	Note           string          `json:"note"`
}

// taskAdviceDoc is the advice set exactly as the service gives it. It has
// no aggregate field, and this client adds none.
type taskAdviceDoc struct {
	TaskID        string            `json:"task_id"`
	TaskState     string            `json:"task_state"`
	Standing      string            `json:"standing"`
	Consultations []consultationDoc `json:"consultations"`
	Replied       int               `json:"replied"`
	Pending       []string          `json:"pending"`
	Unavailable   []string          `json:"unavailable"`
	Complete      bool              `json:"complete"`
	Note          string            `json:"note"`
}

func fetchTaskAdvice(cr hostedCreds, taskID string) (taskAdviceDoc, error) {
	var env struct {
		Data taskAdviceDoc `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/tasks/"+url.PathEscape(taskID)+"/advice", nil, &env)
	return env.Data, err
}

func fetchConsultation(cr hostedCreds, id string) (consultationDoc, error) {
	var env struct {
		Data consultationDoc `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/consultations/"+url.PathEscape(id), nil, &env)
	return env.Data, err
}

// partyText names an agent with its scoped identity.
func partyText(p adviceParty) string {
	return fmt.Sprintf("%s (session %s, %s)", notRecorded(p.AgentName), notRecorded(p.SessionName), notRecorded(p.AgentID))
}

// standingText says whether advice is current or an ended instruction's
// history.
func standingText(s string) string {
	switch s {
	case "current":
		return "current: the instruction that asked is still in flight"
	case "history":
		return "HISTORY: the instruction that asked has ended; this is not current advice"
	case "none":
		return "not tied to an instruction"
	}
	return "unavailable"
}

// adviceSetLines is the head of an advice set: counts as the service gave
// them, and every missing opinion by name.
func adviceSetLines(a taskAdviceDoc) []string {
	out := []string{
		fmt.Sprintf("advice for instruction %s (%s)", a.TaskID, stateLabel("task_state", a.TaskState)),
		"  standing     " + standingText(a.Standing),
		fmt.Sprintf("  consulted    %d adviser(s); %d replied", len(a.Consultations), a.Replied),
	}
	for _, p := range a.Pending {
		out = append(out, "  PENDING      "+sanitize(p))
	}
	for _, u := range a.Unavailable {
		out = append(out, "  UNAVAILABLE  "+sanitize(u))
	}
	switch {
	case len(a.Consultations) == 0:
		out = append(out, "  complete     nobody was consulted")
	case a.Complete:
		out = append(out, "  complete     yes: every adviser asked replied; each reply below is its own adviser's words")
	default:
		out = append(out, "  complete     NO: an answer built on this advice must name the missing opinions above, and may not claim agreement without them")
	}
	out = append(out, "  shown as     each reply separately, attributed and unverified; no consensus, vote or score is computed")
	if a.Note != "" {
		out = append(out, "  service note "+sanitize(a.Note))
	}
	return out
}

// consultationLines is one consultation in full: the expanded drawer entry.
func consultationLines(c consultationDoc) []string {
	out := []string{
		fmt.Sprintf("consultation %s · %s", c.ID, notRecorded(sanitize(c.Label))),
		"  asked by     " + sanitize(partyText(c.Asker)),
		"  adviser      " + sanitize(partyText(c.Adviser)),
	}
	if c.SourceTaskID != "" {
		out = append(out, "  for          instruction "+c.SourceTaskID+"; "+standingText(c.TaskStanding))
	}
	out = append(out,
		fmt.Sprintf("  asked        %s · taken up %s · answered %s · deadline %s", figure(c.RequestedAt), notRecorded(c.DeliveredAt), notRecorded(c.ReceivedAt), figure(c.Deadline)),
		fmt.Sprintf("  context      policy %s · shared set %s · grant %s revision %d", notRecorded(c.Context.Policy), notRecorded(strings.Join(c.Context.Hashes, ",")), notRecorded(c.Context.GrantID), c.Context.GrantRevision),
		"  question     "+sanitize(notRecorded(c.Question)))
	switch {
	case c.Response != nil:
		ver := c.Response.Verification
		if ver == "" || ver == "unverified" {
			ver = "UNVERIFIED"
		}
		by := c.Response.By
		if by == "" {
			by = c.Adviser.AgentID
		}
		out = append(out, fmt.Sprintf("  reply        %s's own words (%s; by %s, attempt %s):", notRecorded(c.Adviser.AgentName), ver, sanitize(notRecorded(by)), notRecorded(c.Response.AttemptID)))
		for _, l := range strings.Split(strings.TrimRight(c.Response.Text, "\n"), "\n") {
			out = append(out, "    | "+sanitize(l))
		}
	case c.DeclinedReason != "":
		out = append(out, "  reply        none: declined ("+sanitize(c.DeclinedReason)+")")
	default:
		out = append(out, "  reply        none, and nothing is shown in its place")
	}
	if c.Note != "" {
		out = append(out, "  note         "+sanitize(c.Note))
	}
	return out
}

// adviceSummaryLines is the collapsed form for ks task show and the drawer:
// one line per adviser, and where to read it in full.
func adviceSummaryLines(a taskAdviceDoc) []string {
	out := adviceSetLines(a)
	for _, c := range a.Consultations {
		first := "no reply"
		if c.Response != nil {
			first = strings.SplitN(strings.TrimSpace(c.Response.Text), "\n", 2)[0]
			first = "\"" + clip(first, 60) + "\" (unverified)"
		}
		out = append(out, fmt.Sprintf("  - %s · %s · %s; full: ks advice show %s", sanitize(partyText(c.Adviser)), sanitize(notRecorded(c.Label)), sanitize(first), c.ID))
	}
	return out
}

// adviceProblem words a failed advice read: an older control plane without
// the route, or a read that failed. Neither is "no advice".
func adviceProblem(err error) string {
	var he *hostedErr
	if errors.As(err, &he) && he.Status == 404 && he.Type != "ks_not_found" {
		return "this control plane does not serve an instruction's advice; nothing is shown, which is not the same as none"
	}
	return "could not be READ, so it is not shown; that is not the same as none: " + sanitize(errText(err))
}

// hostedAdviceList is ks advice list <instruction>: the set, in full.
func hostedAdviceList(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the instruction whose advice to read is required", NextAction: "ks task list --session <session>"})
	}
	a, err := fetchTaskAdvice(cr, id)
	if err != nil {
		die(err)
	}
	emit(a, func() {
		for _, l := range adviceSetLines(a) {
			fmt.Println(l)
		}
		for _, c := range a.Consultations {
			fmt.Println()
			for _, l := range consultationLines(c) {
				fmt.Println(l)
			}
		}
	})
}

// hostedAdviceShow is ks advice show <consultation>.
func hostedAdviceShow(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	if id == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the consultation to show is required", NextAction: "ks advice list <instruction>"})
	}
	c, err := fetchConsultation(cr, id)
	if err != nil {
		die(err)
	}
	emit(c, func() {
		for _, l := range consultationLines(c) {
			fmt.Println(l)
		}
	})
}

// adviceDrawer is the live window's drawer: "advice" shows the advice of
// the instruction in flight (or the one named), collapsed, with each
// consultation's full read named.
func (win *liveWindow) adviceDrawer(cr hostedCreds, arg string) {
	taskID := arg
	if taskID == "" {
		agents, err := fetchAgents(cr, agentSessionID(win.sess))
		if err == nil {
			if a, perr := pickAgent(win.sess, agents, win.agent.ID); perr == nil {
				taskID = a.ActiveTaskID
			}
		}
		if taskID == "" {
			progress("advice: no instruction is in flight; name one: advice <instruction>")
			return
		}
	}
	a, err := fetchTaskAdvice(cr, taskID)
	if err != nil {
		progress("advice for %s %s", taskID, adviceProblem(err))
		return
	}
	progress("--- advice drawer ---")
	for _, l := range adviceSummaryLines(a) {
		progress("%s", l)
	}
	progress("--- end of drawer ---")
}
