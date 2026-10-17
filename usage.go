// usage.go: a session's usage as the service accounts for it (KS-018, the
// usage display of KS-037).
//
//	ks session usage <session>
//
// Model use, KS runtime and storage are shown apart, each with its units,
// where the figure came from and how firm it is (actual, estimated or
// unavailable). A figure the service does not have is printed as
// "unavailable", never as $0: a missing price is not a free call.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

type usageEntryRow struct {
	Kind         string `json:"kind"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	KeySource    string `json:"key_source,omitempty"`
	Calls        int64  `json:"calls,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	// Tokens is a total where only a total is metered (the gateway's meter
	// of a session's machine); Source says which ledger a row came from.
	Tokens       int64  `json:"tokens,omitempty"`
	Source       string `json:"source,omitempty"`
	ReservedLive int64  `json:"reserved_live_tokens,omitempty"`
	MicroUSD     *int64 `json:"microusd"`
	Currency     string `json:"currency"`
	PriceSource  string `json:"price_source_version"`
	Standing     string `json:"standing"`
	ObservedAt   string `json:"observed_at,omitempty"`
	Note         string `json:"note,omitempty"`
}

type sessionUsageDoc struct {
	SessionID string `json:"session_id"`
	Limits    struct {
		TokenCap                 int64  `json:"token_cap"`
		ModelAdmissionLimitMicro *int64 `json:"model_admission_limit_microusd"`
		AdmissionPolicy          string `json:"admission_policy"`
	} `json:"limits"`
	// GatewayBudget is the token budget the gateway enforces on the
	// session's machine; the token cap above governs admitted calls only.
	GatewayBudget *int64          `json:"gateway_budget_tokens"`
	RateBook      string          `json:"rate_book_version"`
	Entries       []usageEntryRow `json:"entries"`
	ObservedAt    string          `json:"observed_at"`
	// Summary is what the session has cost as a person reads it: the
	// receipt the console prints. Absent from an older service.
	Summary *usageSummaryDoc `json:"summary,omitempty"`
}

// usageSummaryDoc is the service's sessionCostSummary: every figure is
// measured, or computed from a measured one at the price book's rate; one
// the meter could not answer is absent (nil) and Missing says why.
type usageSummaryDoc struct {
	Currency string `json:"currency"`
	Charged  *int64 `json:"charged_microusd"`
	Model    struct {
		Tokens       *int64 `json:"tokens"`
		InputOutput  *int64 `json:"input_output_tokens"`
		Cache        *int64 `json:"cache_tokens"`
		Calls        *int64 `json:"calls"`
		KeySource    string `json:"key_source,omitempty"`
		Charged      int64  `json:"charged_microusd"`
		ChargeReason string `json:"charge_reason"`
		// ManagedCharged is what the session's calls on KeepState's key
		// were charged; absent from a service that does not offer it
		ManagedCharged int64 `json:"managed_charged_microusd"`
	} `json:"model"`
	Runtime struct {
		Seconds *int64 `json:"seconds"`
		Charged *int64 `json:"charged_microusd"`
		Rate    int64  `json:"rate_microusd_per_minute"`
	} `json:"runtime"`
	Storage struct {
		Bytes   *int64 `json:"bytes"`
		Monthly *int64 `json:"monthly_microusd"`
		Rate    int64  `json:"rate_microusd_per_gb_month"`
	} `json:"storage"`
	Budget struct {
		Tokens int64  `json:"budget_tokens"`
		Spent  *int64 `json:"spent_tokens"`
		Left   *int64 `json:"left_tokens"`
		Source string `json:"source"`
		Note   string `json:"note"`
	} `json:"budget"`
	Missing []string `json:"missing,omitempty"`
}

// meterSilent is what an unread figure reads, in place of a zero.
const meterSilent = "unavailable: the meter did not answer"

// receiptMoney is micro-dollars as a person reads them: a figure under a cent
// says so rather than rounding to nothing.
func receiptMoney(micro int64) string {
	switch {
	case micro == 0:
		return "$0.00"
	case micro > 0 && micro < 10000:
		return "under $0.01"
	}
	cents := (abs64(micro) + 5000) / 10000
	sign := ""
	if micro < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s$%s.%02d", sign, commas(cents/100), cents%100)
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// receiptMonthly is a monthly charge: "about $0.02 a month".
func receiptMonthly(micro int64) string {
	if micro > 0 && micro < 10000 {
		return "under $0.01 a month"
	}
	return "about " + receiptMoney(micro) + " a month"
}

// rateText is a price-book rate in dollars, with the decimals it needs
// ($0.011, $0.09).
func rateText(micro int64) string {
	s := strings.TrimRight(fmt.Sprintf("%.3f", float64(micro)/1e6), "0")
	if len(s) < 4 {
		s = fmt.Sprintf("%.2f", float64(micro)/1e6)
	}
	return "$" + s
}

// durationText is seconds as a person reads them: 45 s, 2 min, 1 h 5 min, 2 d 3 h.
func durationText(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	if secs < 60 {
		return fmt.Sprintf("%d s", secs)
	}
	m := secs / 60
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	h := m / 60
	if h < 24 {
		if m%60 == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m%60)
	}
	if h%24 == 0 {
		return fmt.Sprintf("%d d", h/24)
	}
	return fmt.Sprintf("%d d %d h", h/24, h%24)
}

