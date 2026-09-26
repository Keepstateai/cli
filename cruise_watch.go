// cruise_watch.go: ks cruise status --watch (KS-076). The control plane
// answers a job's live status from its ledger alone
// (GET /api/jobs/{id}/status): the state in plain words, the attempt in
// flight, a verifier result still pending, the last save point, spend with
// model, validation, runtime and storage kept apart, savings (never shown,
// with the reason), the next permitted actions, and the event cursor with
// the pacing to follow it.
//
// The watch follows GET /api/jobs/{id}/events?since=<cursor> at the pace the
// service names (poll_after_s), prints each new event once, and re-reads the
// status when something happened. A failed read is retried with a doubling
// backoff up to max_backoff_s, from the same cursor, so a reconnect neither
// repeats nor skips an event. poll_after_s 0 means nothing further will
// happen, and the watch ends. Ctrl-C ends ONLY the watch: nothing is sent,
// and the job keeps running.
//
// Money: a figure is printed only when the service gives one. "unavailable"
// and "partial" never read as $0; "nothing_run" is the one zero, and says
// that nothing has run.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type watchMoney struct {
	MicroUSD *int64 `json:"microusd"`
	Standing string `json:"standing"`
	Note     string `json:"note"`
}

type jobLiveStatus struct {
	JobID          string  `json:"job_id"`
	State          string  `json:"state"`
	StateLabel     string  `json:"state_label"`
	Verdict        *string `json:"verdict"`
	Goal           string  `json:"goal"`
	LadderPos      int64   `json:"ladder_pos"`
	Rungs          int     `json:"rungs"`
	CurrentAttempt *struct {
		ID        string `json:"id"`
		Rung      int64  `json:"rung"`
		Model     string `json:"model"`
		State     string `json:"state"`
		StartedAt string `json:"started_at"`
	} `json:"current_attempt"`
	Attempts      int `json:"attempts"`
	PendingVerify *struct {
		AttemptID string `json:"attempt_id"`
		SinceSeq  int64  `json:"since_seq"`
		SinceTS   string `json:"since"`
	} `json:"pending_verification"`
	LastSavePoint *struct {
		CheckpointID string `json:"checkpoint_id"`
		AttemptID    string `json:"attempt_id"`
		Seq          int64  `json:"seq"`
		TS           string `json:"at"`
	} `json:"last_save_point"`
	Spend struct {
		Model       watchMoney `json:"model"`
		Validation  watchMoney `json:"validation"`
		Runtime     watchMoney `json:"runtime"`
		Storage     watchMoney `json:"storage"`
		CeilingUSD  int64      `json:"spend_ceiling_microusd"`
		Reconciled  int64      `json:"attempts_reconciled"`
		Outstanding int64      `json:"attempts_unreconciled"`
	} `json:"spend"`
	Savings struct {
		Shown  bool   `json:"shown"`
		Reason string `json:"reason"`
	} `json:"savings"`
	NextActions []struct {
		Action  string `json:"action"`
		Label   string `json:"label"`
		Request string `json:"request"`
	} `json:"next_actions"`
	Follow struct {
		Events      string `json:"events"`
		Cursor      int64  `json:"cursor"`
		PollAfterS  int    `json:"poll_after_s"`
		MaxBackoffS int    `json:"max_backoff_s"`
		Note        string `json:"note"`
	} `json:"follow"`
	// KS-077: present only in review, and only once cancelled
	Review  *liveReview  `json:"review"`
	Cleanup *liveCleanup `json:"cleanup"`
}

type jobEventRow struct {
	Seq       int64           `json:"seq"`
	TS        string          `json:"ts"`
	Type      string          `json:"type"`
	AttemptID *string         `json:"attempt_id"`
	Detail    json.RawMessage `json:"detail"`
}

func fetchJobStatus(c hostedCreds, id string) (jobLiveStatus, error) {
	var st jobLiveStatus
	err := hostedCall(c, "GET", "/api/jobs/"+id+"/status", nil, &st)
	return st, err
}

func fetchJobEventsSince(c hostedCreds, id string, since int64) ([]jobEventRow, error) {
	var evs []jobEventRow
	err := hostedCall(c, "GET", "/api/jobs/"+id+"/events?since="+strconv.FormatInt(since, 10), nil, &evs)
	return evs, err
}

// moneyText renders one spend line; a figure only where the service gave one.
func moneyText(m watchMoney) string {
	switch {
	case m.Standing == "reconciled" && m.MicroUSD != nil:
		return dollars(*m.MicroUSD) + " (reconciled)"
	case m.Standing == "nothing_run":
		return "$0.00 (nothing has run)"
	case m.Standing == "partial":
		return "unavailable (partial: some attempts are not reconciled, and a partial sum is not shown) — " + sanitize(m.Note)
	}
	return "unavailable — " + sanitize(notRecorded(m.Note))
}

