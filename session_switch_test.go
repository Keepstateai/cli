// The two switches a session carries, from a terminal: ks session network and
// ks session auto. Each shows its switch and what it means; with on or off it
// changes it against the revision just read, in any state but ended, and says
// when it takes effect; a record that moved in between, an ended session and
// the service's refusals are printed with what to do next.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

type switchCtl struct {
	mu         sync.Mutex
	patches    []map[string]any
	net        map[string]string // record -> network_mode
	perm       map[string]string // record -> permission_mode
	rev        int64
	refuse     string // a refusal type for the next change of a running session
	bytes      int64  // the bytes the engine counts today for the running session
	capReached bool
}

func (c *switchCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, typ, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
	}
	fleet := func(id, short, name, rec, state string) map[string]any {
		return map[string]any{"id": id, "short_id": short, "name": name, "runtime_state": state, "record_id": rec,
			"agent_activity": "ready", "task_state": "none", "key_alias": "prod", "observed_at": "x", "last_activity_at": "x", "created_at": "x", "image": "claude", "budget_tokens": 1, "execution_epoch": 1}
	}
	state := map[string]string{"session_run": "running", "session_park": "parked", "session_dead": "dead"}
	record := func(rec string) map[string]any {
		c.mu.Lock()
		defer c.mu.Unlock()
		return map[string]any{"id": rec, "account_id": "acct_x", "revision": c.rev, "name": "n-" + rec, "mode": "agent", "runtime_state": state[rec],
			"network_mode": c.net[rec], "permission_mode": c.perm[rec]}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"api.v2.workspace","availability":"available","summary":"s","surface":"api"},{"id":"session.list","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": []map[string]any{
			fleet("fleetrun000000000000000000000001", "fleetrun0000", "checkout", "session_run", "running"),
			fleet("fleetpark00000000000000000000002", "fleetpark000", "reviewer", "session_park", "parked"),
			fleet("fleetdead00000000000000000000003", "fleetdead000", "old", "session_dead", "dead"),
		}, "next_cursor": "", "observed_at": "x"})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_run/network":
		c.mu.Lock()
		mode, bytes, capped := c.net["session_run"], c.bytes, c.capReached
		c.mu.Unlock()
		env(200, map[string]any{"session_id": "session_run", "network_mode": mode, "known": true, "bytes_today": bytes, "daily_cap_bytes": 5 << 30, "cap_reached": capped, "blocked_packets": 3})
	case r.Method == "GET" && r.URL.Path == "/api/v2/sessions/session_park/network":
		env(200, map[string]any{"session_id": "session_park", "network_mode": "open", "known": false})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/session_"):
		env(200, record(strings.TrimPrefix(r.URL.Path, "/api/v2/sessions/")))
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/session_"):
		rec := strings.TrimPrefix(r.URL.Path, "/api/v2/sessions/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.patches = append(c.patches, body)
		rev, refusal := c.rev, c.refuse
		c.mu.Unlock()
		switch {
		case state[rec] == "dead":
			refuse(409, "ks_session_killed", "this session has ended; its network no longer changes")
		case refusal != "" && state[rec] == "running":
			refuse(502, refusal, "the engine could not change this session's network, so nothing was changed. Try again in a moment")
		case int64(body["expected_revision"].(float64)) != rev:
			refuse(409, "ks_revision_conflict", "the session changed since it was read")
		default:
			c.mu.Lock()
			if v, ok := body["network_mode"].(string); ok {
				c.net[rec] = v
			}
			if v, ok := body["permission_mode"].(string); ok {
				c.perm[rec] = v
			}
			c.rev = rev + 1
			c.mu.Unlock()
			env(200, record(rec))
		}
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "ks_not_found", "message": "no such route"}})
	}
}

func (c *switchCtl) patched() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.patches...)
}

func newSwitchCtl() *switchCtl {
	return &switchCtl{rev: 4,
		net:  map[string]string{"session_run": "open", "session_park": "open", "session_dead": "open"},
		perm: map[string]string{"session_run": "auto", "session_park": "manual", "session_dead": "auto"}}
}

