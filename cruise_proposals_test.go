// KS-071 on the command line: a proposed Cruise job runs nothing until a
// person confirms the digest of the exact manifest, which the client
// recomputes rather than trusts; --yes never approves; a declined proposal
// never runs; the job reads in its own words.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// independentDigest is the canonical rule for a plain-ASCII manifest,
// computed here without the client's own code: sorted keys, no whitespace,
// no HTML escaping.
func independentDigest(t *testing.T, m map[string]any) (json.RawMessage, string) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	raw := bytes.TrimRight(b.Bytes(), "\n")
	return json.RawMessage(raw), shaOf(raw)
}

type proposalCtl struct {
	mu        sync.Mutex
	manifest  json.RawMessage
	sha       string // the digest the service STATES
	state     string
	approvals []map[string]any
	declines  int
	unavail   bool
}

func (c *proposalCtl) row() map[string]any {
	r := map[string]any{"id": "cprop_1", "session_id": "session_1", "agent_id": "agent_1", "task_id": "tsk_1", "proposed_by": "agent:w1",
		"manifest": c.manifest, "manifest_sha": c.sha, "goal": "make the inventory tests pass", "state": c.state, "revision": 1, "created_at": "x",
		"approval": "POST /api/v2/cruise-proposals/cprop_1/approve with manifest_sha"}
	if c.state == "approved" {
		r["job"] = map[string]any{"id": "job_9", "state": "queued", "label": "Queued", "manifest_sha": c.sha}
	}
	return r
}

func (c *proposalCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		a := "available"
		if c.unavail {
			a = "unavailable"
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"not open to accounts yet"}],"limits":{}}}`, a)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetcru0000000000000000000000001", "short_id": "fleetcru0000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/cruise-proposals" && r.URL.Query().Get("session_id") == "session_1":
		env(200, map[string]any{"items": []any{c.row()}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/cruise-proposals/cprop_1" && r.Method == "GET":
		env(200, c.row())
	case r.URL.Path == "/api/v2/cruise-proposals/cprop_1/approve":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.approvals = append(c.approvals, body)
		if body["manifest_sha"] != c.sha {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_manifest_digest_mismatch","type":"ks_manifest_digest_mismatch","message":"the digest you approved is not this proposal's; nothing was created"}}`)
			return
		}
		c.state = "approved"
		env(200, c.row())
	case r.URL.Path == "/api/v2/cruise-proposals/cprop_1/decline":
		c.declines++
		c.state = "declined"
		env(200, c.row())
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_not_found","type":"ks_not_found","message":"no such route"}}`)
	}
}

func TestAProposalRunsOnlyWhenItsRecomputedDigestIsConfirmed(t *testing.T) {
	m := map[string]any{"goal": "make the inventory tests pass", "check": map[string]any{"command": "go test ./..."},
		"ladder": []any{"claude-haiku", "claude-sonnet"}, "limits": map[string]any{"max_tokens": 200000}}
	raw, sha := independentDigest(t, m)
	c := &proposalCtl{manifest: raw, sha: sha, state: "proposed"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "cruise", "proposal", "list", "--session", "fleetcru")
	if code != 0 || !strings.Contains(out, "cprop_1") || !strings.Contains(out, sha[:12]) {
		t.Fatalf("list: %d\n%s%s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "cruise", "proposal", "show", "cprop_1")
	if code != 0 || !strings.Contains(out, sha+" (computed here") || !strings.Contains(out, "go test ./...") || !strings.Contains(out, "--confirm "+sha[:12]) {
		t.Fatalf("show: %d\n%s", code, out)
	}
	// no confirmation, --yes, a wrong digest: nothing approved
	for _, args := range [][]string{{}, {"--yes"}, {"--no-input"}, {"--confirm", strings.Repeat("0", 12)}} {
		if _, errs, code := auditExec(t, bin, cfg, dir, env, append([]string{"cruise", "proposal", "approve", "cprop_1"}, args...)...); code != exitUsage || !strings.Contains(errs, "nothing was approved") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
	}
	if len(c.approvals) != 0 {
		t.Fatal("an unconfirmed approval was sent")
	}
	// confirmed with the 12 characters shown: one approval naming the digest
	out, errs, code = auditExec(t, bin, cfg, dir, env, "cruise", "proposal", "approve", "cprop_1", "--confirm", sha[:12])
	if code != 0 || !strings.Contains(out, "job job_9 (Queued)") || !strings.Contains(errs, "manifest") || len(c.approvals) != 1 || c.approvals[0]["manifest_sha"] != sha {
		t.Fatalf("approve: %d %v\n%s%s", code, c.approvals, out, errs)
	}
}

// The service states a digest the manifest it returned does not hash to:
// refused locally (exit 6) and nothing is sent.
func TestAProposalWhoseDigestDoesNotMatchItsManifestIsRefused(t *testing.T) {
	raw, _ := independentDigest(t, map[string]any{"goal": "g"})
	c := &proposalCtl{manifest: raw, sha: strings.Repeat("a", 64), state: "proposed"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "proposal", "approve", "cprop_1", "--confirm", strings.Repeat("a", 64))
	if code != exitIntegrity || !strings.Contains(errs, "hashes to") || len(c.approvals) != 0 {
		t.Fatalf("mismatch: %d %v\n%s", code, c.approvals, errs)
	}
}

func TestADeclinedProposalNeverRunsAndUnavailableSaysSo(t *testing.T) {
	raw, sha := independentDigest(t, map[string]any{"goal": "g"})
	c := &proposalCtl{manifest: raw, sha: sha, state: "proposed"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "proposal", "decline", "cprop_1", "--reason", "too weak")
	if code != 0 || !strings.Contains(out, "it never runs") || c.declines != 1 {
		t.Fatalf("decline: %d %s", code, out)
	}
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "proposal", "approve", "cprop_1", "--confirm", sha); code != exitConflict || !strings.Contains(errs, "declined") || len(c.approvals) != 0 {
		t.Fatalf("approve after decline: %d\n%s", code, errs)
	}
	c.mu.Lock()
	c.unavail = true
	c.mu.Unlock()
	if _, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "proposal", "list", "--session", "fleetcru"); code != exitFailed || !strings.Contains(errs, "agent.workspace") {
		t.Fatalf("unavailable: %d\n%s", code, errs)
	}
}
