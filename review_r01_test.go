// review_r01_test: the permanent guards for review finding R01 (2026-09-20).
// Replaying an operation key is only safe where the receiver deduplicates
// it. A control plane that ignores the header must get exactly one
// attempt, and a lost reply there must be reported as an unknown outcome,
// never resent and never rounded down to "no work started". Each case
// counts what the fake control plane created.
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

// legacyCtl is the old contract: no capability registry (plain 404), no
// deduplication of Idempotency-Key, and it can commit a create and then
// drop the connection before answering.
type legacyCtl struct {
	mu        sync.Mutex
	creates   int
	posts     int
	dropFirst bool
	dropped   bool
	requests  []string
}

func (c *legacyCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/sessions":
		c.mu.Lock()
		c.posts++
		c.creates++
		id := fmt.Sprintf("synthetic-session-%d", c.creates)
		drop := c.dropFirst && !c.dropped
		if drop {
			c.dropped = true
		}
		c.mu.Unlock()
		if drop {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"image":"base","state":"running"}`, id)
	default:
		w.WriteHeader(404)
		fmt.Fprint(w, "404 page not found")
	}
}

// R01 core: legacy server, create committed, response dropped. At most one
// created session, an unknown outcome, no replay, no recommendation of a
// command the same server disables.
func TestLegacyServerLostReplyCreatesAtMostOne(t *testing.T) {
	for _, mode := range []string{"human", "json"} {
		t.Run(mode, func(t *testing.T) {
			ctl := &legacyCtl{dropFirst: true}
			srv := httptest.NewServer(ctl)
			defer srv.Close()
			bin, cfg := buildAndAuth(t, srv)
			args := []string{"run", "--image", "base"}
			if mode == "json" {
				args = append(args, "--json")
			}
			stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
			if code != exitTemporary {
				t.Fatalf("exit %d, want %d\n%s%s", code, exitTemporary, stdout, stderr)
			}
			if ctl.creates != 1 || ctl.posts != 1 {
				t.Fatalf("the legacy control plane saw %d POST(s) and created %d session(s); want exactly 1 and 1", ctl.posts, ctl.creates)
			}
			all := stdout + stderr
			if strings.Contains(all, "ks operation show") {
				t.Errorf("recommended ks operation show on a control plane that disables it:\n%s", all)
			}
			if !strings.Contains(all, "sent once and not resent") {
				t.Errorf("the refusal to replay is not explained:\n%s", all)
			}
			if mode == "json" {
				env := parseEnvelope(t, stdout)
				e := env["error"].(map[string]any)
				if e["work_started"] != "unknown" {
					t.Errorf("json work_started = %v, want unknown", e["work_started"])
				}
				if strings.Contains(stdout, "retrying") {
					t.Errorf("stdout carries progress text")
				}
			} else if !strings.Contains(stderr, "Remote work started: unknown") {
				t.Errorf("human output does not say the outcome is unknown:\n%s", stderr)
			}
			led := readLedger(t, cfg)
			if len(led) != 1 {
				t.Errorf("local ledger has %d entries, want 1", len(led))
			}
		})
	}
}

// A control plane that is not answering at all cannot confirm replay
// support either: one attempt, unknown outcome, the recorded key named.
func TestUnreachableControlPlaneSendsOnce(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	bin, cfg := buildAndAuth(t, srv)
	srv.Close()
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	if code != exitTemporary || strings.Contains(errs, "retrying") || !strings.Contains(errs, "Remote work started: unknown") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if led := readLedger(t, cfg); len(led) != 1 || !strings.Contains(errs, led[0].Key) {
		t.Errorf("the recorded key is not named in the guidance")
	}
}

// Endpoint removed between observation and execution: the registry says
// available, the route answers 404. One POST, a known failure, no retry.
func TestRouteRemovedBetweenObservationAndExecution(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"operations.idempotent","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.Method == "POST" && r.URL.Path == "/api/sessions":
			posts++
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"no such route"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	if code != exitFailed || posts != 1 || strings.Contains(errs, "retrying") {
		t.Errorf("exit %d, posts %d\n%s", code, posts, errs)
	}
}

// An accepted operation whose record route vanishes (a rollback) is never
// resubmitted: the client reports the unknown outcome and names the id.
func TestAcceptedOperationSurvivesRouteRemoval(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"operations.idempotent","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoint"):
			posts++
			w.WriteHeader(202)
			fmt.Fprint(w, `{"schema_version":2,"data":{"operation_id":"op_gone","state":"accepted"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/operations/"):
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(404)
			fmt.Fprint(w, "404 page not found") // the route rolled back under the operation
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "checkpoint", "s1", "--json")
	if code != exitTemporary || posts != 1 {
		t.Fatalf("exit %d, posts %d\n%s%s", code, posts, stdout, stderr)
	}
	e := parseEnvelope(t, stdout)["error"].(map[string]any)
	if e["work_started"] != "unknown" || e["operation_id"] != "op_gone" || !strings.Contains(fmt.Sprint(e["next_action"]), "do not resubmit") {
		t.Errorf("error shape: %v", e)
	}
}

// Unknown outcome text and JSON agree: neither asserts that no work started.
func TestUnknownOutcomeNeverSaysNoWorkStarted(t *testing.T) {
	ctl := &legacyCtl{dropFirst: true}
	srv := httptest.NewServer(ctl)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, human, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run")
	ctl2 := &legacyCtl{dropFirst: true}
	srv2 := httptest.NewServer(ctl2)
	defer srv2.Close()
	bin2, cfg2 := buildAndAuth(t, srv2)
	js, _, _ := auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "run", "--json")
	var env map[string]any
	_ = json.Unmarshal([]byte(js), &env)
	e, _ := env["error"].(map[string]any)
	if strings.Contains(human, "Remote work started: no") || e["work_started"] == "no" || e["work_started"] == false {
		t.Errorf("an unknown outcome was rounded to no:\nhuman: %s\njson: %s", human, js)
	}
}

