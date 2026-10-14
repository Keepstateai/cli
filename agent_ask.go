// agent_ask.go: a person asks one of an agent's connected advisers a
// question from the terminal, the way the browser console does with a
// message that starts with @handle.
//
//	ks agent ask <name> "@<adviser> <question>" --session <s>
//	@<adviser> <question>     typed into ks agent open, or into the app
//
// The question goes over a connection a person already made (ks adviser
// connect): the drawer (GET /api/v2/agents/{id}/advisers) lists the live
// connections, the handle at the start of the line names exactly one of
// them (the adviser's name, or session/name when two connected agents share
// a name), and the question is sent on the agent's behalf (POST
// /api/v2/agents/{id}/consultations) with no model of the asking agent
// involved. A line that starts with @ is NEVER sent to the agent as an
// instruction: a handle that names nobody, or two agents, is refused with
// the known handles. A paused adviser is woken by the question through its
// session's resume, which that session pays for. The reply is the adviser's
// own words, advice only and unverified; it authorises nothing.
//
// Interrupting ks agent ask while it waits (Ctrl-C, or SIGTERM) cancels the
// question with the same request the console's Cancel sends (POST
// /api/v2/consultations/{id}/cancel): the request is closed and a reply to it
// is refused, and the adviser's own work is not touched. The line says what
// happened: cancelled, a reply that had already arrived, or a cancel that
// could not be confirmed. A second interrupt stops at once.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// adviserDrawerEntry is one live connection as the drawer lists it.
type adviserDrawerEntry struct {
	GrantID       string      `json:"grant_id"`
	GrantRevision int64       `json:"grant_revision"`
	ExpiresAt     string      `json:"expires_at"`
	Adviser       adviceParty `json:"adviser"`
	Readiness     string      `json:"readiness"` // ready | busy | not_dispatchable | no_supervisor | removed | unavailable
	Queued        []string    `json:"queued_requests"`
	Budget        string      `json:"budget"`
}

type adviserDrawerDoc struct {
	AgentID  string               `json:"agent_id"`
	Advisers []adviserDrawerEntry `json:"advisers"`
	Note     string               `json:"note"`
}

