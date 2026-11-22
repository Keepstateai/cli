package main

// claude_plan_org.go: a member's Claude plan under an organization. A member
// signs in under the organization's user (ks login claude --org), the plan is
// used only by that member's agents in that organization, and an owner or
// admin sees every member's row (never a token) and can end one.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type orgPlanRow struct {
	MemberID string `json:"member_id"`
	Who      *struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Email string `json:"email"`
	} `json:"who"`
	Last4      string `json:"last4"`
	State      string `json:"state"`
	StateWords string `json:"state_words"`
	ExpiresAt  string `json:"expires_at"`
	Since      string `json:"since"`
	You        bool   `json:"you"`
}

func (r orgPlanRow) label() string {
	if r.Who != nil {
		if n := strings.TrimSpace(r.Who.Name); n != "" {
			return sanitize(n)
		}
		if e := strings.TrimSpace(r.Who.Email); e != "" {
			return sanitize(e)
		}
	}
	return sanitize(r.MemberID)
}

// resolveOrgArg turns an organization's id, or its name when exactly one of
// yours has it, into its id.
func resolveOrgArg(cr hostedCreds, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "org_") {
		return arg, nil
	}
	orgs, _, err := fetchOrgs(cr)
	if err != nil {
		return "", orgErr(err, "nothing was done")
	}
	var hit []customerOrg
	for _, o := range orgs {
		if strings.EqualFold(o.Name, arg) || o.ID == arg {
			hit = append(hit, o)
		}
	}
	switch len(hit) {
	case 1:
		return hit[0].ID, nil
	case 0:
		return "", &cliError{Code: exitUsage, Kind: "not_found", WorkStarted: workNo,
			Message: fmt.Sprintf("you belong to no organization called %q (an id or a name)", sanitize(arg)), NextAction: "ks organization list"}
	}
	return "", &cliError{Code: exitUsage, Kind: "ambiguous", WorkStarted: workNo,
		Message: fmt.Sprintf("%d of your organizations are called %q; give the id", len(hit), sanitize(arg)), NextAction: "ks organization list"}
}

// orgContextOf is the organization a plan verb acts in: --org, else the
// context ks context use named.
func orgContextOf(cr hostedCreds, inv *Invocation) (string, error) {
	if inv.Set("org") {
		return resolveOrgArg(cr, inv.Str("org"))
	}
	if c := keyContext(cr); c != "" {
		return c, nil
	}
	return "", &cliError{Code: exitUsage, Kind: "usage", WorkStarted: workNo,
		Message: "name the organization with --org (an id or a name), or choose one with ks context use", NextAction: "ks organization list"}
}

