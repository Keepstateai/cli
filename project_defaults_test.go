// KS-039: a repository's project.json is a proposal: its changes are shown,
// it is trusted only by a confirmed digest (--yes never), key material is
// never sent; the idle view shows the policy and the countdown.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type defaultsCtl struct {
	mu        sync.Mutex
	proposals []map[string]any
	trusts    []map[string]any
}

const defaultsDigest = "sha256:0123456789ab0123456789ab0123456789ab0123456789ab0123456789abcdef"

func (c *defaultsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetdef0000000000000000000000001", "short_id": "fleetdef0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/sessions/session_1":
		env(map[string]any{"id": "session_1", "project_id": "proj_1"})
	case r.URL.Path == "/api/v2/projects/proj_1/defaults/proposals":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.proposals = append(c.proposals, b)
		env(map[string]any{"id": "pdp_1", "digest": defaultsDigest, "state": "proposed", "changes": []any{map[string]any{"field": "profile", "from": nil, "to": "claude-default"}, map[string]any{"field": "idle.idle_minutes", "from": 15, "to": 30}}})
	case r.URL.Path == "/api/v2/projects/proj_1/defaults/trust":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.trusts = append(c.trusts, b)
		env(map[string]any{"id": "pdp_1", "state": "trusted"})
	case r.URL.Path == "/api/v2/sessions/session_1/idle":
		env(map[string]any{"enabled": true, "idle_minutes": 15, "warning_seconds": 60, "state": "warned", "park_at": time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339Nano), "note": "an idle agent session is saved and paused"})
	default:
		w.WriteHeader(404)
	}
}

func TestProjectDefaultsAreAProposalTrustedOnlyByDigest(t *testing.T) {
	c := &defaultsCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	repo := project(t)
	_ = os.MkdirAll(filepath.Join(repo, ".keepstate"), 0o755)
	writeFileT(t, filepath.Join(repo, ".keepstate", "project.json"), `{"profile":"claude-default","idle":{"idle_minutes":30}}`)

	out, errs, code := auditExec(t, bin, cfg, repo, env, "project", "configure", "--session", "fleetdef", "--yes")
	if code != 0 || !strings.Contains(out, "NOT trusted") || !strings.Contains(errs, "idle.idle_minutes") || !strings.Contains(errs, "15 -> 30") || len(c.trusts) != 0 {
		t.Fatalf("proposal: %d\n%s%s", code, out, errs)
	}
	if c.proposals[0]["source"] != "repository" {
		t.Fatalf("sent: %v", c.proposals[0])
	}
	if _, _, code := auditExec(t, bin, cfg, repo, env, "project", "configure", "--session", "fleetdef", "--confirm", "ffffffffffff"); code != exitUsage || len(c.trusts) != 0 {
		t.Fatalf("wrong digest: %d", code)
	}
	out, _, code = auditExec(t, bin, cfg, repo, env, "project", "configure", "--session", "fleetdef", "--confirm", "0123456789ab")
	if code != 0 || len(c.trusts) != 1 || c.trusts[0]["expected_digest"] != defaultsDigest || !strings.Contains(out, "trusted") {
		t.Fatalf("trust: %d %v\n%s", code, c.trusts, out)
	}
	// key material: refused here and never sent
	writeFileT(t, filepath.Join(repo, ".keepstate", "project.json"), `{"key_aliases":{"anthropic":"sk-ant-api03-abcdefghijklmnop"}}`)
	n := len(c.proposals)
	if _, errs, code := auditExec(t, bin, cfg, repo, env, "project", "configure", "--session", "fleetdef"); code != exitIntegrity || !strings.Contains(errs, "key material") || len(c.proposals) != n {
		t.Fatalf("key material: %d\n%s", code, errs)
	}
	// the idle countdown
	out, _, code = auditExec(t, bin, cfg, repo, env, "session", "idle", "fleetdef")
	if code != 0 || !strings.Contains(out, "parks after 15 min") || !strings.Contains(out, "warned") || !strings.Contains(out, "(in 4") {
		t.Fatalf("idle: %d\n%s", code, out)
	}
}