func fetchAdviserDrawer(cr hostedCreds, agentID string) (adviserDrawerDoc, error) {
	var env struct {
		Data adviserDrawerDoc `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(agentID)+"/advisers", nil, &env)
	return env.Data, err
}

// adviserHandle is how one connected adviser is called with @: its name,
// or session/name where two connected agents share a name.
type adviserHandle struct {
	GrantID     string `json:"grant_id"`
	AgentID     string `json:"agent_id"`
	Name        string `json:"name"`
	SessionName string `json:"session_name"`
	Handle      string `json:"handle"`
	Readiness   string `json:"readiness"`
	Queued      int    `json:"queued"`
}

func adviserHandles(entries []adviserDrawerEntry) []adviserHandle {
	var out []adviserHandle
	count := map[string]int{}
	for _, e := range entries {
		name := e.Adviser.AgentName
		if name == "" {
			name = e.Adviser.AgentID
		}
		sess := e.Adviser.SessionName
		if sess == "" {
			sess = e.Adviser.SessionID
		}
		count[name]++
		out = append(out, adviserHandle{GrantID: e.GrantID, AgentID: e.Adviser.AgentID, Name: name, SessionName: sess, Readiness: e.Readiness, Queued: len(e.Queued)})
	}
	for i := range out {
		out[i].Handle = out[i].Name
		if count[out[i].Name] > 1 {
			out[i].Handle = out[i].SessionName + "/" + out[i].Name
		}
	}
	return out
}

// mentionShape is "@handle question", the handle being a name or session/name.
var mentionShape = regexp.MustCompile(`^@([A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)?)(?:\s+([\s\S]*))?$`)

// knownHandles lists the connected advisers by handle, for a refusal.
func knownHandles(handles []adviserHandle, sess inventoryRow) string {
	if len(handles) == 0 {
		return "none is connected; connect one: ks adviser connect <session>/<agent> --session " + sess.ShortID
	}
	var hs []string
	for _, h := range handles {
		hs = append(hs, "@"+h.Handle)
	}
	return "the connected agents are " + strings.Join(hs, ", ")
}

// parseMention reads "@handle question": the one connected adviser the
// handle names and the question after it, or why the line names nobody
// (nothing is chosen for the person: a name two connected agents share is
// refused with both handles).
func parseMention(text string, handles []adviserHandle, sess inventoryRow) (*adviserHandle, string, string) {
	m := mentionShape.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return nil, "", "a question to a connected agent starts with its handle: @<adviser> <question>; " + knownHandles(handles, sess)
	}
	name, question := m[1], strings.TrimSpace(m[2])
	var hits []adviserHandle
	var same []string
	for _, h := range handles {
		if h.Handle == name || h.SessionName+"/"+h.Name == name {
			hits = append(hits, h)
		}
		if h.Name == name {
			same = append(same, "@"+h.Handle)
		}
	}
	switch {
	case len(hits) != 1 && len(same) > 1:
		return nil, "", fmt.Sprintf("two connected agents are called %s; write %s", name, strings.Join(same, " or "))
	case len(hits) != 1:
		return nil, "", fmt.Sprintf("no connected agent is called @%s; %s", name, knownHandles(handles, sess))
	case question == "":
		return nil, "", "write the question after @" + hits[0].Handle
	}
	return &hits[0], question, ""
}

// consultAsked is what the consultation route answers: the consultation's
// id and state, and whether the question is waking a paused adviser.
type consultAsked struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	AdviserWake string `json:"adviser_wake,omitempty"`
	Deadline    string `json:"deadline"`
}

// askConsultation sends one person consultation on the agent's behalf. A
// request id already used answers the same consultation (replayed).
func askConsultation(cr hostedCreds, agentID, grantID, question, requestID string) (consultAsked, bool, error) {
	path := "/api/v2/agents/" + url.PathEscape(agentID) + "/consultations"
	body := map[string]any{"grant_id": grantID, "question": question, "request_id": requestID}
	resp, raw, err := doBounded(cr, "POST", path, nil, mustJSON(body))
	if err != nil {
		return consultAsked{}, false, err
	}
	if resp.StatusCode/100 != 2 {
		return consultAsked{}, false, hostedError("POST", path, resp, raw)
	}
	var env struct {
		Data consultAsked `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.ID == "" {
		return consultAsked{}, false, fmt.Errorf("the consultation could not be read back (unexpected shape)")
	}
	return env.Data, resp.StatusCode == 200, nil
}

// askRefusal words the service's refusals of a person's question. Nothing
// was asked in any of them.
func askRefusal(err error, h adviserHandle, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_adviser_not_woken":
		ce.Message = sanitize(he.Message)
		ce.NextAction = "ks session show <the adviser's session> (then ask again once it can be woken)"
	case "ks_adviser_parked":
		ce.Message = sanitize(he.Message) + "; nothing was asked"
		ce.NextAction = fmt.Sprintf("ks agent resume %s --session <its session>, then ask again", h.Name)
	case "ks_no_grant", "ks_grant_revoked", "ks_grant_expired", "ks_grant_stale":
		ce.Message = sanitize(he.Message) + "; nothing was asked"
		ce.NextAction = "ks adviser list --session " + sess.ShortID
	case "ks_consult_budget":
		ce.Message = "the adviser's session has too little of its token budget left for this connection's per-consultation limit, so nothing was asked: " + sanitize(he.Message)
	case "ks_consult_secret_detected":
		ce.Message = "the question looks like it carries a secret, so it was not stored or sent: " + sanitize(he.Message)
	case "ks_grant_limit", "ks_consult_recipients_cap":
		ce.Message = "the connection's limit was reached, so nothing was asked: " + sanitize(he.Message)
	default:
		return err
	}
	return ce
}

// consultClosed: the service has closed the consultation one way or another.
func consultClosed(state string) bool {
	switch state {
	case "replied", "declined", "cancelled", "expired":
		return true
	}
	return false
}

// replyBound is how long this client waits for an answer: an explicit
// --wait-timeout (or the impatience a test asks for), else until the
// service's own deadline has passed and it has closed the consultation, with
// a minute's grace; fifteen minutes when no deadline was reported.
func replyBound(deadline string) time.Duration {
	if b := boundedWait(); b != waitTimeout {
		return b
	}
	t, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		return 15 * time.Minute
	}
	return max(time.Minute, time.Until(t)+time.Minute)
}

