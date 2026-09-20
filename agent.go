// agent.go: the agent window on the command line. A session holds one or
// more agents; this group lists them, opens one, reports one without
// following anything, and queues an instruction for one.
//
// Three properties shape every verb here. Opening an agent CREATES
// NOTHING: the control plane answers the same agent for the same name, so
// opening twice is one agent and two windows. Control is a lease, not a
// mode: a window either holds it or is watching, and a window that is
// watching says so rather than pretending it can steer. And detaching is
// local: Ctrl-C closes this window, prints that it did, and leaves the
// agent working — nothing here ever cancels remote work.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------
// what the control plane reports
// ---------------------------------------------------------------------

type agentRow struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	Name          string `json:"name"`
	IsPrimary     bool   `json:"is_primary"`
	Activity      string `json:"activity"`
	ObservedAt    string `json:"observed_at"`
	ActiveTaskID  string `json:"active_task_id"`
	QueueRevision int64  `json:"queue_revision"`
	Revision      int64  `json:"revision"`
	CreatedAt     string `json:"created_at"`
}

// agentLease is the control lease of one window. Its token authorises
// steering, so it is held in memory for the requests that need it and is
// never printed, emitted or written down.
type agentLease struct {
	ID        string `json:"id"`
	Fence     int64  `json:"fence"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Mode      string `json:"mode"`
}

type agentWindow struct {
	Agent       agentRow    `json:"agent"`
	Reconnected bool        `json:"reconnected"`
	Lease       *agentLease `json:"lease"`
	Resume      struct {
		AfterSeq int64  `json:"after_seq"`
		Cursor   string `json:"cursor"`
	} `json:"resume"`
	QueueDepth int64 `json:"queue_depth"`
}

type taskRow struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	SubmissionID string `json:"submission_id"`
	State        string `json:"state"`
	QueueSeq     int64  `json:"queue_seq"`
	Origin       string `json:"origin"`
	ContentRef   string `json:"content_ref"`
	CreatedAt    string `json:"created_at"`
}

// submittedTask is a task as the submission route answers it: the task,
// plus whether this submission id had already been accepted, in which case
// the original task comes back and nothing was queued twice.
type submittedTask struct {
	taskRow
	Replayed bool `json:"replayed"`
}

type approvalRow struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	Kind            string          `json:"kind"`
	Summary         string          `json:"summary"`
	ArgumentsJSON   json.RawMessage `json:"arguments_json,omitempty"`
	ExpiresAt       string          `json:"expires_at"`
	State           string          `json:"state"`
	Revision        int64           `json:"revision"`
	ExactActionHash string          `json:"exact_action_hash"`
}

// journalEvent is one row of the session's journal, as the stream serves it.
type journalEvent struct {
	EventID     string          `json:"event_id"`
	StreamSeq   int64           `json:"stream_seq"`
	SubjectType string          `json:"subject_type"`
	SubjectID   string          `json:"subject_id"`
	ObservedAt  string          `json:"observed_at"`
	Payload     json.RawMessage `json:"payload"`
}

// ---------------------------------------------------------------------
// resolution: which session, which agent
// ---------------------------------------------------------------------

// agentSessionID is the id the agent routes are keyed by: the session's
// workspace record where the inventory reports one, and the session's own
// id otherwise, so an account whose inventory predates the record still
// reaches its agents.
func agentSessionID(r inventoryRow) string {
	if r.RecordID != "" {
		return r.RecordID
	}
	return r.ID
}

// agentSession answers the one session --session names, through the same
// resolution the session verbs use: a full id, or a prefix unique among
// the account's sessions.
func agentSession(cr hostedCreds, inv *Invocation) inventoryRow {
	if inv.Str("session") == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--session names the session the agent lives in", NextAction: "ks session list"})
	}
	r, err := resolveSession(cr, inv.Str("session"))
	if err != nil {
		die(err)
	}
	return r
}

func fetchAgents(cr hostedCreds, sessionID string) ([]agentRow, error) {
	var env struct {
		Data struct {
			Items []agentRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"session_id": {sessionID}}
	if err := hostedCall(cr, "GET", "/api/v2/agents?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// resolveAgent answers the one agent an argument names inside a session:
// its id, or its name when exactly one agent carries it. Two agents of one
// name is an error that names them; nothing is chosen for the caller.
func resolveAgent(cr hostedCreds, sess inventoryRow, arg string) (agentRow, error) {
	if arg == "" {
		return agentRow{}, &cliError{Code: exitUsage, Kind: "usage", Message: "an agent name is required", NextAction: "ks agent list --session " + sess.ShortID}
	}
	agents, err := fetchAgents(cr, agentSessionID(sess))
	if err != nil {
		return agentRow{}, err
	}
	var byName []agentRow
	for _, a := range agents {
		if a.ID == arg {
			return a, nil
		}
		if a.Name != "" && a.Name == arg {
			byName = append(byName, a)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return agentRow{}, &cliError{Code: exitUsage, Kind: "not_found",
			Message:    fmt.Sprintf("session %s has no agent named %q", sess.ShortID, arg),
			NextAction: "ks agent list --session " + sess.ShortID}
	}
	sort.Slice(byName, func(i, j int) bool { return byName[i].ID < byName[j].ID })
	var ids []string
	for _, a := range byName {
		ids = append(ids, a.ID)
	}
	return agentRow{}, &cliError{Code: exitUsage, Kind: "ambiguous",
		Message:    fmt.Sprintf("%d agents in session %s are named %q: %s; give the id", len(byName), sess.ShortID, arg, strings.Join(ids, ", ")),
		NextAction: "ks agent list --session " + sess.ShortID}
}

func fetchTasks(cr hostedCreds, agentID string) ([]taskRow, error) {
	var env struct {
		Data struct {
			Items []taskRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"agent_id": {agentID}}
	if err := hostedCall(cr, "GET", "/api/v2/tasks?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func fetchPendingApprovals(cr hostedCreds, sessionID string) ([]approvalRow, error) {
	var env struct {
		Data struct {
			Items []approvalRow `json:"items"`
		} `json:"data"`
	}
	q := url.Values{"session_id": {sessionID}, "state": {"pending"}}
	if err := hostedCall(cr, "GET", "/api/v2/approvals?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// finishedTaskStates are the states the service reports for work that is
// over. A state this client does not know counts as waiting, because
// reporting a queue as shorter than it is would be the wrong mistake.
var finishedTaskStates = map[string]bool{
	"done": true, "completed": true, "succeeded": true, "failed": true,
	"cancelled": true, "canceled": true, "rejected": true, "expired": true,
}

// queueState reads the task list into the two facts a person asks for:
// what the agent is working on, and how much is waiting behind it.
func queueState(a agentRow, tasks []taskRow) (waiting int, current *taskRow) {
	for i := range tasks {
		if a.ActiveTaskID != "" && tasks[i].ID == a.ActiveTaskID {
			current = &tasks[i]
			continue
		}
		if !finishedTaskStates[strings.ToLower(tasks[i].State)] {
			waiting++
		}
	}
	return waiting, current
}

// ---------------------------------------------------------------------
// ks agent list
// ---------------------------------------------------------------------

func agentRole(a agentRow) string {
	if a.IsPrimary {
		return "primary"
	}
	return "-"
}

func agentLine(a agentRow) string {
	task := a.ActiveTaskID
	if task == "" {
		task = "none"
	}
	return fmt.Sprintf("%-16s %-16s %-11s %-8s %s", clip(a.Name, 16), clip(a.ID, 16), clip(figure(a.Activity), 11), agentRole(a), task)
}

func hostedAgentList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	agents, err := fetchAgents(cr, agentSessionID(sess))
	if err != nil {
		die(err)
	}
	emit(map[string]any{"session": sess.ID, "agents": agents, "count": len(agents)}, func() {
		if len(agents) == 0 {
			fmt.Printf("No agents in session %s.\n", sess.ShortID)
			return
		}
		fmt.Printf("%-16s %-16s %-11s %-8s %s\n", "AGENT", "ID", "ACTIVITY", "ROLE", "CURRENT TASK")
		for _, a := range agents {
			fmt.Println(agentLine(a))
		}
		fmt.Printf("%d agent(s); open one: ks agent open <name> --session %s\n", len(agents), sess.ShortID)
	})
}

// ---------------------------------------------------------------------
// ks agent status
// ---------------------------------------------------------------------

func hostedAgentStatus(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	tasks, err := fetchTasks(cr, a.ID)
	if err != nil {
		die(err)
	}
	waiting, current := queueState(a, tasks)
	emit(map[string]any{"session": sess.ID, "agent": a, "queue_depth": waiting, "current_task": current, "tasks": len(tasks)}, func() {
		fmt.Printf("agent %s (%s) in session %s\n", a.Name, a.ID, sess.ShortID)
		fmt.Printf("  activity       %s\n", figure(a.Activity))
		fmt.Printf("  role           %s\n", agentRole(a))
		fmt.Printf("  queue          %d waiting\n", waiting)
		if current != nil {
			fmt.Printf("  current task   %s (%s) at queue position %d\n", current.ID, figure(current.State), current.QueueSeq)
		} else {
			fmt.Printf("  current task   none\n")
		}
		fmt.Printf("  revision       %d (queue %d)\n", a.Revision, a.QueueRevision)
		fmt.Printf("  observed       %s\n", figure(a.ObservedAt))
	})
}

// ---------------------------------------------------------------------
// ks agent open
// ---------------------------------------------------------------------

// openAgent asks for the window. The route never creates an agent, so a
// second open of the same name is the same agent; what it settles is
// whether this window holds the control lease, and where in the journal
// the window should resume from.
func openAgent(cr hostedCreds, sess inventoryRow, a agentRow, take bool) (*agentWindow, error) {
	var env struct {
		Data agentWindow `json:"data"`
	}
	err := hostedCall(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/open", map[string]any{"take_control": take}, &env)
	if err == nil {
		return &env.Data, nil
	}
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_controller_held":
			// another window is steering this agent. Taking control from it
			// is a decision, never a retry the client makes for you.
			return nil, &cliError{Code: exitConflict, Kind: "controller_held",
				Message:    fmt.Sprintf("another window holds control of agent %s: %s", a.Name, sanitize(he.Message)),
				NextAction: fmt.Sprintf("ks agent open %s --session %s --take-control", a.Name, sess.ShortID)}
		case "ks_not_found":
			return nil, &cliError{Code: exitUsage, Kind: "not_found",
				Message:    fmt.Sprintf("agent %s is no longer in session %s", a.Name, sess.ShortID),
				NextAction: "ks agent list --session " + sess.ShortID}
		}
	}
	return nil, err
}

// controlLine says, in one phrase, whether this window may steer.
func controlLine(w *agentWindow) string {
	if w.Lease == nil {
		return "watching (another window holds control)"
	}
	mode := w.Lease.Mode
	if mode == "" {
		mode = "control"
	}
	return fmt.Sprintf("you hold control (lease %s, %s)", w.Lease.ID, mode)
}

// controlFacts is the lease as a script may read it: everything except the
// token, which authorises steering and therefore never leaves the process.
func controlFacts(w *agentWindow) map[string]any {
	if w.Lease == nil {
		return map[string]any{"held": false}
	}
	return map[string]any{"held": true, "lease_id": w.Lease.ID, "fence": w.Lease.Fence, "expires_at": w.Lease.ExpiresAt, "mode": w.Lease.Mode}
}

func approvalHuman(ap approvalRow) string {
	return sanitize(fmt.Sprintf("!! permission request %s: %s — %s (expires %s); the agent waits until it is decided in the console",
		ap.ID, figure(ap.Kind), figure(ap.Summary), figure(ap.ExpiresAt)))
}

func hostedAgentOpen(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	w, err := openAgent(cr, sess, a, inv.Bool("take-control"))
	if err != nil {
		die(err)
	}
	joined := "opened"
	if w.Reconnected {
		joined = "rejoined"
	}
	// the header is a fact about the window, not a result: stderr, so a
	// piped stdout carries the agent's events and nothing else
	progress("agent %s (%s) in session %s · %s · %s · %s · %d queued",
		a.Name, a.ID, sess.ShortID, figure(w.Agent.Activity), joined, controlLine(w), w.QueueDepth)

	pending, perr := fetchPendingApprovals(cr, agentSessionID(sess))
	if perr != nil {
		progress("the pending permission requests could not be read: %s", sanitize(perr.Error()))
	}

	if inv.Bool("no-follow") {
		emit(map[string]any{
			"session": sess.ID, "agent": w.Agent, "reconnected": w.Reconnected,
			"control": controlFacts(w), "queue_depth": w.QueueDepth,
			"resume":            map[string]any{"after_seq": w.Resume.AfterSeq, "cursor": w.Resume.Cursor},
			"approvals_pending": pending,
		}, func() {
			fmt.Printf("agent %s (%s) in session %s\n", a.Name, a.ID, sess.ShortID)
			fmt.Printf("  activity       %s\n", figure(w.Agent.Activity))
			fmt.Printf("  control        %s\n", controlLine(w))
			fmt.Printf("  queue          %d queued\n", w.QueueDepth)
			fmt.Printf("  events from    %d\n", w.Resume.AfterSeq)
			for _, ap := range pending {
				fmt.Println(approvalHuman(ap))
			}
			fmt.Printf("follow it: ks agent open %s --session %s\n", a.Name, sess.ShortID)
		})
		return
	}
	for _, ap := range pending {
		emitLine(map[string]any{"type": "approval_pending", "approval": ap}, approvalHuman(ap))
	}
	followAgent(cr, sess, w)
}

// followAgent renders the session journal from where the window resumes,
// one line per event, until the stream ends or the person detaches.
//
// Ctrl-C (and SIGTERM) detach: they close THIS window. The agent keeps
// working, nothing is cancelled, and the exit is a success, because
// detaching is what was asked for. The signal is watched on its own
// goroutine because the reader blocks on the stream rather than polling,
// which is the one difference from the wait loop in operations.go.
func followAgent(cr hostedCreds, sess inventoryRow, w *agentWindow) {
	// the window reads no keystrokes, so it puts the terminal into no mode
	// of its own and has nothing to undo; the hook stays so an input path
	// cannot be added without one.
	restore := func() {}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		restore()
		fmt.Fprintln(os.Stderr, "[detached; the agent keeps working]")
		os.Exit(exitOK)
	}()
	err := streamEvents(cr, agentSessionID(sess), w.Resume.AfterSeq, func(kind string, data []byte) bool {
		switch kind {
		case "hello":
			var hello struct {
				Epoch any `json:"epoch"`
			}
			_ = json.Unmarshal(data, &hello)
			progress("following from event %d (epoch %v); Ctrl-C detaches and the agent keeps working", w.Resume.AfterSeq, figure(hello.Epoch))
		case "event":
			var e journalEvent
			if json.Unmarshal(data, &e) != nil {
				return false
			}
			emitLine(e, agentEventLine(e))
		case "end":
			progress("the stream ended; the agent keeps working")
			return true
		}
		return false
	})
	restore()
	if err != nil {
		die(err)
	}
}

// agentEventLine renders one journal row for a person: its position, the
// time of day, what it is about, and the payload the service sent,
// compact. A permission request is marked so it cannot be skimmed past.
func agentEventLine(e journalEvent) string {
	line := fmt.Sprintf("%-6d %s %s %s", e.StreamSeq, clock(e.ObservedAt), figure(e.SubjectType), figure(e.SubjectID))
	if d := compactPayload(e.Payload); d != "" {
		line += " " + d
	}
	if isApprovalEvent(e) {
		line = "!! " + line + " — the agent waits until it is decided in the console"
	}
	return sanitize(line)
}

func isApprovalEvent(e journalEvent) bool {
	s := strings.ToLower(e.SubjectType)
	return strings.Contains(s, "approval") || strings.Contains(s, "permission")
}

// clock renders an event's time for a live window: the time of day when
// the service sent its usual timestamp, the value itself otherwise.
func clock(ts string) string {
	if len(ts) >= 19 && ts[10] == 'T' {
		return ts[11:19]
	}
	if ts == "" {
		return "unavailable"
	}
	return ts
}

func compactPayload(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	s := b.String()
	if s == "{}" || s == "null" {
		return ""
	}
	return s
}

// streamEvents reads the session's event stream and hands each frame to
// the caller, which reports whether the stream is finished. The body is
// not time-bounded: a live window is as long as the person leaves it open.
func streamEvents(cr hostedCreds, sessionID string, afterSeq int64, onFrame func(kind string, data []byte) bool) error {
	q := url.Values{"after_seq": {strconv.FormatInt(afterSeq, 10)}}
	path := "/api/v2/sessions/" + url.PathEscape(sessionID) + "/events/stream?" + q.Encode()
	resp, err := hostedDo(cr, "GET", path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return hostedError("GET", path, resp, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	kind := "message"
	var data []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 && onFrame(kind, []byte(strings.Join(data, "\n"))) {
				return nil
			}
			kind, data = "message", nil
		case strings.HasPrefix(line, ":"): // a comment, which keep-alives are
		case strings.HasPrefix(line, "event:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// a stream that ended on a frame the blank line never followed: the
	// frame was still sent, so it is still rendered
	if len(data) > 0 {
		onFrame(kind, []byte(strings.Join(data, "\n")))
	}
	return nil
}

// ---------------------------------------------------------------------
// ks agent tell
// ---------------------------------------------------------------------

// newSubmissionID mints the id one instruction will carry for as long as
// it takes to land: 16 random bytes, like the operation keys.
func newSubmissionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// submissionID answers the id this instruction submits under. The id is
// written to the local operation journal BEFORE anything is sent, and an
// instruction retried against the same agent on the same control plane
// reuses the recorded id: the control plane then answers the retry with
// the task it already queued, rather than queueing a second one. This is
// the same discipline as the idempotency keys in operations.go, one level
// up: the key protects the HTTP request, the submission id protects the
// queue.
func submissionID(cr hostedCreds, path, text string) (string, error) {
	sum := sha256Hex([]byte(text))
	for _, o := range recordedOperations() {
		if o.Submission != "" && o.Method == "POST" && o.Path == path && o.CTL == cr.CTL && o.BodySHA == sum {
			return o.Submission, nil
		}
	}
	id, err := newSubmissionID()
	if err != nil {
		return "", err
	}
	op := localOp{Key: "kssub_" + id, Submission: id, Method: "POST", Path: path, BodySHA: sum, CTL: cr.CTL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := recordOperation(op); err != nil {
		return "", fmt.Errorf("could not record the submission locally before sending it (%v); nothing was sent", err)
	}
	return id, nil
}

func hostedAgentTell(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	text := inv.Arg(1)
	if strings.TrimSpace(text) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the instruction is empty",
			NextAction: fmt.Sprintf("ks agent tell %s \"run the tests\" --session %s", a.Name, sess.ShortID)})
	}
	path := "/api/v2/agents/" + url.PathEscape(a.ID) + "/tasks"
	sid, err := submissionID(cr, path, text)
	if err != nil {
		die(err)
	}
	// this process holds no control lease (a window's lease lives in the
	// window), so the instruction is submitted standalone: no origin, no
	// fence, no lease fields at all.
	var env struct {
		Data submittedTask `json:"data"`
	}
	if err := hostedMutate(cr, "POST", path, map[string]any{"submission_id": sid, "text": text}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) {
			switch he.Type {
			case "ks_controller_stale":
				die(&cliError{Code: exitConflict, Kind: "controller_stale",
					Message:    fmt.Sprintf("the control of agent %s moved to another window: %s", a.Name, sanitize(he.Message)),
					NextAction: fmt.Sprintf("ks agent open %s --session %s --take-control", a.Name, sess.ShortID)})
			case "ks_queue_full":
				die(&cliError{Code: exitConflict, Kind: "queue_full",
					Message:    fmt.Sprintf("the queue of agent %s is full: %s", a.Name, sanitize(he.Message)),
					NextAction: fmt.Sprintf("ks agent status %s --session %s", a.Name, sess.ShortID)})
			}
		}
		die(err)
	}
	t := env.Data
	emit(map[string]any{"session": sess.ID, "agent": a.ID, "submission_id": sid, "replayed": t.Replayed, "task": t.taskRow}, func() {
		if t.Replayed {
			fmt.Printf("already queued: task %s for agent %s at queue position %d (%s); the same instruction was submitted once\n", t.ID, a.Name, t.QueueSeq, figure(t.State))
			return
		}
		fmt.Printf("queued: task %s for agent %s at queue position %d (%s)\n", t.ID, a.Name, t.QueueSeq, figure(t.State))
	})
}
