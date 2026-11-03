// Terminal status (OSC 7501): the encoder against the protocol's reference
// encoder and parser vectors, a reference decoder that enforces every limit
// of the specification, the emission policy, and the streams that write it.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// decoded is one report as a reader of the protocol takes it.
type decoded struct {
	State, Kind, ID, App, Title, Msg string
}

var (
	refKey   = regexp.MustCompile(`^[a-z]{1,16}$`)
	refValue = regexp.MustCompile(`^[A-Za-z0-9_.,+/=-]*$`)
	refSeg   = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)
)

func refText(v string, max int) (string, error) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(v); err != nil {
			return "", fmt.Errorf("not base64: %v", err)
		}
	}
	if len(b) > max {
		return "", fmt.Errorf("decoded %d bytes, limit %d", len(b), max)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("not UTF-8")
	}
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", fmt.Errorf("control character %U after decoding", r)
		}
	}
	return string(b), nil
}

// refDecode reads every OSC 7501 sequence in b by the specification:
// ESC ] 7501 ; pairs ST (ESC \ or BEL), at most 4096 bytes, keys [a-z]{1,16},
// values from the protocol's alphabet, the id grammar (segments of
// [A-Za-z0-9_.+-]{1,32}, at most 8 levels and 128 bytes), msg at most 2048
// and title at most 192 decoded bytes, with no control character after
// decoding. A sequence that breaks any rule is an error, not a report.
func refDecode(t *testing.T, b string) []decoded {
	t.Helper()
	var out []decoded
	for {
		i := strings.Index(b, "\x1b]7501;")
		if i < 0 {
			return out
		}
		b = b[i:]
		end, termLen := strings.Index(b, "\x1b\\"), 2
		if bel := strings.IndexByte(b, '\x07'); bel >= 0 && (end < 0 || bel < end) {
			end, termLen = bel, 1
		}
		if end < 0 {
			t.Fatalf("unterminated sequence %q", b)
		}
		seq := b[:end+termLen]
		b = b[end+termLen:]
		if len(seq) > 4096 {
			t.Fatalf("sequence of %d bytes", len(seq))
		}
		var d decoded
		for _, pair := range strings.Split(seq[len("\x1b]7501;"):end], ":") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok || !refKey.MatchString(k) || !refValue.MatchString(v) {
				t.Fatalf("pair %q breaks the grammar in %q", pair, seq)
			}
			var err error
			switch k {
			case "state":
				d.State = v
			case "kind":
				d.Kind = v
			case "app":
				if !refSeg.MatchString(v) {
					t.Fatalf("app %q", v)
				}
				d.App = v
			case "id":
				parts := strings.Split(v, "/")
				if len(v) > 128 || len(parts) > 8 {
					t.Fatalf("id %q too long", v)
				}
				for _, p := range parts {
					if !refSeg.MatchString(p) {
						t.Fatalf("id segment %q", p)
					}
				}
				d.ID = v
			case "title":
				d.Title, err = refText(v, 192)
			case "msg":
				d.Msg, err = refText(v, 2048)
			}
			if err != nil {
				t.Fatalf("%s in %q: %v", k, seq, err)
			}
		}
		switch d.State {
		case "idle", "working", "done", "blocked", "error", "clear":
		default:
			t.Fatalf("state %q in %q", d.State, seq)
		}
		if d.Kind != "" && (d.State != "blocked" || (d.Kind != "permission" && d.Kind != "question" && d.Kind != "auth")) {
			t.Fatalf("kind %q with state %q", d.Kind, d.State)
		}
		out = append(out, d)
	}
}

func mustEncode(t *testing.T, r oscStatus) string {
	t.Helper()
	s, ok := encodeOSC7501(r)
	if !ok {
		t.Fatalf("refused %+v", r)
	}
	return s
}

