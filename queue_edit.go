// queue_edit.go: an agent's pending work, and moving one queued instruction
// (KS-045).
//
//	ks agent queue list <name>   the pending work in dispatch order, the
//	                             instruction executing now shown apart, and
//	                             the queue revision an edit must name
//	ks task move <task>          move a QUEUED instruction before or after
//	                             another pending one, against that revision
//
// A move is bound to a view of the queue. With --queue-revision it is bound
// to the view the person actually read (the one ks agent queue list
// printed); without it, the view is read now and displayed before the move
// is sent. Either way a queue that changed in between is refused by the
// service as ks_queue_revision_conflict: nothing moves, the refreshed
// positions are printed, and nothing is retried -- the person decides again
// against what the queue now is.
//
// The instruction's first line is shown only when the service returns it
// (an operator may read instructions; a reader sees the row without it).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type pendingItem struct {
	TaskID        string `json:"task_id"`
	Position      int    `json:"position"`
	QueueSeq      int64  `json:"queue_seq"`
	State         string `json:"state"`
	HeldReason    string `json:"held_reason,omitempty"`
	Summary       string `json:"summary,omitempty"`
	SummaryShown  bool   `json:"summary_shown"`
	SubmitterType string `json:"submitter_type"`
	SubmitterID   string `json:"submitter_id"`
	Origin        string `json:"origin"`
	CreatedAt     string `json:"created_at"`
	AgeSeconds    int64  `json:"age_seconds"`
	Movable       bool   `json:"movable"`
}

type pendingView struct {
	AgentID       string        `json:"agent_id"`
	QueueRevision string        `json:"queue_revision"`
	Active        *pendingItem  `json:"active"`
	Pending       []pendingItem `json:"pending"`
}

