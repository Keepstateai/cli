// org_billing.go: an organization's billing details and billing contacts on
// the command line. The details (legal name, address, country, VAT or tax id
// and the default purchase order number) are what the organization's
// purchases and statements will carry; changing them changes the next
// statement only, never one already issued. Owner and billing change them;
// admin and auditor read them.
package main

import (
	"errors"
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

// ---- buying credit as the organization ---------------------------------------------

type customerOrgCheckout struct {
	CheckoutID   string `json:"checkout_id"`
	URL          string `json:"url"`
	AmountMicros int64  `json:"amount_micros"`
	OrgID        string `json:"org_id"`
	OrgName      string `json:"org_name"`
	BillTo       struct {
		LegalName string `json:"legal_name"`
		Address   string `json:"address"`
		TaxID     string `json:"tax_id"`
		TaxIDSent bool   `json:"tax_id_sent"`
	} `json:"bill_to"`
	PONumber string `json:"po_number"`
	Note     string `json:"note"`
}

// hostedOrgBuy shows who the purchase is for and what the receipt will name,
// and opens the checkout only when that is confirmed (--yes); the payment
// itself happens on Polar's page, never in this client.
func hostedOrgBuy(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was bought"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	amount := inv.Int("amount")
	if !inv.Set("amount") || amount < 1_000_000 || amount%10_000 != 0 || amount > 10_000_000_000 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--amount is whole cents from $1 to $10,000, such as --amount 50; " + nothing})
	}
	b := fetchOrgBilling(cr, org, nothing)
	if len(b.Missing) > 0 || b.Profile == nil {
		fail(&cliError{Code: exitConflict, Kind: "billing_details_incomplete", Message: "the organization's receipt needs its billing details first: " + strings.Join(b.Missing, ", ") + "; " + nothing, NextAction: "ks organization billing set " + org})
	}
	p := b.Profile
	withTax := p.TaxID != "" && !inv.Bool("without-tax-id")
	receipt := p.LegalName
	switch {
	case withTax:
		receipt += ", VAT id " + p.TaxID
	case p.TaxID != "":
		receipt += ", without its VAT id (Polar will charge VAT)"
	}
	if !out.yes {
		preview := map[string]any{"organization": org, "amount_micros": amount, "bill_to": p.LegalName, "address": orgBillingAddress(p), "tax_id_sent": withTax, "confirmed": false}
		emit(preview, func() {
			fmt.Printf("You are buying %s of credit for the organization %s. The receipt from Polar will name %s.\n", dollars(amount), org, receipt)
			fmt.Printf("  billed to  %s\n", orgBillingAddress(p))
			fmt.Println("The credit is the organization's, never yours. Nothing was bought; run again with --yes to open the payment page.")
		})
		return
	}
	body := map[string]any{"amount_micros": amount}
	if inv.Set("po-number") {
		body["po_number"] = inv.Str("po-number")
	}
	if inv.Bool("without-tax-id") {
		body["without_tax_id"] = true
	}
	var env struct {
		Data customerOrgCheckout `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/billing/checkout", body, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_tax_id_refused" {
			fail(&cliError{Code: exitConflict, Kind: "tax_id_refused", Message: he.Message + ". Two ways on: correct it (ks organization billing set " + org + " --tax-id ...) and buy again, or buy without it (ks organization buy " + org + " --amount ... --without-tax-id --yes)", NextAction: "ks organization billing set " + org})
		}
		die(orgErr(err, nothing))
	}
	c := env.Data
	emit(c, func() {
		fmt.Println(c.Note)
		fmt.Printf("Pay on Polar's page: %s\n", c.URL)
		fmt.Println("The credit arrives when Polar confirms the payment: ks organization credits " + org)
	})
}

type customerOrgCredits struct {
	OrgID     string `json:"org_id"`
	Balance   int64  `json:"balance_micros"`
	Purchased int64  `json:"purchased_micros"`
	HasBought bool   `json:"has_bought"`
	Entries   []struct {
		Seq          int64  `json:"seq"`
		TS           string `json:"ts"`
		Kind         string `json:"kind"`
		AmountMicros int64  `json:"amount_micros"`
		Ref          string `json:"ref"`
		BalanceAfter int64  `json:"balance_after_micros"`
	} `json:"entries"`
	More   bool `json:"more"`
	Orders []struct {
		CheckoutID   string `json:"checkout_id"`
		OrderID      string `json:"order_id"`
		AmountMicros int64  `json:"amount_micros"`
		Status       string `json:"status"`
		PONumber     string `json:"po_number"`
		CreatedAt    string `json:"created_at"`
	} `json:"orders"`
	Note string `json:"note"`
}

func hostedOrgCredits(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Arg(0), "nothing was read")
	var env struct {
		Data customerOrgCredits `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/credits", nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	c := env.Data
	emit(c, func() {
		fmt.Printf("The organization %s's credit: %s (bought: %s)\n", org, receiptMoney(c.Balance), receiptMoney(c.Purchased))
		if !c.HasBought {
			fmt.Println("It has bought no credit yet: buy some with ks organization buy " + org + " --amount 50")
		}
		if len(c.Orders) > 0 {
			fmt.Println("Purchases:")
			for _, o := range c.Orders {
				po := ""
				if o.PONumber != "" {
					po = "  PO " + o.PONumber
				}
				fmt.Printf("  %s  %-8s %s%s\n", o.CreatedAt, o.Status, receiptMoney(o.AmountMicros), po)
			}
		}
		if len(c.Entries) > 0 {
			fmt.Println("Ledger, newest first:")
			for _, e := range c.Entries {
				fmt.Printf("  %s  %-6s %12s  balance %s\n", e.TS, e.Kind, receiptMoney(e.AmountMicros), receiptMoney(e.BalanceAfter))
			}
		}
		if c.More {
			fmt.Println("Older ledger rows are kept on the service.")
		}
	})
}

// ---- statements ------------------------------------------------------------------

type customerOrgStatementRow struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	From        string `json:"period_from"`
	To          string `json:"period_to"`
	Total       int64  `json:"statement_total_micros"`
	ContentHash string `json:"content_hash"`
	IssuedAt    string `json:"issued_at"`
	BillTo      string `json:"bill_to"`
	PONumber    string `json:"po_number"`
	Corrects    string `json:"corrects,omitempty"`
}

