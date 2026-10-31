// org_billing.go: an organization's billing details and billing contacts on
// the command line. The details (legal name, address, country, VAT or tax id
// and the default purchase order number) are what the organization's
// purchases and statements will carry; changing them changes the next
// statement only, never one already issued. Owner and billing change them;
// admin and auditor read them.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type customerOrgBillingProfile struct {
	LegalName    string `json:"legal_name"`
	AddressLine1 string `json:"address_line1"`
	AddressLine2 string `json:"address_line2"`
	City         string `json:"city"`
	Region       string `json:"region"`
	PostalCode   string `json:"postal_code"`
	Country      string `json:"country"`
	TaxID        string `json:"tax_id"`
	TaxIDType    string `json:"tax_id_type"`
	PONumber     string `json:"po_number"`
	Revision     int64  `json:"revision"`
	UpdatedBy    string `json:"updated_by"`
	UpdatedAt    string `json:"updated_at"`
	Digest       string `json:"digest"`
}

type customerOrgBillingContact struct {
	AccountID string `json:"account_id"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	AddedBy   string `json:"added_by"`
	AddedAt   string `json:"added_at"`
	Receives  bool   `json:"receives"`
}

type customerOrgBilling struct {
	OrgID    string                         `json:"org_id"`
	Profile  *customerOrgBillingProfile     `json:"profile"`
	Missing  []string                       `json:"missing"`
	Issuer   struct{ Name, Address string } `json:"issuer"`
	Contacts []customerOrgBillingContact    `json:"contacts"`
	Note     string                         `json:"note"`
	YourRole string                         `json:"your_role"`
	CanEdit  bool                           `json:"can_edit"`
}

// orgArgOrContext is the organization a verb acts on: the one named, or the
// context's (ks context use). A personal context names none, so the verb
// asks for one rather than guessing.
func orgArgOrContext(cr hostedCreds, named, nothing string) string {
	org := strings.TrimSpace(named)
	if org == "" {
		org = currentContext(cr)
	}
	if org == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the organization, or choose it first: ks context use ORGANIZATION; " + nothing, NextAction: "ks organization list"})
	}
	if !strings.HasPrefix(org, "org_") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: org + " is not an organization's id (ks organization list shows yours); " + nothing, NextAction: "ks organization list"})
	}
	return org
}

func fetchOrgBilling(cr hostedCreds, org, nothing string) customerOrgBilling {
	var env struct {
		Data customerOrgBilling `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/billing", nil, &env); err != nil {
		die(orgErr(err, nothing))
	}
	return env.Data
}

func orgBillingAddress(p *customerOrgBillingProfile) string {
	var parts []string
	for _, v := range []string{p.AddressLine1, p.AddressLine2, p.City, p.Region, p.PostalCode, p.Country} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ", ")
}

func printOrgBilling(b customerOrgBilling) {
	fmt.Printf("Billing details of %s\n", b.OrgID)
	if b.Profile == nil {
		fmt.Println("  none yet: a purchase and a statement need the legal name, address and country")
	} else {
		p := b.Profile
		fmt.Printf("  legal name   %s\n", p.LegalName)
		fmt.Printf("  address      %s\n", orgBillingAddress(p))
		if p.TaxID != "" {
			fmt.Printf("  tax id       %s (%s)\n", p.TaxID, p.TaxIDType)
		} else {
			fmt.Println("  tax id       none")
		}
		if p.PONumber != "" {
			fmt.Printf("  PO number    %s\n", p.PONumber)
		}
		fmt.Printf("  revision %d, changed %s by %s\n", p.Revision, p.UpdatedAt, p.UpdatedBy)
	}
	if len(b.Missing) > 0 {
		fmt.Printf("  still needed to buy credit and issue a statement: %s\n", strings.Join(b.Missing, ", "))
	}
	fmt.Printf("Statements are issued by %s, %s.\n", b.Issuer.Name, b.Issuer.Address)
	if len(b.Contacts) == 0 {
		fmt.Println("No billing contacts: statement notices and credit alerts go to the organization's owners.")
	} else {
		fmt.Println("Billing contacts:")
		for _, c := range b.Contacts {
			fmt.Printf("  %s  %s%s\n", c.AccountID, c.Kind, orgContactReceives(c))
		}
	}
	if b.Note != "" {
		fmt.Println(b.Note)
	}
}

func orgContactReceives(c customerOrgBillingContact) string {
	if c.Receives {
		return ""
	}
	return " (receives nothing now: no longer a member with a seat in a role that reads the organization's money)"
}