// followupCtl models the follow-up review's fixtures (2026-09-20): an
// unprotected first handler under a registry that says "available"
// (mixed_upgrade), a front door that reports 503 after the origin created
// the resource (proxy_503), a 500 after the resource was created
// (postcommit_500), and a 200 whose body is cut off (truncated_success).
type followupCtl struct {
	mu      sync.Mutex
	mode    string
	posts   int
	created int
	seen    map[string]string
}

func (c *followupCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		if r.URL.Path == "/api/capabilities" && (c.mode == "mixed_upgrade" || c.mode == "protected_records") {
			fmt.Fprint(w, `{"schema_version":2,"request_id":"r","data":{"registry_version":"test","build":"new-node","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"operations.idempotent","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"route unavailable"}`)
		return
	}
	if r.URL.Path != "/api/sessions" {
		w.WriteHeader(404)
		return
	}
	c.mu.Lock()
	c.posts++
	attempt := c.posts
	key := r.Header.Get("Idempotency-Key")
	protected := c.mode == "mixed_upgrade" && attempt > 1
	id, dup := c.seen[key]
	if !(protected && dup) {
		c.created++
		id = fmt.Sprintf("synthetic-session-%d", c.created)
		if protected {
			c.seen[key] = id
		}
	}
	c.mu.Unlock()
	switch {
	case attempt == 1 && c.mode == "mixed_upgrade":
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	case attempt == 1 && c.mode == "proxy_503":
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"upstream reply unavailable; outcome not known at proxy"}`)
	case c.mode == "postcommit_500":
		w.WriteHeader(500)
		fmt.Fprint(w, `{"message":"response assembly failed after dispatch"}`)
	case c.mode == "truncated_success":
		fmt.Fprint(w, `{"id":"synthetic-session-1","ima`)
	default:
		fmt.Fprintf(w, `{"id":%q,"image":"base","state":"running"}`, id)
	}
}

// R01-U, R01-S, R01-O and the truncated success: in every case the fixture
// creates at most one resource, the client exits non-zero, stdout is one
// document, and the certainty is "unknown", never "no".
func TestFollowupReviewFixtures(t *testing.T) {
	for _, mode := range []string{"mixed_upgrade", "proxy_503", "postcommit_500", "truncated_success"} {
		t.Run(mode, func(t *testing.T) {
			ctl := &followupCtl{mode: mode, seen: map[string]string{}}
			srv := httptest.NewServer(ctl)
			defer srv.Close()
			bin, cfg := buildAndAuth(t, srv)
			stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--image", "base", "--json")
			if ctl.created > 1 {
				t.Fatalf("%s: one intended action created %d sessions", mode, ctl.created)
			}
			if code == 0 {
				t.Fatalf("%s: exit 0 with no verified result for the ambiguous submission\n%s%s", mode, stdout, stderr)
			}
			env := parseEnvelope(t, stdout)
			e, _ := env["error"].(map[string]any)
			if e == nil || e["work_started"] != "unknown" {
				t.Errorf("%s: error %v, want work_started unknown", mode, e)
			}
			if e != nil && e["operation_id"] == nil {
				t.Errorf("%s: the operation key is not preserved", mode)
			}
			if ctl.posts != 1 {
				t.Errorf("%s: %d POST(s); the submission must be sent once", mode, ctl.posts)
			}
		})
	}
}

// A certified KS pre-admission refusal (a typed ks_ error on 429) is a
// definite "no work started"; a bare 429 is not.
func TestTypedRefusalIsDefiniteBareIsNot(t *testing.T) {
	for _, c := range []struct {
		body string
		want string
		exit int
	}{
		{`{"error":{"type":"ks_tier_fence","message":"one session at a time"}}`, "no", exitConflict},
		{`{"message":"too many requests"}`, "unknown", exitTemporary},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == "POST" && r.URL.Path == "/api/sessions" {
				w.WriteHeader(429)
				fmt.Fprint(w, c.body)
				return
			}
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"route unavailable"}`)
		}))
		bin, cfg := buildAndAuth(t, srv)
		stdout, _, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "run", "--json")
		srv.Close()
		e, _ := parseEnvelope(t, stdout)["error"].(map[string]any)
		if code != c.exit || e == nil || e["work_started"] != c.want {
			t.Errorf("429 %s: exit %d (want %d), work_started %v (want %s)", c.body, code, c.exit, e["work_started"], c.want)
		}
	}
}
