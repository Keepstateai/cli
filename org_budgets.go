// org_budgets.go: an organization's monthly budgets and its spend. A budget
// is set for the organization, a team or a member, counts KeepState's
// charges only (session time, saved data, model use on KeepState's key) and,
// once reached, saves and pauses new work in it, never killing anything.
// Tokens on the organization's own keys are billed by the provider: shown
// beside the budgets, never counted in them.
//
// A budget is named by its id, by "organization", by a member's account id,
// or by a team as the team verbs name one (id, path, or a name only one team
// has).
package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type customerOrgBudget struct {
	ID         string   `json:"id"`
	ScopeKind  string   `json:"scope_kind"`
	ScopeID    string   `json:"scope_id"`
	ScopeName  string   `json:"scope_name"`
	ScopePath  []string `json:"scope_path"`
	Period     string   `json:"period"`
	PeriodKey  string   `json:"period_key"`
	Amount     int64    `json:"amount_microusd"`
	AlertPcts  []int    `json:"alert_pcts"`
	AtLimit    string   `json:"at_limit"`
	Charged    *int64   `json:"charged_microusd"`
	Held       *int64   `json:"held_microusd"`
	Percent    *int64   `json:"percent"`
	Status     string   `json:"status"`
	Overcommit *struct {
		Children int64 `json:"children_microusd"`
		Count    int   `json:"children"`
	} `json:"overcommit"`
	Words     string `json:"words"`
	SetBy     string `json:"set_by"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type customerOrgSpendRow struct {
	ScopeKind      string `json:"scope_kind"`
	ScopeID        string `json:"scope_id"`
	Name           string `json:"name"`
	Charged        *int64 `json:"charged_microusd"`
	Held           *int64 `json:"held_microusd"`
	ProviderTokens int64  `json:"provider_billed_tokens"`
	BudgetID       string `json:"budget_id"`
	BudgetAmount   *int64 `json:"budget_microusd"`
}

type customerOrgSpend struct {
	Period string                `json:"period"`
	Items  []customerOrgSpendRow `json:"items"`
	Note   string                `json:"note"`
}

type customerBudgetRaise struct {
	ID         string   `json:"id"`
	BudgetID   string   `json:"budget_id"`
	Amount     *int64   `json:"amount_microusd"`
	Recipients []string `json:"recipients"`
	Words      string   `json:"words"`
}

const providerTokensWords = "tokens on the organization's own keys are billed by the provider: shown, not counted in any budget"

// budgetPeriodFlag reads --period, a month written YYYY-MM.
func budgetPeriodFlag(inv *Invocation, nothing string) string {
	p := strings.TrimSpace(inv.Str("period"))
	if p == "" {
		return ""
	}
	ok := len(p) == 7 && p[4] == '-'
	if ok {
		y, e1 := strconv.Atoi(p[:4])
		m, e2 := strconv.Atoi(p[5:])
		ok = e1 == nil && e2 == nil && y >= 2000 && m >= 1 && m <= 12
	}
	if !ok {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--period is a month written YYYY-MM, like 2026-11; " + nothing})
	}
	return p
}

// monthlyFlag reads --monthly DOLLARS into micro-USD exactly, whole cents.
func monthlyFlag(inv *Invocation, nothing string) int64 {
	s := strings.TrimSpace(inv.Str("monthly"))
	m, err := parseUSD(s)
	if err != nil || m <= 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--monthly is a dollar amount above zero, like 500 or 12.50 (%q is not); %s", s, nothing)})
	}
	if m%10_000 != 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--monthly is in whole cents, like 12.50 (%s is not); %s", s, nothing)})
	}
	return m
}

// alertsFlag reads --alerts 50,80,100.
func alertsFlag(inv *Invocation, nothing string) []int {
	if !inv.Set("alerts") {
		return nil
	}
	var out []int
	seen := map[int]bool{}
	for _, p := range strings.Split(inv.Str("alerts"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 100 || seen[n] {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--alerts is a list of distinct whole percents from 1 to 100, like 50,80,100; " + nothing})
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func moneyOrUnavailable(m *int64) string {
	if m == nil {
		return "unavailable"
	}
	return dollars(*m)
}

func budgetScopeText(b customerOrgBudget) string {
	switch b.ScopeKind {
	case "org":
		return "the organization"
	case "team":
		if len(b.ScopePath) > 0 {
			return "team " + strings.Join(b.ScopePath, "/")
		}
		return "team " + b.ScopeName
	}
	if b.ScopeName != "" && b.ScopeName != b.ScopeID {
		return "member " + b.ScopeName + " (" + b.ScopeID + ")"
	}
	return "member " + b.ScopeID
}

func budgetSpentText(b customerOrgBudget) string {
	if b.Charged == nil || b.Held == nil || b.Status == "unavailable" {
		return "spent unavailable"
	}
	pct := ""
	if b.Percent != nil {
		pct = fmt.Sprintf(", %d%%", *b.Percent)
	}
	return fmt.Sprintf("%s spent, %s held%s", dollars(*b.Charged), dollars(*b.Held), pct)
}

func budgetLine(b customerOrgBudget) {
	fmt.Printf("%s  %s: %s a month; %s; %s\n", b.ID, budgetScopeText(b), dollars(b.Amount), budgetSpentText(b), b.Status)
}

func fetchOrgBudgets(cr hostedCreds, org, period string) ([]customerOrgBudget, error) {
	path := "/api/v2/orgs/" + url.PathEscape(org) + "/budgets"
	if period != "" {
		path += "?" + url.Values{"period": {period}}.Encode()
	}
	var env struct {
		Data struct {
			Items []customerOrgBudget `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// pickBudget finds one budget by its id, "organization", a member's account
// id, or a team as the team verbs name one.
func pickBudget(budgets []customerOrgBudget, teams []customerOrgTeam, ref string) (*customerOrgBudget, string) {
	ref = strings.TrimSpace(ref)
	for i := range budgets {
		b := &budgets[i]
		if b.ID == ref || (b.ScopeKind == "member" && b.ScopeID == ref) || (b.ScopeKind == "org" && (strings.EqualFold(ref, "organization") || b.ScopeID == ref)) {
			return b, ""
		}
	}
	if len(teams) > 0 {
		if t, _ := pickTeam(teams, ref); t != nil {
			for i := range budgets {
				if budgets[i].ScopeKind == "team" && budgets[i].ScopeID == t.ID {
					return &budgets[i], ""
				}
			}
			return nil, fmt.Sprintf("team %s has no budget you may read", t.pathText())
		}
	}
	return nil, fmt.Sprintf("no budget %q you may read in this organization", ref)
}

// resolveBudget reads the budgets (and the teams, when the reference may be
// one) and finds one, or stops.
func resolveBudget(cr hostedCreds, org, ref, nothing string) *customerOrgBudget {
	budgets, err := fetchOrgBudgets(cr, org, "")
	if err != nil {
		die(orgErr(err, nothing))
	}
	var teams []customerOrgTeam
	if !strings.HasPrefix(ref, "obud_") && !strings.HasPrefix(ref, "acct_") && !strings.EqualFold(ref, "organization") {
		teams, _, err = fetchOrgTeams(cr, org)
		if err != nil {
			die(orgErr(err, nothing))
		}
	}
	b, why := pickBudget(budgets, teams, ref)
	if b == nil {
		fail(&cliError{Code: exitUsage, Kind: "not_found", Message: why + "; " + nothing, NextAction: "ks budget list --organization " + org})
	}
	return b
}

func hostedBudgetList(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was read")
	period := budgetPeriodFlag(inv, "nothing was read")
	budgets, err := fetchOrgBudgets(cr, org, period)
	if err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(map[string]any{"organization": org, "budgets": budgets, "count": len(budgets)}, func() {
		if len(budgets) == 0 {
			fmt.Println("No budgets you may read. An owner, admin or billing member sets one: ks budget set --team TEAM --monthly 500 --organization " + org)
			return
		}
		for _, b := range budgets {
			budgetLine(b)
			if b.Overcommit != nil {
				fmt.Printf("    the %d team budgets under it add up to %s; each is enforced on its own\n", b.Overcommit.Count, dollars(b.Overcommit.Children))
			}
		}
		fmt.Println("Budgets count KeepState's charges only; " + providerTokensWords + ".")
	})
}

func hostedBudgetShow(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was read")
	b := resolveBudget(cr, org, inv.Arg(0), "nothing was read")
	var env struct {
		Data customerOrgBudget `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/budgets/"+url.PathEscape(b.ID), nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	d := env.Data
	emit(d, func() {
		budgetLine(d)
		if d.Words != "" {
			fmt.Println(d.Words + ".")
		}
		var alerts []string
		for _, a := range d.AlertPcts {
			alerts = append(alerts, strconv.Itoa(a)+"%")
		}
		fmt.Printf("Alerts at %s; at the limit new work is saved and paused, never killed, and a person resumes it.\n", strings.Join(alerts, ", "))
	})
}

func hostedBudgetSet(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	n := 0
	for _, f := range []bool{inv.Bool("organization-wide"), inv.Set("team"), inv.Set("member")} {
		if f {
			n++
		}
	}
	if n != 1 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name one budget: --organization-wide, --team TEAM or --member ACCOUNT; nothing was changed"})
	}
	amount := monthlyFlag(inv, "nothing was changed")
	alerts := alertsFlag(inv, "nothing was changed")
	kind, id := "org", org
	switch {
	case inv.Set("team"):
		t, _ := resolveTeam(cr, org, inv.Str("team"), "nothing was changed")
		kind, id = "team", t.ID
	case inv.Set("member"):
		kind, id = "member", strings.TrimSpace(inv.Str("member"))
	}
	budgets, err := fetchOrgBudgets(cr, org, "")
	if err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	rev := int64(0)
	for _, b := range budgets {
		if b.ScopeKind == kind && b.ScopeID == id {
			rev = b.Revision
		}
	}
	body := map[string]any{"scope_kind": kind, "scope_id": id, "amount_microusd": amount, "expected_revision": rev}
	if alerts != nil {
		body["alert_pcts"] = alerts
	}
	var env struct {
		Data customerOrgBudget `json:"data"`
	}
	if err := hostedMutate(cr, "PUT", "/api/v2/orgs/"+url.PathEscape(org)+"/budgets", body, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	d := env.Data
	emit(d, func() {
		if rev == 0 {
			fmt.Print("Set ")
		} else {
			fmt.Print("Changed ")
		}
		budgetLine(d)
		if d.Overcommit != nil {
			fmt.Printf("The %d team budgets under it add up to %s; each is enforced on its own, so the tightest stops first.\n", d.Overcommit.Count, dollars(d.Overcommit.Children))
		}
	})
}

func hostedBudgetClear(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	b := resolveBudget(cr, org, inv.Arg(0), "nothing was changed")
	var env struct {
		Data struct {
			ID      string `json:"id"`
			Cleared bool   `json:"cleared"`
		} `json:"data"`
	}
	path := "/api/v2/orgs/" + url.PathEscape(org) + "/budgets/" + url.PathEscape(b.ID) + "?" + url.Values{"expected_revision": {strconv.FormatInt(b.Revision, 10)}}.Encode()
	if err := hostedMutate(cr, "DELETE", path, nil, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	emit(env.Data, func() {
		fmt.Printf("Cleared the budget of %s (%s). The budgets above it still hold.\n", budgetScopeText(*b), b.ID)
	})
}

func hostedBudgetAsk(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was sent")
	body := map[string]any{}
	if inv.Set("monthly") {
		body["amount_microusd"] = monthlyFlag(inv, "nothing was sent")
	}
	b := resolveBudget(cr, org, inv.Arg(0), "nothing was sent")
	var env struct {
		Data customerBudgetRaise `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/budgets/"+url.PathEscape(b.ID)+"/raise-requests", body, &env); err != nil {
		die(orgErr(err, "nothing was sent"))
	}
	d := env.Data
	emit(d, func() {
		if d.Words != "" {
			fmt.Println(d.Words + ".")
			return
		}
		fmt.Printf("Asked to raise the budget of %s (%s); %d people who may raise it were told.\n", budgetScopeText(*b), b.ID, len(d.Recipients))
	})
}

func hostedOrgSpend(cr hostedCreds, inv *Invocation) {
	org := strings.TrimSpace(inv.Arg(0))
	period := budgetPeriodFlag(inv, "nothing was read")
	q := url.Values{}
	if inv.Set("by") {
		switch by := strings.TrimSpace(inv.Str("by")); by {
		case "member", "team", "member,team":
			q.Set("by", by)
		default:
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--by is member or team; nothing was read"})
		}
	}
	if period != "" {
		q.Set("period", period)
	}
	path := "/api/v2/orgs/" + url.PathEscape(org) + "/spend"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var env struct {
		Data customerOrgSpend `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("Spend for %s (KeepState's charges, and holds of calls still open):\n", d.Period)
		for _, r := range d.Items {
			budget := ""
			if r.BudgetAmount != nil {
				budget = "; budget " + dollars(*r.BudgetAmount)
			}
			fmt.Printf("  %-6s %s: %s spent, %s held%s; %d provider-billed tokens\n", r.ScopeKind, r.Name, moneyOrUnavailable(r.Charged), moneyOrUnavailable(r.Held), budget, r.ProviderTokens)
		}
		fmt.Println("Provider-billed tokens: " + providerTokensWords + ".")
	})
}