func TestSessionNetworkShowsAndSwitchesTheMachinesNetwork(t *testing.T) {
	c := newSwitchCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// no state: what it is and what it means; nothing is sent
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "network", "fleetrun")
	if code != 0 || !strings.Contains(out, "session fleetrun0000: the network is on") || !strings.Contains(out, "ports 80, 443 and 22") || !strings.Contains(out, "always blocked") || !strings.Contains(out, "ks session network off fleetrun0000") {
		t.Fatalf("show: %d\n%s%s", code, out, errs)
	}
	if len(c.patched()) != 0 {
		t.Fatalf("showing the network sent a change: %v", c.patched())
	}

	// off, on a running session: against the revision just read, in effect now
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "network", "off", "fleetrun")
	if code != 0 || !strings.Contains(out, "The network is now off for session fleetrun0000") || !strings.Contains(out, "in effect now") {
		t.Fatalf("off: %d\n%s%s", code, out, errs)
	}
	p := c.patched()
	if len(p) != 1 || p[0]["network_mode"] != "off" || p[0]["expected_revision"] != float64(4) || len(p[0]) != 2 {
		t.Fatalf("what was sent: %v", p)
	}
	// the same again changes nothing and sends nothing
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "off", "fleetrun")
	if code != 0 || !strings.Contains(out, "already off") || len(c.patched()) != 1 {
		t.Fatalf("off again: %d\n%s (patches %d)", code, out, len(c.patched()))
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "fleetrun")
	if code != 0 || !strings.Contains(out, "the network is off") || !strings.Contains(out, "reaches its model and KeepState and nothing else") {
		t.Fatalf("show after off: %d\n%s", code, out)
	}
	// a paused session takes it for its next start
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "off", "fleetpark")
	if code != 0 || !strings.Contains(out, "in effect when the session next starts") {
		t.Fatalf("off on a paused session: %d\n%s", code, out)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "on", "fleetrun", "--json")
	if code != 0 || !strings.Contains(out, `"state": "on"`) && !strings.Contains(out, `"state":"on"`) {
		t.Fatalf("on, as json: %d\n%s", code, out)
	}
	if got := c.net["session_run"]; got != "open" {
		t.Fatalf("the record holds %q after on", got)
	}
}

func TestSessionNetworkRefusalsAreInWordsWithTheNextAction(t *testing.T) {
	c := newSwitchCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	// an ended session
	_, errs, code := auditExec(t, bin, cfg, dir, env, "session", "network", "off", "fleetdead")
	if code == 0 || !strings.Contains(errs, "has ended") || !strings.Contains(errs, "nothing changed") || !strings.Contains(errs, "ks session list") {
		t.Fatalf("ended: %d\n%s", code, errs)
	}
	// the engine could not take it: nothing changed, and the way to try again
	c.refuse = "ks_engine_refused"
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "network", "off", "fleetrun")
	// the client treats a 5xx answer to a change as an outcome it cannot be sure of (it
	// never resends), and says what the service said and how to check
	if code != exitTemporary || !strings.Contains(errs, "the engine could not change this session's network, so nothing was changed") || !strings.Contains(errs, "check the console's session list") {
		t.Fatalf("engine refused: %d\n%s", code, errs)
	}
	if c.net["session_run"] != "open" {
		t.Fatalf("a refused change moved the record to %q", c.net["session_run"])
	}
	c.refuse = ""
	// something other than on or off first
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "network", "maybe", "fleetrun")
	if code != exitUsage || !strings.Contains(errs, "not on or off") || !strings.Contains(errs, "Nothing changed") {
		t.Fatalf("a state that is neither: %d\n%s", code, errs)
	}
	// a record that moved between the read and the change
	c.mu.Lock()
	c.rev = 4
	c.mu.Unlock()
	stale := &staleOnce{ctl: c}
	srv2 := httptest.NewServer(stale)
	defer srv2.Close()
	bin2, cfg2 := buildAndAuth(t, srv2)
	_, errs, code = auditExec(t, bin2, cfg2, dir, fastEnv(cfg2), "session", "network", "off", "fleetrun")
	if code == 0 || !strings.Contains(errs, "changed since it was read") || !strings.Contains(errs, "nothing changed") {
		t.Fatalf("a stale revision: %d\n%s", code, errs)
	}
}

// staleOnce moves the record's revision between the read and the change.
type staleOnce struct {
	ctl  *switchCtl
	read bool
}

func (s *staleOnce) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.ctl.ServeHTTP(w, r)
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/session_") && !s.read {
		s.read = true
		s.ctl.mu.Lock()
		s.ctl.rev++
		s.ctl.mu.Unlock()
	}
}