func TestOSC7501EncoderMatchesTheReferenceVectors(t *testing.T) {
	// the reference encoder's order (state, id, kind, app, title, msg),
	// terminated with ST, for the report its own round-trip test sends
	got := mustEncode(t, oscStatus{State: "blocked", Kind: "permission", ID: "a/b", App: "claude-code", Title: "Run tests", Msg: "Apply these changes?"})
	want := "\x1b]7501;state=blocked:id=a/b:kind=permission:app=claude-code:title=UnVuIHRlc3Rz:msg=QXBwbHkgdGhlc2UgY2hhbmdlcz8=\x1b\\"
	if got != want {
		t.Errorf("encoded\n%q\nwant\n%q", got, want)
	}
	if got := mustEncode(t, oscStatus{State: "clear"}); got != "\x1b]7501;state=clear\x1b\\" {
		t.Errorf("clear: %q", got)
	}
	// the parser's own test strings decode to what this encoder writes for
	// the same report (the parser reads pairs in any order, BEL or ST)
	for _, tc := range []struct {
		parser string
		report oscStatus
	}{
		{"\x1b]7501;state=blocked:kind=permission:id=a/b:msg=QXBwbHk/\x07", oscStatus{State: "blocked", Kind: "permission", ID: "a/b", Msg: "Apply?"}},
		{"\x1b]7501;state=clear\x1b\\", oscStatus{State: "clear"}},
	} {
		a, b := refDecode(t, tc.parser), refDecode(t, mustEncode(t, tc.report))
		if len(a) != 1 || len(b) != 1 || a[0] != b[0] {
			t.Errorf("parser vector %q reads %+v; the encoder's %+v", tc.parser, a, b)
		}
	}
	// the sequences this client documents, byte for byte
	if got := mustEncode(t, oscStatus{State: "blocked", Kind: "permission", App: statusApp, Title: statusTitle("checkout", "3f2a1c"), Msg: "Run: npm test"}); got !=
		"\x1b]7501;state=blocked:kind=permission:app=keepstate:title=Y2hlY2tvdXQgwrcgM2YyYTFj:msg=UnVuOiBucG0gdGVzdA==\x1b\\" {
		t.Errorf("permission example: %q", got)
	}
	if got := mustEncode(t, oscStatus{State: "blocked", App: statusApp, Msg: "Paused: out of credit. Add credit, then resume."}); got !=
		"\x1b]7501;state=blocked:app=keepstate:msg=UGF1c2VkOiBvdXQgb2YgY3JlZGl0LiBBZGQgY3JlZGl0LCB0aGVuIHJlc3VtZS4=\x1b\\" {
		t.Errorf("funds example: %q", got)
	}
	// a kind outside blocked, an unknown state, a bad id or app: refused or dropped
	if s := mustEncode(t, oscStatus{State: "working", Kind: "permission"}); strings.Contains(s, "kind=") {
		t.Errorf("a kind went out with working: %q", s)
	}
	for _, bad := range []oscStatus{{State: "bogus"}, {State: ""}, {State: "working", ID: "a b"}, {State: "working", ID: strings.Repeat("a/", 8) + "a"},
		{State: "working", ID: strings.Repeat("x", 33)}, {State: "working", App: "key\x1b"}} {
		if s, ok := encodeOSC7501(bad); ok {
			t.Errorf("%+v encoded as %q", bad, s)
		}
	}
}

func TestOSC7501WordsAreCleanedAndClipped(t *testing.T) {
	long := "line one\nline two \x1b]0;evil\x07 \x9b2J tab\there " + strings.Repeat("é", 1500) // over 3 KB
	if len(long) < 3000 {
		t.Fatalf("fixture is %d bytes", len(long))
	}
	s := mustEncode(t, oscStatus{State: "working", App: statusApp, Title: strings.Repeat("t\x07", 200), Msg: long})
	d := refDecode(t, s)
	if len(d) != 1 {
		t.Fatalf("decoded %v", d)
	}
	if len(d[0].Msg) > oscMaxMsg || len(d[0].Title) > oscMaxTitle {
		t.Errorf("msg %d bytes, title %d bytes", len(d[0].Msg), len(d[0].Title))
	}
	if !strings.HasPrefix(d[0].Msg, "line one · line two  ]0;evil") {
		t.Errorf("newline and escapes: %q", d[0].Msg)
	}
	if strings.Count(s, "\x1b") != 2 { // the introducer and the terminator, nothing else
		t.Errorf("escape bytes inside the sequence: %q", s)
	}
	if !utf8.ValidString(d[0].Msg) {
		t.Error("clipped inside a character")
	}
	// every record id this client derives is one the grammar accepts
	for _, raw := range []string{"agt_main0001", "agt/with/slashes", "a b:c", strings.Repeat("z", 80), "", "é"} {
		if id := statusID(raw); !oscID(id) {
			t.Errorf("statusID(%q) = %q, not a record id", raw, id)
		}
	}
}

