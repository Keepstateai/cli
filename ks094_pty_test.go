//go:build unix

// KS-094 client half: a PTY harness driven by the F11 terminal matrix
// (testdata/c12/f11-terminal-matrix.json, vendored from the service's C12
// fixtures). This client has no dependency that makes a pseudo-terminal, so
// the harness runs the binary under script(1), which does, with the
// terminal size set by stty inside it -- a real TTY, not a pipe.
//
// Automated here, per matrix cell (size x text x mode) on the live window
// (ks agent view):
//   - no escape or bell byte reaches the terminal from content: the
//     escape-injection sample is inert (no title change, no clear, no colour)
//   - every line fits the terminal's width by the columns it occupies
//     (wide characters count two), so nothing wraps into a control line
//   - below the 80x24 minimum, a stated minimum-size notice
//   - Turkish text survives byte-exact
//   - no-colour and --plain produce the same bytes (this client prints no
//     colour at all)
//
// And, without a PTY, the interaction the matrix names first: a multi-line
// paste is one instruction (QA-094-1), and a pasted window command is text.
//
// NOT automated (manual review, VER-094-1/2): screenshots or recordings of
// every screen in every size, and the screen-reader walkthrough.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

type f11Matrix struct {
	Sizes []struct {
		Cols, Rows   int
		BelowMinimum bool `json:"below_minimum"`
	} `json:"sizes"`
	Text []struct {
		Name, Sample string
	} `json:"text"`
	Modes        []string `json:"modes"`
	Interactions []string `json:"interactions"`
	Screens      []string `json:"screens"`
}

func loadF11(t *testing.T) f11Matrix {
	t.Helper()
	b, err := os.ReadFile("testdata/c12/f11-terminal-matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	var m f11Matrix
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Sizes) == 0 || len(m.Text) == 0 {
		t.Fatal("the F11 matrix is empty")
	}
	return m
}

// inPTY runs bin under script(1) at a terminal size, returning what the
// terminal received.
func inPTY(t *testing.T, env []string, cols, rows int, bin string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is not installed: no PTY without a dependency")
	}
	inner := append([]string{"sh", "-c", fmt.Sprintf(`stty cols %d rows %d 2>/dev/null; exec "$0" "$@"`, cols, rows), bin}, args...)
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" || strings.HasSuffix(runtime.GOOS, "bsd") {
		cmd = exec.Command("script", append([]string{"-q", "/dev/null"}, inner...)...)
	} else {
		quoted := make([]string, len(inner))
		for i, a := range inner {
			quoted[i] = shellQuote(a)
		}
		cmd = exec.Command("script", "-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null")
	}
	cmd.Env = append(os.Environ(), env...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func f11ViewServer(sample string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := func(data any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": data})
		}
		switch {
		case r.URL.Path == "/api/capabilities":
			fmt.Fprint(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[{"id":"agent.workspace","availability":"available","summary":"s","surface":"api"}],"limits":{}}}`)
		case r.URL.Path == "/api/v2/sessions":
			env(map[string]any{"items": []any{map[string]any{"id": "fleetvw00000000000000000000000001", "short_id": "fleetvw00000", "name": "checkout", "runtime_state": "running", "record_id": "session_1"}}, "next_cursor": ""})
		case r.URL.Path == "/api/v2/agents":
			env(map[string]any{"items": []any{map[string]any{"id": "agent_1", "session_id": "session_1", "name": "main", "is_primary": true}}})
		case r.URL.Path == "/api/v2/agents/agent_1/view":
			env(map[string]any{
				"header": map[string]any{"session_id": "session_1", "session_name": "checkout", "agent_id": "agent_1", "agent_name": "main",
					"controller": "view_only", "runtime_state": "running", "activity": "working", "observed_at": "12:00:01", "key_routes": []string{"anthropic"}},
				"runner":       map[string]any{"label": sample, "certification": "not certified", "reported": true},
				"footer":       map[string]any{"queued": 1, "held": 0, "pending_approvals": 0, "advisers": 0},
				"status":       map[string]any{"label": sample, "observed_at": "x", "age_seconds": 3, "stale": false, "stale_after_seconds": 15},
				"current_task": map[string]any{"id": "tsk_1", "label": sample},
				"palette": []any{
					map[string]any{"label": "Adviser says", "effect": sample, "requests": []any{}, "local": true},
					map[string]any{"label": "Pending work", "effect": "queued and held instructions in order", "requests": []any{}, "local": true},
				},
				"next_action": map[string]any{"action": "x", "label": sample, "request": "GET /api/v2/x"}})
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"schema_version":2,"error":{"type":"ks_not_found","message":"no route"}}`)
		}
	}))
}