// bytesText is bytes as a person reads them (1 GB is 1,000,000,000 bytes).
func bytesText(b int64) string {
	if b < 0 {
		b = 0
	}
	switch {
	case b < 1000:
		return fmt.Sprintf("%d bytes", b)
	case b < 1e6:
		return fmt.Sprintf("%d KB", (b+500)/1000)
	case b < 1e9:
		return fmt.Sprintf("%d MB", (b+500000)/1000000)
	}
	tenths := (b + 50000000) / 100000000
	if tenths%10 == 0 {
		return fmt.Sprintf("%d GB", tenths/10)
	}
	return fmt.Sprintf("%d.%d GB", tenths/10, tenths%10)
}

// keySourceText says whose key the model calls went on.
func keySourceText(ks string) string {
	byok, house := strings.Contains(ks, "byok"), strings.Contains(ks, "managed") || strings.Contains(ks, "house")
	switch {
	case byok && house:
		return "on your key and KeepState's key (" + sanitize(ks) + ")"
	case byok:
		return "on your key (" + sanitize(ks) + ")"
	case house:
		return "on KeepState's key (" + sanitize(ks) + ")"
	case ks == "":
		return "key source unavailable"
	}
	return "on " + sanitize(ks)
}

// receiptLines is the receipt the console prints, from the summary: a
// figure the meter did not answer reads unavailable, never 0.
func receiptLines(s *usageSummaryDoc, state string, onKeepState bool) []string {
	running := state == "running" || state == "checkpointing"
	ended := sessionEnded(state)
	var out []string
	if s.Charged != nil {
		out = append(out, fmt.Sprintf("  KeepState charges so far   %s (for this session, before tax; saved data is billed by the month)", receiptMoney(*s.Charged)))
	} else {
		out = append(out, "  KeepState charges so far   "+meterSilent)
	}
	rt := s.Runtime
	switch {
	case rt.Seconds == nil || rt.Charged == nil:
		out = append(out, "  running time   "+meterSilent)
	case *rt.Seconds == 0 && !running:
		out = append(out, "  running time   $0.00: not run yet")
	case running:
		out = append(out, fmt.Sprintf("  running time   %s: %s at %s a minute, running now", receiptMoney(*rt.Charged), durationText(*rt.Seconds), rateText(rt.Rate)))
	case ended:
		out = append(out, fmt.Sprintf("  running time   %s: %s at %s a minute; ended, so no more time is charged", receiptMoney(*rt.Charged), durationText(*rt.Seconds), rateText(rt.Rate)))
	default:
		out = append(out, fmt.Sprintf("  running time   %s: %s at %s a minute; paused, so no time is charged now", receiptMoney(*rt.Charged), durationText(*rt.Seconds), rateText(rt.Rate)))
	}
	st := s.Storage
	switch {
	case st.Bytes == nil || st.Monthly == nil:
		out = append(out, "  saved data     "+meterSilent)
	case *st.Bytes == 0:
		out = append(out, "  saved data     $0.00: nothing saved yet")
	case ended:
		out = append(out, fmt.Sprintf("  saved data     %s: %s until you delete it (%s per GB a month)", receiptMonthly(*st.Monthly), bytesText(*st.Bytes), rateText(st.Rate)))
	default:
		out = append(out, fmt.Sprintf("  saved data     %s: %s while kept (%s per GB a month)", receiptMonthly(*st.Monthly), bytesText(*st.Bytes), rateText(st.Rate)))
	}
	m := s.Model
	charge := "No charge"
	if m.Charged != 0 {
		charge = receiptMoney(m.Charged)
	}
	// calls on KeepState's key are charged: the line says what, never "No charge"
	managed := onKeepState || m.ManagedCharged > 0
	managedCharge := m.ManagedCharged
	if managedCharge == 0 {
		managedCharge = m.Charged
	}
	if managed {
		charge = receiptMoney(m.Charged) + " charged"
	}
	figure := func(n *int64, what string) string {
		if n == nil {
			return what + " unavailable"
		}
		return commas(*n) + " " + what
	}
	switch {
	case m.Calls != nil && *m.Calls == 0:
		out = append(out, fmt.Sprintf("  model use      %s: no model calls yet; %s", charge, sanitize(notRecorded(m.ChargeReason))))
	case m.Tokens == nil:
		out = append(out, fmt.Sprintf("  model use      %s: the token total is %s; %s, %s, %s, %s; %s", charge, meterSilent,
			figure(m.InputOutput, "input and output"), figure(m.Cache, "cache tokens"), figure(m.Calls, "calls"), keySourceText(m.KeySource), sanitize(notRecorded(m.ChargeReason))))
	default:
		out = append(out, fmt.Sprintf("  model use      %s: %s tokens in %s, %s, %s, %s; %s", charge, commas(*m.Tokens), figure(m.Calls, "calls"),
			figure(m.InputOutput, "input and output"), figure(m.Cache, "cache"), keySourceText(m.KeySource), sanitize(notRecorded(m.ChargeReason))))
	}
	if managed {
		out = append(out, fmt.Sprintf("  model use on KeepState's key: %s charged, the provider's list price plus the spread, paid from your purchased credit", receiptMoney(managedCharge)))
	}
	b := s.Budget
	switch {
	case b.Tokens > 0 && b.Spent != nil && b.Left != nil:
		line := fmt.Sprintf("  token budget   %s of %s used; %s left", commas(*b.Spent), commas(b.Tokens), commas(*b.Left))
		switch {
		case *b.Spent >= b.Tokens:
			line += "; the budget is used up, so the session was saved and paused"
		default:
			line += "; when the budget runs out, the session is saved and paused automatically"
		}
		out = append(out, line)
	case b.Tokens > 0:
		out = append(out, fmt.Sprintf("  token budget   spent and left %s (budget %s)", meterSilent, commas(b.Tokens)))
	default:
		out = append(out, "  token budget   unavailable: no budget was reported")
	}
	for _, miss := range s.Missing {
		out = append(out, "  unavailable    "+sanitize(miss))
	}
	return out
}

