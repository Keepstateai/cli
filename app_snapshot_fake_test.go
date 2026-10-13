package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// snapCtl is the control plane the app's snapshots run against: the agent
// fake (agentCtl) with a fuller account around it, so every screen the app
// has can be reached by a person's keys. The account holds:
//
//	checkout  main (waiting for permission) and reviewer (ready)
//	api       dev1, in a session that is paused (saved and stopped)
//	billing   ops, in a session paused because the account ran out of credit
//	migrate   fixer, whose runner stopped in the middle of an instruction
//	697ca7    a plain session (no agents)
//
// and a new agent session can be started (two usable keys: the app asks
// which one), with a provisioning step slow enough to be seen.
type snapCtl struct {
	*agentCtl
	smu        sync.Mutex
	resumed    bool // the api session was resumed
	created    bool // the new agent session exists
	slow       time.Duration
	streamSent map[string]bool
}

const (
	snapParkRec   = "session_park"
	snapFundsRec  = "session_funds"
	snapRecovRec  = "session_rec2"
	snapNewRec    = "session_new1"
	snapPlainID   = "697ca7f1a5df"
	snapDisclose  = "resuming starts the session's runtime again, and runtime is charged from the moment it runs; the session's usage shows the standing"
	snapParkNote  = "the session is saved and stopped. Opening did not wake it; nothing runs and no runtime is charged until you resume it"
	snapFundsNote = "the account's credits ran out, so the session was saved and paused (never killed); nothing runs and nothing is charged until it is resumed"
)

func newSnapCtl() *snapCtl {
	c := &snapCtl{agentCtl: newAgentCtl(), slow: 1500 * time.Millisecond, streamSent: map[string]bool{}}
	// the snapshots show one permission request, for the checkout session
	c.agentCtl.approvals = map[string]map[string]any{
		"apr_push1": approvalDoc("apr_push1", "shell", "git push origin main", "pending", 1, "hash-push-1"),
	}
	c.agentCtl.approvals["apr_push1"]["arguments_json"] = map[string]any{"cmd": "git push origin main", "cwd": "/workspace/checkout"}
	c.agentCtl.approvals["apr_push1"]["expires_at"] = time.Now().UTC().Add(9 * time.Minute).Format(time.RFC3339)
	c.agentCtl.approvals["apr_push1"]["requested_for_task"] = "tsk_1"
	return c
}

func snapAgent(id, rec, name, activity string, primary bool, runtime map[string]any) map[string]any {
	a := map[string]any{"id": id, "session_id": rec, "name": name, "is_primary": primary, "activity": activity,
		"observed_at": time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339), "active_task_id": "",
		"queue_revision": 1, "revision": 2, "created_at": "2026-09-20T11:00:00Z", "supervisor_live": true}
	if runtime != nil {
		a["session_runtime"] = runtime
	}
	return a
}

func (c *snapCtl) agentsOf(rec string) []map[string]any {
	c.smu.Lock()
	resumed := c.resumed
	c.smu.Unlock()
	switch rec {
	case agentSessionRecord:
		return []map[string]any{
			snapAgent("agt_main0001", rec, "main", "waiting_approval", true, nil),
			snapAgent("agt_twin0001", rec, "reviewer", "ready", false, nil),
		}
	case snapParkRec:
		if resumed {
			return []map[string]any{snapAgent("agt_dev10001", rec, "dev1", "ready", true, nil)}
		}
		return []map[string]any{snapAgent("agt_dev10001", rec, "dev1", "ready", true,
			map[string]any{"state": "parked", "note": snapParkNote})}
	case snapFundsRec:
		return []map[string]any{snapAgent("agt_ops00001", rec, "ops", "working", true,
			map[string]any{"state": "parked", "reason": parkReasonFunds, "note": snapFundsNote, "next_action": "add_credits"})}
	case snapRecovRec:
		return []map[string]any{snapAgent("agt_fix00001", rec, "fixer", "recovery_required", true, nil)}
	case snapNewRec:
		return []map[string]any{snapAgent("agt_help0001", rec, "helper", "ready", true, nil)}
	}
	return nil
}

