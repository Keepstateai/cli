// org.go: organizations on the command line, the first verbs: create one,
// list yours, show one, and list its members. An organization is separate
// from your personal account: creating one moves nothing of yours into it,
// and you become its owner with its first seat. Inviting people, changing
// roles and seats, and choosing an organization as the context of other
// verbs arrive with the service's next organization changes (org_members.go
// holds inviting, joining, roles, seats, the seat limit and the audit log).
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

type orgSeats struct {
	Used  int64  `json:"used"`
	Limit *int64 `json:"limit"`
}

type customerOrg struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Revision  int64    `json:"revision"`
	Seats     orgSeats `json:"seats"`
	YourRole  string   `json:"your_role"`
	YourSeat  string   `json:"your_seat"`
}

type customerOrgMember struct {
	AccountID string `json:"account_id"`
	Role      string `json:"role"`
	SeatState string `json:"seat_state"`
	JoinedAt  string `json:"joined_at"`
	InvitedBy string `json:"invited_by,omitempty"`
	You       bool   `json:"you,omitempty"`
	Revision  int64  `json:"revision"`
}

// orgPagesMax bounds how many pages a list reads before it says it stopped.
const orgPagesMax = 50

// orgNameProblem is the service's rule for a name, checked here so a name
// the service refuses is never sent.
func orgNameProblem(name string) string {
	if name == "" || strings.TrimSpace(name) != name || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 {
		return "an organization's name is 1 to 80 characters with no space at either end"
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "an organization's name carries no control characters or line breaks"
		}
	}
	return ""
}

// orgErr turns a refusal into words that say what happened to the
// organization verbs; a control plane that predates them answers a bare 404.
func orgErr(err error, nothing string) error {
	var he *hostedErr
	if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) && he.Type == "" {
		return &cliError{Code: exitFailed, Kind: "organizations_unsupported", Message: "this control plane does not serve organizations; " + nothing}
	}
	return err
}

func orgSeatsText(s orgSeats) string {
	if s.Limit == nil {
		return fmt.Sprintf("%d used, no limit", s.Used)
	}
	return fmt.Sprintf("%d of %d used", s.Used, *s.Limit)
}

func orgYourSeatText(seat string) string {
	switch seat {
	case "assigned":
		return "you hold a seat"
	case "unassigned":
		return "you hold no seat, so the organization's work is closed to you; an admin can assign you one"
	case "suspended":
		return "your seat is suspended, so the organization's work is closed to you"
	}
	return "your seat: " + seat
}

func hostedOrgCreate(cr hostedCreds, inv *Invocation) {
	name := inv.Arg(0)
	if why := orgNameProblem(name); why != "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: why + "; nothing was created"})
	}
	var env struct {
		Data customerOrg `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs", map[string]any{"name": name}, &env); err != nil {
		die(orgErr(err, "nothing was created"))
	}
	o := env.Data
	emit(o, func() {
		fmt.Printf("Organization %q created: %s\n", o.Name, o.ID)
		fmt.Println("You are its owner and hold its first seat. Nothing of your personal account moved into it.")
		fmt.Printf("Seats: %s\n", orgSeatsText(o.Seats))
	})
}

func fetchOrgs(cr hostedCreds) ([]customerOrg, bool, error) {
	var all []customerOrg
	cursor := ""
	for page := 0; page < orgPagesMax; page++ {
		path := "/api/v2/orgs"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerOrg `json:"items"`
				NextCursor string        `json:"next_cursor"`
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

func hostedOrgList(cr hostedCreds, inv *Invocation) {
	orgs, complete, err := fetchOrgs(cr)
	if err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(map[string]any{"organizations": orgs, "count": len(orgs), "complete": complete}, func() {
		if len(orgs) == 0 {
			fmt.Println("You belong to no organization. Create one: ks organization create NAME")
			return
		}
		fmt.Printf("%-28s %-8s %-11s %-18s %s\n", "ORGANIZATION", "ROLE", "YOUR SEAT", "SEATS", "NAME")
		for _, o := range orgs {
			fmt.Printf("%-28s %-8s %-11s %-18s %s\n", o.ID, o.YourRole, o.YourSeat, orgSeatsText(o.Seats), o.Name)
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}

func hostedOrgShow(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var env struct {
		Data customerOrg `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(id), nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	o := env.Data
	emit(o, func() {
		fmt.Printf("%s  %s\n", o.ID, o.Name)
		fmt.Printf("  state %s · seats %s\n", o.State, orgSeatsText(o.Seats))
		fmt.Printf("  your role %s · %s\n", o.YourRole, orgYourSeatText(o.YourSeat))
		fmt.Printf("  created %s · revision %d\n", o.CreatedAt, o.Revision)
	})
}

// fetchOrgMembers reads every page of an organization's members.
func fetchOrgMembers(cr hostedCreds, org string) ([]customerOrgMember, bool, error) {
	var all []customerOrgMember
	cursor := ""
	for page := 0; page < orgPagesMax; page++ {
		path := "/api/v2/orgs/" + url.PathEscape(org) + "/members"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerOrgMember `json:"items"`
				NextCursor string              `json:"next_cursor"`
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

func hostedMemberList(cr hostedCreds, inv *Invocation) {
	org := strings.TrimSpace(inv.Str("organization"))
	if org == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the organization: --organization ID (ks organization list shows yours); nothing was read", NextAction: "ks organization list"})
	}
	all, complete, err := fetchOrgMembers(cr, org)
	if err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(map[string]any{"organization": org, "members": all, "count": len(all), "complete": complete}, func() {
		fmt.Printf("%-40s %-8s %-11s %s\n", "MEMBER", "ROLE", "SEAT", "JOINED")
		for _, m := range all {
			who := m.AccountID
			if m.You {
				who += " (you)"
			}
			fmt.Printf("%-40s %-8s %-11s %s\n", who, m.Role, m.SeatState, m.JoinedAt)
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}
