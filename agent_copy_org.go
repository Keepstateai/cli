// agent_copy_org.go: copy a personal agent into an organization (BACKLOG-740,
// ADR-065), from the terminal.
//
//	ks agent copy-to-org AGENT --organization ORG [--session ID] [--point ID]
//	    [--name NAME] [--project ID] [--managed PROVIDER] [--share --handle H --title T ...]
//	ks agent copy-to-org AGENT ... --yes
//	ks agent copy-to-org AGENT ... --execute DIGEST [--plan-id ID]
//	ks agent copies AGENT [--session ID]
//
// The copy is two steps, as a fork's are. Without --yes or --execute the
// command plans: it prints the saved point it would copy, what is copied,
// what is not, who will read the copy, the warning that everything in the
// agent's files becomes the organization's, what it costs and who pays, and
// the digest of that plan; nothing is created. --yes carries out the plan it
// has just shown. --execute DIGEST carries out the plan with that digest: with
// --plan-id that exact plan, without it a fresh plan that must have the same
// digest (so what a person read is what runs, and a plan that changed since is
// refused).
//
// The copy is one way, personal to organization. The person's agent is not
// moved and not changed; the copy is the organization's, parked, made by the
// person and paid by the organization.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type agentCopyShareView struct {
	Handle string `json:"handle"`
	Title  string `json:"title"`
}

type agentCopyPlanView struct {
	ID         string `json:"id"`
	PlanDigest string `json:"plan_digest"`
	OrgID      string `json:"org_id"`
	OrgName    string `json:"org_name"`
	AgentName  string `json:"agent_name"`
	Name       string `json:"name"`
	Point      struct {
		ID    string `json:"id"`
		Words string `json:"words"`
	} `json:"point"`
	Copies      []string `json:"copies"`
	DoesNotCopy []string `json:"does_not_copy"`
	Warnings    []string `json:"warnings"`
	WhoCanSee   string   `json:"who_can_see"`
	SourceStays string   `json:"source_stays"`
	Keys        struct {
		Managed []string `json:"managed"`
		Note    string   `json:"note"`
	} `json:"keys"`
	Storage struct {
		Words string `json:"words"`
	} `json:"storage"`
	Billing    string              `json:"billing"`
	Share      *agentCopyShareView `json:"share"`
	ShareWords string              `json:"share_words"`
	Earlier    []struct {
		CopiedAt string `json:"copied_at"`
		Name     string `json:"name"`
		OrgName  string `json:"org_name"`
	} `json:"earlier_copies"`
	State string `json:"state"`
}

type agentCopyResultView struct {
	SessionID    string   `json:"session_id"`
	AgentIDs     []string `json:"agent_ids"`
	Name         string   `json:"name"`
	OrgID        string   `json:"org_id"`
	OrgName      string   `json:"org_name"`
	CheckpointID string   `json:"checkpoint_id"`
	RuntimeState string   `json:"runtime_state"`
	Library      *struct {
		Done      bool   `json:"done"`
		EntryID   string `json:"entry_id"`
		Handle    string `json:"handle"`
		VersionID string `json:"version_id"`
		Version   int    `json:"version"`
		Step      string `json:"step"`
		Code      string `json:"code"`
		Why       string `json:"why"`
	} `json:"library"`
	Note string `json:"note"`
}

// agentCopyPlanBody is the plan request this invocation makes.
func agentCopyPlanBody(inv *Invocation, org string) map[string]any {
	body := map[string]any{"org_id": org}
	if inv.Set("point") {
		body["checkpoint_id"] = strings.TrimSpace(inv.Str("point"))
	}
	if inv.Set("name") {
		body["name"] = inv.Str("name")
	}
	if inv.Set("project") {
		body["project_id"] = inv.Str("project")
	}
	if inv.Set("managed") {
		body["managed"] = inv.List("managed")
	}
	if inv.Bool("share") {
		share := map[string]any{"handle": trimHandle(inv.Str("handle")), "title": inv.Str("title")}
		if inv.Set("description") {
			share["description"] = inv.Str("description")
		}
		if inv.Set("tag") {
			share["tags"] = inv.List("tag")
		}
		if inv.Bool("show-instructions") {
			share["show_instructions"] = true
		}
		body["share"] = share
	}
	return body
}

