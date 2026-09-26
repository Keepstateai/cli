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
)

type usageEntryRow struct {
	Kind         string `json:"kind"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	KeySource    string `json:"key_source,omitempty"`
	Calls        int64  `json:"calls,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
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
	RateBook   string          `json:"rate_book_version"`
	Entries    []usageEntryRow `json:"entries"`
	ObservedAt string          `json:"observed_at"`
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
		lim := fmt.Sprintf("token cap %s", commas(u.Limits.TokenCap))
		if u.Limits.ModelAdmissionLimitMicro != nil {
			lim += ", model spend limit " + money(u.Limits.ModelAdmissionLimitMicro, "USD", "actual")
		}
		fmt.Printf("  limits   %s; admission %s\n", lim, figure(u.Limits.AdmissionPolicy))
		if len(u.Entries) == 0 {
			fmt.Println("  nothing recorded yet")
			return
		}
		for _, e := range u.Entries {
			switch e.Kind {
			case "model":
				fmt.Printf("  model    %s/%s via %s: %d call(s), %s in + %s out tokens", figure(e.Provider), figure(e.Model), figure(e.KeySource), e.Calls, commas(e.InputTokens), commas(e.OutputTokens))
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