func (c *snapCtl) inventory() []map[string]any {
	c.smu.Lock()
	resumed, created := c.resumed, c.created
	c.smu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	row := func(id, short, name, state, rec string) map[string]any {
		r := map[string]any{"id": id, "short_id": short, "name": name, "runtime_state": state, "image": "base",
			"budget_tokens": 500000, "execution_epoch": 1, "observed_at": now, "created_at": "2026-09-20T11:00:00Z",
			"agent_activity": "unavailable", "task_state": "unavailable", "key_alias": "prod"}
		if rec != "" {
			r["record_id"] = rec
		}
		return r
	}
	parkState := "parked"
	if resumed {
		parkState = "running"
	}
	funds := row("fundsession00000000000000000cccc", "9c41e0", "billing", "parked", snapFundsRec)
	funds["park_reason"] = parkReasonFunds
	items := []map[string]any{
		row("agentsession0000000000000000aaaa", "3f2a1c", "checkout", "running", agentSessionRecord),
		row("parksession00000000000000000bbbb", "5335fa", "api", parkState, snapParkRec),
		funds,
		row("recovsession0000000000000000dddd", "b7d210", "migrate", "running", snapRecovRec),
		row(snapPlainID, "697ca7", "697ca7", "running", ""),
	}
	if created {
		items = append(items, row("newsession000000000000000000eeee", "e81f02", "helper-session", "running", snapNewRec))
	}
	return items
}