func agentCopyUsageChecks(inv *Invocation) {
	nothing := "nothing was copied"
	if inv.Bool("share") {
		if trimHandle(inv.Str("handle")) == "" || strings.TrimSpace(inv.Str("title")) == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--share adds the copy to the organization's pool and needs --handle and --title; " + nothing})
		}
	} else {
		for _, f := range []string{"handle", "title", "description", "tag", "show-instructions"} {
			if inv.Set(f) {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--" + f + " goes with --share (adding the copy to the pool); " + nothing})
			}
		}
	}
	if inv.Set("plan-id") && !inv.Set("execute") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--plan-id goes with --execute DIGEST; " + nothing})
	}
	if inv.Set("execute") && strings.TrimSpace(inv.Str("execute")) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--execute names the digest of the plan to carry out; " + nothing})
	}
}

func agentCopyPrintPlan(d agentCopyPlanView) {
	fmt.Printf("copy plan for agent %s into %s: NOTHING was created\n", sanitize(d.AgentName), sanitize(firstNonEmpty(d.OrgName, d.OrgID)))
	fmt.Printf("  from       %s\n", sanitize(d.Point.Words))
	fmt.Printf("  copy name  %s\n", sanitize(d.Name))
	for _, c := range d.Copies {
		fmt.Printf("  copies     %s\n", sanitize(c))
	}
	for _, c := range d.DoesNotCopy {
		fmt.Printf("  not copied %s\n", sanitize(c))
	}
	fmt.Printf("  keys       %s\n", sanitize(d.Keys.Note))
	fmt.Printf("  readers    %s\n", sanitize(d.WhoCanSee))
	fmt.Printf("  storage    %s\n", sanitize(d.Storage.Words))
	fmt.Printf("  pays       %s\n", sanitize(d.Billing))
	fmt.Printf("  yours      %s\n", sanitize(d.SourceStays))
	fmt.Printf("  pool       %s\n", sanitize(d.ShareWords))
	for _, e := range d.Earlier {
		fmt.Printf("  earlier    copied before on %s as %q into %s\n", sanitize(e.CopiedAt), sanitize(e.Name), sanitize(e.OrgName))
	}
	for _, w := range d.Warnings {
		fmt.Printf("WARNING: %s\n", sanitize(w))
	}
	fmt.Printf("  digest     %s\n", d.PlanDigest)
}