func TestStatusEmissionPolicy(t *testing.T) {
	out = outputSettings{}
	defer func() { out = outputSettings{} }()
	for _, tc := range []struct {
		env, term   string
		json, plain bool
		on          bool
		why         string
	}{
		{"", "xterm", false, false, false, "off (stdout is not a terminal)"}, // go test's stdout is not a terminal
		{"auto", "xterm", false, false, false, "off (stdout is not a terminal)"},
		{"on", "dumb", false, false, true, "on (KS_STATUS_OSC=on)"},
		{"on", "xterm", true, false, false, "off (--json output)"},
		{"on", "xterm", false, true, false, "off (--plain output)"},
		{"off", "xterm", false, false, false, "off (KS_STATUS_OSC=off)"},
	} {
		t.Setenv("KS_STATUS_OSC", tc.env)
		t.Setenv("TERM", tc.term)
		on, why := statusOSCModeOf(tc.json, tc.plain)
		if on != tc.on || why != tc.why {
			t.Errorf("%+v: %v %q", tc, on, why)
		}
	}
	// an emitter that is off writes nothing, and asks for no status frames
	t.Setenv("KS_STATUS_OSC", "off")
	var wrote []string
	e := newStatusEmitter(func(s string) { wrote = append(wrote, s) }, "", nil)
	e.report(oscStatus{State: "working", App: statusApp})
	e.leave()
	if len(wrote) != 0 || e.active() {
		t.Errorf("off wrote %q (active %v)", wrote, e.active())
	}
	// on: one report per change, per id, and one clear on leaving
	t.Setenv("KS_STATUS_OSC", "on")
	wrote = nil
	e = newStatusEmitter(func(s string) { wrote = append(wrote, s) }, "", nil)
	for _, r := range []oscStatus{{State: "working"}, {State: "working"}, {State: "blocked", Kind: "permission", Msg: "x"}, {State: "blocked", Kind: "permission", Msg: "x"},
		{State: "working", ID: "a"}, {State: "working", ID: "a"}, {State: "done"}} {
		r.App = statusApp
		e.report(r)
	}
	e.leave()
	e.leave()
	e.report(oscStatus{State: "working", App: statusApp})
	if len(wrote) != 5 || wrote[4] != "\x1b]7501;state=clear\x1b\\" {
		t.Errorf("wrote %d: %q", len(wrote), wrote)
	}
}

// statusCtl serves the agent fake's routes, plus the status frames on the
// event stream (only when asked with status=1) and the agent status route.
type statusCtl struct {
	*agentCtl
	frames []string // the stream's frames, as "kind data"
	record string   // the status route's record; empty answers 404
	gap    time.Duration
	mu     sync.Mutex
	asked  []string
}

