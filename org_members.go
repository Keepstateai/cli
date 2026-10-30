// org_members.go: inviting people into an organization and joining one,
// roles and seats, renaming an organization and its seat limit, and its
// audit log. Every change names the record's revision the service last
// answered, so two admins changing one member at once cannot overwrite each
// other: the second is refused and reads again.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type customerOrgInvitation struct {
	ID                string `json:"id"`
	OrgID             string `json:"org_id"`
	Email             string `json:"email"`
	Role              string `json:"role"`
	AssignSeat        bool   `json:"assign_seat"`
	State             string `json:"state"`
	InvitedBy         string `json:"invited_by"`
	CreatedAt         string `json:"created_at"`
	ExpiresAt         string `json:"expires_at"`
	PendingAccountID  string `json:"pending_account_id,omitempty"`
	PendingEmail      string `json:"pending_email,omitempty"`
	AcceptedAccountID string `json:"accepted_account_id,omitempty"`
	Revision          int64  `json:"revision"`
}

type customerOrgAuditEvent struct {
	Seq        int64             `json:"seq"`
	ActorKind  string            `json:"actor_kind"`
	ActorID    string            `json:"actor_id"`
	Action     string            `json:"action"`
	TargetKind string            `json:"target_kind"`
	TargetID   string            `json:"target_id"`
	RequestID  string            `json:"request_id"`
	Detail     map[string]string `json:"detail"`
	RecordedAt string            `json:"recorded_at"`
	PrevDigest string            `json:"prev_digest"`
	Digest     string            `json:"digest"`
}

var orgRoleWords = []string{"owner", "admin", "billing", "auditor", "member"}

func orgFlag(inv *Invocation, nothing string) string {
	org := strings.TrimSpace(inv.Str("organization"))
	if org == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the organization: --organization ID (ks organization list shows yours); " + nothing, NextAction: "ks organization list"})
	}
	return org
}

// ---- the organization ----------------------------------------------------------------

func hostedOrgSet(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	body := map[string]any{}
	if inv.Set("name") {
		name := inv.Str("name")
		if why := orgNameProblem(name); why != "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: why + "; nothing was changed"})
		}
		body["name"] = name
	}
	switch {
	case inv.Set("seat-limit") && inv.Bool("no-seat-limit"):
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--seat-limit and --no-seat-limit are two different changes; give one; nothing was changed"})
	case inv.Set("seat-limit"):
		if inv.Int("seat-limit") < 1 {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--seat-limit is 1 or more; nothing was changed"})
		}
		body["seat_limit"] = inv.Int("seat-limit")
	case inv.Bool("no-seat-limit"):
		body["no_seat_limit"] = true
	}
	if len(body) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give --name, --seat-limit or --no-seat-limit; nothing was changed"})
	}
	var cur struct {
		Data customerOrg `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(id), nil, &cur); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	body["expected_revision"] = cur.Data.Revision
	var env struct {
		Data customerOrg `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/orgs/"+url.PathEscape(id), body, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	o := env.Data
	emit(o, func() {
		fmt.Printf("%s  %s\n", o.ID, o.Name)
		fmt.Printf("  seats %s · revision %d\n", orgSeatsText(o.Seats), o.Revision)
	})
}

