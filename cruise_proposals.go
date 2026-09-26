// cruise_proposals.go: Cruise jobs an agent PROPOSED, reviewed and decided by
// a person (KS-071).
//
//	ks cruise proposal list      a session's proposals, newest first
//	ks cruise proposal show      one proposal: its manifest in full, the
//	                             digest the service states and the digest
//	                             this client computes from the manifest
//	ks cruise proposal approve   run it, by confirming the manifest digest
//	ks cruise proposal decline   it never runs
//
// A proposal is not a job. Nothing runs until a person approves the EXACT
// manifest, and the approval names its digest. This client does not take the
// service's word for that digest: it recomputes it from the manifest it
// displays (the same canonical rule ks cruise approve uses), refuses when the
// two differ, and sends only the digest it computed. The person confirms it
// by typing it at a terminal or with --confirm <digest> (the whole digest or
// the 12 characters shown); --yes never confirms an approval.
//
// Labels stay apart: a proposal reads proposed, approved or declined; the job
// it became reads in the job's own words (Queued, Running, Verifying, Checks
// passed only for an accepted verdict, Review required, Failed, Cancelled).
// An agent saying its work is done is never any of these.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type cruiseProposalRow struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	AgentID     string          `json:"agent_id"`
	TaskID      string          `json:"task_id,omitempty"`
	ProposedBy  string          `json:"proposed_by"`
	Manifest    json.RawMessage `json:"manifest"`
	ManifestSHA string          `json:"manifest_sha"`
	Goal        string          `json:"goal"`
	Note        string          `json:"note,omitempty"`
	State       string          `json:"state"`
	DecidedBy   string          `json:"decided_by,omitempty"`
	DecidedAt   string          `json:"decided_at,omitempty"`
	Decline     string          `json:"decline_reason,omitempty"`
	Revision    int64           `json:"revision"`
	CreatedAt   string          `json:"created_at"`
	Job         *struct {
		ID          string  `json:"id"`
		State       string  `json:"state"`
		Verdict     *string `json:"verdict"`
		Label       string  `json:"label"`
		ManifestSHA string  `json:"manifest_sha"`
		Route       string  `json:"route"`
	} `json:"job,omitempty"`
}

// proposalDigest recomputes the manifest's canonical digest from the bytes
// the service returned.
func proposalDigest(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return "", fmt.Errorf("the proposal's manifest is not a JSON object")
	}
	return manifestSHA(m)
}

func fetchProposal(cr hostedCreds, id string) (cruiseProposalRow, error) {
	var env struct {
		Data cruiseProposalRow `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/cruise-proposals/"+url.PathEscape(id), nil, &env)
	return env.Data, err
}

func proposalLine(p cruiseProposalRow) string {
	job := ""
	if p.Job != nil {
		job = "  job " + p.Job.ID + " " + p.Job.Label
	}
	return sanitize(fmt.Sprintf("%-18s %-9s %s  digest %s  agent %s%s", p.ID, p.State, clip(p.Goal, 40), short(p.ManifestSHA), p.AgentID, job))
}

func hostedCruiseProposalList(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	var env struct {
		Data struct {
			Items []cruiseProposalRow `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/cruise-proposals?"+url.Values{"session_id": {agentSessionID(sess)}}.Encode(), nil, &env); err != nil {
		die(err)
	}
	ps := env.Data.Items
	emit(map[string]any{"session": agentSessionID(sess), "proposals": ps, "count": len(ps)}, func() {
		if len(ps) == 0 {
			fmt.Printf("no Cruise jobs were proposed in session %s\n", sess.ShortID)
			return
		}
		for _, p := range ps {
			fmt.Println(proposalLine(p))
		}
		fmt.Println("a proposal runs nothing until you approve its manifest: ks cruise proposal show <id>")
	})
}