func (s *statusCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked = append(s.asked, r.Method+" "+r.URL.RequestURI())
	s.mu.Unlock()
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/events/stream"):
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		withStatus := r.URL.Query().Get("status") == "1"
		for _, f := range s.frames {
			kind, data, _ := strings.Cut(f, " ")
			if kind == "status" && !withStatus {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
			fl.Flush()
			time.Sleep(s.gap)
		}
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/agents/") && strings.HasSuffix(r.URL.Path, "/status"):
		if s.record == "" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"schema_version":2,"error":{"type":"not_found","message":"no such route"}}`)
			return
		}
		fmt.Fprintf(w, `{"schema_version":2,"data":%s}`, s.record)
	default:
		s.agentCtl.ServeHTTP(w, r)
	}
}

func (s *statusCtl) sawAsked(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.asked {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func statusFixture(t *testing.T, frames []string, record string) (*statusCtl, string, string) {
	t.Helper()
	c := &statusCtl{agentCtl: newAgentCtl(), frames: frames, record: record}
	syncFixture(t, &c.agentCtl.mu)
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return c, bin, cfg
}

func statusFrame(state, kind, msg string) string {
	b, _ := json.Marshal(map[string]any{"agent_id": "agt_main0001", "session_id": agentSessionRecord, "name": "main",
		"state": state, "kind": kind, "reason": "r", "msg": msg, "needs_you": state == "blocked" || state == "error", "source": "keepstate"})
	return "status " + string(b)
}

var openFrames = []string{
	`hello {"epoch":7}`,
	statusFrame("working", "", "Working."),
	`event {"event_id":"ev_1","stream_seq":42,"subject_type":"task","subject_id":"tsk_1","observed_at":"2026-09-20T12:00:03Z","payload":{"state":"running"}}`,
	statusFrame("working", "", "Working."),
	statusFrame("blocked", "permission", "Run: npm test\x1b[2J"),
	`heartbeat {}`,
	statusFrame("blocked", "permission", "Run: npm test\x1b[2J"),
	statusFrame("done", "", "Finished instruction tsk_1."),
	`end {}`,
}

func TestAgentOpenWritesOneStatusPerChangeAndClearsOnLeaving(t *testing.T) {
	c, bin, cfg := statusFixture(t, openFrames, "")
	stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), append(fastEnv(cfg), "KS_STATUS_OSC=on"), "agent", "open", "main", "--session", agentSessionShort)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !c.sawAsked("GET /api/v2/sessions/" + agentSessionRecord + "/events/stream?after_seq=41&status=1") {
		t.Errorf("the stream was not asked for status frames: %v", c.asked)
	}
	got := refDecode(t, stdout)
	title := "main · " + agentSessionShort
	want := []decoded{
		{State: "working", App: "keepstate", Title: title, Msg: "Working."},
		{State: "blocked", Kind: "permission", App: "keepstate", Title: title, Msg: "Run: npm test [2J"},
		{State: "done", App: "keepstate", Title: title, Msg: "Finished instruction tsk_1."},
		{State: "clear"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reports\n%+v\nwant\n%+v\nstdout %q", got, want, stdout)
	}
	if strings.Count(stdout, "\x1b") != 2*len(want) {
		t.Errorf("escape bytes outside the reports: %q", stdout)
	}
}

func TestNoStatusIsWrittenOrAskedForWhenItIsOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{"not a terminal", nil, nil},
		{"json", []string{"KS_STATUS_OSC=on"}, []string{"--json"}},
		{"off", []string{"KS_STATUS_OSC=off"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, bin, cfg := statusFixture(t, openFrames, "")
			args := append([]string{"agent", "open", "main", "--session", agentSessionShort}, tc.args...)
			stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), append(fastEnv(cfg), tc.env...), args...)
			if code != 0 {
				t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
			}
			if strings.Contains(stdout+stderr, "]7501") {
				t.Errorf("status written: %q", stdout)
			}
			if c.sawAsked("GET /api/v2/sessions/"+agentSessionRecord+"/events/stream?after_seq=41&status=1") || !c.sawAsked("GET /api/v2/sessions/"+agentSessionRecord+"/events/stream?after_seq=41") {
				t.Errorf("stream requests: %v", c.asked)
			}
		})
	}
}

func watchStatus(t *testing.T, bin, cfg string, env []string, until func(string) bool) string {
	t.Helper()
	cmd := exec.Command(bin, "agent", "status", "main", "--session", agentSessionShort, "--watch")
	cmd.Env = append(os.Environ(), env...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the watch to show the agent", func() bool { return until(so.String()) })
	_ = cmd.Process.Signal(os.Interrupt)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("watch exited %v\n%s", err, se.String())
	}
	return so.String()
}

func TestAgentStatusWatchReportsTheServedRecord(t *testing.T) {
	_, rec, _ := strings.Cut(statusFrame("blocked", "auth", "The key this session calls is disabled."), " ")
	c, bin, cfg := statusFixture(t, nil, rec)
	all := watchStatus(t, bin, cfg, append(fastEnv(cfg), "KS_STATUS_OSC=on"), func(s string) bool { return strings.Contains(s, "\x1b\\") })
	got := refDecode(t, all)
	if len(got) != 2 || got[0].State != "blocked" || got[0].Kind != "auth" || got[0].Msg != "The key this session calls is disabled." || got[1].State != "clear" {
		t.Errorf("reports %+v\n%q", got, all)
	}
	if !c.sawAsked("GET /api/v2/agents/agt_main0001/status") {
		t.Errorf("status route not read: %v", c.asked)
	}
}

func TestAnOlderControlPlaneWithoutTheStatusRouteWritesNothing(t *testing.T) {
	_, bin, cfg := statusFixture(t, nil, "")
	all := watchStatus(t, bin, cfg, append(fastEnv(cfg), "KS_STATUS_OSC=on"), func(s string) bool { return strings.Count(s, "observed ") >= 2 })
	if strings.Contains(all, "]7501") {
		t.Errorf("a status was guessed: %q", all)
	}
}

// The attach relay copies the session's bytes unchanged, so a harness's own
// status inside the session reaches the terminal exactly as it wrote it.
func TestAttachPassesTerminalStatusThroughUnchanged(t *testing.T) {
	payload := "hello \x1b]7501;state=working\x1b\\ there \x1b]7501;state=blocked:kind=question:msg=QXBwbHk/\x07 end"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: ks-attach\r\nConnection: Upgrade\r\n\r\n"+payload)
	}()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	tok, _ := json.Marshal(map[string]string{"token": "guard-test-token", "ctl": "http://" + ln.Addr().String()})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "token.json"), tok, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, _ := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "attach", "sess_1")
	if stdout != payload {
		t.Errorf("relayed\n%q\nwant\n%q\nstderr %s", stdout, payload, stderr)
	}
}

func TestDoctorSaysWhetherTerminalStatusIsWritten(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := append(fastEnv(cfg), "KS_UPDATE_BASE="+srv.URL)
	out1, _, _ := auditExec(t, bin, cfg, t.TempDir(), env, "doctor")
	if !strings.Contains(out1, "Terminal status (OSC 7501): off (stdout is not a terminal)") {
		t.Errorf("doctor:\n%s", out1)
	}
	out2, _, _ := auditExec(t, bin, cfg, t.TempDir(), append(env, "KS_STATUS_OSC=off"), "doctor")
	if !strings.Contains(out2, "Terminal status (OSC 7501): off (KS_STATUS_OSC=off)") {
		t.Errorf("doctor off:\n%s", out2)
	}
}
