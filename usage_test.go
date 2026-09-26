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