func hostedOrgAudit(cr hostedCreds, inv *Invocation) {
	id := inv.Arg(0)
	var all []customerOrgAuditEvent
	cursor, complete := "", false
	for page := 0; page < orgPagesMax; page++ {
		path := "/api/v2/orgs/" + url.PathEscape(id) + "/audit"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerOrgAuditEvent `json:"items"`
				NextCursor string                  `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
			die(orgErr(err, "nothing was read"))
		}
		all = append(all, env.Data.Items...)
		if env.Data.NextCursor == "" {
			complete = true
			break
		}
		cursor = env.Data.NextCursor
	}
	// the chain, as this client read it: each event names the digest of the
	// one before it
	chained := true
	for i := 1; i < len(all); i++ {
		if all[i].PrevDigest != all[i-1].Digest || all[i].Seq != all[i-1].Seq+1 {
			chained = false
		}
	}
	emit(map[string]any{"organization": id, "events": all, "count": len(all), "complete": complete, "chained": chained}, func() {
		if len(all) == 0 {
			fmt.Println("No events you may read.")
			return
		}
		fmt.Printf("%-5s %-31s %-28s %-36s %s\n", "SEQ", "WHEN", "ACTION", "BY", "ON")
		for _, a := range all {
			fmt.Printf("%-5d %-31s %-28s %-36s %s %s\n", a.Seq, a.RecordedAt, a.Action, a.ActorID, a.TargetKind, a.TargetID)
		}
		if !chained {
			fmt.Println("The events read here do not chain one to the next: some are missing from what you may read, or the log was changed.")
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the log continues on the service.\n", orgPagesMax)
		}
	})
}

// ---- roles and seats ----------------------------------------------------------------

// orgMemberChange reads the member's revision and sends one change of it.
func orgMemberChange(cr hostedCreds, org, account string, change map[string]any, nothing string) customerOrgMember {
	members, _, err := fetchOrgMembers(cr, org)
	if err != nil {
		die(orgErr(err, nothing))
	}
	var cur *customerOrgMember
	for i := range members {
		if members[i].AccountID == account {
			cur = &members[i]
		}
	}
	if cur == nil {
		fail(&cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("%s is not a member of %s (ks member list --organization %s); %s", account, org, org, nothing)})
	}
	change["expected_revision"] = cur.Revision
	var env struct {
		Data customerOrgMember `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/orgs/"+url.PathEscape(org)+"/members/"+url.PathEscape(account), change, &env); err != nil {
		die(orgErr(err, nothing))
	}
	return env.Data
}

func memberLine(m customerOrgMember) {
	fmt.Printf("%s: role %s, seat %s\n", m.AccountID, m.Role, m.SeatState)
}

func hostedMemberRole(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	role := inv.Arg(1)
	known := false
	for _, r := range orgRoleWords {
		known = known || r == role
	}
	if !known {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "a role is owner, admin, billing, auditor or member; nothing was changed"})
	}
	m := orgMemberChange(cr, org, inv.Arg(0), map[string]any{"role": role}, "nothing was changed")
	emit(m, func() { memberLine(m) })
}

func hostedSeat(assign bool) func(hostedCreds, *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		org := orgFlag(inv, "nothing was changed")
		state := "unassigned"
		if assign {
			state = "assigned"
		}
		m := orgMemberChange(cr, org, inv.Arg(0), map[string]any{"seat_state": state}, "nothing was changed")
		emit(m, func() {
			memberLine(m)
			if m.SeatState != "assigned" {
				fmt.Println("Without a seat, the organization's work is closed to this member.")
			}
		})
	}
}

// ---- invitations ------------------------------------------------------------------------