// showProposal writes the proposal for review: its manifest in full, and
// both digests.
func showProposal(w *os.File, p cruiseProposalRow, computed string) {
	fmt.Fprintf(w, "proposal %s  %s\n", p.ID, strings.ToUpper(p.State))
	fmt.Fprintf(w, "  goal         %s\n", visible(sanitize(p.Goal)))
	fmt.Fprintf(w, "  proposed by  %s (agent %s, instruction %s) at %s\n", sanitize(p.ProposedBy), p.AgentID, notRecorded(p.TaskID), p.CreatedAt)
	if p.Note != "" {
		fmt.Fprintf(w, "  note         %s\n", visible(sanitize(p.Note)))
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, p.Manifest, "    ", "  ") == nil {
		fmt.Fprintf(w, "  manifest\n    %s\n", visible(sanitize(pretty.String())))
	}
	fmt.Fprintf(w, "  digest       %s (the service)\n", p.ManifestSHA)
	fmt.Fprintf(w, "               %s (computed here from the manifest above)\n", computed)
	switch p.State {
	case "approved":
		fmt.Fprintf(w, "  approved     by %s at %s\n", figure(p.DecidedBy), figure(p.DecidedAt))
	case "declined":
		fmt.Fprintf(w, "  declined     by %s at %s: %s\n", figure(p.DecidedBy), figure(p.DecidedAt), notRecorded(p.Decline))
	}
	if p.Job != nil {
		fmt.Fprintf(w, "  job          %s: %s (the job's own state; a task's Finished is never this)\n", p.Job.ID, p.Job.Label)
	}
}

func hostedCruiseProposalShow(cr hostedCreds, inv *Invocation) {
	p, err := fetchProposal(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	computed, derr := proposalDigest(p.Manifest)
	if derr != nil {
		computed = "not computable: " + derr.Error()
	}
	emit(map[string]any{"proposal": p, "computed_manifest_sha": computed, "digests_agree": computed == p.ManifestSHA}, func() {
		showProposal(os.Stdout, p, computed)
		if p.State == "proposed" {
			fmt.Printf("approve exactly this manifest: ks cruise proposal approve %s --confirm %s\n", p.ID, short(computed))
		}
	})
}

func hostedCruiseProposalApprove(cr hostedCreds, inv *Invocation) {
	p, err := fetchProposal(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if p.State != "proposed" && p.State != "approved" {
		fail(&cliError{Code: exitConflict, Kind: "proposal_state", Message: fmt.Sprintf("proposal %s is %s; only a proposed job can be approved, and nothing was created", p.ID, p.State)})
	}
	computed, err := proposalDigest(p.Manifest)
	if err != nil {
		fail(integrity("manifest_unreadable", err.Error()+"; nothing was approved"))
	}
	if computed != strings.ToLower(p.ManifestSHA) {
		fail(integrity("manifest_digest_mismatch", fmt.Sprintf("the service states digest %s but the manifest it returned hashes to %s; nothing was approved", short(p.ManifestSHA), short(computed))))
	}
	// the review, before anything is sent (stderr, so --json stays one document)
	showProposal(os.Stderr, p, computed)
	given := strings.ToLower(strings.TrimSpace(inv.Str("confirm")))
	matches := func(s string) bool {
		s = strings.ToLower(strings.TrimSpace(s))
		return s == computed || (len(s) == 12 && s == short(computed))
	}
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch",
				Message: fmt.Sprintf("--confirm %q is not the digest of this manifest (%s); nothing was approved", given, short(computed))})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
			Message:    "an approval is confirmed by the manifest's digest and none was given; --yes does not approve a Cruise job, and nothing was approved",
			NextAction: fmt.Sprintf("ks cruise proposal approve %s --confirm %s", p.ID, short(computed))})
	default:
		fmt.Fprintf(os.Stderr, "type the digest (%s) to run this job, or anything else to stop: ", short(computed))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !matches(line) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the digest was not confirmed; nothing was approved"})
		}
	}
	var env struct {
		Data cruiseProposalRow `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/cruise-proposals/"+url.PathEscape(p.ID)+"/approve", map[string]any{"manifest_sha": computed}, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		if d.Job != nil {
			fmt.Printf("approved %s: job %s (%s) runs the manifest with digest %s\n", d.ID, d.Job.ID, d.Job.Label, short(computed))
			fmt.Printf("follow it: ks cruise status %s; cancel it only with ks cruise cancel %s (cancelling the agent's work does not)\n", d.Job.ID, d.Job.ID)
		} else {
			fmt.Printf("approved %s (%s); the service linked no job in its answer\n", d.ID, d.State)
		}
	})
}

func hostedCruiseProposalDecline(cr hostedCreds, inv *Invocation) {
	body := map[string]any{"reason": strings.TrimSpace(inv.Str("reason"))}
	var env struct {
		Data cruiseProposalRow `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/cruise-proposals/"+url.PathEscape(inv.Arg(0))+"/decline", body, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("declined %s: it never runs (state %s)\n", d.ID, d.State)
	})
}
