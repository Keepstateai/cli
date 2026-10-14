// KS-018/KS-037 usage display: model, runtime and storage apart; a missing
// figure reads unavailable, never $0 (QA-037-2).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsageShowsUnavailableNeverZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"budget.admission","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.URL.Path == "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetuse0000000000000000000000001", "short_id": "fleetuse0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/sessions/session_1/usage":
			priced := int64(1234567)
			env(map[string]any{"session_id": "session_1", "limits": map[string]any{"token_cap": 500000, "model_admission_limit_microusd": nil, "admission_policy": "token_only"},
				"rate_book_version": "rates-1", "observed_at": "2026-09-26T12:00:00Z", "entries": []any{
					map[string]any{"kind": "model", "provider": "anthropic", "model": "claude-x", "key_source": "byok", "calls": 3, "input_tokens": 1200, "output_tokens": 300, "microusd": nil, "currency": "USD", "standing": "unavailable", "note": "no published rate for this model"},
					map[string]any{"kind": "model", "provider": "anthropic", "model": "claude-y", "key_source": "byok", "calls": 1, "input_tokens": 10, "output_tokens": 5, "microusd": priced, "currency": "USD", "standing": "estimated", "price_source_version": "rates-1"},
					map[string]any{"kind": "runtime", "microusd": nil, "currency": "USD", "standing": "unavailable"},
				}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetuse")
	if code != 0 || strings.Contains(out, "$0") || strings.Contains(out, "0.000000") || !strings.Contains(out, "cost unavailable") ||
		!strings.Contains(out, "1.234567 USD (estimated)") || !strings.Contains(out, "runtime  cost unavailable") || !strings.Contains(out, "no published rate") {
		t.Fatalf("usage: %d\n%s%s", code, out, errs)
	}
}

// Found by the production live check of the app: the gateway meter's row
// carries a token TOTAL, and the renderer printed "0 in + 0 out" for a call
// that used 55 tokens. The row says what the meter says, and the budget the
// gateway enforces is shown beside the admission cap.
func TestUsageShowsTheGatewayMetersTotalAndBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"budget.admission","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.URL.Path == "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetgw00000000000000000000000001", "short_id": "fleetgw00000", "name": "c", "runtime_state": "running", "record_id": "session_2"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/sessions/session_2/usage":
			env(map[string]any{"session_id": "session_2", "limits": map[string]any{"token_cap": 500000, "admission_policy": "dollar"}, "gateway_budget_tokens": 2000000,
				"rate_book_version": "unratified", "observed_at": "2026-09-30T12:00:00Z", "entries": []any{
					map[string]any{"kind": "model", "provider": "gateway", "source": "gateway meter", "key_source": "byok", "calls": 1, "tokens": 55, "microusd": nil, "currency": "USD", "standing": "unavailable"},
				}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetgw")
	if code != 0 || !strings.Contains(out, "gateway meter via byok: 1 call(s), 55 tokens (a total); cost unavailable") ||
		!strings.Contains(out, "gateway budget 2,000,000 tokens") || strings.Contains(out, "0 in + 0 out") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

// usageSrv serves one session whose usage answers the given summary, so a
// receipt case needs only the summary it is about.
func usageSrv(t *testing.T, state string, summary map[string]any) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"budget.admission","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.URL.Path == "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetrcpt000000000000000000000001", "short_id": "fleetrcpt000", "name": "c", "runtime_state": state, "record_id": "session_r"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/sessions/session_r/usage":
			env(map[string]any{"session_id": "session_r", "limits": map[string]any{"token_cap": 500000, "admission_policy": "token_only"}, "gateway_budget_tokens": 500000,
				"rate_book_version": "rates-1", "observed_at": "2026-10-14T12:00:00Z", "summary": summary, "entries": []any{
					map[string]any{"kind": "model", "provider": "gateway", "source": "gateway meter", "key_source": "byok", "calls": 3, "tokens": 12345, "microusd": nil, "currency": "USD", "standing": "unavailable"},
				}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

// The receipt the console prints, from the service's summary: what KeepState
// has charged so far, running time, saved data, model use (no charge, with
// the reason), the budget spent and left, and every figure the meter did not
// answer named as unavailable, never printed as a zero.
func TestUsagePrintsTheReceiptFromTheSummary(t *testing.T) {
	bin, cfg := usageSrv(t, "running", map[string]any{
		"currency": "USD", "charged_microusd": 123456,
		"model":   map[string]any{"tokens": 12345, "input_output_tokens": 10000, "cache_tokens": 2345, "calls": 3, "key_source": "byok", "charged_microusd": 0, "charge_reason": "your provider bills your key for model use; KeepState adds nothing"},
		"runtime": map[string]any{"seconds": 7500, "charged_microusd": 123456, "rate_microusd_per_minute": 11000},
		"storage": map[string]any{"bytes": 1200000000, "monthly_microusd": 24000, "rate_microusd_per_gb_month": 20000},
		"budget":  map[string]any{"budget_tokens": 500000, "spent_tokens": 400000, "left_tokens": 100000, "source": "gateway meter", "note": "n"},
	})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetrcpt")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"KeepState charges so far   $0.12",
		"running time   $0.12: 2 h 5 min at $0.011 a minute",
		"saved data     about $0.02 a month: 1.2 GB while kept",
		"model use      No charge: 12,345 tokens in 3 calls, 10,000 input and output, 2,345 cache, on your key (byok); your provider bills your key for model use; KeepState adds nothing",
		"token budget   400,000 of 500,000 used; 100,000 left",
		// the detail stays under the receipt
		"gateway meter via byok: 3 call(s), 12,345 tokens (a total); cost unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the receipt lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unavailable: the meter did not answer") {
		t.Errorf("a figure the meter answered reads unavailable:\n%s", out)
	}
}

func TestUsageReceiptNeverPrintsZeroForAFigureTheMeterDidNotAnswer(t *testing.T) {
	missing := "The engine on this session's machine does not report its running time and saved data yet, so they and the charge are not shown."
	bin, cfg := usageSrv(t, "parked", map[string]any{
		"currency": "USD", "charged_microusd": nil,
		"model":   map[string]any{"tokens": nil, "input_output_tokens": 5000, "cache_tokens": nil, "calls": 2, "key_source": "managed", "charged_microusd": 0, "charge_reason": "KeepState does not charge for model use"},
		"runtime": map[string]any{"seconds": nil, "charged_microusd": nil, "rate_microusd_per_minute": 11000},
		"storage": map[string]any{"bytes": nil, "monthly_microusd": nil, "rate_microusd_per_gb_month": 20000},
		"budget":  map[string]any{"budget_tokens": 500000, "spent_tokens": nil, "left_tokens": nil, "source": "unavailable", "note": "the meter could not be read"},
		"missing": []string{missing},
	})
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetrcpt")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"KeepState charges so far   unavailable: the meter did not answer",
		"running time   unavailable: the meter did not answer",
		"saved data     unavailable: the meter did not answer",
		"model use      No charge: the token total is unavailable: the meter did not answer; 5,000 input and output, cache tokens unavailable, 2 calls, on KeepState's key (managed); KeepState does not charge for model use",
		"token budget   spent and left unavailable: the meter did not answer (budget 500,000)",
		"unavailable    " + missing,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the receipt lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"$0.00", "0 s", "0 bytes", "0 of 500,000"} {
		if strings.Contains(out, not) {
			t.Errorf("an unread figure reads %q:\n%s", not, out)
		}
	}
	// an older service without a summary: no receipt figures, said so, and the entries as before
	bin, cfg = usageSrv(t, "running", nil)
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "session", "usage", "fleetrcpt")
	if code != 0 || !strings.Contains(out, "receipt         unavailable: this service answered no summary") || strings.Contains(out, "$") {
		t.Errorf("without a summary: exit %d\n%s", code, out)
	}
}