func fetchPending(cr hostedCreds, agentID string) (pendingView, error) {
	var env struct {
		Data pendingView `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(agentID)+"/pending", nil, &env)
	return env.Data, err
}

func pendingLine(it pendingItem) string {
	pos := fmt.Sprintf("%d", it.Position)
	if it.Position == 0 {
		pos = "now"
	}
	state := it.State
	if it.HeldReason != "" {
		state += " (" + sanitize(it.HeldReason) + ")"
	}
	who := it.SubmitterType
	if it.SubmitterID != "" {
		who += " " + it.SubmitterID
	}
	line := fmt.Sprintf("  %-4s %-24s %-22s %-24s %s old", pos, it.TaskID, clip(state, 22), clip(sanitize(who), 24), (time.Duration(it.AgeSeconds) * time.Second).String())
	switch {
	case !it.SummaryShown:
		line += "\n       (first line not shown: reading instructions needs the operator role)"
	case it.Summary != "":
		line += "\n       " + visible(sanitize(it.Summary))
	default:
		line += "\n       (first line unavailable)"
	}
	return line
}

func printPending(v pendingView) { printPendingTo(os.Stdout, v) }

func printPendingTo(w io.Writer, v pendingView) {
	if v.Active != nil {
		fmt.Fprintln(w, "executing now (never moved or edited):")
		fmt.Fprintln(w, pendingLine(*v.Active))
	} else {
		fmt.Fprintln(w, "executing now: nothing")
	}
	if len(v.Pending) == 0 {
		fmt.Fprintln(w, "pending: nothing")
	} else {
		fmt.Fprintf(w, "pending, in dispatch order (%d):\n", len(v.Pending))
		for _, it := range v.Pending {
			fmt.Fprintln(w, pendingLine(it))
		}
	}
	fmt.Fprintf(w, "queue revision %s\n", v.QueueRevision)
}

func hostedAgentQueueList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	v, err := fetchPending(cr, a.ID)
	if err != nil {
		die(err)
	}
	emit(v, func() {
		fmt.Printf("agent %s (%s) in session %s\n", a.Name, a.ID, sess.ShortID)
		printPending(v)
		if len(v.Pending) > 1 {
			fmt.Printf("move one: ks task move <task> --before <task> --session %s --queue-revision %s\n", sess.ShortID, v.QueueRevision)
		}
	})
}

// queueConflict carries the refreshed view a conflict answers with.
type queueConflict struct {
	QueueRevision string        `json:"queue_revision"`
	Pending       []pendingItem `json:"pending"`
}

func (q queueConflict) detailLines() []string {
	lines := []string{"the queue as it is now:"}
	for _, it := range q.Pending {
		lines = append(lines, pendingLine(it))
	}
	return append(lines, "queue revision "+q.QueueRevision)
}

func hostedTaskMove(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	before, after := inv.Str("before"), inv.Str("after")
	if (before == "") == (after == "") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name exactly one of --before or --after; nothing was moved"})
	}
	anchor := before + after
	if anchor == id {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "an instruction is not moved relative to itself; nothing was moved"})
	}
	sess := agentSession(cr, inv)
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	rev, bound := inv.Str("queue-revision"), "the queue revision you gave"
	if rev == "" {
		v, err := fetchPending(cr, a.ID)
		if err != nil {
			die(err)
		}
		rev, bound = v.QueueRevision, "the queue as read just now"
		printPendingTo(os.Stderr, v)
	}
	placement := "before"
	if after != "" {
		placement = "after"
	}
	// what is about to change, shown first (stderr, so --json stays one document)
	fmt.Fprintf(os.Stderr, "move %s %s %s in agent %s's queue (session %s), against %s (%s)\n", id, placement, anchor, a.Name, sess.ShortID, bound, rev)
	body := map[string]any{"expected_queue_revision": rev}
	if before != "" {
		body["before_task_id"] = before
	} else {
		body["after_task_id"] = after
	}
	raw, _ := json.Marshal(body)
	path := "/api/v2/tasks/" + url.PathEscape(id) + "/move"
	resp, rb, err := doBounded(cr, "POST", path, nil, raw)
	if err != nil {
		die(err)
	}
	if resp.StatusCode/100 != 2 {
		herr := hostedError("POST", path, resp, rb)
		var he *hostedErr
		if errors.As(herr, &he) && he.Type == "ks_queue_revision_conflict" && resp.StatusCode == http.StatusConflict {
			var e struct {
				Error queueConflict `json:"error"`
			}
			_ = json.Unmarshal(rb, &e)
			ce := classify(herr)
			ce.Message = "the queue changed since it was read, so nothing was moved and nothing is retried: " + ce.Message
			ce.Detail = e.Error
			ce.NextAction = fmt.Sprintf("decide again against the queue as it is: ks task move %s --%s %s --session %s --queue-revision %s", id, placement, anchor, sess.ShortID, e.Error.QueueRevision)
			die(ce)
		}
		if errors.As(herr, &he) && he.Type == "ks_task_not_queued" {
			ce := classify(herr)
			ce.NextAction = "ks agent queue list " + a.Name + " --session " + sess.ShortID
			die(ce)
		}
		die(herr)
	}
	var env struct {
		Data struct {
			TaskID        string        `json:"task_id"`
			From          int           `json:"from_position"`
			To            int           `json:"to_position"`
			QueueRevision string        `json:"queue_revision"`
			Pending       []pendingItem `json:"pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		die(fmt.Errorf("the move was answered with a result this client could not read; read the queue: ks agent queue list %s --session %s", a.Name, sess.ShortID))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("moved %s from position %d to %d (queue revision now %s)\n", d.TaskID, d.From, d.To, d.QueueRevision)
		for _, it := range d.Pending {
			fmt.Println(pendingLine(it))
		}
	})
}

// ---- replacing one pending instruction ------------------------------------------

// taskMaxText is C04's 64 KiB bound on one instruction.
const taskMaxText = 64 << 10

// readQueueRevision answers the revision an edit is bound to: the one the
// person gives, or the view read now, which is then displayed first.
func readQueueRevision(cr hostedCreds, inv *Invocation, agentID string) (rev, bound string) {
	if r := inv.Str("queue-revision"); r != "" {
		return r, "the queue revision you gave"
	}
	v, err := fetchPending(cr, agentID)
	if err != nil {
		die(err)
	}
	printPendingTo(os.Stderr, v)
	return v.QueueRevision, "the queue as read just now"
}

