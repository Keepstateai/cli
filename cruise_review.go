// cruise_review.go: the Cruise review screen and the decisions taken from it
// (KS-077). When a job stops in review, GET /api/jobs/{id}/status carries a
// review block: why it stopped, every attempt with its exact check result
// and cost, what remains of the approved ceiling (or that it is
// unavailable), and the options (resume, resume with a different ladder,
// cancel) each with its effect. Once cancelled, a cleanup block reports the
// teardown apart from the accounting: pending, then confirmed or not_needed.
//
//	ks cruise review JOB     the screen; decides nothing
//	ks cruise resume JOB     shows the option's effect and what it may spend,
//	                         then resumes; the service records it as your
//	                         decision on a new approval revision
//	ks cruise cancel JOB     shows the effect, then cancels; the cleanup is
//	                         reported apart from the costs, which stand
//
// Money is printed only where the service gives a figure: "unavailable" is
// never $0. A resume with a changed ladder says what it may spend beyond
// what has been spent, against the unchanged ceiling, or that this is not
// known.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type liveReview struct {
	Reason   string `json:"reason"`
	Attempts []struct {
		ID             string   `json:"id"`
		Rung           int64    `json:"rung"`
		Model          string   `json:"model"`
		Verdict        *string  `json:"verdict"`
		Reason         string   `json:"reason"`
		CheckResult    string   `json:"check_result"`
		TestsTail      string   `json:"tests_tail"`
		PolicyProblems []string `json:"policy_problems"`
		CostMicroUSD   *int64   `json:"cost_microusd"`
	} `json:"attempts"`
	RemainingUSD *int64 `json:"remaining_microusd"`
	Remaining    string `json:"remaining_standing"`
	Options      []struct {
		Action  string `json:"action"`
		Label   string `json:"label"`
		Request string `json:"request"`
		Effect  string `json:"effect"`
	} `json:"options"`
}

type liveCleanup struct {
	State string `json:"state"`
	At    string `json:"at"`
	Note  string `json:"note"`
}

// statusUnsupported: the status route is not served (an older control
// plane), as distinct from a job that does not exist.
func statusUnsupported(err error) bool {
	var he *hostedErr
	return errors.As(err, &he) && he.Status == 404 && he.Type != "ks_not_found"
}

func remainingText(r *liveReview) string {
	if r.Remaining == "known" && r.RemainingUSD != nil {
		return dollars(*r.RemainingUSD) + " of the approved ceiling remains"
	}
	return "unavailable: earlier attempts have no reconciled cost, so what remains of the ceiling is not known"
}

func reviewLines(s jobLiveStatus) []string {
	r := s.Review
	out := []string{
		fmt.Sprintf("job %s is IN REVIEW: nothing runs until you decide", s.JobID),
		"  why it stopped " + sanitize(notRecorded(r.Reason)),
		fmt.Sprintf("  attempts       %d, oldest first (kept whatever you decide):", len(r.Attempts)),
	}
	for _, a := range r.Attempts {
		cost := "unavailable"
		if a.CostMicroUSD != nil {
			cost = dollars(*a.CostMicroUSD)
		}
		verdict := "none"
		if a.Verdict != nil && *a.Verdict != "" {
			verdict = *a.Verdict
		}
		out = append(out, fmt.Sprintf("    %s  rung %d  %s  check %s  verdict %s  cost %s", a.ID, a.Rung+1, sanitize(figure(a.Model)), sanitize(notRecorded(a.CheckResult)), sanitize(verdict), cost))
		if a.Reason != "" {
			out = append(out, "      reason   "+sanitize(a.Reason))
		}
		for _, p := range a.PolicyProblems {
			out = append(out, "      policy   "+sanitize(p))
		}
		if t := strings.TrimSpace(a.TestsTail); t != "" {
			lines := strings.Split(t, "\n")
			if len(lines) > 6 {
				lines = lines[len(lines)-6:]
			}
			out = append(out, "      tests (last lines):")
			for _, l := range lines {
				out = append(out, "        | "+sanitize(l))
			}
		}
	}
	out = append(out, "  ceiling        "+remainingText(r), "  your options (nothing is chosen for you):")
	for _, o := range r.Options {
		out = append(out, fmt.Sprintf("    %s: %s", sanitize(o.Label), reviewVerb(o.Action, s.JobID)), "      effect   "+sanitize(o.Effect))
	}
	return out
}

