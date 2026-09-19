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
	"net"
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
			c.created[key] = id
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
// retry carries the same key and returns the original session.
func TestLostReplyRetriesTheSameKey(t *testing.T) {
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
	if !strings.Contains(errs, "retrying the same operation") || !strings.Contains(errs, "already had this operation") {
		t.Errorf("the retry and the replay were not reported:\n%s", errs)
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

// QA-004-2: nothing answers. Three bounded attempts, then a recovery
// command that names the key, exit 4, and no endless spinner.
func TestUnreachableControlPlaneEndsWithARecoveryCommand(t *testing.T) {
	// a listener that is closed immediately: the port refuses, fast
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + l.Addr().String()
	l.Close()
	srv := httptest.NewServer(http.NotFoundHandler())
	bin, cfg := buildAndAuth(t, srv)
	srv.Close()
	tok, _ := json.Marshal(map[string]string{"token": "t", "ctl": addr})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "token.json"), tok, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	took := time.Since(start)
	if code != 4 {
		t.Fatalf("exit %d, want 4\n%s%s", code, out, errs)
	}
	led := readLedger(t, cfg)
	if len(led) != 1 {
		t.Fatalf("ledger has %d entries, want the one operation", len(led))
	}
	if !strings.Contains(errs, "Next: ks operation show "+led[0].Key) || !strings.Contains(errs, "No remote work was started.") {
		t.Errorf("no recovery command with the key:\n%s", errs)
	}
	if strings.Count(errs, "retrying the same operation") != 2 {
		t.Errorf("want exactly 2 retries (3 attempts):\n%s", errs)
	}
	if took > 10*time.Second {
		t.Errorf("took %s; the attempts are not bounded", took)
	}
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
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "operation", "show", "ksop_whatever")
	if code == 0 || !strings.Contains(errs, "does not serve operation records") {
		t.Errorf("older control plane: exit %d\n%s", code, errs)
	}
}
