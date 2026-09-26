// BACKLOG-170 client: the legacy session verbs addressed by a session_...
// id show the named refusals the service now answers, with the next action.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func noShellCtl() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		refuse := func(code, msg, next string) {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg, "next_action": next})
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/sessions/session_agent/exec"):
			refuse("ks_agent_session_no_shell", "this is an agent workspace session, and `ks exec` is refused on one", "ks agent tell")
		case strings.HasPrefix(r.URL.Path, "/api/sessions/session_agent/attach"):
			refuse("ks_agent_session_no_shell", "this is an agent workspace session, and it has no terminal to attach to", "ks agent open")
		case strings.HasPrefix(r.URL.Path, "/api/sessions/session_empty"):
			refuse("ks_session_no_engine", "this workspace session has no engine session behind it, so there is nothing to act on. Nothing was sent to an engine", "GET /api/v2/sessions/session_empty")
		case strings.HasPrefix(r.URL.Path, "/api/sessions/session_agent"):
			refuse("ks_agent_session_lifecycle", "this is an agent workspace session; its lifecycle is on the workspace routes", "POST /api/v2/sessions/session_agent/pause")
		default:
			w.WriteHeader(404)
		}
	})
}

func TestLegacyVerbsShowTheNamedRefusals(t *testing.T) {
	srv := httptest.NewServer(noShellCtl())
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"exec", "session_agent", "--", "ls"}, []string{"[ks_agent_session_no_shell]", "ks agent tell <agent> \"<the work>\" --session session_agent"}},
		{[]string{"attach", "session_agent"}, []string{"[ks_agent_session_no_shell]", "no terminal to attach to", "ks agent open <agent> --session session_agent"}},
		{[]string{"exec", "session_empty", "--", "ls"}, []string{"[ks_session_no_engine]", "the session's record: GET /api/v2/sessions/session_empty"}},
		{[]string{"kill", "session_empty"}, []string{"[ks_session_no_engine]", "no runtime to kill"}},
		{[]string{"wake", "session_agent"}, []string{"ks_agent_session_lifecycle", "POST /api/v2/sessions/session_agent/pause (as the service names it)"}},
	}
	for _, c := range cases {
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), c.args...)
		if code != exitConflict {
			t.Errorf("%v: exit %d\n%s", c.args, code, errs)
		}
		for _, w := range c.want {
			if !strings.Contains(errs, w) {
				t.Errorf("%v lacks %q:\n%s", c.args, w, errs)
			}
		}
	}
	// --json names the refusal as its kind
	out, errs, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "exec", "--json", "session_agent", "--", "ls")
	if !strings.Contains(out+errs, `"ks_agent_session_no_shell"`) {
		t.Errorf("json kind: %s%s", out, errs)
	}
}