func reviewVerb(action, id string) string {
	switch action {
	case "resume":
		return "ks cruise resume " + id
	case "resume_ladder":
		return "ks cruise resume " + id + " --ladder family:model,..."
	case "cancel":
		return "ks cruise cancel " + id
	}
	return "(no client command for " + sanitize(action) + ")"
}

// cleanupLine reports the teardown apart from the accounting.
func cleanupLine(c *liveCleanup) string {
	switch c.State {
	case "confirmed":
		return "  cleanup        confirmed at " + figure(c.At) + ": " + sanitize(c.Note)
	case "not_needed":
		return "  cleanup        not needed: " + sanitize(c.Note)
	case "pending":
		return "  cleanup        PENDING: " + sanitize(c.Note)
	}
	return "  cleanup        " + stateLabel("cleanup", c.State) + ": " + sanitize(c.Note)
}

// cruiseReview is ks cruise review JOB.
func cruiseReview(inv *Invocation) {
	c := mustCreds()
	id := strings.TrimSpace(inv.Arg(0))
	st, err := fetchJobStatus(c, id)
	if err != nil {
		if statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve a job's review; nothing was changed", NextAction: "ks cruise status " + id})
		}
		die(err)
	}
	emit(map[string]any{"job_id": st.JobID, "state": st.State, "review": st.Review, "cleanup": st.Cleanup, "spend": st.Spend}, func() {
		if st.Review == nil {
			fmt.Printf("job %s is %s (%s), not in review: there is nothing to decide\n", st.JobID, sanitize(st.StateLabel), sanitize(st.State))
		} else {
			for _, l := range reviewLines(st) {
				fmt.Println(l)
			}
		}
		fmt.Println("  accounting     model spend " + moneyText(st.Spend.Model) + "; runtime and storage " + moneyText(st.Spend.Runtime))
		if st.Cleanup != nil {
			fmt.Println(cleanupLine(st.Cleanup))
		}
	})
}

// optionEffect answers the service's stated effect for an option.
func optionEffect(r *liveReview, action string) string {
	if r == nil {
		return ""
	}
	for _, o := range r.Options {
		if o.Action == action {
			return o.Effect
		}
	}
	return ""
}

// decisionRecord reads the attributed decision the service journalled after
// the cursor: who decided, and the revision.
func decisionRecord(c hostedCreds, id string, since int64, kind string) (map[string]any, bool) {
	evs, err := fetchJobEventsSince(c, id, since)
	if err != nil {
		return nil, false
	}
	var found map[string]any
	for _, e := range evs {
		if e.Type != kind {
			continue
		}
		var d map[string]any
		dec := json.NewDecoder(bytes.NewReader(e.Detail))
		dec.UseNumber()
		_ = dec.Decode(&d)
		if d != nil && d["decided_by"] != nil {
			found = d
		}
	}
	return found, found != nil
}

func actorText(d map[string]any) string {
	by, _ := d["decided_by"].(map[string]any)
	if by == nil {
		return "not recorded"
	}
	cred := jstr(by, "credential")
	if cred == "bearer token" {
		cred = "an API token" // said in this client's words: the redactor hides "bearer ..." on purpose
	}
	s := "account " + jstr(by, "account_id") + " via " + cred
	if fp, _ := by["token_fingerprint"].(string); fp != "" {
		s += " (token " + fp + ")"
	}
	return sanitize(s)
}

// preDecision reads the status before a decision. An older control plane
// without the route is said, and the decision proceeds as before.
func preDecision(c hostedCreds, id string) (*jobLiveStatus, bool) {
	st, err := fetchJobStatus(c, id)
	if err != nil {
		if statusUnsupported(err) {
			progress("this control plane does not report the job's review, so the option's effect cannot be shown from it")
			return nil, false
		}
		die(err)
	}
	return &st, true
}

// resumeSpendLines states what a resume may spend.
func resumeSpendLines(st *jobLiveStatus, ladder []string) []string {
	var out []string
	if len(ladder) > 0 {
		out = append(out, "  new ladder     "+sanitize(strings.Join(ladder, ", "))+" (starting again at its first rung)")
	}
	r := st.Review
	ceiling := dollars(st.Spend.CeilingUSD)
	if r != nil && r.Remaining == "known" && r.RemainingUSD != nil {
		out = append(out, fmt.Sprintf("  may spend      up to %s more: what remains of the unchanged %s ceiling (model spend so far %s)", dollars(*r.RemainingUSD), ceiling, moneyText(st.Spend.Model)))
	} else {
		out = append(out, "  may spend      NOT KNOWN: earlier attempts have no reconciled cost, so what remains of the "+ceiling+" ceiling is unavailable and no figure bounds this resume's extra spend yet")
	}
	if len(ladder) > 0 {
		out = append(out, "  changed ladder the ceiling is unchanged; the models you named are billed at their own rates within it")
	}
	return out
}