// wakeLine says, once each, that the question is waking a paused adviser
// and that the adviser then ran.
func wakeLine(c consultationDoc, said map[string]bool) (string, string) {
	if c.AdviserWake == nil || said[c.AdviserWake.State] {
		return "", ""
	}
	said[c.AdviserWake.State] = true
	switch c.AdviserWake.State {
	case "waking":
		return "waking", "waking the adviser: its session is paused, so the question resumes it (that session pays its runtime from now); the answer's deadline runs from the moment it runs"
	case "woken":
		return "woken", "the adviser is running (woke at " + figure(c.AdviserWake.WokeAt) + "); waiting for its answer"
	}
	return "", ""
}

// waitForReply polls one consultation until the service closes it (replied,
// declined, cancelled or expired), saying what is worth saying while it
// waits. Ctrl-C stops the waiting here only: the question stands.
func waitForReply(cr hostedCreds, id, deadline string, say func(kind, line string)) (consultationDoc, error) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return waitForReplyOn(cr, id, deadline, sig, say)
}

// waitForReplyOn is waitForReply on a signal channel the caller holds, for a
// caller that acts on the interrupt and must still see a second one.
func waitForReplyOn(cr hostedCreds, id, deadline string, sig <-chan os.Signal, say func(kind, line string)) (consultationDoc, error) {
	bound := replyBound(deadline)
	cadence := pollCadence(boundedWait())
	started := time.Now()
	said := map[string]bool{}
	var last consultationDoc
	for {
		c, err := fetchConsultation(cr, id)
		if err != nil {
			say("read_failed", "the consultation could not be read just now ("+sanitize(errText(err))+"); still waiting")
		} else {
			last = c
			if kind, line := wakeLine(c, said); line != "" {
				say(kind, line)
			}
			if consultClosed(c.State) {
				return c, nil
			}
		}
		if time.Since(started) > bound {
			return last, &cliError{Code: exitTemporary, Kind: "advice_pending", WorkStarted: workYes,
				Message:    fmt.Sprintf("no answer after %s; the question stands and the adviser may still answer it", time.Since(started).Round(time.Second)),
				NextAction: "ks advice show " + id}
		}
		select {
		case <-sig:
			return last, &cliError{Code: exitInterrupt, Kind: "interrupted", WorkStarted: workYes,
				Message:    "stopped waiting; the question stands and the adviser may still answer it",
				NextAction: "ks advice show " + id}
		case <-time.After(cadence):
		}
	}
}

// cancelAsked is what an interrupt does to the question ks agent ask is
// waiting on: it sends the console's cancel, says on one line what came of
// it, and exits as an interrupted command. A second interrupt while the
// cancel is in flight stops at once, with the cancel's outcome unknown.
func cancelAsked(cr hostedCreds, id, handle string, sig <-chan os.Signal) {
	type answer struct {
		resp *http.Response
		raw  []byte
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		resp, raw, err := doBounded(cr, "POST", "/api/v2/consultations/"+url.PathEscape(id)+"/cancel",
			map[string]string{"Idempotency-Key": newIdempotencyKey()}, mustJSON(map[string]string{"reason": "cancelled from the terminal (interrupted)"}))
		done <- answer{resp, raw, err}
	}()
	var a answer
	select {
	case a = <-done:
	case <-sig:
		emitLine(map[string]any{"type": "cancel_abandoned", "consultation": id},
			"stopped without waiting for the cancel's answer: whether consultation "+id+" was cancelled is not known, and "+sanitize(handle)+" may still answer it; ks advice show "+id)
		os.Exit(exitInterrupt)
	}
	emitLine(cancelOutcome(cr, id, handle, a.resp, a.raw, a.err))
	os.Exit(exitInterrupt)
}