func TestKS094TerminalMatrixOnTheLiveWindow(t *testing.T) {
	m := loadF11(t)
	modes := map[string][]string{"color": nil, "no-color": {"NO_COLOR=1"}, "plain": nil}
	for _, tx := range m.Text {
		srv := f11ViewServer(tx.Sample)
		bin, cfg := buildAndAuth(t, srv)
		for _, sz := range m.Sizes {
			var outputs []string
			for _, mode := range []string{"color", "no-color", "plain"} {
				args := []string{"agent", "view", "main", "--session", "fleetvw"}
				if mode == "plain" {
					args = append(args, "--plain")
				}
				out := inPTY(t, append(fastEnv(cfg), modes[mode]...), sz.Cols, sz.Rows, bin, args...)
				cell := fmt.Sprintf("%dx%d/%s/%s", sz.Cols, sz.Rows, tx.Name, mode)
				if !strings.Contains(out, "checkout/main") {
					t.Errorf("%s: the view did not render:\n%q", cell, out)
					continue
				}
				if strings.ContainsAny(out, "\x1b\x07") {
					t.Errorf("%s: an escape or bell reached the terminal:\n%q", cell, out)
				}
				if tx.Name == "escape_injection" && (strings.Contains(out, "\x1b]0;") || strings.Contains(out, "\x1b[2J")) {
					t.Errorf("%s: the injected sequence is live", cell)
				}
				if tx.Name == "turkish" && !strings.Contains(out, "İstanbul'daki şube") {
					t.Errorf("%s: the Turkish text did not survive:\n%s", cell, out)
				}
				if sz.BelowMinimum || sz.Cols < 80 || sz.Rows < 24 {
					if !strings.Contains(out, "is laid out for at least 80x24") {
						t.Errorf("%s: no minimum-size notice below 80x24:\n%s", cell, out)
					}
				} else {
					for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
						if w := displayWidth(l); w > sz.Cols {
							t.Errorf("%s: a line occupies %d columns of %d: %q", cell, w, sz.Cols, l)
						}
					}
				}
				outputs = append(outputs, out)
			}
			if len(outputs) == 3 && (outputs[0] != outputs[1] || outputs[0] != outputs[2]) {
				t.Errorf("%dx%d/%s: colour, no-colour and plain differ; this client prints no colour, so they must be the same bytes", sz.Cols, sz.Rows, tx.Name)
			}
		}
		srv.Close()
	}
}

// The fit is by display columns: a wide sample never pushes a line past 80.
func TestKS094FitIsByDisplayColumns(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("テストを実行 — 👍 ", 12),
		strings.Repeat("combining é and zero​width ", 6),
		strings.Repeat("İstanbul'daki şube ığşçöü ", 6),
	} {
		f := fitWidth(s, 80)
		if displayWidth(f) > 80 {
			t.Errorf("fitted to %d columns: %q", displayWidth(f), f)
		}
	}
	if displayWidth("テ") != 2 || displayWidth("é") != 1 || displayWidth("a​b") != 2 {
		t.Error("display width is not by columns")
	}
}

// QA-094-1: a multi-line bracketed paste is one instruction, byte for byte,
// and a pasted "q" or "a <id>" is text, never a window command.
func TestKS094APastedMultiLineInstructionIsOneInstruction(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	c.mu.Lock()
	c.endless = true
	c.mu.Unlock()
	w := openWindow(t, bin, cfg, "agent", "open", "main", "--session", agentSessionShort)
	w.waitFor(t, "anything else to send it to the agent as an instruction")
	w.typeLine(t, "\x1b[200~line one")
	w.typeLine(t, "q")
	w.typeLine(t, "a apr_1")
	w.typeLine(t, "")
	w.typeLine(t, "  indented line four\twith tab")
	w.typeLine(t, "line five\x1b[201~")
	w.waitFor(t, "sent: task tsk_new1")
	w.detach(t)
	bodies := c.bodies()
	if len(bodies) != 1 {
		t.Fatalf("the paste became %d submissions: %v", len(bodies), bodies)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(bodies[0]), &body)
	want := "line one\nq\na apr_1\n\n  indented line four\twith tab\nline five"
	if body["text"] != want {
		t.Errorf("the pasted instruction was not kept intact:\n got %q\nwant %q", body["text"], want)
	}
	if d := c.decisions(); len(d) != 0 {
		t.Errorf("a pasted line decided an approval: %v", d)
	}
}

func TestKS094PasteBufferUnit(t *testing.T) {
	if p, text, done := startPaste("\x1b[200~one line\x1b[201~"); p != nil || !done || text != "one line" {
		t.Errorf("single-line paste: %v %q %v", p, text, done)
	}
	p, _, done := startPaste("before\x1b[200~first")
	if p == nil || done {
		t.Fatal("a paste did not start")
	}
	if _, done, ok := p.feed("second"); !ok || done {
		t.Fatal("a paste ended early")
	}
	if text, done, ok := p.feed("third\x1b[201~after"); !ok || !done || text != "first\nsecond\nthird" {
		t.Errorf("paste text %q", text)
	}
	var none *pasteBuffer
	if _, _, ok := none.feed("x"); ok {
		t.Error("a line with no paste in progress was taken as one")
	}
}