// cruiseResume is ks cruise resume JOB [--ladder ...].
func cruiseResume(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	req := map[string]any{}
	specs := csvList(inv.Str("ladder"))
	if len(specs) > 0 {
		ladder, err := parseLadder(specs)
		if err != nil {
			die(err)
		}
		req["ladder"] = ladder
		// KS-075: a wider ladder is checked against the live catalog first
		var la []any
		for _, r := range ladder {
			la = append(la, map[string]any{"family": r.Family, "model": r.Model})
		}
		if err := checkLadderLive(c, map[string]any{"ladder": la}); err != nil {
			die(err)
		}
	}
	st, ok := preDecision(c, id)
	var since int64
	if ok {
		if st.Review == nil {
			fail(&cliError{Code: exitConflict, Kind: "job_state", Message: fmt.Sprintf("job %s is %s (%s); resume is legal only from review, so nothing was sent", id, sanitize(st.StateLabel), sanitize(st.State)), NextAction: "ks cruise status " + id})
		}
		since = st.Follow.Cursor
		action := "resume"
		if len(specs) > 0 {
			action = "resume_ladder"
		}
		progress("resuming job %s: %s", id, sanitize(notRecorded(optionEffect(st.Review, action))))
		for _, l := range resumeSpendLines(st, specs) {
			progress("%s", l)
		}
	}
	var job map[string]any
	if err := hostedMutate(c, "POST", "/api/jobs/"+id+"/resume", req, &job); err != nil {
		die(err)
	}
	rec, recorded := decisionRecord(c, id, since, "job.queued")
	// the job document as before, with the decision beside its fields
	doc := map[string]any{}
	for k, v := range job {
		doc[k] = v
	}
	doc["decision"] = rec
	emit(doc, func() {
		progress("job %s %s", id, jstr(job, "state"))
		if recorded {
			progress("recorded as your decision: revision %s replaces %s; decided by %s", fieldText(rec, "manifest_version"), fieldText(rec, "previous_manifest_version"), actorText(rec))
		} else {
			progress("the decision's record could not be read back; ks cruise logs %s shows it", id)
		}
		fmt.Println(id)
	})
}

// cruiseCancel is ks cruise cancel JOB.
func cruiseCancel(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	st, ok := preDecision(c, id)
	var since int64
	if ok {
		since = st.Follow.Cursor
		effect := optionEffect(st.Review, "cancel")
		if effect == "" {
			effect = "stops the job; its sessions are released through a tracked cleanup; attempts, costs and results are kept"
		}
		progress("cancelling job %s (%s): %s", id, sanitize(st.State), sanitize(effect))
	}
	var job map[string]any
	if err := hostedMutate(c, "POST", "/api/jobs/"+id+"/cancel", map[string]any{}, &job); err != nil {
		die(err)
	}
	rec, recorded := decisionRecord(c, id, since, "job.cancelled")
	var after *jobLiveStatus
	if ok {
		if s2, err := fetchJobStatus(c, id); err == nil {
			after = &s2
		}
	}
	doc := map[string]any{}
	for k, v := range job {
		doc[k] = v
	}
	doc["decision"] = rec
	if after != nil {
		doc["cleanup"], doc["spend"] = after.Cleanup, after.Spend
	}
	emit(doc, func() {
		progress("job %s %s", id, jstr(job, "state"))
		if recorded {
			progress("recorded as your decision on revision %s; decided by %s", fieldText(rec, "manifest_version"), actorText(rec))
		}
		if after != nil {
			// the teardown and the costs are separate facts
			if after.Cleanup != nil {
				progress("%s", strings.TrimSpace(cleanupLine(after.Cleanup)))
			}
			progress("accounting: model spend %s (costs already incurred stand)", moneyText(after.Spend.Model))
		}
		fmt.Println(id)
	})
}

// fieldText renders one recorded field as given, or "not recorded".
func fieldText(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case nil:
		return "not recorded"
	case string:
		return sanitize(v)
	default:
		return sanitize(fmt.Sprint(v))
	}
}
