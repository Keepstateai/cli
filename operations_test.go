// operations_test: the permanent guards of KS-004's client half. A
// scripted control plane on the loopback plays the failure modes the
// network produces: a reply lost after the commit, an address nobody
// answers, a checkpoint that outlasts the response deadline, a body that
// never finishes. Every case counts what the control plane was asked to
// create, because "one session" is the whole point.
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

// opCtl is a control plane with the KS-004 operation layer, in miniature:
// it keys creations by Idempotency-Key, can drop the reply of the first
// arrival after committing, and can make a checkpoint run long.
type opCtl struct {
	mu        sync.Mutex
	created   map[string]string // key -> session id
	creates   int               // committed creations
	dropFirst bool              // drop the connection after the first commit
	dropped   bool
	slowOps   map[string]time.Time // operation id -> finish time
	requests  []string
}

func newOpCtl() *opCtl { return &opCtl{created: map[string]string{}, slowOps: map[string]time.Time{}} }

func (c *opCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	key := r.Header.Get("Idempotency-Key")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/sessions":
		c.mu.Lock()
		id, seen := c.created[key]
		if !seen {
			c.creates++
			id = fmt.Sprintf("sess-%d", c.creates)
			c.created[key] = id // the record: this control plane enforces the key
			if c.dropFirst && !c.dropped {
				c.dropped = true
				c.mu.Unlock()
				// committed, then the wire dies before the reply
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		} else {
			w.Header().Set("KS-Operation-Replayed", "true")
		}
		c.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"image":"base","state":"running"}`, id)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/operations/ksop_"):
		// the record of a create, read back by its key
		c.mu.Lock()
		id, seen := c.created[strings.TrimPrefix(r.URL.Path, "/api/operations/")]
		c.mu.Unlock()
		if !seen {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"type":"ks_not_found","message":"no such operation"}}`)
			return
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"operation_id":"op_%s","state":"succeeded","http_status":200,"finished_at":"2026-09-20T00:00:01Z","accepted_at":"2026-09-20T00:00:00Z","method":"POST","path":"/api/sessions","response":{"id":%q,"image":"base","state":"running"}}}`, id, id)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoint"):
		if strings.Contains(r.Header.Get("Prefer"), "respond-async") {
			c.mu.Lock()
			c.slowOps["op_"+key] = time.Now().Add(700 * time.Millisecond)
			c.mu.Unlock()
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"schema_version":2,"data":{"operation_id":%q,"state":"accepted"}}`, "op_"+key)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	case r.Method == "GET" && r.URL.Path == "/api/capabilities":
		// the KS-004 control plane declares the capability the record verbs need
		fmt.Fprint(w, `{"schema_version":2,"request_id":"r","data":{"registry_version":"test","build":"abc","fetched_at":"2026-09-20T00:00:00Z","price_book":"v1.3","capabilities":[{"id":"operations.idempotent","availability":"available","summary":"ops","surface":"api"}],"limits":{}}}`)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/operations/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/operations/")
		c.mu.Lock()
		finish, ok := c.slowOps[id]
		c.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"type":"ks_not_found","message":"no such operation"}}`)
			return
		}
		state := "running"
		resp := ""
		if time.Now().After(finish) {
			state = "succeeded"
			resp = `,"http_status":200,"finished_at":"2026-09-20T00:00:01Z","response":{"ok":true,"checkpoint":"cp-1"}`
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":{"operation_id":%q,"state":%q,"method":"POST","path":"/api/sessions/s1/checkpoint","accepted_at":"2026-09-20T00:00:00Z"%s}}`, id, state, resp)
	default:
		fmt.Fprint(w, `{}`)
	}
}

func fastEnv(cfg string) []string {
	return []string{"XDG_CONFIG_HOME=" + cfg, "KS_HTTP_TIMEOUT_MS=400", "KS_WAIT_TIMEOUT_MS=3000"}
}

func readLedger(t *testing.T, cfg string) []localOp {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg, "keepstate", "operations.jsonl"))
	if err != nil {
		return nil
	}
	var out []localOp
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var o localOp
		if json.Unmarshal([]byte(l), &o) == nil {
			out = append(out, o)
		}
	}
	return out
}

// QA-004-1: the control plane commits a run then drops its reply; the
// outcome is recovered by READING the record under the same key, never
// by resending: one POST, the original session.
func TestLostReplyRecoversByRead(t *testing.T) {
	ctl := newOpCtl()
	ctl.dropFirst = true
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	if code != 0 || strings.TrimSpace(out) != "sess-1" {
		t.Fatalf("exit %d, stdout %q\n%s", code, out, errs)
	}
	if ctl.creates != 1 {
		t.Fatalf("the control plane created %d sessions for one command, want 1", ctl.creates)
	}
	posts := 0
	for _, r := range ctl.requests {
		if strings.HasPrefix(r, "POST ") {
			posts++
		}
	}
	if posts != 1 || strings.Contains(errs, "retrying") || !strings.Contains(errs, "holds a record") {
		t.Errorf("want one POST and a recovery by read, got %d POST(s):\n%s", posts, errs)
	}
	keys := map[string]bool{}
	for _, r := range ctl.requests {
		_ = r
	}
	led := readLedger(t, cfg)
	if len(led) != 1 || led[0].Method != "POST" || led[0].Path != "/api/sessions" {
		t.Fatalf("local ledger: %+v", led)
	}
	if _, ok := ctl.created[led[0].Key]; !ok {
		t.Errorf("the key recorded locally (%s) is not the key the control plane saw", led[0].Key)
	}
	_ = keys
}

