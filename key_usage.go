// key_usage.go: ks key usage, what each key has been used for in a period:
// tokens in and out, cache tokens and spend, with a total.
//
// Spend is said for what it is, as the service sends it. On KeepState's key it
// is a charge. On your own provider key it is an ESTIMATE at list price, and
// your provider bills you, not KeepState. A Claude plan has no price per
// token, so only its tokens are shown. An estimate is never printed as a
// charge, and the two are never added into one figure.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type keySpendDoc struct {
	Kind           string `json:"kind"`
	Micros         *int64 `json:"micros"`
	Words          string `json:"words"`
	UnpricedTokens int64  `json:"unpriced_tokens"`
}

type keyUsageRowDoc struct {
	KeyID               string      `json:"key_id"`
	Kind                string      `json:"kind"`
	Provider            string      `json:"provider"`
	Alias               string      `json:"alias"`
	Last4               string      `json:"last4"`
	Label               string      `json:"label"`
	Calls               int64       `json:"calls"`
	InputTokens         int64       `json:"input_tokens"`
	OutputTokens        int64       `json:"output_tokens"`
	CacheCreationTokens int64       `json:"cache_creation_tokens"`
	CacheReadTokens     int64       `json:"cache_read_tokens"`
	Spend               keySpendDoc `json:"spend"`
}

type keyUsageDoc struct {
	Period struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	} `json:"period"`
	Rows  []keyUsageRowDoc `json:"rows"`
	Total struct {
		Calls               int64        `json:"calls"`
		InputTokens         int64        `json:"input_tokens"`
		OutputTokens        int64        `json:"output_tokens"`
		CacheCreationTokens int64        `json:"cache_creation_tokens"`
		CacheReadTokens     int64        `json:"cache_read_tokens"`
		Charged             *keySpendDoc `json:"charged"`
		Estimate            *keySpendDoc `json:"estimate"`
	} `json:"total"`
	Notes []string `json:"notes"`
}

// usdText is micro-USD as a person reads it: cents above a cent, four places below.
func usdText(micros int64) string {
	if micros >= 10_000 {
		return fmt.Sprintf("$%s.%02d", commas(micros/1_000_000), (micros%1_000_000)/10_000)
	}
	return fmt.Sprintf("$0.%04d", micros/100)
}

// spendText is one spend as the screen says it: the kind is in the words.
func spendText(s keySpendDoc) string {
	switch s.Kind {
	case "charged":
		if s.Micros == nil {
			return "unavailable"
		}
		return usdText(*s.Micros) + " charged"
	case "estimate":
		if s.Micros == nil {
			return "no estimate (no list price)"
		}
		if *s.Micros == 0 && s.UnpricedTokens == 0 {
			return "none"
		}
		pre := "about "
		if s.UnpricedTokens > 0 {
			pre = "at least about "
		}
		return pre + usdText(*s.Micros) + " (estimate)"
	case "plan_covered":
		return "covered by the plan"
	}
	return "no charge to you"
}

func keyUsageName(r keyUsageRowDoc) string {
	if r.Kind == "api_key" {
		n := r.Provider
		if r.Alias != "" {
			n += " " + r.Alias
		}
		return n + " …" + r.Last4
	}
	return r.Label
}

var keyUsageCols = []Col{{Title: "Key", Flex: true}, {Title: "In", Right: true}, {Title: "Out", Right: true}, {Title: "Cache", Right: true}, {Title: "Spend", Wrap: true}}

func keyUsageScreen(org string, d keyUsageDoc) listScreen {
	title := "Usage by key"
	if org != "" {
		title = "Usage by key of the organization " + org
	}
	s := listScreen{Title: title, Detail: d.Period.Label, Cols: keyUsageCols}
	for _, r := range d.Rows {
		s.Rows = append(s.Rows, []string{keyUsageName(r), commas(r.InputTokens), commas(r.OutputTokens), commas(r.CacheCreationTokens + r.CacheReadTokens), spendText(r.Spend)})
	}
	var tot []string
	if d.Total.Charged != nil {
		tot = append(tot, spendText(*d.Total.Charged))
	}
	if d.Total.Estimate != nil {
		tot = append(tot, spendText(*d.Total.Estimate))
	}
	if len(tot) == 0 {
		tot = []string{"none"}
	}
	s.Rows = append(s.Rows, []string{"Total", commas(d.Total.InputTokens), commas(d.Total.OutputTokens), commas(d.Total.CacheCreationTokens + d.Total.CacheReadTokens), strings.Join(tot, "; ")})
	s.Notes = append(s.Notes, "Cache is tokens written to and read from the provider's cache.")
	s.Notes = append(s.Notes, d.Notes...)
	return s
}

func hostedKeyUsage(cr hostedCreds, inv *Invocation) {
	org := keyContext(cr)
	period := inv.Str("period")
	if period != "" && period != "month" && period != "30d" && period != "all" {
		fail(&cliError{Code: exitUsage, Kind: "usage", WorkStarted: workNo, Message: "--period is month, 30d or all", NextAction: "ks key usage --period 30d"})
	}
	path := "/api/v2/keys/usage"
	if arg := inv.Arg(0); arg != "" {
		id := arg
		k, err := resolveKeyIn(cr, org, arg)
		switch {
		case err == nil:
			id = k.ID
		case strings.HasPrefix(arg, "vlt_"):
			// an id the inventory does not list (a member's Claude plan, a removed
			// key) is asked for as it is; the service answers for what it may show
		default:
			die(err)
		}
		path = "/api/v2/keys/" + url.PathEscape(id) + "/usage"
	}
	if period != "" {
		path += "?period=" + url.QueryEscape(period)
	}
	var env struct {
		Data keyUsageDoc `json:"data"`
	}
	if err := hostedCall(cr, "GET", keyPath(org, path), nil, &env); err != nil {
		die(keyOrgErr(err, org))
	}
	d := env.Data
	emit(d, func() {
		o, _ := cmdUI()
		o.List(keyUsageScreen(org, d))
	})
}