// money renders micro-units, or "unavailable" when the service has no figure.
func money(micro *int64, currency, standing string) string {
	if micro == nil || standing == "unavailable" {
		return "unavailable"
	}
	if currency == "" {
		currency = "USD"
	}
	s := fmt.Sprintf("%d.%06d %s", *micro/1_000_000, *micro%1_000_000, currency)
	if standing != "" && standing != "actual" {
		s += " (" + standing + ")"
	}
	return s
}

func hostedSessionUsage(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: fmt.Sprintf("session %s has no workspace record, so the service keeps no usage ledger for it; ks meter %s shows its spend", sess.ShortID, sess.ShortID)})
	}
	var env struct {
		Data sessionUsageDoc `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/usage", nil, &env); err != nil {
		die(err)
	}
	u := env.Data
	emit(u, func() {
		fmt.Printf("usage of session %s, observed %s (rate book %s)\n", sess.ShortID, figure(u.ObservedAt), figure(u.RateBook))
		// the receipt first, as the console prints it; the entries below
		// are the detail
		if u.Summary != nil {
			for _, l := range receiptLines(u.Summary, sess.RuntimeState, onKeepStatesKey(u.Entries)) {
				fmt.Println(l)
			}
		} else {
			fmt.Println("  receipt         unavailable: this service answered no summary; the entries below are the detail it keeps")
		}
		lim := fmt.Sprintf("token cap %s", commas(u.Limits.TokenCap))
		if u.Limits.ModelAdmissionLimitMicro != nil {
			lim += ", model spend limit " + money(u.Limits.ModelAdmissionLimitMicro, "USD", "actual")
		}
		fmt.Printf("  limits   %s; admission %s\n", lim, figure(u.Limits.AdmissionPolicy))
		if u.GatewayBudget != nil {
			fmt.Printf("           gateway budget %s tokens: what the gateway enforces on this session's machine (the token cap governs admitted calls)\n", commas(*u.GatewayBudget))
		}
		if len(u.Entries) == 0 {
			fmt.Println("  nothing recorded yet")
			return
		}
		for _, e := range u.Entries {
			switch {
			case "model" == e.Kind && e.Source == "gateway meter":
				// the gateway meters a total, not input and output apart
				fmt.Printf("  model    gateway meter via %s: %d call(s), %s tokens (a total)", keySourceLabel(e.KeySource), e.Calls, commas(e.Tokens))
				fmt.Printf("; cost %s\n", money(e.MicroUSD, e.Currency, e.Standing))
			case "model" == e.Kind:
				fmt.Printf("  model    %s/%s via %s: %d call(s), %s in + %s out tokens", figure(e.Provider), figure(e.Model), keySourceLabel(e.KeySource), e.Calls, commas(e.InputTokens), commas(e.OutputTokens))
				if e.ReservedLive > 0 {
					fmt.Printf(", %s reserved now", commas(e.ReservedLive))
				}
				fmt.Printf("; cost %s\n", money(e.MicroUSD, e.Currency, e.Standing))
			default:
				fmt.Printf("  %-8s cost %s\n", e.Kind, money(e.MicroUSD, e.Currency, e.Standing))
			}
			if e.PriceSource != "" {
				fmt.Printf("           price source %s\n", e.PriceSource)
			}
			if e.Note != "" {
				fmt.Printf("           %s\n", sanitize(e.Note))
			}
		}
	})
}

// onKeepStatesKey: an entry of the usage is a model use on KeepState's key.
func onKeepStatesKey(entries []usageEntryRow) bool {
	for _, e := range entries {
		if strings.HasPrefix(e.KeySource, "managed:") {
			return true
		}
	}
	return false
}

// keySourceLabel is an entry's key source as a person reads it: KeepState's
// key for a provider, or the service's word as it is.
func keySourceLabel(ks string) string {
	if p, ok := strings.CutPrefix(ks, "managed:"); ok {
		return "KeepState's key (" + sanitize(p) + ")"
	}
	return figure(ks)
}