// liveStatusLines is the status block.
func liveStatusLines(s jobLiveStatus) []string {
	label := s.StateLabel
	if label == "" {
		label = "Status unavailable"
	}
	out := []string{fmt.Sprintf("job %s · %s (%s)", s.JobID, sanitize(label), sanitize(figure(s.State)))}
	if s.Verdict != nil && *s.Verdict != "" {
		out = append(out, "  verdict        "+sanitize(*s.Verdict))
	}
	out = append(out, fmt.Sprintf("  rung           %d of %d; %d attempt(s)", s.LadderPos+1, s.Rungs, s.Attempts))
	if a := s.CurrentAttempt; a != nil {
		out = append(out, fmt.Sprintf("  in flight      attempt %s on rung %d (%s), %s since %s", a.ID, a.Rung+1, sanitize(figure(a.Model)), sanitize(figure(a.State)), figure(a.StartedAt)))
	} else {
		out = append(out, "  in flight      no attempt")
	}
	if p := s.PendingVerify; p != nil {
		out = append(out, fmt.Sprintf("  verifier       result PENDING for attempt %s (checking since %s)", p.AttemptID, figure(p.SinceTS)))
	} else {
		out = append(out, "  verifier       no result pending")
	}
	if sp := s.LastSavePoint; sp != nil {
		out = append(out, fmt.Sprintf("  last save      %s (attempt %s, at %s)", sp.CheckpointID, notRecorded(sp.AttemptID), figure(sp.TS)))
	} else {
		out = append(out, "  last save      none recorded")
	}
	out = append(out,
		"  model spend    "+moneyText(s.Spend.Model),
		"  validation     "+moneyText(s.Spend.Validation),
		"  runtime        "+moneyText(s.Spend.Runtime),
		"  storage        "+moneyText(s.Spend.Storage),
		fmt.Sprintf("  ceiling        %s; attempts reconciled %d, not yet %d", dollars(s.Spend.CeilingUSD), s.Spend.Reconciled, s.Spend.Outstanding))
	if s.Savings.Shown {
		// the service shows none today; if it ever does, this client still
		// does not compute or print a figure of its own
		out = append(out, "  savings        the service reports a figure; this client does not show one")
	} else {
		out = append(out, "  savings        not shown: "+sanitize(notRecorded(s.Savings.Reason)))
	}
	if s.Cleanup != nil {
		out = append(out, cleanupLine(s.Cleanup))
	}
	if s.Review != nil {
		out = append(out, "  review         "+remainingText(s.Review)+"; the attempts, check results and options: ks cruise review "+s.JobID)
	}
	if len(s.NextActions) == 0 {
		out = append(out, "  next           nothing to decide")
	}
	for _, a := range s.NextActions {
		out = append(out, "  next           "+sanitize(a.Label)+": "+cruiseVerbFor(a.Action, s.JobID))
	}
	return out
}

// cruiseVerbFor is this client's command for a next action the service names.
func cruiseVerbFor(action, id string) string {
	switch action {
	case "cancel":
		return "ks cruise cancel " + id
	case "resume":
		return "ks cruise resume " + id
	case "artifact":
		return "ks cruise artifact " + id
	}
	return "(no client command for " + sanitize(action) + ")"
}

func eventLine(e jobEventRow) string {
	att := ""
	if e.AttemptID != nil && *e.AttemptID != "" {
		att = " " + *e.AttemptID
	}
	d := compactPayload(e.Detail)
	if d == "{}" {
		d = ""
	}
	return sanitize(strings.TrimSpace(fmt.Sprintf("%-5d %s %s%s %s", e.Seq, figure(e.TS), figure(e.Type), att, d)))
}

// watchSecond is one of the service's seconds; tests shorten it.
func watchSecond() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("KS_WATCH_SECOND_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Second
}

func printStatus(s jobLiveStatus) {
	emitLine(map[string]any{"kind": "status", "status": s}, strings.Join(liveStatusLines(s), "\n"))
}

// cruiseWatch follows one job until nothing further will happen or Ctrl-C.
func cruiseWatch(c hostedCreds, id string) {
	st, err := fetchJobStatus(c, id)
	if err != nil {
		var he *hostedErr
		if errors.As(err, &he) && statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve a job's live status, so there is nothing to watch; nothing was changed", NextAction: "ks cruise status " + id})
		}
		die(err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	printStatus(st)
	cursor := st.Follow.Cursor
	unit := watchSecond()
	backoff := time.Duration(0)
	for {
		if st.Follow.PollAfterS <= 0 {
			progress("nothing further will happen: %s", sanitize(notRecorded(st.Follow.Note)))
			return
		}
		wait := time.Duration(st.Follow.PollAfterS) * unit
		if backoff > 0 {
			wait = backoff
		}
		select {
		case <-stop:
			progress("stopped watching; nothing was sent, and job %s is unchanged", id)
			return
		case <-time.After(wait):
		}
		evs, err := fetchJobEventsSince(c, id, cursor)
		if err == nil && len(evs) > 0 {
			var fresh jobLiveStatus
			fresh, err = fetchJobStatus(c, id)
			if err == nil {
				for _, e := range evs {
					if e.Seq <= cursor {
						continue // never twice
					}
					emitLine(map[string]any{"kind": "event", "event": e}, eventLine(e))
					cursor = e.Seq
				}
				st = fresh
				if st.Follow.Cursor > cursor {
					cursor = st.Follow.Cursor
				}
				printStatus(st)
			}
		}
		if err != nil {
			maxB := time.Duration(st.Follow.MaxBackoffS) * unit
			if maxB <= 0 {
				maxB = 60 * unit
			}
			if backoff == 0 {
				backoff = time.Duration(st.Follow.PollAfterS) * unit
			}
			backoff *= 2
			if backoff > maxB {
				backoff = maxB
			}
			progress("the job could not be read (%s); reconnecting in %s from event %d, nothing is repeated or skipped", sanitize(errText(err)), (backoff / unit * time.Second).String(), cursor)
			continue
		}
		backoff = 0
	}
}