// cancelOutcome reads the cancel's answer into the line ks agent ask prints.
// A cancel the control plane refused leaves the question standing; one with
// no answer, or a failure that says nothing certain, may or may not have
// arrived, and the line says so rather than guessing.
func cancelOutcome(cr hostedCreds, id, handle string, resp *http.Response, raw []byte, err error) (map[string]any, string) {
	who := sanitize(handle)
	show := "; ks advice show " + id
	switch {
	case err != nil:
		return map[string]any{"type": "cancel_unconfirmed", "consultation": id, "error": errText(err)},
			"the cancel of consultation " + id + " was sent and no answer came back (" + errText(err) + "): it may not have arrived, and " + who + " may still answer the question" + show
	case resp.StatusCode/100 == 2:
		var env struct {
			Data consultationDoc `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		state := env.Data.State
		if state == "" {
			state = "cancelled"
		}
		return map[string]any{"type": "cancelled", "consultation": id, "state": state},
			"cancelled the question to " + who + " (consultation " + id + "): the request is closed, a reply to it is refused, and " + who + "'s own work was not touched"
	}
	var he *hostedErr
	refusal := hostedError("POST", "/api/v2/consultations/"+id+"/cancel", resp, raw)
	if errors.As(refusal, &he) && resp.StatusCode == http.StatusConflict && he.Type == "ks_consultation_closed" {
		// closed before the cancel arrived: read how, rather than say
		state := ""
		if c, rerr := fetchConsultation(cr, id); rerr == nil {
			state = c.State
		}
		switch state {
		case "replied":
			return map[string]any{"type": "cancel_too_late", "consultation": id, "state": state},
				"the reply had already arrived, so nothing was cancelled (consultation " + id + ")" + show
		case "":
			return map[string]any{"type": "cancel_too_late", "consultation": id, "state": "unknown"},
				"consultation " + id + " had already closed before the cancel arrived (" + errText(refusal) + "), so nothing was cancelled" + show
		}
		return map[string]any{"type": "cancel_too_late", "consultation": id, "state": state},
			"consultation " + id + " had already closed as " + sanitize(state) + " before the cancel arrived, so nothing was cancelled" + show
	}
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return map[string]any{"type": "cancel_refused", "consultation": id, "error": errText(refusal)},
			"the cancel of consultation " + id + " was refused (" + errText(refusal) + "): the question stands, and " + who + " may still answer it" + show
	}
	return map[string]any{"type": "cancel_unconfirmed", "consultation": id, "error": errText(refusal)},
		"the cancel of consultation " + id + " could not be confirmed (" + errText(refusal) + "): it may not have arrived, and " + who + " may still answer the question" + show
}

// replyLines renders the closed consultation as a person reads it: the
// adviser's own words headed as advice only and unverified, or why there
// are none.
func replyLines(c consultationDoc, adviser string) []string {
	switch c.State {
	case "replied":
		ver := "unverified"
		if c.Response != nil && c.Response.Verification != "" {
			ver = c.Response.Verification
		}
		out := []string{fmt.Sprintf("%s's words, advice only, %s (consultation %s):", sanitize(adviser), sanitize(ver), c.ID)}
		text := ""
		if c.Response != nil {
			text = c.Response.Text
		}
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			out = append(out, "  | "+sanitize(l))
		}
		return append(out, "advice authorises nothing; "+sanitize(adviser)+" did no work in the agent's workspace")
	case "declined":
		if c.AdviserWake != nil && c.AdviserWake.State == "failed" {
			return []string{"declined: the adviser's session could not be resumed, so nothing was asked: " + sanitize(notRecorded(c.DeclinedReason))}
		}
		return []string{"declined: " + sanitize(notRecorded(c.DeclinedReason)) + "; no advice was given"}
	case "cancelled":
		return []string{"cancelled: " + sanitize(notRecorded(c.DeclinedReason)) + "; no advice is delivered for it"}
	case "expired":
		return []string{"unavailable: no reply arrived before the deadline (consultation " + c.ID + "); no advice exists and none is shown"}
	}
	return []string{"consultation " + c.ID + " reads " + sanitize(notRecorded(c.State)) + "; nothing is shown in a reply's place"}
}

// hostedAgentAsk is ks agent ask <name> "@<adviser> <question>".
func hostedAgentAsk(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	text := strings.TrimSpace(inv.Arg(1))
	drawer, err := fetchAdviserDrawer(cr, a.ID)
	if err != nil {
		die(err)
	}
	handles := adviserHandles(drawer.Advisers)
	h, question, problem := parseMention(text, handles, sess)
	if problem != "" {
		fail(&cliError{Code: exitUsage, Kind: "no_such_adviser",
			Message:    problem + ". Nothing was asked, and nothing was sent to " + a.Name + " as an instruction",
			NextAction: "ks adviser list --session " + sess.ShortID})
	}
	// the same question to the same agent is the same request: its id is
	// recorded locally before it is sent, so a repeat answers the same
	// consultation and never asks twice
	path := "/api/v2/agents/" + url.PathEscape(a.ID) + "/consultations"
	rid, err := submissionID(cr, path, text)
	if err != nil {
		die(err)
	}
	asked, replayed, err := askConsultation(cr, a.ID, h.GrantID, question, rid)
	if err != nil {
		die(askRefusal(err, *h, sess))
	}
	// from here an interrupt cancels the question; the channel stays held
	// through the cancel, so a second interrupt is seen and stops at once
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	word := "asked"
	if replayed {
		word = "already asked"
	}
	emitLine(map[string]any{"type": "asked", "consultation": asked.ID, "adviser": h, "agent": a.ID, "replayed": replayed, "request_id": rid},
		fmt.Sprintf("%s %s for %s (consultation %s): advice only, the reply authorises nothing", word, h.Handle, a.Name, asked.ID))
	c, err := waitForReplyOn(cr, asked.ID, asked.Deadline, sig, func(kind, line string) {
		emitLine(map[string]any{"type": kind, "consultation": asked.ID}, line)
	})
	var ce *cliError
	if errors.As(err, &ce) && ce.Kind == "interrupted" {
		cancelAsked(cr, asked.ID, h.Handle, sig)
	}
	if err != nil {
		die(err)
	}
	emitLine(c, strings.Join(replyLines(c, h.Handle), "\n"))
	if c.State == "expired" {
		os.Exit(exitTemporary)
	}
}

// askAdviser is "@handle question" typed into a window: the question goes
// to the connected adviser it names, never to the agent as an instruction,
// and the reply is printed in the window when it arrives.
func (win *liveWindow) askAdviser(cr hostedCreds, line string) {
	if _, held := win.hold(); !held {
		win.refuseAs("ask a connected agent", &cliError{Code: exitConflict, Kind: "not_controller",
			Message: "this window is watching, so it asks nothing", NextAction: win.takeControlLine()})
		return
	}
	drawer, err := fetchAdviserDrawer(cr, win.agent.ID)
	if err != nil {
		win.refuseAs("ask a connected agent", err)
		return
	}
	handles := adviserHandles(drawer.Advisers)
	h, question, problem := parseMention(line, handles, win.sess)
	if problem != "" {
		next := "to send it to " + win.agent.Name + " as an instruction, take out the @"
		win.refuseAs("ask a connected agent", &cliError{Code: exitUsage, Kind: "no_such_adviser",
			Message: problem + "; nothing was sent to " + win.agent.Name, NextAction: next})
		return
	}
	// every typed question is its own request: a fresh id, recorded before
	// it is sent, so a retry of THIS send is the same consultation
	id, err := newSubmissionID()
	if err != nil {
		win.refuseAs("ask a connected agent", err)
		return
	}
	path := "/api/v2/agents/" + url.PathEscape(win.agent.ID) + "/consultations"
	if err := recordOperation(localOp{Key: "kssub_" + id, Submission: id, Method: "POST", Path: path, BodySHA: sha256Hex([]byte(line)), CTL: cr.CTL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		win.refuseAs("ask a connected agent", fmt.Errorf("could not record the question locally before sending it (%v); nothing was sent", err))
		return
	}
	asked, _, err := askConsultation(cr, win.agent.ID, h.GrantID, question, id)
	if err != nil {
		win.refuseAs("ask a connected agent", askRefusal(err, *h, win.sess))
		return
	}
	win.sayAsk("asked", map[string]any{"consultation": asked.ID, "adviser": h.Handle},
		fmt.Sprintf("asked %s for %s (consultation %s): advice only, the reply authorises nothing", h.Handle, win.agent.Name, asked.ID))
	go func() {
		c, err := waitForReply(cr, asked.ID, asked.Deadline, func(kind, l string) {
			win.sayAsk(kind, map[string]any{"consultation": asked.ID}, l)
		})
		if err != nil {
			win.refuseAs("read the adviser's answer", err)
			return
		}
		win.sayAsk("consultation_closed", map[string]any{"consultation": c}, strings.Join(replyLines(c, h.Handle), "\n"))
	}()
}

// sayAsk prints one line of a consultation's progress in the window.
func (win *liveWindow) sayAsk(kind string, extra map[string]any, human string) {
	data := map[string]any{"type": kind}
	for k, v := range extra {
		data[k] = v
	}
	if win.app {
		human = notice(iconState, toneNeutral, human, appTerminalWidth())
	}
	emitLine(data, human)
}