type customerOrgStatement struct {
	Statement struct {
		ID          string `json:"id"`
		Seq         int64  `json:"seq"`
		From        string `json:"period_from"`
		To          string `json:"period_to"`
		Total       int64  `json:"statement_total_micros"`
		ContentHash string `json:"content_hash"`
		Corrects    string `json:"corrects"`
	} `json:"statement"`
	Text        string   `json:"text"`
	CorrectedBy []string `json:"corrected_by"`
}

func orgPeriod(from, to string) string {
	if from == "" {
		from = "the start"
	}
	if to == "" {
		to = "now"
	}
	return from + " to " + to
}

func hostedOrgStatements(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Arg(0), "nothing was read")
	var env struct {
		Data struct {
			Items []customerOrgStatementRow `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/statements", nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	st := env.Data.Items
	emit(map[string]any{"organization": org, "statements": st, "count": len(st)}, func() {
		if len(st) == 0 {
			fmt.Println("No statements yet. An owner or billing member issues one: ks organization statement issue " + org)
			return
		}
		fmt.Printf("%-14s %-4s %-12s %-28s %s\n", "STATEMENT", "#", "TOTAL", "BILLED TO", "PERIOD")
		for _, s := range st {
			extra := ""
			if s.Corrects != "" {
				extra = " (corrects " + s.Corrects + ")"
			}
			fmt.Printf("%-14s %-4d %-12s %-28s %s%s\n", s.ID, s.Seq, receiptMoney(s.Total), s.BillTo, orgPeriod(s.From, s.To), extra)
		}
	})
}

// hostedOrgStatementIssue issues the organization's statement for a period,
// or a correction of one. Issuing settles the period's usage, as every
// statement does, so it asks for --yes first.
func hostedOrgStatementIssue(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was issued"
	org := orgArgOrContext(cr, inv.Arg(0), nothing)
	body := map[string]any{}
	for _, f := range []struct{ flag, key string }{{"from", "from"}, {"to", "to"}, {"po-number", "po_number"}, {"corrects", "corrects"}} {
		if inv.Set(f.flag) {
			body[f.key] = inv.Str(f.flag)
		}
	}
	if !out.yes {
		what := "a statement for " + orgPeriod(inv.Str("from"), inv.Str("to"))
		if inv.Set("corrects") {
			what = "a correction of " + inv.Str("corrects") + " (a new statement for its period; it stands as issued)"
		}
		emit(map[string]any{"organization": org, "request": body, "confirmed": false}, func() {
			fmt.Printf("This issues %s for the organization %s, with its billing details as they are now; the statement never changes afterwards, and issuing it settles the period's usage from the organization's credit.\n", what, org)
			fmt.Println("Nothing was issued; run again with --yes.")
		})
		return
	}
	var env struct {
		Data customerOrgStatement `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/statements", body, &env); err != nil {
		die(orgErr(err, nothing))
	}
	s := env.Data
	emit(s, func() {
		fmt.Print(s.Text)
		fmt.Printf("\nIssued %s. Check it any time: ks organization statement verify %s --organization %s\n", s.Statement.ID, s.Statement.ID, org)
	})
}

func hostedOrgStatementShow(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Str("organization"), "nothing was read")
	var env struct {
		Data customerOrgStatement `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(org)+"/statements/"+url.PathEscape(inv.Arg(0)), nil, &env); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	s := env.Data
	emit(s, func() {
		fmt.Print(s.Text)
		if len(s.CorrectedBy) > 0 {
			fmt.Printf("\nCorrected by: %s (this statement stands as issued)\n", strings.Join(s.CorrectedBy, ", "))
		}
	})
}

func hostedOrgStatementVerify(cr hostedCreds, inv *Invocation) {
	org := orgArgOrContext(cr, inv.Str("organization"), "nothing was read")
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/statements/"+url.PathEscape(inv.Arg(0))+"/verify", nil, &env); err != nil {
		die(orgErr(err, "nothing was checked"))
	}
	v := env.Data
	ok, _ := v["reconciled"].(bool)
	emit(v, func() {
		if ok {
			fmt.Printf("%s verifies: its total, its content hash, its chain, its billing details and the engine's statement all agree.\n", inv.Arg(0))
			return
		}
		var bad []string
		for _, k := range []string{"self_total_ok", "content_hash_ok", "chain_ok", "live_statement_ok", "billing_profile_ok"} {
			if b, present := v[k].(bool); present && !b {
				bad = append(bad, strings.TrimSuffix(k, "_ok"))
			}
		}
		fmt.Printf("%s does NOT verify: %s.\n", inv.Arg(0), strings.Join(bad, ", "))
	})
	if !ok {
		fail(&cliError{Code: exitFailed, Kind: "statement_unverified", Message: inv.Arg(0) + " does not verify"})
	}
}
