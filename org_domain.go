// org_domain.go: an organization's work domain and the one-time offer of it.
// A person who signs in with a verified work email lands in the organization
// of that work domain, made for them when none exists. The domain has a mode:
// auto means people who sign up with an address there join as members,
// approve means an owner or admin approves each one first. A person whose
// account already existed is offered the organization once, and joins it or
// turns it down with the offer verbs. The routes are dark until the service
// turns its work-domain switch on; while it is off every one answers with
// that refusal, and its words are printed as they come.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type customerOrgDomain struct {
	OrgID              string `json:"org_id"`
	Domain             string `json:"domain"`
	Mode               string `json:"mode"`
	CreatedAt          string `json:"created_at,omitempty"`
	PendingRequests    *int64 `json:"pending_requests,omitempty"`
	MembersWithoutSeat *int64 `json:"members_without_seat,omitempty"`
}

type customerDomainRequest struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt string `json:"decided_at,omitempty"`
}

type customerDomainOffer struct {
	State    string `json:"state"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Mode     string `json:"mode,omitempty"`
	SeatText string `json:"seat_state,omitempty"`
	Headline string `json:"headline,omitempty"`
	Words    string `json:"words,omitempty"`
}

var orgDomainModes = []string{"auto", "approve"}

func orgDomainPath(org string) string { return "/api/v2/orgs/" + url.PathEscape(org) + "/domain" }

func orgDomainModeText(mode string) string {
	switch mode {
	case "auto":
		return "auto: people who sign up with an address there join as members"
	case "approve":
		return "approve: an owner or admin approves each person who signs up with an address there"
	}
	return mode
}

func printOrgDomain(d customerOrgDomain) {
	if d.Domain == "" {
		fmt.Printf("%s holds no work domain.\n", d.OrgID)
		return
	}
	fmt.Printf("%s  work domain %s\n", d.OrgID, d.Domain)
	fmt.Printf("  mode %s\n", orgDomainModeText(d.Mode))
	if d.PendingRequests != nil {
		fmt.Printf("  waiting requests %d\n", *d.PendingRequests)
	}
	if d.MembersWithoutSeat != nil {
		fmt.Printf("  members without a seat %d\n", *d.MembersWithoutSeat)
	}
	if d.CreatedAt != "" {
		fmt.Printf("  claimed %s\n", d.CreatedAt)
	}
}

func hostedOrgDomainShow(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was read"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	var env struct {
		Data customerOrgDomain `json:"data"`
	}
	if err := hostedCall(cr, "GET", orgDomainPath(org), nil, &env); err != nil {
		die(orgErr(err, nothing))
	}
	d := env.Data
	emit(d, func() { printOrgDomain(d) })
}

func hostedOrgDomainSet(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	if !inv.Set("mode") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give --mode auto or --mode approve; " + nothing})
	}
	mode := inv.Str("mode")
	known := false
	for _, m := range orgDomainModes {
		known = known || m == mode
	}
	if !known {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "a mode is auto or approve; " + nothing})
	}
	var env struct {
		Data customerOrgDomain `json:"data"`
	}
	if err := hostedCall(cr, "PUT", orgDomainPath(org), map[string]any{"mode": mode}, &env); err != nil {
		die(orgErr(err, nothing))
	}
	d := env.Data
	emit(d, func() { printOrgDomain(d) })
}

func hostedOrgDomainRemove(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	var env struct {
		Data customerOrgDomain `json:"data"`
	}
	if err := hostedCall(cr, "DELETE", orgDomainPath(org), nil, &env); err != nil {
		die(orgErr(err, nothing))
	}
	d := env.Data
	if d.OrgID == "" {
		d.OrgID = org
	}
	emit(d, func() {
		fmt.Printf("%s no longer holds a work domain.\n", d.OrgID)
		fmt.Println("People who already joined stay members; nobody new joins by their address.")
	})
}

func fetchDomainRequests(cr hostedCreds, org string) ([]customerDomainRequest, bool, error) {
	var all []customerDomainRequest
	cursor := ""
	for page := 0; page < orgPagesMax; page++ {
		path := orgDomainPath(org) + "/requests"
		if cursor != "" {
			path += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		var env struct {
			Data struct {
				Items      []customerDomainRequest `json:"items"`
				NextCursor string                  `json:"next_cursor"`
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

func hostedOrgDomainRequests(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was read"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	all, complete, err := fetchDomainRequests(cr, org)
	if err != nil {
		die(orgErr(err, nothing))
	}
	emit(map[string]any{"organization": org, "requests": all, "count": len(all), "complete": complete}, func() {
		if len(all) == 0 {
			fmt.Println("No request is waiting.")
			return
		}
		fmt.Printf("%-28s %-36s %-8s %s\n", "REQUEST", "EMAIL", "STATE", "ASKED")
		for _, r := range all {
			fmt.Printf("%-28s %-36s %-8s %s\n", r.ID, r.Email, r.State, r.CreatedAt)
		}
		fmt.Printf("Decide one: ks organization domain approve %s REQUEST, or decline\n", org)
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}

// hostedOrgDomainDecide approves or declines one waiting request. A decided
// request is refused by the service, so a repeat changes nothing.
func hostedOrgDomainDecide(verb, past string) func(hostedCreds, *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		nothing := "nothing was " + past
		org := orgArgOrContext(cr, inv.Arg(0), nothing)
		req := strings.TrimSpace(inv.Arg(1))
		if req == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the request (ks organization domain requests " + org + " lists them); " + nothing})
		}
		var env struct {
			Data customerDomainRequest `json:"data"`
		}
		if err := hostedCall(cr, "POST", orgDomainPath(org)+"/requests/"+url.PathEscape(req)+"/"+verb, nil, &env); err != nil {
			die(orgErr(err, nothing))
		}
		r := env.Data
		emit(r, func() {
			fmt.Printf("Request %s from %s: %s\n", r.ID, r.Email, r.State)
			if r.State == "approved" {
				fmt.Println("They join the organization as a member.")
			}
		})
	}
}

// ---- the offer -------------------------------------------------------------------------

func printDomainOffer(o customerDomainOffer) {
	if o.Headline == "" && o.Words == "" {
		switch o.State {
		case "none", "":
			fmt.Println("You have no work-domain offer.")
		default:
			fmt.Printf("Your work-domain offer: %s\n", o.State)
		}
		return
	}
	if o.Headline != "" {
		fmt.Println(o.Headline)
	}
	if o.Words != "" {
		fmt.Println(o.Words)
	}
	if o.State == "offered" {
		fmt.Println("Join it: ks organization offer join. Turn it down: ks organization offer dismiss")
	}
}

func hostedOrgOfferShow(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data customerDomainOffer `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/domain-offer", nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	o := env.Data
	emit(o, func() { printDomainOffer(o) })
}

func hostedOrgOfferAnswer(verb, nothing string) func(hostedCreds, *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		var env struct {
			Data customerDomainOffer `json:"data"`
		}
		if err := hostedCall(cr, "POST", "/api/v2/domain-offer/"+verb, nil, &env); err != nil {
			die(orgErr(err, nothing))
		}
		o := env.Data
		emit(o, func() { printDomainOffer(o) })
	}
}
