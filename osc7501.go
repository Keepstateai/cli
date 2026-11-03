// osc7501.go: terminal status (OSC 7501, the Program Status Protocol).
//
// The service derives one status record per agent from its own state (a
// session paused for credit, a permission request waiting, an instruction
// running) and serves it on the event stream as opt-in status frames and on
// the agent status routes. This client writes that record to the terminal
// as OSC 7501, so a terminal or multiplexer that reads the protocol shows
// whether the agent works, needs you, is done or failed:
//
//	ESC ] 7501 ; state=<s>[:id=<id>][:kind=<k>]:app=keepstate[:title=<b64>][:msg=<b64>] ESC \
//
// Fields follow the reference encoder's order (state, id, kind, app, title,
// msg); the terminator is always ST, never BEL; title and msg are standard
// base64 of UTF-8 with padding.
//
// THE SERVICE NEVER SENDS ESCAPE BYTES. Every sequence is built here from
// fields that are checked one by one: the state and kind come from fixed
// sets, an id from its grammar, and the words are cleaned of every control
// character and clipped before they are encoded. So neither the service nor
// an agent's words can put an escape sequence on a terminal.
//
// When: KS_STATUS_OSC=auto (the default) writes only when stdout is a
// terminal, the output is not --json or --plain, and TERM is not dumb; on
// writes even when stdout is not a terminal (never into --json); off never
// writes. One report per change, never on a timer, and leaving a stream
// sends state=clear, so a closed window never keeps a stale "working".
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// statusRecord is the service's status record for one agent.
type statusRecord struct {
	AgentID     string        `json:"agent_id"`
	SessionID   string        `json:"session_id"`
	Name        string        `json:"name"`
	SessionName string        `json:"session_name"`
	State       string        `json:"state"`
	Kind        string        `json:"kind"`
	Reason      string        `json:"reason"`
	Msg         string        `json:"msg"`
	Action      *statusAction `json:"action"`
	Since       string        `json:"since"`
	NeedsYou    bool          `json:"needs_you"`
	Source      string        `json:"source"`
	Parent      *statusParent `json:"parent"`
}

type statusAction struct {
	Label   string `json:"label"`
	Verb    string `json:"verb"`
	Console string `json:"console"`
}

type statusParent struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// oscStatus is one report as written to the terminal.
type oscStatus struct {
	State, ID, Kind, App, Title, Msg string
}

const (
	oscMaxSequence = 4096
	oscMaxMsg      = 512 // this client's limit; the protocol allows 2048
	oscMaxTitle    = 96  // this client's limit; the protocol allows 192
	oscMaxIDBytes  = 128
	oscMaxIDLevels = 8
	statusApp      = "keepstate"
)