func (c *snapCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	env := func(code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	fault := func(code int, kind, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "code": kind, "message": message}})
	}
	p := r.URL.Path
	agentID := strings.SplitN(strings.TrimPrefix(p, "/api/v2/agents/"), "/", 2)[0]
	lease := func() map[string]any {
		c.agentCtl.mu.Lock()
		defer c.agentCtl.mu.Unlock()
		return c.agentCtl.leaseDoc()
	}
	window := func(a map[string]any) {
		env(200, map[string]any{"agent": a, "reconnected": false, "lease": lease(),
			"resume": map[string]any{"after_seq": 41, "cursor": "cur_41"}, "queue_depth": 0})
	}
	switch {
	case p == "/api/capabilities":
		caps := []map[string]any{}
		for _, id := range []string{"session.list", "agent.workspace", "agents.stop", "agents.open", "deletion.plans"} {
			caps = append(caps, map[string]any{"id": id, "availability": "available", "summary": "s", "surface": "api"})
		}
		env(200, map[string]any{"registry_version": "t", "build": "b", "fetched_at": "2026-09-20T12:00:00Z", "price_book": "v1.3",
			"capabilities": caps, "limits": map[string]any{}})
		return
	case r.Method == "POST" && p == "/api/v2/sessions/"+agentSessionRecord+"/deletion-plan":
		env(201, map[string]any{"id": "dplan_7c1", "expires_at": time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339), "plan": map[string]any{
			"session":         map[string]any{"id": agentSessionRecord, "name": "checkout", "revision": 4, "runtime_state": "running", "execution_epoch": 1},
			"agents":          map[string]any{"count": 2, "effect": "removed; each keeps its history"},
			"tasks":           map[string]any{"queued_or_held": 1, "running": 0, "effect": "queued and held are cancelled; running move to cancelling and read cancelled only on a confirmed stop"},
			"approvals":       map[string]any{"pending": 1, "effect": "revoked"},
			"controllers":     map[string]any{"live": 1, "effect": "released; every fence is invalid"},
			"admissions":      map[string]any{"reserved": 0, "effect": "released; reconciled usage stays attributed"},
			"checkpoints":     map[string]any{"records": 3, "sharing_content_with_a_sibling": 0, "effect": "records removed from this session; once its runtime stops, the engine erases the content only this session held"},
			"results":         map[string]any{"count": 1, "effect": "deleted with their bytes; every outstanding download grant revoked"},
			"advisers":        map[string]any{"connections": 0, "waiting_consultations": 0, "effect": "withdrawn"},
			"branches":        map[string]any{"surviving_children": 0, "effect": "forks are separate sessions and are not deleted"},
			"runtime_cleanup": "the fleet session agentsession0000 is stopped for good by a cleanup operation recorded in the deletion's own commit",
			"cost":            "no charge for the deletion; retained storage continues to meter under the policy until account deletion",
			"erasure":         map[string]any{"expected": "complete", "why": "no surviving fork or parent shares this session's content"},
			"retention":       map[string]any{"checkpoint_content": "erased with the session unless a preservation ruling covers it"},
		}})
		return
	case r.Method == "DELETE" && p == "/api/v2/sessions/"+agentSessionRecord:
		env(200, map[string]any{"deleted": true, "executed_at": time.Now().UTC().Format(time.RFC3339), "agents_removed": 2, "tasks_cancelled": 1, "results_deleted": 1,
			"runtime_cleanup": map[string]any{
				"runtime_stop":  map[string]any{"state": "stopped", "detail": "the machine was stopped for good", "operation": "dplan_7c1", "attempts": 1},
				"content_purge": map[string]any{"state": "erased", "detail": "the content only this session held was erased"}},
			"retention": map[string]any{"checkpoint_content": "erased"}})
		return
	case r.Method == "POST" && strings.HasSuffix(p, "/stop") && strings.HasPrefix(p, "/api/v2/agents/"):
		env(200, map[string]any{"interrupt_requested_task": "tsk_1", "task_state": "cancelling", "held_tasks": []string{"tsk_2"},
			"session_runtime_state": "running", "charges": "the session keeps running, so runtime and storage may still be charged",
			"park":   map[string]any{"action": "save_and_pause", "label": "Save and pause", "request": "POST /api/v2/sessions/" + agentSessionRecord + "/pause"},
			"resume": "ks agent queue release main --session 3f2a1c", "hold": map[string]any{"id": "hold_9"}})
		return
	case p == "/api/v2/sessions" && r.Method == "GET" && r.URL.Query().Get("source") == "fleet":
		env(200, map[string]any{"items": c.inventory(), "next_cursor": "", "observed_at": "x"})
		return
	case p == "/api/v2/agents" && r.Method == "GET":
		rec := r.URL.Query().Get("session_id")
		if rec == snapPlainID {
			fault(404, "ks_not_found", "no such resource")
			return
		}
		items := c.agentsOf(rec)
		if items == nil {
			items = []map[string]any{}
		}
		env(200, map[string]any{"items": items, "observed_at": "x"})
		return
	case p == "/api/v2/approvals" && r.Method == "GET" && r.URL.Query().Get("session_id") != agentSessionRecord:
		env(200, map[string]any{"items": []any{}, "observed_at": "x"})
		return
	case r.Method == "POST" && strings.HasSuffix(p, "/open") && agentID == "agt_dev10001":
		c.smu.Lock()
		resumed := c.resumed
		c.smu.Unlock()
		a := c.agentsOf(snapParkRec)[0]
		if resumed {
			window(a)
			return
		}
		env(200, map[string]any{"agent": a, "reconnected": false, "lease": nil, "queue_depth": 0,
			"runtime": map[string]any{"session_id": snapParkRec, "state": "parked", "running": false, "woken": false,
				"note": snapParkNote, "last_saved_at": time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339), "last_checkpoint_id": "ck_4471",
				"actions": []any{
					map[string]any{"action": "resume_session", "label": "Resume session", "request": "POST /api/v2/sessions/" + snapParkRec + "/resume", "discloses": snapDisclose},
					map[string]any{"action": "view_saved_transcript", "label": "View saved transcript", "request": "GET /api/v2/sessions/" + snapParkRec + "/events"},
				}}})
		return
	case r.Method == "POST" && strings.HasSuffix(p, "/open") && agentID == "agt_ops00001":
		env(200, map[string]any{"agent": c.agentsOf(snapFundsRec)[0], "reconnected": false, "lease": nil, "queue_depth": 1,
			"runtime": map[string]any{"session_id": snapFundsRec, "state": "parked", "running": false, "woken": false,
				"note": snapFundsNote,
				"actions": []any{
					map[string]any{"action": "add_credits", "label": "Add credit", "request": "console: billing"},
					map[string]any{"action": "resume_session", "label": "Resume session", "request": "POST /api/v2/sessions/" + snapFundsRec + "/resume", "discloses": snapDisclose},
				}}})
		return
	case r.Method == "POST" && strings.HasSuffix(p, "/open") && agentID == "agt_fix00001":
		env(200, map[string]any{"agent": c.agentsOf(snapRecovRec)[0], "reconnected": false, "lease": nil, "queue_depth": 2,
			"recovery": map[string]any{"activity": "recovery_required",
				"note": "the runner stopped in the middle of instruction tsk_9; what it did is unknown and the queue is held behind it",
				"actions": []any{
					map[string]any{"action": "open_recovery", "label": "Open recovery", "request": "GET /api/v2/queue-holds?agent_id=agt_fix00001"},
				}}})
		return
	case r.Method == "POST" && strings.HasSuffix(p, "/open") && agentID == "agt_help0001":
		window(c.agentsOf(snapNewRec)[0])
		return
	case r.Method == "POST" && p == "/api/v2/sessions/"+snapParkRec+"/resume":
		time.Sleep(c.slow)
		c.smu.Lock()
		c.resumed = true
		c.smu.Unlock()
		env(200, map[string]any{"runtime_state": "resuming", "note": "resumed from the last save"})
		return
	case r.Method == "POST" && p == "/api/v2/preflight":
		env(200, map[string]any{"blockers": []any{}})
		return
	case r.Method == "GET" && p == "/api/v2/keys":
		env(200, map[string]any{"items": []any{
			map[string]any{"id": "key_team", "provider": "anthropic", "alias": "team", "last4": "9f2c", "enabled": true},
			map[string]any{"id": "key_personal", "provider": "anthropic", "alias": "personal", "last4": "11ab", "enabled": true},
		}})
		return
	case r.Method == "POST" && p == "/api/v2/sessions":
		env(201, map[string]any{"id": snapNewRec, "revision": 1, "primary_agent_id": "agt_help0001"})
		return
	case r.Method == "POST" && p == "/api/v2/sessions/"+snapNewRec+"/bindings":
		env(200, map[string]any{"revision": 2})
		return
	case r.Method == "POST" && p == "/api/v2/sessions/"+snapNewRec+"/provision":
		time.Sleep(c.slow)
		c.smu.Lock()
		c.created = true
		c.smu.Unlock()
		env(200, map[string]any{"session_id": snapNewRec, "agent_id": "agt_help0001", "ready": true, "supervised": true, "agent_activity": "ready",
			"phases": []any{
				map[string]any{"phase": "machine", "done": true, "detail": "a machine is running for the session"},
				map[string]any{"phase": "supervisor", "done": true, "detail": "the agent's supervisor reported in"},
			}, "note": "the agent is ready for its first instruction"})
		return
	case r.Method == "GET" && strings.HasSuffix(p, "/view") && strings.HasPrefix(p, "/api/v2/agents/"):
		name, activity, label, pending := "main", "waiting_approval", "Waiting for you", 0
		c.agentCtl.mu.Lock()
		for id := range c.agentCtl.approvals {
			if c.agentCtl.approvals[id]["state"] == "pending" {
				pending++
			}
		}
		c.agentCtl.mu.Unlock()
		if pending == 0 {
			activity, label = "ready", "Ready"
		}
		switch agentID {
		case "agt_dev10001":
			name, activity, label, pending = "dev1", "ready", "Ready", 0
		case "agt_help0001":
			name, activity, label, pending = "helper", "ready", "Ready", 0
		case "agt_twin0001":
			name, activity, label, pending = "reviewer", "ready", "Ready", 0
		}
		env(200, map[string]any{
			"header": map[string]any{"agent_name": name, "session_name": "checkout", "controller": "controller", "runtime_state": "running", "activity": activity},
			"footer": map[string]any{"queued": 0, "pending_approvals": pending},
			"status": map[string]any{"label": label, "age_seconds": 2, "stale": false, "stale_after_seconds": 15},
			"usage":  map[string]any{"model_microusd": nil, "model_standing": "unavailable", "entries": []any{map[string]any{"kind": "model", "source": "gateway meter", "tokens": 1234}}},
		})
		return
	case r.Method == "GET" && strings.HasSuffix(p, "/events/stream") && strings.Contains(p, snapParkRec):
		c.serveCrashStream(w, r)
		return
	}
	c.agentCtl.ServeHTTP(w, r)
}

// serveCrashStream is the resumed api session's stream: its agent's runner
// stopped in the middle of an instruction while the session was paused, and
// the service closed the instruction as needing a decision.
func (c *snapCtl) serveCrashStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	frame := func(kind, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		fl.Flush()
	}
	frame("hello", `{"epoch":7}`)
	c.smu.Lock()
	sent := c.streamSent[snapParkRec]
	c.streamSent[snapParkRec] = true
	c.smu.Unlock()
	if !sent {
		time.Sleep(600 * time.Millisecond)
		frame("event", `{"event_id":"ev_c1","stream_seq":42,"subject_type":"task","subject_id":"tsk_mig","task_id":"tsk_mig","observed_at":"2026-09-20T12:00:03Z","payload":{"type":"task.finished","state":"reconciliation_required","summary":"the runner stopped while running the database migration","outcome":{"runner_turn":"interrupted","work":"unknown","acceptance":"not_reached","unsettled":[{"kind":"tool","id":"toolu_x1","detail":"Bash: ./migrate.sh up"}]}}}`)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
			frame("heartbeat", `{}`)
		}
	}
}
