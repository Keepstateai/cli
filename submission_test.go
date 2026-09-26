// KS-036: a lost acknowledgement is resolved through the submission lookup:
// accepted shows the receipt and sends nothing again; not_received resends
// under the SAME id; an unreadable answer is an error and nothing is resent.
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

type subCtl struct {
	mu        sync.Mutex
	mode      string // arrived | lost | unreadable
	posts     []string
	delivered map[string]bool
}

func (c *subCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
	}
	drop := func() {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}
	switch {
	case r.URL.Path == "/api/capabilities":
		fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
	case r.URL.Path == "/api/v2/sessions":
		env(200, map[string]any{"items": []any{map[string]any{"id": "fleetsub0000000000000000000000001", "short_id": "fleetsub0000", "name": "c", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
	case r.URL.Path == "/api/v2/agents":
		env(200, map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
	case r.URL.Path == "/api/v2/agents/agent_1/tasks" && r.Method == "POST":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		sid := fmt.Sprint(b["submission_id"])
		c.posts = append(c.posts, sid)
		switch {
		case c.mode == "arrived" && len(c.posts) == 1:
			c.delivered[sid] = true // committed, then the answer is lost
			drop()
		case c.mode == "lost" && len(c.posts) == 1:
			drop() // never arrived
		case c.mode == "unreadable":
			drop()
		default:
			c.delivered[sid] = true
			env(201, map[string]any{"id": "tsk_1", "agent_id": "agent_1", "state": "queued", "queue_seq": 4})
		}
	case strings.HasPrefix(r.URL.Path, "/api/v2/agents/agent_1/submissions/"):
		sid := strings.TrimPrefix(r.URL.Path, "/api/v2/agents/agent_1/submissions/")
		switch {
		case c.mode == "unreadable":
			w.WriteHeader(503)
			fmt.Fprint(w, `{"schema_version":2,"error":{"code":"ks_receipt_unreadable","type":"ks_receipt_unreadable","message":"the receipt could not be read"}}`)
		case c.delivered[sid]:
			env(200, map[string]any{"status": "accepted", "task": map[string]any{"id": "tsk_1", "agent_id": "agent_1", "state": "queued", "queue_seq": 4}, "queue_position": 1})
		default:
			env(200, map[string]any{"status": "not_received"})
		}
	default:
		w.WriteHeader(404)
	}
}

func TestALostAcknowledgementIsResolvedNeverGuessed(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		code      int
		posts     int
		want      string
		sameIDTwo bool
	}{
		{"arrived", 0, 1, "resolved after a lost acknowledgement", false},
		{"lost", 0, 2, "sending it again under the same id", true},
		{"unreadable", exitTemporary, 1, "it was NOT sent again", false},
	} {
		c := &subCtl{mode: tc.mode, delivered: map[string]bool{}}
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "agent", "tell", "main", "run the tests", "--session", "fleetsub")
		srv.Close()
		if code != tc.code || len(c.posts) != tc.posts || !strings.Contains(out+errs, tc.want) {
			t.Errorf("%s: exit %d posts %v\n%s%s", tc.mode, code, c.posts, out, errs)
		}
		if tc.sameIDTwo && (len(c.posts) != 2 || c.posts[0] != c.posts[1]) {
			t.Errorf("%s: the resend did not reuse the submission id: %v", tc.mode, c.posts)
		}
		if tc.code == 0 && !strings.Contains(out, "tsk_1") {
			t.Errorf("%s: no receipt shown:\n%s", tc.mode, out)
		}
	}
}