var (
	oscStates   = map[string]bool{"idle": true, "working": true, "done": true, "blocked": true, "error": true, "clear": true}
	oscKinds    = map[string]bool{"permission": true, "question": true, "auth": true}
	oscSegment  = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)
	oscIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.+-]`)
)

// oscID reports whether id is a record path the protocol accepts.
func oscID(id string) bool {
	if id == "" || len(id) > oscMaxIDBytes {
		return false
	}
	parts := strings.Split(id, "/")
	if len(parts) > oscMaxIDLevels {
		return false
	}
	for _, p := range parts {
		if !oscSegment.MatchString(p) {
			return false
		}
	}
	return true
}

// statusID turns an agent id into one record id segment: characters the
// grammar does not allow become _, and it is clipped to 32 bytes.
func statusID(s string) string {
	s = oscIDUnsafe.ReplaceAllString(s, "_")
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		s = "agent"
	}
	return s
}

// oscText cleans words for a report: newlines become " · ", every other
// control character (C0, DEL, C1) a space, invalid UTF-8 the replacement
// character, and the result is clipped to max bytes at a character
// boundary.
func oscText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(" · ")
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	t := strings.TrimSpace(b.String())
	if len(t) <= max {
		return t
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	return strings.TrimSpace(t[:cut])
}

// encodeOSC7501 builds the sequence for one report, or answers false when
// a field is not one the protocol accepts (nothing is then written).
func encodeOSC7501(r oscStatus) (string, bool) {
	if !oscStates[r.State] {
		return "", false
	}
	fields := []string{"state=" + r.State}
	if r.ID != "" {
		if !oscID(r.ID) {
			return "", false
		}
		fields = append(fields, "id="+r.ID)
	}
	if r.State != "clear" {
		if r.Kind != "" && r.State == "blocked" && oscKinds[r.Kind] {
			fields = append(fields, "kind="+r.Kind)
		}
		if r.App != "" {
			if !oscSegment.MatchString(r.App) {
				return "", false
			}
			fields = append(fields, "app="+r.App)
		}
		if t := oscText(r.Title, oscMaxTitle); t != "" {
			fields = append(fields, "title="+base64.StdEncoding.EncodeToString([]byte(t)))
		}
		if m := oscText(r.Msg, oscMaxMsg); m != "" {
			fields = append(fields, "msg="+base64.StdEncoding.EncodeToString([]byte(m)))
		}
	}
	seq := "\x1b]7501;" + strings.Join(fields, ":") + "\x1b\\"
	if len(seq) > oscMaxSequence {
		return "", false
	}
	return seq, true
}

// statusTitle is the title of an agent's report: its name and session.
func statusTitle(name, session string) string {
	if session == "" {
		return name
	}
	return name + " · " + session
}

// recordStatus is the report for a service record, or false when the
// record says nothing a report can carry (no state, or a state this client
// does not know): an unknown state is never guessed.
func recordStatus(rec statusRecord, id, title string) (oscStatus, bool) {
	if !oscStates[rec.State] {
		return oscStatus{}, false
	}
	kind := ""
	if rec.State == "blocked" && oscKinds[rec.Kind] {
		kind = rec.Kind
	}
	return oscStatus{State: rec.State, ID: id, Kind: kind, App: statusApp, Title: title, Msg: rec.Msg}, true
}

// statusOSCMode answers whether this invocation writes terminal status,
// and why, in the words ks doctor prints.
func statusOSCMode() (bool, string) { return statusOSCModeOf(out.json, out.plain) }

// statusOSCModeOf is the rule for an output that is (or is not) --json or
// --plain; ks doctor asks it for a stream's ordinary output.
func statusOSCModeOf(jsonOut, plainOut bool) (bool, string) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KS_STATUS_OSC")))
	switch v {
	case "off", "0", "false", "no":
		return false, "off (KS_STATUS_OSC=off)"
	}
	if jsonOut {
		return false, "off (--json output)"
	}
	if plainOut {
		return false, "off (--plain output)"
	}
	switch v {
	case "on", "1", "true", "yes":
		return true, "on (KS_STATUS_OSC=on)"
	}
	if !stdoutIsTerminal() {
		return false, "off (stdout is not a terminal)"
	}
	if os.Getenv("TERM") == "dumb" {
		return false, "off (TERM is dumb)"
	}
	return true, "on (stdout is a terminal)"
}

// statusEmitter writes one stream's reports: once per change for each
// record id, and a clear when the stream is left. It also hands the root
// record to herdr when the stream runs in a herdr pane. A nil emitter, or
// one with nothing enabled, writes nothing.
type statusEmitter struct {
	mu    sync.Mutex
	write func(string)
	osc   bool
	herdr *herdrReporter
	last  map[string]string
	sent  bool
	done  bool
}

// newStatusEmitter is the emitter for a stream that writes with write (nil
// means stdout). herdrName and resume, when set, are the herdr pane's agent
// label and the command that reopens the stream.
func newStatusEmitter(write func(string), herdrName string, resume []string) *statusEmitter {
	on, _ := statusOSCMode()
	if write == nil {
		write = func(s string) { _, _ = os.Stdout.WriteString(s) }
	}
	e := &statusEmitter{write: write, osc: on, last: map[string]string{}}
	if herdrName != "" && !out.json {
		e.herdr = newHerdrReporter(herdrName, resume)
	}
	onExit(e.leave)
	return e
}

// active: something reads this stream's status, so it is worth asking the
// service for status frames.
func (e *statusEmitter) active() bool { return e != nil && (e.osc || e.herdr != nil) }

// report writes one record's report when it differs from the last one
// written for that id. The root record (id "") also goes to herdr.
func (e *statusEmitter) report(r oscStatus) {
	if e == nil {
		return
	}
	seq, ok := encodeOSC7501(r)
	if !ok {
		return
	}
	e.mu.Lock()
	if e.done || e.last[r.ID] == seq {
		e.mu.Unlock()
		return
	}
	e.last[r.ID] = seq
	e.sent = true
	osc := e.osc
	e.mu.Unlock()
	if osc {
		e.write(seq)
	}
	if r.ID == "" && e.herdr != nil {
		e.herdr.report(r.State, r.Kind, oscText(r.Msg, oscMaxMsg))
	}
}

// forget drops one record: a clear for its id, written once.
func (e *statusEmitter) forget(id string) {
	if e == nil || id == "" {
		return
	}
	e.mu.Lock()
	_, had := e.last[id]
	delete(e.last, id)
	osc := e.osc && had && !e.done
	e.mu.Unlock()
	if osc {
		if seq, ok := encodeOSC7501(oscStatus{State: "clear", ID: id}); ok {
			e.write(seq)
		}
	}
}

// leave ends the stream's status: one clear for every record it wrote, and
// the herdr pane released. Safe to call more than once.
func (e *statusEmitter) leave() {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.done {
		e.mu.Unlock()
		return
	}
	e.done = true
	sent, osc := e.sent, e.osc
	e.mu.Unlock()
	if osc && sent {
		e.write("\x1b]7501;state=clear\x1b\\")
	}
	if e.herdr != nil {
		e.herdr.release()
	}
}

// fetchAgentStatus reads one agent's status record
// (GET /api/v2/agents/{id}/status, in the v2 data envelope).
func fetchAgentStatus(cr hostedCreds, agentID string) (statusRecord, error) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	var rec statusRecord
	if err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(agentID)+"/status", nil, &env); err != nil {
		return rec, err
	}
	err := json.Unmarshal(env.Data, &rec)
	return rec, err
}