func hostedMemberInvite(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was sent")
	email := strings.TrimSpace(inv.Arg(0))
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\n") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give one address, like name@example.com; nothing was sent"})
	}
	role := "member"
	if inv.Set("role") {
		role = inv.Str("role")
	}
	known := false
	for _, r := range orgRoleWords {
		known = known || r == role
	}
	if !known {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "a role is owner, admin, billing, auditor or member; nothing was sent"})
	}
	// not resent on a lost answer: the token is shown once, and a second
	// invitation to the address is refused while this one is open
	var env struct {
		Data struct {
			Invitation customerOrgInvitation `json:"invitation"`
			Token      string                `json:"token"`
			EmailSent  bool                  `json:"email_sent"`
			EmailNote  string                `json:"email_note"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/invitations", map[string]any{"email": email, "role": role, "assign_seat": inv.Bool("seat")}, &env); err != nil {
		die(orgErr(err, "nothing was sent"))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("Invited %s as %s: %s (expires %s)\n", d.Invitation.Email, d.Invitation.Role, d.Invitation.ID, d.Invitation.ExpiresAt)
		if d.Invitation.AssignSeat {
			fmt.Println("They take a seat when they join, if one is free then.")
		}
		if d.EmailSent {
			fmt.Println("The invitation email was sent.")
		} else {
			fmt.Println("The invitation email was not sent: " + d.EmailNote)
		}
		fmt.Println("The invitation's token, shown this once; it works only for the invited address:")
		fmt.Println("  ks invite accept " + d.Token)
	})
}

func hostedInviteList(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was read")
	var all []customerOrgInvitation
	cursor, complete := "", false
	for page := 0; page < orgPagesMax; page++ {
		path := "/api/v2/orgs/" + url.PathEscape(org) + "/invitations"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerOrgInvitation `json:"items"`
				NextCursor string                  `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
			die(orgErr(err, "nothing was read"))
		}
		all = append(all, env.Data.Items...)
		if env.Data.NextCursor == "" {
			complete = true
			break
		}
		cursor = env.Data.NextCursor
	}
	emit(map[string]any{"organization": org, "invitations": all, "count": len(all), "complete": complete}, func() {
		if len(all) == 0 {
			fmt.Println("No invitations.")
			return
		}
		fmt.Printf("%-22s %-32s %-8s %-22s %s\n", "INVITATION", "ADDRESS", "ROLE", "STATE", "EXPIRES")
		for _, v := range all {
			fmt.Printf("%-22s %-32s %-8s %-22s %s\n", v.ID, v.Email, v.Role, v.State, v.ExpiresAt)
			if v.State == "awaiting_confirmation" {
				who := v.PendingEmail
				if who == "" {
					who = "an account with no address"
				}
				fmt.Printf("  %s asked to join signed in as %s: ks invite confirm %s --organization %s --account %s\n", v.PendingAccountID, who, v.ID, org, v.PendingAccountID)
			}
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}

func hostedInviteRevoke(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	var env struct {
		Data customerOrgInvitation `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/orgs/"+url.PathEscape(org)+"/invitations/"+url.PathEscape(inv.Arg(0)), nil, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	v := env.Data
	emit(v, func() { fmt.Printf("Invitation %s to %s withdrawn; its token no longer works.\n", v.ID, v.Email) })
}

func hostedInviteConfirm(cr hostedCreds, inv *Invocation) {
	org := orgFlag(inv, "nothing was changed")
	account := strings.TrimSpace(inv.Str("account"))
	if account == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the account that asked to join: --account ID (ks invite list shows it); nothing was changed"})
	}
	var env struct {
		Data customerOrgMember `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/invitations/"+url.PathEscape(inv.Arg(0))+"/confirm", map[string]any{"account_id": account}, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	m := env.Data
	emit(m, func() {
		fmt.Printf("%s joined %s: ", m.AccountID, org)
		memberLine(m)
	})
}

// inviteToken reads the token from what was pasted: the token, or any text
// that carries it (an email line, a link).
func inviteToken(s string) string {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "ksi_")
	if i < 0 {
		return ""
	}
	s = s[i:]
	end := len(s)
	for j, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			end = j
			break
		}
	}
	return s[:end]
}

func hostedInviteAccept(cr hostedCreds, inv *Invocation) {
	tok := inviteToken(inv.Arg(0))
	if tok == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give the invitation's token (it starts with ksi_) or the text that carries it; nothing was sent"})
	}
	var env struct {
		Data struct {
			Org    customerOrg       `json:"org"`
			Member customerOrgMember `json:"member"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/invitations/accept", map[string]any{"token": tok}, &env); err != nil {
		die(orgErr(err, "you have not joined"))
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("You joined %q (%s) as %s.\n", d.Org.Name, d.Org.ID, d.Member.Role)
		fmt.Println(orgYourSeatText(d.Member.SeatState) + ".")
		fmt.Println("Your personal account and its work are unchanged.")
	})
}