func hostedOrgBillingShow(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Arg(0), "nothing was read")
	b := fetchOrgBilling(cr, org, "nothing was read")
	emit(b, func() { printOrgBilling(b) })
}

// hostedOrgBillingSet reads the current details, changes the fields given and
// sends the whole profile with the revision it read, so a change made
// meanwhile by someone else is refused rather than overwritten.
func hostedOrgBillingSet(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	fields := []struct{ flag, key string }{
		{"legal-name", "legal_name"}, {"address-line1", "address_line1"}, {"address-line2", "address_line2"}, {"city", "city"},
		{"region", "region"}, {"postal-code", "postal_code"}, {"country", "country"}, {"tax-id", "tax_id"}, {"tax-id-type", "tax_id_type"}, {"po-number", "po_number"},
	}
	given := inv.Bool("no-tax-id")
	for _, f := range fields {
		given = given || inv.Set(f.flag)
	}
	if !given {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give at least one detail to change, such as --legal-name or --country; " + nothing})
	}
	if inv.Bool("no-tax-id") && (inv.Set("tax-id") || inv.Set("tax-id-type")) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--no-tax-id removes the tax id; it is not given with --tax-id or --tax-id-type; " + nothing})
	}
	if inv.Set("country") {
		if c := inv.Str("country"); len(c) != 2 || strings.ToUpper(c) != c {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--country is the two-letter code in capitals, such as US, DE or GB; " + nothing})
		}
	}
	cur := fetchOrgBilling(cr, org, nothing)
	body := map[string]any{"expected_revision": int64(0)}
	if p := cur.Profile; p != nil {
		body = map[string]any{"legal_name": p.LegalName, "address_line1": p.AddressLine1, "address_line2": p.AddressLine2, "city": p.City,
			"region": p.Region, "postal_code": p.PostalCode, "country": p.Country, "tax_id": p.TaxID, "tax_id_type": p.TaxIDType,
			"po_number": p.PONumber, "expected_revision": p.Revision}
	}
	for _, f := range fields {
		if inv.Set(f.flag) {
			body[f.key] = inv.Str(f.flag)
		}
	}
	if inv.Bool("no-tax-id") {
		body["tax_id"], body["tax_id_type"] = "", ""
	}
	var env struct {
		Data customerOrgBilling `json:"data"`
	}
	if err := hostedCall(cr, "PUT", "/api/v2/orgs/"+url.PathEscape(org)+"/billing/profile", body, &env); err != nil {
		die(orgErr(err, nothing))
	}
	b := env.Data
	emit(b, func() {
		printOrgBilling(b)
		fmt.Println("Saved. The next statement and the next purchase carry these details; statements already issued do not change.")
	})
}

func hostedOrgBillingContacts(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Arg(0), "nothing was read")
	var env struct {
		Data struct {
			Items []customerOrgBillingContact `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/billing/contacts", nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	cs := env.Data.Items
	emit(map[string]any{"organization": org, "contacts": cs, "count": len(cs)}, func() {
		if len(cs) == 0 {
			fmt.Println("No billing contacts: statement notices and credit alerts go to the organization's owners.")
			return
		}
		fmt.Printf("%-40s %-11s %-8s %s\n", "CONTACT", "RECEIVES", "ROLE", "ADDED")
		for _, c := range cs {
			fmt.Printf("%-40s %-11s %-8s %s%s\n", c.AccountID, c.Kind, c.Role, c.AddedAt, orgContactReceives(c))
		}
	})
}

func hostedOrgBillingContactAdd(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Str("organization"), nothing)
	kind := inv.Str("kind")
	if kind == "" {
		kind = "both"
	}
	if kind != "statements" && kind != "alerts" && kind != "both" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--kind is statements, alerts or both; " + nothing})
	}
	var env struct {
		Data customerOrgBillingContact `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/billing/contacts", map[string]any{"account_id": inv.Arg(0), "kind": kind}, &env); err != nil {
		die(orgErr(err, nothing))
	}
	c := env.Data
	emit(c, func() {
		fmt.Printf("%s is a billing contact of %s and receives %s by the address they sign in with.\n", c.AccountID, org, map[string]string{"both": "statement notices and credit alerts", "statements": "statement notices", "alerts": "credit alerts"}[c.Kind])
	})
}

func hostedOrgBillingContactRemove(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Str("organization"), nothing)
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "DELETE", "/api/v2/orgs/"+url.PathEscape(org)+"/billing/contacts/"+url.PathEscape(inv.Arg(0)), nil, &env); err != nil {
		die(orgErr(err, nothing))
	}
	emit(env.Data, func() { fmt.Printf("%s no longer receives %s's statement notices or alerts.\n", inv.Arg(0), org) })
}