// queueConflictFrom reads the refreshed positions a queue conflict carries.
func queueConflictFrom(err error) (queueConflict, bool) {
	var he *hostedErr
	if !errors.As(err, &he) || he.Type != "ks_queue_revision_conflict" {
		return queueConflict{}, false
	}
	var e struct {
		Error queueConflict `json:"error"`
	}
	_ = json.Unmarshal(he.Raw, &e)
	return e.Error, true
}

func hostedTaskReplace(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	text := inv.Str("text")
	if strings.TrimSpace(text) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--text is the replacement instruction and it is empty; nothing was replaced"})
	}
	if len(text) > taskMaxText {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "an instruction is at most 64 KiB; nothing was replaced"})
	}
	sess := agentSession(cr, inv)
	t, err := fetchTask(cr, id)
	if err != nil {
		die(err)
	}
	a, err := resolveAgent(cr, sess, t.AgentID)
	if err != nil {
		die(err)
	}
	rev, bound := readQueueRevision(cr, inv, a.ID)
	path := "/api/v2/tasks/" + url.PathEscape(id) + "/replace"
	// the submission id is recorded before anything is sent, and a repeat of
	// this same replacement reuses it: the service then answers the
	// replacement it already made instead of making a second one
	sid, err := submissionID(cr, path, text)
	if err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "replace %s (%s) in agent %s's queue (session %s), against %s (%s): it is cancelled with its content kept, and the new instruction takes its place\n",
		id, figure(t.State), a.Name, sess.ShortID, bound, rev)
	body := map[string]any{"submission_id": sid, "text": text, "expected_queue_revision": rev}
	if r := strings.TrimSpace(inv.Str("reason")); r != "" {
		body["reason"] = r
	}
	var env struct {
		Data struct {
			Superseded    *taskRow      `json:"superseded"`
			Replacement   *taskRow      `json:"replacement"`
			Position      int           `json:"position"`
			QueueRevision string        `json:"queue_revision"`
			Pending       []pendingItem `json:"pending"`
			Replayed      bool          `json:"replayed"`
			Note          string        `json:"note"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", path, body, &env); err != nil {
		if q, ok := queueConflictFrom(err); ok {
			ce := classify(err)
			ce.Message = "the queue changed since it was read, so nothing was cancelled or submitted and nothing is retried: " + ce.Message
			ce.Detail = q
			ce.NextAction = fmt.Sprintf("decide again against the queue as it is: ks task replace %s --text ... --session %s --queue-revision %s", id, sess.ShortID, q.QueueRevision)
			die(ce)
		}
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_task_not_queued" {
			ce := classify(err)
			ce.NextAction = fmt.Sprintf("ks task cancel %s --session %s (an instruction in flight is cancelled, never edited)", id, sess.ShortID)
			die(ce)
		}
		die(err)
	}
	d := env.Data
	emit(d, func() {
		newID := "(not returned)"
		if d.Replacement != nil {
			newID = d.Replacement.ID
		}
		if d.Replayed {
			fmt.Printf("already replaced: %s was replaced by %s earlier; nothing was recorded again\n", id, newID)
		} else {
			fmt.Printf("replaced %s with %s at position %d (queue revision now %s); %s is cancelled and its content kept\n", id, newID, d.Position, d.QueueRevision, id)
		}
		for _, it := range d.Pending {
			fmt.Println(pendingLine(it))
		}
		if d.Note != "" {
			fmt.Println("note: " + sanitize(d.Note))
		}
	})
}

// ---- cancelling pending work in bulk, behind a preview -----------------------------

type bulkPreview struct {
	AgentID       string         `json:"agent_id"`
	QueueRevision string         `json:"queue_revision"`
	States        []string       `json:"states"`
	Affected      []pendingItem  `json:"affected"`
	ByState       map[string]int `json:"by_state"`
	Excluded      *pendingItem   `json:"excluded_active,omitempty"`
	Note          string         `json:"note"`
}

func (p bulkPreview) detailLines() []string {
	lines := []string{fmt.Sprintf("would cancel %d pending instruction(s) (%s):", len(p.Affected), strings.Join(p.States, ", "))}
	for _, it := range p.Affected {
		lines = append(lines, pendingLine(it))
	}
	if p.Excluded != nil {
		lines = append(lines, "never included, executing now: "+p.Excluded.TaskID+" (to stop it: ks task cancel)")
	}
	return append(lines, "queue revision "+p.QueueRevision)
}

func bulkStatesArg(inv *Invocation) []string {
	s := strings.TrimSpace(inv.Str("states"))
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "queued" && part != "held" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--states is queued, held or queued,held; %q is neither. Nothing was cancelled", part)})
		}
		out = append(out, part)
	}
	return out
}

func hostedAgentQueueCancel(cr hostedCreds, inv *Invocation) {
	preview, confirm := inv.Bool("preview"), strings.TrimSpace(inv.Str("confirm"))
	if preview == (confirm != "") {
		fail(&cliError{Code: exitUsage, Kind: "usage",
			Message:    "a bulk cancellation is previewed first (--preview) and then confirmed by naming that preview's queue revision (--confirm QREV): one of the two. Nothing was cancelled",
			NextAction: "ks agent queue cancel <name> --preview"})
	}
	states := bulkStatesArg(inv)
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if preview {
		q := url.Values{}
		if len(states) > 0 {
			q.Set("states", strings.Join(states, ","))
		}
		p := "/api/v2/agents/" + url.PathEscape(a.ID) + "/cancel-preview"
		if enc := q.Encode(); enc != "" {
			p += "?" + enc
		}
		var env struct {
			Data bulkPreview `json:"data"`
		}
		if err := hostedCall(cr, "GET", p, nil, &env); err != nil {
			die(err)
		}
		v := env.Data
		emit(v, func() {
			for _, l := range v.detailLines() {
				fmt.Println(l)
			}
			fmt.Println("nothing was cancelled: this is a preview")
			if len(v.Affected) > 0 {
				st := ""
				if len(states) > 0 {
					st = " --states " + strings.Join(states, ",")
				}
				fmt.Printf("confirm exactly this: ks agent queue cancel %s --session %s%s --confirm %s\n", a.Name, sess.ShortID, st, v.QueueRevision)
			}
		})
		return
	}
	body := map[string]any{"expected_queue_revision": confirm}
	if len(states) > 0 {
		body["states"] = states
	}
	if r := strings.TrimSpace(inv.Str("reason")); r != "" {
		body["reason"] = r
	}
	fmt.Fprintf(os.Stderr, "cancel the pending instructions of agent %s (session %s) previewed at %s\n", a.Name, sess.ShortID, confirm)
	var env struct {
		Data struct {
			Cancelled     []string       `json:"cancelled"`
			ByState       map[string]int `json:"by_state"`
			QueueRevision string         `json:"queue_revision"`
			Note          string         `json:"note"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/agents/"+url.PathEscape(a.ID)+"/cancel-pending", body, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_queue_revision_conflict" {
			var e struct {
				Error struct {
					Preview bulkPreview `json:"preview"`
				} `json:"error"`
			}
			_ = json.Unmarshal(he.Raw, &e)
			ce := classify(err)
			ce.Message = "the queue changed since the preview, so nothing was cancelled and nothing is retried: " + ce.Message
			ce.Detail = e.Error.Preview
			ce.NextAction = fmt.Sprintf("if the preview above is what you mean: ks agent queue cancel %s --session %s --confirm %s", a.Name, sess.ShortID, e.Error.Preview.QueueRevision)
			die(ce)
		}
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("cancelled %d pending instruction(s): %s (queue revision now %s); their content is kept, and nothing executing was touched\n",
			len(d.Cancelled), strings.Join(d.Cancelled, ", "), d.QueueRevision)
		if d.Note != "" {
			fmt.Println("note: " + sanitize(d.Note))
		}
	})
}
