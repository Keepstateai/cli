// KS-010 on the command line: the client declares its protocol on every
// request; an operation refused as ks_client_too_old is named with the
// update; a capability newer than this client is disabled before anything
// is sent; an unavailable capability shows the service's own reason; a
// state this client does not know reads unknown, never a guess.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type protoCtl struct {
	mu        sync.Mutex
	headers   []string
	since     int    // agent.workspace since_protocol
	avail     string // agent.workspace availability
	tooOld    bool   // the agents route refuses as ks_client_too_old
	agentWord string // the activity the agent reports
}

func (c *protoCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.URL.Path+" "+r.Header.Get("X-KS-Protocol"))
	env := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3",
			"capabilities":[{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","note":"engineering note","unavailable_reason":"agent sessions are not available in this release","since_protocol":%d},
			{"id":"session.list","availability":"available","summary":"s","surface":"api","since_protocol":1}],
			"protocol":{"current":2,"minimum_client":1,"header":"X-KS-Protocol"},
			"states":{"agent_activity":["ready","working","daydreaming"],"task_state":["queued"]},"limits":{}}}`, c.avail, c.since)
	case r.URL.Path == "/api/v2/sessions":
		env(map[string]any{"items": []any{map[string]any{"id": "fleetpro0000000000000000000000001", "short_id": "fleetpro0000", "name": "c", "runtime_state": "hibernating", "agent_activity": "daydreaming", "task_state": "none", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents" && c.tooOld:
		w.WriteHeader(426)
		fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_client_too_old","type":"ks_client_too_old","message":"this operation belongs to agent.workspace, introduced at protocol 3; your client declared protocol 2 and cannot safely read its answer. Nothing was done; update the client","next_action":"ks update"}}`)
	case r.URL.Path == "/api/v2/agents":
		env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true, "activity": c.agentWord}}})
	default:
		w.WriteHeader(404)
	}
}

func TestTheClientNegotiatesItsProtocolAndStates(t *testing.T) {
	c := &protoCtl{since: 2, avail: "available", agentWord: "daydreaming"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// every request declares protocol 2; an unknown state reads unknown
	out, errs, code := auditExec(t, bin, cfg, dir, env, "agent", "list", "--session", "fleetpro")
	if code != 0 || !strings.Contains(out, "unknown ") || strings.Contains(out, "daydream") {
		t.Fatalf("agent list: %d\n%s%s", code, out, errs)
	}
	for _, h := range c.headers {
		if !strings.HasSuffix(h, " 2") {
			t.Errorf("a request did not declare protocol 2: %q", h)
		}
	}
	out, _, _ = auditExec(t, bin, cfg, dir, env, "session", "show", "fleetpro")
	if !strings.Contains(out, "unknown (hibernating)") || !strings.Contains(out, "unknown (daydreaming)") {
		t.Fatalf("session show:\n%s", out)
	}

	// the service refuses by name: the capability named, the update named
	c.mu.Lock()
	c.tooOld = true
	c.mu.Unlock()
	out, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "list", "--session", "fleetpro", "--json")
	if code != exitFailed || !strings.Contains(out, "ks_client_too_old") || !strings.Contains(out, "agent.workspace") || !strings.Contains(out, "ks update") {
		t.Fatalf("too old: %d\n%s%s", code, out, errs)
	}

	// a capability newer than this client: disabled before anything is sent
	c.mu.Lock()
	c.tooOld, c.since, c.headers = false, 3, nil
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "list", "--session", "fleetpro")
	if code != exitFailed || !strings.Contains(errs, "protocol 3") || !strings.Contains(errs, "ks update") {
		t.Fatalf("newer capability: %d\n%s", code, errs)
	}
	for _, h := range c.headers {
		if strings.HasPrefix(h, "/api/v2/agents") {
			t.Fatalf("a disabled command reached the service: %v", c.headers)
		}
	}

	// unavailable: the service's own reason, not the engineering note
	c.mu.Lock()
	c.since, c.avail = 2, "unavailable"
	c.mu.Unlock()
	_, errs, code = auditExec(t, bin, cfg, dir, env, "agent", "list", "--session", "fleetpro")
	if code != exitFailed || !strings.Contains(errs, "agent sessions are not available in this release") || strings.Contains(errs, "engineering note") {
		t.Fatalf("unavailable reason: %d\n%s", code, errs)
	}

	// doctor names the protocol and the published states this client does not know
	out, errs, _ = auditExec(t, bin, cfg, dir, env, "doctor")
	if !strings.Contains(out+errs, "this client speaks 2; the control plane speaks 2") || !strings.Contains(out+errs, "agent_activity daydreaming") {
		t.Fatalf("doctor:\n%s%s", out, errs)
	}
}
