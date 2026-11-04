package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// restartServer plays a deploy of the control plane under an open stream:
// the first connection sends events 6 and 7 and the service's restart frame;
// the next finds the old instance still draining (503 ks_draining); the one
// after reaches the new instance, which sends 7 again (a cursor off by one
// must not show it twice), 8, and an ordinary end.
type restartServer struct {
	mu        sync.Mutex
	conns     int
	after     []string
	always503 bool
}

func (s *restartServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.conns++
	n := s.conns
	s.after = append(s.after, r.URL.Query().Get("after_seq"))
	s.mu.Unlock()
	ev := func(seq int64) string {
		b, _ := json.Marshal(map[string]any{"stream_seq": seq, "subject_type": "agent", "payload": map[string]any{"type": "agent.transcript", "kind": "assistant_text", "text": fmt.Sprint("part ", seq)}})
		return "event: event\ndata: " + string(b) + "\n\n"
	}
	status := "event: status\ndata: {\"agent_id\":\"a1\",\"state\":\"working\",\"reason\":\"working\"}\n\n"
	switch {
	case n == 1:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: hello\ndata: {\"after_seq\":5}\n\n"+ev(6)+ev(7)+status)
		fmt.Fprint(w, "event: end\ndata: {\"reason\":\"service_restarting\",\"reconnect\":true,\"after_seq\":7,\"retry_after_ms\":30}\n\n")
	case n == 2 || s.always503:
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"code":"ks_draining","message":"this control-plane instance is being replaced"}}`)
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: hello\ndata: {\"after_seq\":7}\n\n"+ev(7)+ev(8)+"event: status\ndata: {\"agent_id\":\"a1\",\"state\":\"idle\",\"reason\":\"ready\"}\n\n")
		fmt.Fprint(w, "event: end\ndata: {\"reason\":\"session_ended\"}\n\n")
	}
}

func restartQuick(t *testing.T) {
	t.Helper()
	q, b, m := restartQuiet, restartBackoff, restartBackoffMax
	restartQuiet, restartBackoff, restartBackoffMax = 2*time.Second, 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { restartQuiet, restartBackoff, restartBackoffMax = q, b, m })
}

// ks agent open and the app follow the stream through a restart: one
// reconnecting and one reconnected frame, every event once, from the cursor,
// and never the restart's end frame as an end.
func TestAStreamOutlastsARestartOfTheService(t *testing.T) {
	restartQuick(t)
	rs := &restartServer{}
	srv := httptest.NewServer(rs)
	defer srv.Close()
	var kinds []string
	var seqs []int64
	err := streamEventsWith(hostedCreds{CTL: srv.URL, Token: "t"}, "sess1", 5, true, func(kind string, data []byte) bool {
		kinds = append(kinds, kind)
		if kind == "event" {
			var e journalEvent
			_ = json.Unmarshal(data, &e)
			seqs = append(seqs, e.StreamSeq)
		}
		if kind == "end" {
			var e struct{ Reason string }
			_ = json.Unmarshal(data, &e)
			if e.Reason == "service_restarting" {
				t.Errorf("the restart's end frame reached the caller as an end")
			}
			return true
		}
		return false
	})
	if err != nil {
		t.Fatalf("the stream through a restart: %v", err)
	}
	if got := strings.Join(kinds, ","); got != "hello,event,event,status,reconnecting,reconnected,event,status,end" {
		t.Errorf("frames %s", got)
	}
	if fmt.Sprint(seqs) != "[6 7 8]" {
		t.Errorf("events %v; want 6 7 8, each once", seqs)
	}
	if fmt.Sprint(rs.after) != "[5 7 7]" {
		t.Errorf("connections asked after_seq %v; a reconnect continues after the last event shown", rs.after)
	}
}

// A restart that does not come back within the quiet window says so, with
// the way to reconnect, instead of waiting for ever.
func TestARestartThatDoesNotComeBackSaysTheConnectionWasLost(t *testing.T) {
	restartQuick(t)
	restartQuiet = 150 * time.Millisecond
	rs := &restartServer{always503: true}
	rs.conns = 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		first := rs.conns == 0
		rs.mu.Unlock()
		if first {
			rs.ServeHTTP(w, r)
			return
		}
		rs.mu.Lock()
		rs.conns++
		rs.mu.Unlock()
		w.WriteHeader(503)
	}))
	defer srv.Close()
	start := time.Now()
	err := streamEventsWith(hostedCreds{CTL: srv.URL, Token: "t"}, "sess1", 5, false, func(string, []byte) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "the live connection was lost") || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("a restart that never comes back answered %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("it waited %s, past the quiet window", d)
	}
}

// ks agent wait follows the status frames through a restart the same way.
func TestAWaitOutlastsARestartOfTheService(t *testing.T) {
	restartQuick(t)
	rs := &restartServer{}
	srv := httptest.NewServer(rs)
	defer srv.Close()
	var states []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := streamStatus(ctx, hostedCreds{CTL: srv.URL, Token: "t"}, "sess1", func(d statusRecord) bool {
		states = append(states, d.State)
		return d.State == "idle"
	})
	if err != nil || fmt.Sprint(states) != "[working idle]" {
		t.Fatalf("the wait through a restart: %v, states %v", err, states)
	}
}

// The app's conversation says an instruction paused at a limit calmly, and
// its carry-on in one line; no needs-attention card asks anyone to decide.
func TestTheAppSaysPausedAtTheBudgetAndCarryingOn(t *testing.T) {
	ev := func(p map[string]any) journalEvent {
		b, _ := json.Marshal(p)
		return journalEvent{SubjectType: "task", SubjectID: "task_a", Payload: b}
	}
	paused := ev(map[string]any{"type": "task.finished", "state": "failed", "summary": "the session's model budget stopped this task", "stopped_at": "token_budget"})
	line, ok := appEventLine(paused, "oz_test_agent", "acct_me")
	if !ok || !strings.Contains(line, "Paused at the token budget. It carries on when the session resumes.") || strings.Contains(line, "Failed") {
		t.Errorf("the paused instruction's line: %q", line)
	}
	if c := appRecoveryCard(ev(map[string]any{"type": "task.finished", "state": "reconciliation_required", "stopped_at": "token_budget"}), "oz_test_agent", "s1"); c != nil {
		t.Errorf("a needs-attention card for an instruction that carries on: %q", c)
	}
	carry, ok := appEventLine(ev(map[string]any{"type": "task.accepted", "carry_on_of": "task_a", "line": "Budget raised, carrying on."}), "oz_test_agent", "acct_me")
	if !ok || !strings.Contains(carry, "Budget raised, carrying on.") {
		t.Errorf("the carry-on's line: %q", carry)
	}
	if w := blockedWord(queueHold{Cause: causePausedAtLimit}); w != "paused instruction" {
		t.Errorf("the recovery view calls it %q", w)
	}
}
