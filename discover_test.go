// KS-061: discovery lists the agents you may consult and chooses nothing
// among a shared name (409 with candidates).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdviserDiscoverListsAndNeverChooses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case "/api/v2/agent-registry":
			if r.URL.Query().Get("name") == "main" {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_name_ambiguous","type":"ks_name_ambiguous","message":"more than one agent is named main","candidates":[{"agent_id":"agent_1","selector":"checkout/main","availability":"available"},{"agent_id":"agent_2","selector":"billing/main","availability":"parked"}]}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"items": []any{
				map[string]any{"agent_id": "agent_1", "name": "main", "selector": "checkout/main", "name_ambiguous": true, "runner_mode": "claude-code", "runner_version": "2.1.251", "runner_model": "claude-x", "availability": "available", "last_seen_at": "t"},
				map[string]any{"agent_id": "agent_2", "name": "main", "selector": "billing/main", "name_ambiguous": true, "availability": "parked"},
			}, "next_cursor": ""}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "adviser", "discover")
	if code != 0 || !strings.Contains(out, "checkout/main *") || !strings.Contains(out, "claude-code 2.1.251 claude-x") || !strings.Contains(out, "not reported") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "adviser", "discover", "main")
	if code != exitUsage || !strings.Contains(errs, "none is chosen") || !strings.Contains(errs, "checkout/main") || !strings.Contains(errs, "billing/main") {
		t.Fatalf("ambiguous: %d\n%s", code, errs)
	}
}