func hostedAgentCopyToOrg(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was copied"
	agentCopyUsageChecks(inv)
	org := libraryOrg(cr, inv, nothing)
	sess := agentSession(cr, inv)
	src, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	base := "/api/v2/agents/" + url.PathEscape(src.ID)
	cmd := "ks agent copy-to-org " + inv.Arg(0) + " --session " + sess.ShortID + " --organization " + org
	exec := strings.TrimSpace(inv.Str("execute"))
	planID := strings.TrimSpace(inv.Str("plan-id"))
	var plan agentCopyPlanView
	if planID == "" {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := hostedMutate(cr, "POST", base+"/org-copy-plan", agentCopyPlanBody(inv, org), &env); err != nil {
			die(orgErr(err, nothing))
		}
		if json.Unmarshal(env.Data, &plan) != nil || plan.PlanDigest == "" || plan.ID == "" {
			fail(&cliError{Code: exitFailed, Kind: "plan_unreadable", Message: "the service's plan could not be read; " + nothing})
		}
		planID = plan.ID
		if exec == "" && !out.yes {
			emit(env.Data, func() {
				agentCopyPrintPlan(plan)
				fmt.Printf("copy exactly this plan: %s --execute %s --plan-id %s\n", cmd, plan.PlanDigest, plan.ID)
				fmt.Println("or plan and copy in one go with --yes; either way " + nothing + " until then.")
			})
			return
		}
		if exec != "" && exec != plan.PlanDigest {
			fail(&cliError{Code: exitConflict, Kind: "plan_changed", Message: fmt.Sprintf("the plan now has digest %s, not the %s you named: what would be copied changed since it was shown; %s", plan.PlanDigest, exec, nothing), NextAction: cmd})
		}
		if exec == "" { // --yes: carry out the plan just shown
			exec = plan.PlanDigest
			if !out.json {
				agentCopyPrintPlan(plan)
			}
		}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := hostedMutate(cr, "POST", base+"/org-copy", map[string]any{"plan_id": planID, "plan_digest": exec}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) {
			switch he.Type {
			case "ks_plan_stale", "ks_plan_digest_mismatch", "ks_checkpoint_mismatch", "ks_checkpoint_not_restorable", "ks_checkpoint_not_retained":
				ce := classify(err)
				ce.Message = nothing + ": " + ce.Message
				ce.NextAction = "plan again: " + cmd
				die(ce)
			}
		}
		die(orgErr(err, nothing))
	}
	var d agentCopyResultView
	if json.Unmarshal(env.Data, &d) != nil || d.SessionID == "" {
		fail(&cliError{Code: exitFailed, Kind: "result_unreadable", Message: "the service answered the copy and its answer could not be read; look at ks session list before copying again"})
	}
	partial := d.Library != nil && !d.Library.Done
	if !(partial && out.json) { // one document: a partly done copy is the error's
		agentCopyEmitResult(src, d, env.Data)
	}
	if partial {
		l := d.Library
		next := "from the organization's context (ks context use " + d.OrgID + "): ks agent share <agent> --handle HANDLE --title TITLE --save-now"
		fail(&cliError{Code: exitFailed, Kind: "partly_done", Message: fmt.Sprintf("the copy was made (session %s), but it was not added to the pool: the %s step stopped: %s", d.SessionID, sanitize(l.Step), sanitize(l.Why)), NextAction: next})
	}
}

func agentCopyEmitResult(src agentRow, d agentCopyResultView, raw json.RawMessage) {
	emit(raw, func() {
		fmt.Printf("copied agent %s into %s: session %s %q, %s, from its saved point.\n", sanitize(src.Name), sanitize(firstNonEmpty(d.OrgName, d.OrgID)), d.SessionID, sanitize(d.Name), sanitize(d.RuntimeState))
		fmt.Printf("  saved point  %s\n", d.CheckpointID)
		if l := d.Library; l != nil {
			if l.Done {
				fmt.Printf("  pool         added as @%s, version v%d\n", l.Handle, l.Version)
			} else {
				fmt.Printf("  pool         NOT added: stopped at the %s step (%s): %s\n", sanitize(l.Step), sanitize(l.Code), sanitize(l.Why))
			}
		}
		if d.Note != "" {
			fmt.Println(sanitize(d.Note))
		}
		fmt.Printf("open it: ks context use %s, then ks session show %s\n", d.OrgID, d.SessionID)
	})
}

func hostedAgentCopies(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	src, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var env struct {
		Data struct {
			Copies []struct {
				CopiedAt string `json:"copied_at"`
				Name     string `json:"name"`
				OrgID    string `json:"org_id"`
				OrgName  string `json:"org_name"`
			} `json:"copies"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(src.ID)+"/org-copies", nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(env.Data, func() {
		if len(env.Data.Copies) == 0 {
			fmt.Printf("agent %s has not been copied into an organization.\n", sanitize(src.Name))
			return
		}
		for _, c := range env.Data.Copies {
			fmt.Printf("%s  %q in %s (%s)\n", c.CopiedAt, sanitize(c.Name), sanitize(firstNonEmpty(c.OrgName, c.OrgID)), c.OrgID)
		}
	})
}