// QA-004-3: a checkpoint that outlasts the ordinary response deadline is
// accepted with 202 and finished by polling, never restarted.
func TestLongCheckpointGoesThroughAcceptedAndPolling(t *testing.T) {
	ctl := newOpCtl()
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "checkpoint", "s1")
	if code != 0 || !strings.Contains(out, "checkpointed s1") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	posts, polls := 0, 0
	for _, r := range ctl.requests {
		if strings.HasPrefix(r, "POST ") {
			posts++
		}
		if strings.HasPrefix(r, "GET /api/operations/") {
			polls++
		}
	}
	if posts != 1 || polls < 1 {
		t.Errorf("posts %d (want 1), polls %d (want >= 1): %v", posts, polls, ctl.requests)
	}
	if !strings.Contains(errs, "accepted; waiting") {
		t.Errorf("the 202 was not reported:\n%s", errs)
	}
	// ks operation show reads the same record
	out, _, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "operation", "show", "op_"+readLedger(t, cfg)[0].Key)
	if code != 0 || !strings.Contains(out, "state succeeded") || !strings.Contains(out, `"checkpoint":"cp-1"`) {
		t.Errorf("operation show:\n%s", out)
	}
}

// VER-004-2: the timeout classes, each bounded on its own.
func TestTimeoutClassesAreBounded(t *testing.T) {
	// connect: a non-routable address must give up at the connect deadline
	srv := httptest.NewServer(http.NotFoundHandler())
	bin, cfg := buildAndAuth(t, srv)
	srv.Close()
	tok, _ := json.Marshal(map[string]string{"token": "t", "ctl": "http://10.255.255.1:9"})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "token.json"), tok, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "meter", "s1")
	if code == 0 || time.Since(start) > 5*time.Second {
		t.Errorf("connect timeout: exit %d after %s\n%s", code, time.Since(start), errs)
	}

	// ordinary read: a server that accepts and never answers
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	defer stall.Close()
	bin, cfg = buildAndAuth(t, stall)
	start = time.Now()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "meter", "s1")
	if code == 0 || time.Since(start) > 2500*time.Millisecond {
		t.Errorf("read timeout: exit %d after %s\n%s", code, time.Since(start), errs)
	}

	// operation wait: a checkpoint whose operation never finishes ends at
	// the wait bound with the record named, exit 4, and one POST only
	ctl := newOpCtl()
	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/operations/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"schema_version":2,"data":{"operation_id":"op_x","state":"running","accepted_at":"2026-09-20T00:00:00Z"}}`)
			return
		}
		ctl.ServeHTTP(w, r)
	}))
	defer never.Close()
	bin, cfg = buildAndAuth(t, never)
	start = time.Now()
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "checkpoint", "s1")
	if code != 4 || !strings.Contains(errs, "still running") || !strings.Contains(errs, "ks operation show op_") {
		t.Errorf("wait bound: exit %d after %s\n%s", code, time.Since(start), errs)
	}
	posts := 0
	for _, r := range ctl.requests {
		if strings.HasPrefix(r, "POST ") {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("a timed-out wait re-posted: %d posts", posts)
	}
}

// A control plane without the operation layer: the client keeps working
// synchronously and says the record cannot be read back, rather than
// inventing one.
func TestOlderControlPlaneWithoutOperations(t *testing.T) {
	// a real older control plane answers an unknown route with the mux's
	// plain 404, not with JSON; mount only the routes it has
	rec := &auditRecorder{}
	mux := http.NewServeMux()
	mux.Handle("/api/sessions", rec)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	if code != 0 || strings.TrimSpace(out) != "sess-audit" {
		t.Fatalf("plain 200 path: exit %d %q", code, out)
	}
	// the record verb is disabled by the missing capability registry, with
	// the reason, before it asks for a record the older control plane has not got
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "operation", "show", "ksop_whatever")
	if code == 0 || !strings.Contains(errs, "publishes no capability registry") {
		t.Errorf("older control plane: exit %d\n%s", code, errs)
	}
}

// KS-013 on the client: a record in reconciliation_required is terminal
// for the command (unknown, exit 4, the id named, no resubmit), and the
// resources a record names are shown.
func TestReconciliationRequiredIsTerminalUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"operations.idempotent","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoint"):
			w.WriteHeader(202)
			fmt.Fprint(w, `{"schema_version":2,"data":{"operation_id":"op_rec","state":"accepted"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/operations/"):
			fmt.Fprint(w, `{"schema_version":2,"data":{"operation_id":"op_rec","state":"reconciliation_required","reconciliation_state":"pending","error":"the control plane restarted before this operation finished; its outcome is unknown until reconciled","accepted_at":"x","method":"POST","path":"/api/sessions/s1/checkpoint","resource_ids":["checkpoint:cp_9"]}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	stdout, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "checkpoint", "s1", "--json")
	if code != exitTemporary {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	e := parseEnvelope(t, stdout)["error"].(map[string]any)
	if e["code"] != "reconciliation_required" || e["work_started"] != "unknown" || e["operation_id"] != "op_rec" || !strings.Contains(fmt.Sprint(e["next_action"]), "do not resubmit") {
		t.Errorf("error: %v", e)
	}
	out, _, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "operation", "show", "op_rec")
	if !strings.Contains(out, "reconciliation: pending") || !strings.Contains(out, "resources: checkpoint:cp_9") {
		t.Errorf("operation show:\n%s", out)
	}
}
