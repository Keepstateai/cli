// ks session harness: reads a session's harness, its model, the harnesses
// its image carries and whether it can switch now; switches it only against
// the revision just read, while the session is paused; the service's
// refusals are printed with the next action.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type harnessCtl struct{ modelCtl }

func (c *harnessCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	refuse := func(code int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "error": map[string]any{"code": typ, "type": typ, "message": msg, "work_started": "no"}})
	}
	if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "session_park") {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.patches = append(c.patches, body)
		c.mu.Unlock()
		switch body["harness"] {
		case "codex":
			w.Header().Set("Content-Type", "application/json")
			model, _ := body["runner_model"].(string)
			if model == "" {
				model = "gpt-6-luna"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"id": "session_park", "revision": 6, "runtime_state": "parked", "runner_model": model, "harness": "codex"}})
		case "aider":
			refuse(422, "ks_harness_invalid", "harness is one of claude-code, codex")
		}
		return
	}
	if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "session_run") {
		refuse(409, "ks_session_not_paused", "the session is running; pause it to switch its harness. The switch takes effect when it resumes")
		return
	}
	if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "session_dead") {
		refuse(409, "ks_harness_not_in_image", "this session was created before Codex CLI was in its image, so it cannot switch to it; start a new session to use Codex CLI")
		return
	}
	c.modelCtl.ServeHTTP(w, r)
}

func TestSessionHarnessReadsAndSwitchesOnlyWhilePaused(t *testing.T) {
	c := &harnessCtl{modelCtl{model: "claude-haiku-4-5-20251001", rev: 5}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)

	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark")
	if code != 0 || !strings.Contains(out, "session fleetpark000 runs claude-code on claude-haiku-4-5-20251001") || !strings.Contains(out, "can switch now") ||
		!strings.Contains(out, "carries claude-code") {
		t.Fatalf("read of a paused session: %d\n%s%s", code, out, errs)
	}
	out, _, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetrun")
	if code != 0 || !strings.Contains(out, "pause it first (ks agent pause main --session fleetrun0000)") {
		t.Fatalf("read of a running session: %d\n%s", code, out)
	}
	if len(c.patched()) != 0 {
		t.Fatalf("a read sent a change: %v", c.patched())
	}
	// the switch: against the revision just read, the model only when named
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "codex")
	if code != 0 || !strings.Contains(out, "session fleetpark000 will run codex on gpt-6-luna from its next resume (was claude-code") ||
		!strings.Contains(out, "the next instruction starts a new conversation") || !strings.Contains(out, "ks agent history main --session fleetpark000") {
		t.Fatalf("switch: %d\n%s%s", code, out, errs)
	}
	ps := c.patched()
	if len(ps) != 1 || ps[0]["harness"] != "codex" || ps[0]["expected_revision"] != float64(5) {
		t.Fatalf("the switch sent: %v", ps)
	}
	if _, sent := ps[0]["runner_model"]; sent {
		t.Fatalf("a model nobody named was sent: %v", ps[0])
	}
	if _, _, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "codex", "--model", "gpt-6.1-sol"); code != 0 || c.patched()[1]["runner_model"] != "gpt-6.1-sol" {
		t.Fatalf("--model was not sent: %v", c.patched())
	}
	// refusals, each with its next action
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetrun", "codex")
	if code != exitConflict || !strings.Contains(errs, "pause it to switch its harness") || !strings.Contains(errs, "ks agent pause main --session fleetrun0000, then ks session harness fleetrun0000 codex") {
		t.Fatalf("switch of a running session: %d\n%s", code, errs)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetdead", "codex")
	if code == 0 || !strings.Contains(errs, "created before Codex CLI was in its image") || !strings.Contains(errs, "ks run --agent --harness codex") {
		t.Fatalf("switch of a session from before the image: %d\n%s", code, errs)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, env, "session", "harness", "fleetpark", "claude-code")
	if code != exitUsage || !strings.Contains(errs, "already runs claude-code") {
		t.Fatalf("switch to the same harness: %d\n%s", code, errs)
	}
}
