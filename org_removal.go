// org_removal.go: a member's profile details, removing a member through the
// plan the service shows first, and leaving an organization. A removal runs
// only with the plan's digest confirmed (typed, or --confirm); --yes never
// implies it.
package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type customerOrgMemberProfile struct {
	DisplayName string `json:"display_name"`
	WorkEmail   string `json:"work_email"`
	JobTitle    string `json:"job_title"`
	Department  string `json:"department"`
	CostCenter  string `json:"cost_center"`
	EmployeeRef string `json:"employee_ref"`
	Timezone    string `json:"timezone"`
	Locale      string `json:"locale"`
}

type customerOrgRemovalPlan struct {
	PlanID            string   `json:"plan_id"`
	AccountID         string   `json:"account_id"`
	Role              string   `json:"role"`
	Content           string   `json:"content"`
	TransferTo        string   `json:"transfer_to"`
	Sessions          []any    `json:"sessions"`
	AgentsTotal       int64    `json:"agents_total"`
	SeatFreed         bool     `json:"seat_freed"`
	ProfileEraseAfter string   `json:"profile_erase_after"`
	Steps             []string `json:"steps"`
	Digest            string   `json:"plan_digest"`
	ExpiresAt         string   `json:"expires_at"`
}

// orgProfileFlags: each profile flag and the field it sets.
var orgProfileFlags = []struct{ flag, field string }{
	{"display-name", "display_name"}, {"job-title", "job_title"}, {"department", "department"},
	{"cost-center", "cost_center"}, {"employee-ref", "employee_ref"}, {"timezone", "timezone"}, {"locale", "locale"},
}

func hostedMemberShow(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was read")
	var env struct {
		Data struct {
			customerOrgMember
			Profile *customerOrgMemberProfile `json:"profile"`
			Teams   []struct {
				TeamID string `json:"team_id"`
				Name   string `json:"name"`
				Role   string `json:"role"`
				Home   bool   `json:"home"`
			} `json:"teams"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/members/"+url.PathEscape(inv.Arg(0)), nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	d := env.Data
	emit(d, func() {
		memberLine(d.customerOrgMember)
		home := "none: their spend counts against the organization only"
		for _, t := range d.Teams {
			tag := t.Role
			if t.Home {
				home = t.Name + " (" + t.TeamID + ")"
				tag += ", home team"
			}
			fmt.Printf("  team %s (%s)\n", t.Name, tag)
		}
		fmt.Println("  home team " + home)
		if d.Profile == nil {
			fmt.Println("Profile details are shown to the member and to the organization's owners and admins.")
			return
		}
		p := d.Profile
		for _, kv := range [][2]string{{"name", p.DisplayName}, {"work email", p.WorkEmail}, {"job title", p.JobTitle}, {"department", p.Department},
			{"cost center", p.CostCenter}, {"employee ref", p.EmployeeRef}, {"timezone", p.Timezone}, {"locale", p.Locale}} {
			if kv[1] != "" {
				fmt.Printf("  %-13s %s\n", kv[0], kv[1])
			}
		}
	})
}

func hostedMemberSet(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	change := map[string]any{}
	for _, f := range orgProfileFlags {
		if inv.Set(f.flag) {
			change[f.field] = inv.Str(f.flag)
		}
	}
	if inv.Set("home-team") && inv.Bool("no-home-team") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--home-team and --no-home-team are two different changes; give one; nothing was changed"})
	}
	if len(change) == 0 && !inv.Set("home-team") && !inv.Bool("no-home-team") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give at least one detail to set, like --display-name, --timezone or --home-team; nothing was changed"})
	}
	switch {
	case inv.Set("home-team"):
		t, _ := resolveTeam(cr, org, inv.Str("home-team"), "nothing was changed")
		change["home_team_id"] = t.ID
	case inv.Bool("no-home-team"):
		change["home_team_id"] = ""
	}
	m := orgMemberChange(cr, org, inv.Arg(0), change, "nothing was changed")
	emit(m, func() {
		fmt.Printf("%s: details set\n", m.AccountID)
		if _, ok := change["home_team_id"]; ok {
			if m.HomeTeamID == "" {
				fmt.Println("No home team: their spend counts against the organization only.")
			} else {
				fmt.Printf("Home team %s: they are in it, and from the budgets on their spend counts against it and the teams above it.\n", m.HomeTeamID)
			}
		}
	})
}

func hostedMemberRemove(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	account := inv.Arg(0)
	content := inv.Str("content")
	if content != "transfer" && content != "remove" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "choose what happens to the member's organization sessions and agents: --content transfer (they move to you) or --content remove (they are deleted); nothing was changed"})
	}
	base := "/api/v2/orgs/" + url.PathEscape(org) + "/members/" + url.PathEscape(account)
	var penv struct {
		Data customerOrgRemovalPlan `json:"data"`
	}
	// a plan changes nothing: it is asked for plainly, never as an operation
	if err := hostedCall(cr, "POST", base+"/removal-plan", map[string]any{"content": content}, &penv); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	p := penv.Data
	fmt.Fprintf(os.Stderr, "removing %s (%s) from %s would (plan %s, digest %s):\n", p.AccountID, p.Role, org, p.PlanID, short(p.Digest))
	for _, s := range p.Steps {
		fmt.Fprintln(os.Stderr, "  - "+s)
	}
	given := strings.ToLower(strings.TrimSpace(inv.Str("confirm")))
	matches := func(s string) bool {
		s = strings.TrimSpace(s)
		return s != "" && (s == p.Digest || s == short(p.Digest))
	}
	confirmed := false
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not this plan's digest (%s); nothing was changed", given, short(p.Digest))})
		}
		confirmed = true
	case out.noInput || !stdinIsTerminal():
	default:
		fmt.Fprintf(os.Stderr, "type the digest (%s) to remove them exactly as shown, or anything else to stop: ", short(p.Digest))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		confirmed = matches(line)
	}
	if !confirmed {
		emit(map[string]any{"plan": p, "removed": false}, func() {
			fmt.Printf("planned, NOT removed: nothing changed. Remove exactly as shown: ks member remove %s --organization %s --content %s --confirm %s\n", account, org, content, short(p.Digest))
		})
		return
	}
	var renv struct {
		Data struct {
			Removed             bool   `json:"removed"`
			SessionsTransferred int64  `json:"sessions_transferred"`
			LeasesEnded         int64  `json:"leases_ended"`
			ProfileEraseAfter   string `json:"profile_erase_after"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", base+"/remove", map[string]any{"plan_id": p.PlanID, "plan_digest": p.Digest}, &renv); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	r := renv.Data
	emit(r, func() {
		fmt.Printf("%s removed from %s. ", account, org)
		if content == "transfer" {
			fmt.Printf("%d organization sessions moved to you, tagged transferred. ", r.SessionsTransferred)
		}
		fmt.Printf("Their profile details are erased on %s; their personal account is not touched.\n", r.ProfileEraseAfter)
	})
}

func hostedOrgLeave(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var env struct {
		Data struct {
			Left              bool   `json:"left"`
			ProfileEraseAfter string `json:"profile_erase_after"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(id)+"/leave", nil, &env); err != nil {
		die(orgErr(err, "you have not left"))
	}
	emit(env.Data, func() {
		fmt.Printf("You left %s. Your details there are erased on %s. Your personal account and its work are unchanged.\n", id, env.Data.ProfileEraseAfter)
	})
}