func fetchOrgPlanRows(cr hostedCreds, org string) ([]orgPlanRow, error) {
	var env struct {
		Data struct {
			Items []orgPlanRow `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/plan-keys", nil, &env); err != nil {
		return nil, orgErr(claudePlanErr(err), "nothing was read")
	}
	return env.Data.Items, nil
}

func orgPlanStanding(r orgPlanRow, now time.Time) string {
	st := planState(customerKey{State: r.State, ExpiresAt: r.ExpiresAt}, now)
	switch st {
	case "ok":
		return "working"
	case "expiring":
		return "ends soon"
	case "expired":
		return "ended"
	case "invalid":
		return "no longer works"
	}
	return sanitize(st)
}

// hostedOrgPlanKeys lists the Claude plans members have put in an
// organization: your own row, and for an owner or admin every member's.
func hostedOrgPlanKeys(cr hostedCreds, inv *Invocation) {
	org, err := orgContextOf(cr, inv)
	if err != nil {
		fail(err)
	}
	rows, err := fetchOrgPlanRows(cr, org)
	if err != nil {
		fail(err)
	}
	now := time.Now()
	emit(map[string]any{"organization": org, "plan_keys": rows, "count": len(rows)}, func() {
		uo, _ := cmdUI()
		renderOrgPlanKeys(uo, org, rows, now)
	})
}

// renderOrgPlanKeys is the list of members' Claude plans in an organization.
func renderOrgPlanKeys(uo *ui, org string, rows []orgPlanRow, now time.Time) {
	uo.Title("", toneNeutral, "Claude plans in "+sanitize(org), fmt.Sprintf("%d", len(rows)))
	if len(rows) == 0 {
		uo.Note("No member has put a Claude plan in this organization yet.")
		uo.Blank()
		uo.Next("ks login claude --org " + org)
		return
	}
	var table [][]string
	for _, r := range rows {
		who := r.label()
		if r.You {
			who += " (you)"
		}
		ends := "-"
		if t, ok := planEnds(customerKey{ExpiresAt: r.ExpiresAt}); ok {
			ends = t.UTC().Format("2 Jan 2006")
		}
		table = append(table, []string{who, "…" + sanitize(r.Last4), orgPlanStanding(r, now), ends})
	}
	uo.Table([]Col{{Title: "MEMBER", Flex: true}, {Title: "LAST4"}, {Title: "STANDING"}, {Title: "ENDS", Second: true}}, table)
	for _, r := range rows {
		if r.You {
			if w := planStateWords(customerKey{State: r.State, StateWords: r.StateWords, ExpiresAt: r.ExpiresAt}, now); w != "" {
				uo.Blank()
				uo.Warn("Check your sign-in", w)
			}
		}
	}
}

// renderOrgPlanRemoved is the card that says a member's plan ended there.
func renderOrgPlanRemoved(uo *ui, who string) {
	uo.Success("Ended "+who+" Claude plan in this organization", [][2]string{
		{"Still there", "the person's own sign-in, and their personal sessions that use it"},
		{"Ended", "its use by agents in this organization"},
	})
}

// hostedOrgPlanKeyRemove ends one member's Claude plan in an organization:
// yours, or as an owner or admin another member's. The person's own sign-in
// stays where it is; only the permission in this organization ends.
func hostedOrgPlanKeyRemove(cr hostedCreds, inv *Invocation) {
	org, err := orgContextOf(cr, inv)
	if err != nil {
		fail(err)
	}
	rows, err := fetchOrgPlanRows(cr, org)
	if err != nil {
		fail(err)
	}
	arg := strings.TrimSpace(inv.Arg(0))
	var hit []orgPlanRow
	for _, r := range rows {
		if r.MemberID == arg || (arg == "me" && r.You) || (r.Who != nil && (strings.EqualFold(r.Who.Email, arg) || strings.EqualFold(r.Who.Name, arg))) {
			hit = append(hit, r)
		}
	}
	switch len(hit) {
	case 0:
		fail(&cliError{Code: exitUsage, Kind: "not_found", WorkStarted: workNo,
			Message: fmt.Sprintf("no member with a Claude plan in this organization is %q (a member id, an email or a name; me is you)", sanitize(arg)), NextAction: "ks organization plan-keys --org " + org})
	case 2, 3, 4, 5, 6, 7, 8, 9:
		fail(&cliError{Code: exitUsage, Kind: "ambiguous", WorkStarted: workNo,
			Message: fmt.Sprintf("%d members match %q; give the member id", len(hit), sanitize(arg)), NextAction: "ks organization plan-keys --org " + org})
	}
	m := hit[0]
	who := m.label()
	if m.You {
		who = "your"
	} else {
		who += "'s"
	}
	_, ue := cmdUI()
	ok, cerr := ue.Confirm("End "+who+" Claude plan in this organization?", false, "--yes", "Nothing was removed.")
	if cerr != nil {
		var ce *cliError
		if errors.As(cerr, &ce) && ce.NextAction == "" {
			ce.NextAction = "ks organization plan-keys remove " + sanitize(arg) + " --org " + org + " --yes"
		}
		fail(cerr)
	}
	if !ok {
		return
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/orgs/"+url.PathEscape(org)+"/plan-keys/"+url.PathEscape(m.MemberID), nil, nil); err != nil {
		fail(orgErr(claudePlanErr(err), "nothing was removed"))
	}
	emit(map[string]any{"organization": org, "member_id": m.MemberID, "removed": true}, func() {
		uo, _ := cmdUI()
		renderOrgPlanRemoved(uo, who)
	})
}

// chooseClaudePlanInOrg is --key claude-plan for an organization's agent: the
// plan the member put under that organization.
func chooseClaudePlanInOrg(cr hostedCreds, org, provider string) (runKeyChoice, error) {
	c := runKeyChoice{plan: true}
	if provider != claudePlanAnthropic {
		return c, claudePlanRunnerErr("this agent calls " + providerName(provider) + ", not a Claude model")
	}
	rows, err := fetchOrgPlanRows(cr, org)
	if err != nil {
		return c, err
	}
	for _, r := range rows {
		if !r.You {
			continue
		}
		k := customerKey{Provider: claudePlanProvider, Kind: claudePlanProvider, Last4: r.Last4, Enabled: true, State: r.State, StateWords: r.StateWords, ExpiresAt: r.ExpiresAt}
		if !planUsable(k, time.Now()) {
			return c, claudePlanUnusableErr(k)
		}
		c.planKey = k
		return c, nil
	}
	return c, claudePlanErr(&hostedErr{Status: 409, Type: "ks_claude_plan_org", Message: "no Claude plan in this organization"})
}