func TestSessionAutoShowsAndSwitchesAutoMode(t *testing.T) {
	c := newSwitchCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "auto", "fleetrun")
	if code != 0 || !strings.Contains(out, "Auto Mode is on: the agent runs everything without asking, except deleting files") {
		t.Fatalf("show: %d\n%s%s", code, out, errs)
	}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "auto", "off", "fleetrun")
	if code != 0 || !strings.Contains(out, "Auto Mode is now off for session fleetrun0000") || !strings.Contains(out, "earlier decisions are unchanged") {
		t.Fatalf("off: %d\n%s%s", code, out, errs)
	}
	p := c.patched()
	if len(p) != 1 || p[0]["permission_mode"] != "manual" || len(p[0]) != 2 {
		t.Fatalf("what was sent: %v", p)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "auto", "on", "fleetpark")
	if code != 0 || !strings.Contains(out, "now on") || c.perm["session_park"] != "auto" {
		t.Fatalf("on for a paused session: %d\n%s (record %q)", code, out, c.perm["session_park"])
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "auto", "on", "fleetpark")
	if code != 0 || !strings.Contains(out, "already on") {
		t.Fatalf("on again: %d\n%s", code, out)
	}
}

// With no session named, the verbs follow the same order every session verb
// does: this project's binding.
func TestSessionNetworkAndAutoFollowTheProjectBinding(t *testing.T) {
	c := newSwitchCtl()
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	signIn(t, cfg, srv.URL, "guard-test-token", "acct_x")
	dir := project(t)
	env := fastEnv(cfg)
	if _, errs, code := auditExec(t, bin, cfg, dir, env, "session", "use", "fleetpark"); code != 0 {
		t.Fatalf("session use: %d\n%s", code, errs)
	}
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "network", "off")
	if code != 0 || !strings.Contains(out, "fleetpark000") || c.net["session_park"] != "off" {
		t.Fatalf("network off by binding: %d\n%s%s (record %q)", code, out, errs, c.net["session_park"])
	}
	if !strings.Contains(errs, "the binding") {
		t.Errorf("the target was not shown before acting:\n%s", errs)
	}
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "auto", "on")
	if code != 0 || c.perm["session_park"] != "auto" {
		t.Fatalf("auto on by binding: %d\n%s%s", code, out, errs)
	}
}

func TestTheTwoVerbsAreInTheRegistryAndTheManifest(t *testing.T) {
	for _, verb := range []string{"session network", "session auto"} {
		found := false
		for _, row := range loadManifest(t) {
			if row.Verb == verb {
				found = true
				if row.Surface != "hosted" || !strings.Contains(row.Usage, "[on|off]") && !strings.Contains(row.Usage, "[state]") {
					t.Errorf("%s in the manifest: surface %q usage %q", verb, row.Surface, row.Usage)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in commands.json", verb)
		}
	}
}

// BACKLOG-806 review: the day's bytes against the cap are shown beside the
// switch; nothing is shown when the machine could not be asked; and the words
// do not say KeepState's own services are unreachable (its public site and API are not).
func TestSessionNetworkShowsTheDaysBytesAgainstTheCapAndSaysWhenItIsReached(t *testing.T) {
	c := newSwitchCtl()
	c.bytes = 1536 << 10
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "network", "fleetrun")
	if code != 0 || !strings.Contains(out, "today") || !strings.Contains(out, "1.5 MiB of 5.0 GiB") {
		t.Fatalf("show: %d\n%s%s", code, out, errs)
	}
	c.mu.Lock()
	c.bytes, c.capReached = 5<<30, true
	c.mu.Unlock()
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "fleetrun")
	if code != 0 || !strings.Contains(out, "daily data limit reached") || !strings.Contains(out, "5.0 GiB of 5.0 GiB") {
		t.Fatalf("show at the cap: %d\n%s", code, out)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "fleetrun", "--json")
	if code != 0 || !strings.Contains(out, `"bytes_today"`) || !strings.Contains(out, `"cap_reached"`) {
		t.Fatalf("show as json: %d\n%s", code, out)
	}
	// a machine the service could not ask: no figure, and no zero
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "network", "fleetpark")
	if code != 0 || strings.Contains(out, "today") {
		t.Fatalf("a machine the service could not ask shows a figure: %d\n%s", code, out)
	}
}

func TestTheNetworkWordsDoNotOverclaimWhatStaysUnreachable(t *testing.T) {
	for _, f := range []string{"session_switch.go", "main.go", "docs/changelog.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "KeepState's own services") {
				t.Errorf("%s says KeepState's own services stay unreachable, which its public site and API are not: %s", f, strings.TrimSpace(line))
			}
		}
	}
	b, _ := os.ReadFile("session_switch.go")
	if !strings.Contains(string(b), "KeepState's internal services") {
		t.Error("the verb's always-blocked note must name KeepState's internal services")
	}
}
