// capabilities_test: KS-010's guards. A command that needs a capability
// runs only when the live registry says available; absent, unavailable,
// degraded, an unknown state, or no registry at all each disable it with
// the reason; doctor shows the negotiation; the cache is never trusted
// for execution.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func capServer(t *testing.T, availability string, listed bool, registry bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/capabilities":
			if !registry {
				w.WriteHeader(404)
				fmt.Fprint(w, "404 page not found")
				return
			}
			rows := `{"id":"session.run","availability":"available","summary":"s","surface":"api"}`
			if listed {
				rows += fmt.Sprintf(`,{"id":"operations.idempotent","availability":%q,"summary":"ops","surface":"api","note":"why"}`, availability)
			}
			fmt.Fprintf(w, `{"schema_version":2,"request_id":"r","data":{"registry_version":"test","build":"abc","fetched_at":"2026-09-20T00:00:00Z","price_book":"v1.3","capabilities":[%s],"limits":{}}}`, rows)
		case strings.HasPrefix(r.URL.Path, "/api/operations/"):
			fmt.Fprint(w, `{"schema_version":2,"data":{"operation_id":"op_1","state":"succeeded","http_status":200,"finished_at":"x","accepted_at":"x","response":{"ok":true}}}`)
		case r.URL.Path == "/healthz":
			fmt.Fprint(w, "ok")
		case r.URL.Path == "/api/whoami":
			fmt.Fprint(w, `{"account_id":"acct_t","cohort_state":"active"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
}

func TestCapabilityNegotiationGatesCommands(t *testing.T) {
	cases := []struct {
		name         string
		availability string
		listed       bool
		registry     bool
		wantExit     int
		wantText     string
	}{
		{"available", "available", true, true, 0, "state succeeded"},
		{"unavailable", "unavailable", true, true, exitFailed, "reports unavailable: why"},
		{"degraded", "degraded", true, true, exitFailed, "reports degraded"},
		{"unknown state", "sideways", true, true, exitFailed, "does not understand"},
		{"not listed", "", false, true, exitFailed, "does not list"},
		{"no registry", "", false, false, exitFailed, "publishes no capability registry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := capServer(t, c.availability, c.listed, c.registry)
			defer srv.Close()
			bin, cfg := buildAndAuth(t, srv)
			out, errs, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "operation", "show", "op_1")
			if code != c.wantExit || !strings.Contains(out+errs, c.wantText) {
				t.Errorf("exit %d (want %d)\n%s%s", code, c.wantExit, out, errs)
			}
		})
	}
}

// QA-010-2 in its general form: a cached "available" never lets a command
// run when the live registry says otherwise.
func TestCacheIsNeverTrustedForExecution(t *testing.T) {
	good := capServer(t, "available", true, true)
	bin, cfg := buildAndAuth(t, good)
	if _, _, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "operation", "show", "op_1"); code != 0 {
		t.Fatal("warm-up against the good registry failed")
	}
	good.Close()
	// the same config now points at a control plane that says unavailable;
	// the cache still says available
	bad := capServer(t, "unavailable", true, true)
	defer bad.Close()
	tok := fmt.Sprintf(`{"token":"guard-test-token","ctl":%q}`, bad.URL)
	writeFileT(t, cfg+"/keepstate/token.json", tok)
	if _, errs, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "operation", "show", "op_1"); code == 0 || !strings.Contains(errs, "unavailable") {
		t.Errorf("the cached availability was trusted: exit %d %s", code, errs)
	}
}

func TestDoctorShowsTheNegotiation(t *testing.T) {
	srv := capServer(t, "unavailable", true, true)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, _ := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "doctor")
	if !strings.Contains(out, "capabilities: 1 available, 1 unavailable") || !strings.Contains(out, "ks operation show: disabled, operations.idempotent is unavailable") {
		t.Errorf("doctor negotiation lines missing:\n%s", out)
	}
	old := capServer(t, "", false, false)
	defer old.Close()
	bin, cfg = buildAndAuth(t, old)
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "doctor")
	if !strings.Contains(out, "not published by this control plane") {
		t.Errorf("doctor against an older control plane:\n%s", out)
	}
}
