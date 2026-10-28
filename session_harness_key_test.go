// ks session harness --key: a switch to a harness of another provider binds a
// key for that provider in the same change; the service's key refusals are
// printed with the next action, and a session being saved is not told to
// pause.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type harnessKeyCtl struct{ harnessCtl }

func (c *harnessKeyCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "session_park") {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.patches = append(c.patches, body)
		c.mu.Unlock()
		refuse := func(code int, typ, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
		}
		switch body["key"] {
		case nil:
			refuse(409, "ks_key_ambiguous", "the account holds more than one enabled OpenAI key; name the one the new harness calls (key: <id>)")
		case "vlt_bad":
			refuse(422, "ks_key_invalid", "key vlt_bad is not an enabled OpenAI key of this account")
		case "vlt_none":
			refuse(422, "ks_no_inference_key", "the new harness calls OpenAI, and this session has no route to it")
		case "vlt_split":
			refuse(409, "ks_key_unroutable", "this switch would leave the session unable to call Anthropic, which it calls now")
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "session_park", "revision": 6, "runtime_state": "parked", "runner_model": "gpt-6-luna", "harness": "codex"}})
		}
		return
	}
	c.harnessCtl.ServeHTTP(w, r)
}

func TestSessionHarnessKeyIsSentAndItsRefusalsSayWhatToDo(t *testing.T) {
	c := &harnessKeyCtl{harnessCtl{modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "codex", "--key", "keepstate")
	if code != 0 || !strings.Contains(out, "go on KeepState's key") {
		t.Fatalf("switch on KeepState's key: %d\n%s%s", code, out, errs)
	}
	if ps := c.patched(); len(ps) != 1 || ps[0]["key"] != "keepstate" || ps[0]["harness"] != "codex" {
		t.Fatalf("the switch sent: %v", ps)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "codex", "--key", "vlt_own")
	if code != 0 || !strings.Contains(out, "go on your key vlt_own") {
		t.Fatalf("switch on an own key: %d\n%s", code, out)
	}
	for _, tc := range []struct{ key, says, next string }{
		{"", "more than one enabled OpenAI key", "ks key list, then ks session harness fleetpark000 codex --key <key-id>"},
		{"vlt_bad", "not an enabled OpenAI key", "ks session harness fleetpark000 codex --key <key-id>"},
		{"vlt_none", "no route to it", "ks key add --provider <provider> < key.txt, or ks session harness fleetpark000 codex --key keepstate"},
		{"vlt_split", "unable to call Anthropic", "ks session key set fleetpark000 --key <key-id|keepstate>, then ks session harness fleetpark000 codex"},
	} {
		args := []string{"session", "harness", "fleetpark", "codex"}
		if tc.key != "" {
			args = append(args, "--key", tc.key)
		}
		_, errs, code = auditExec(t, bin, cfg, dir, env, args...)
		if code == 0 || !strings.Contains(errs, tc.says) || !strings.Contains(errs, "nothing changed") || !strings.Contains(errs, tc.next) {
			t.Fatalf("refusal for key %q: %d\n%s", tc.key, code, errs)
		}
	}
	// --key without a harness is a usage error, and nothing is sent
	n := len(c.patched())
	if _, errs, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "--key", "keepstate"); code != exitUsage || !strings.Contains(errs, "ks session key set fleetpark000") {
		t.Fatalf("--key without a harness: %d\n%s", code, errs)
	}
	if len(c.patched()) != n {
		t.Fatal("--key without a harness sent a change")
	}
}

func TestSessionHarnessSaysASessionBeingSavedIsNotRunning(t *testing.T) {
	for _, st := range []string{"stopping", "checkpointing"} {
		can, text := harnessSwitchStanding(st, "ks agent pause main --session s")
		if can || strings.Contains(text, "pause it first") || !strings.Contains(text, "being saved") {
			t.Fatalf("%s: %v %q", st, can, text)
		}
		if can, text = modelChangeStanding(st, "ks agent pause main --session s"); can || strings.Contains(text, "pause it first") || !strings.Contains(text, "being saved") {
			t.Fatalf("model, %s: %v %q", st, can, text)
		}
	}
}
