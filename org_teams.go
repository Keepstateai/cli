// org_teams.go: an organization's teams and each member's home team. Teams
// nest up to 4 deep; owners and admins create, rename, move and delete them
// and change who is in them; every member who holds a seat reads them. A
// team is named by its id, by its path with a slash ("Engineering/Platform",
// or "/Engineering" for a top-level one) or by a name only one team has.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type customerOrgTeamMember struct {
	AccountID string `json:"account_id"`
	Role      string `json:"role"`
	Home      bool   `json:"home"`
	AddedAt   string `json:"added_at"`
}

type customerOrgTeam struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	ParentTeamID string                  `json:"parent_team_id"`
	Path         []string                `json:"path"`
	PathNames    []string                `json:"path_names"`
	Depth        int                     `json:"depth"`
	Revision     int64                   `json:"revision"`
	CreatedAt    string                  `json:"created_at"`
	Members      []customerOrgTeamMember `json:"members"`
}

func (t customerOrgTeam) pathText() string { return strings.Join(t.PathNames, "/") }

func fetchOrgTeams(cr hostedCreds, org string) ([]customerOrgTeam, bool, error) {
	var all []customerOrgTeam
	cursor := ""
	for page := 0; page < orgPagesMax; page++ {
		path := "/api/v2/orgs/" + url.PathEscape(org) + "/teams"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerOrgTeam `json:"items"`
				NextCursor string            `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
			return nil, false, err
		}
		all = append(all, env.Data.Items...)
		if env.Data.NextCursor == "" {
			return all, true, nil
		}
		cursor = env.Data.NextCursor
	}
	return all, false, nil
}

// pickTeam finds one team by its id, its path, or a name only one team has.
func pickTeam(teams []customerOrgTeam, ref string) (*customerOrgTeam, string) {
	ref = strings.TrimSpace(ref)
	var byName []*customerOrgTeam
	for i := range teams {
		t := &teams[i]
		// an id; a path when the reference has a slash; else a name
		if t.ID == ref || strings.Contains(ref, "/") && strings.EqualFold(t.pathText(), strings.Trim(ref, "/")) {
			return t, ""
		}
		if !strings.Contains(ref, "/") && strings.EqualFold(t.Name, ref) {
			byName = append(byName, t)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], ""
	case 0:
		return nil, fmt.Sprintf("no team %q in this organization (ks team list shows them)", ref)
	}
	var paths []string
	for _, t := range byName {
		paths = append(paths, t.pathText())
	}
	return nil, fmt.Sprintf("%d teams are called %q (%s); name one by its path or id", len(byName), ref, strings.Join(paths, ", "))
}

// resolveTeam reads the organization's teams and finds one, or stops.
func resolveTeam(cr hostedCreds, org, ref, nothing string) (*customerOrgTeam, []customerOrgTeam) {
	teams, _, err := fetchOrgTeams(cr, org)
	if err != nil {
		die(orgErr(err, nothing))
	}
	t, why := pickTeam(teams, ref)
	if t == nil {
		fail(&cliError{Code: exitUsage, Kind: "not_found", Message: why + "; " + nothing, NextAction: "ks team list --organization " + org})
	}
	return t, teams
}

func teamLine(t customerOrgTeam) {
	fmt.Printf("%s  %s (depth %d, %d in it)\n", t.ID, t.pathText(), t.Depth, len(t.Members))
}

func hostedTeamList(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was read")
	teams, complete, err := fetchOrgTeams(cr, org)
	if err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(map[string]any{"organization": org, "teams": teams, "count": len(teams), "complete": complete}, func() {
		if len(teams) == 0 {
			fmt.Println("No teams yet. An owner or admin makes one: ks team create NAME --organization " + org)
			return
		}
		// the service answers parents before the teams under them
		for _, t := range teams {
			indent := strings.Repeat("  ", t.Depth-1)
			fmt.Printf("%s%s  %s\n", indent, t.Name, t.ID)
			for _, m := range t.Members {
				tags := m.Role
				if m.Home {
					tags += ", home team"
				}
				fmt.Printf("%s    %s (%s)\n", indent, m.AccountID, tags)
			}
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}

func hostedTeamCreate(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	name := inv.Arg(0)
	if why := orgNameProblem(name); why != "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: strings.Replace(why, "an organization's name", "a team's name", 1) + "; nothing was changed"})
	}
	body := map[string]any{"name": name}
	if inv.Set("parent") {
		p, _ := resolveTeam(cr, org, inv.Str("parent"), "nothing was changed")
		body["parent_team_id"] = p.ID
	}
	var env struct {
		Data customerOrgTeam `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/teams", body, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	t := env.Data
	emit(t, func() {
		fmt.Print("Created ")
		teamLine(t)
	})
}

// teamPatch reads the team's revision (resolveTeam) and sends one change.
func teamPatch(cr hostedCreds, org string, t *customerOrgTeam, change map[string]any) customerOrgTeam {
	change["expected_revision"] = t.Revision
	var env struct {
		Data customerOrgTeam `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(t.ID), change, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	return env.Data
}

func hostedTeamRename(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	name := inv.Arg(1)
	if why := orgNameProblem(name); why != "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: strings.Replace(why, "an organization's name", "a team's name", 1) + "; nothing was changed"})
	}
	t, _ := resolveTeam(cr, org, inv.Arg(0), "nothing was changed")
	r := teamPatch(cr, org, t, map[string]any{"name": name})
	emit(r, func() {
		fmt.Print("Renamed ")
		teamLine(r)
	})
}

func hostedTeamMove(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	if inv.Set("parent") == inv.Bool("top") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give --parent TEAM to move it under another team, or --top to make it a top-level team; nothing was changed"})
	}
	t, teams := resolveTeam(cr, org, inv.Arg(0), "nothing was changed")
	parent := ""
	if inv.Set("parent") {
		p, why := pickTeam(teams, inv.Str("parent"))
		if p == nil {
			fail(&cliError{Code: exitUsage, Kind: "not_found", Message: why + "; nothing was changed", NextAction: "ks team list --organization " + org})
		}
		parent = p.ID
	}
	r := teamPatch(cr, org, t, map[string]any{"parent_team_id": parent})
	emit(r, func() {
		fmt.Print("Moved ")
		teamLine(r)
	})
}

func hostedTeamDelete(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	t, _ := resolveTeam(cr, org, inv.Arg(0), "nothing was changed")
	var env struct {
		Data struct {
			ID          string `json:"id"`
			Deleted     bool   `json:"deleted"`
			MembersLeft int64  `json:"members_left"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(t.ID), nil, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("Deleted team %s (%s). The %d people in it stay in the organization.\n", t.pathText(), d.ID, d.MembersLeft)
	})
}

func hostedTeamAdd(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	account := strings.TrimSpace(inv.Arg(1))
	role := "member"
	if inv.Bool("lead") {
		role = "lead"
	}
	t, _ := resolveTeam(cr, org, inv.Arg(0), "nothing was changed")
	var env struct {
		Data customerOrgTeam `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(t.ID)+"/members", map[string]any{"account_id": account, "role": role}, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	r := env.Data
	emit(r, func() { fmt.Printf("%s is in %s as %s.\n", account, r.pathText(), role) })
}

func hostedTeamRemove(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	account := strings.TrimSpace(inv.Arg(1))
	t, _ := resolveTeam(cr, org, inv.Arg(0), "nothing was changed")
	var env struct {
		Data customerOrgTeam `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(t.ID)+"/members/"+url.PathEscape(account), nil, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	r := env.Data
	emit(r, func() { fmt.Printf("%s is no longer in %s. They stay in the organization.\n", account, r.pathText()) })
}
