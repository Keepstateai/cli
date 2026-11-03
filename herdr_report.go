// herdr_report.go: terminal status for herdr's terminal app, which does not
// read OSC 7501. Inside a herdr pane (HERDR_ENV=1 with HERDR_PANE_ID and
// HERDR_BIN_PATH set) a stream also reports its agent's state the way
// herdr's guide for agents asks:
//
//	"$HERDR_BIN_PATH" pane report-agent "$HERDR_PANE_ID" --source keepstate
//	    --agent "<name> (KeepState)" --state <s> --message "<words>"
//	    --seq <unix ms> -- ks agent open <name> --session <short>
//
// and releases the pane when the stream is left. herdr has no blocked kind,
// so the kind goes into the message ("Needs you: permission. <msg>"); done
// reports idle (herdr tells the person when working turns idle) and an
// error reports blocked ("Error: <msg>"). Reports run in the background
// with a 2 second limit, only the newest waiting report is kept, and a
// failure is ignored: herdr never slows a stream down.
//
// herdr 0.9.5 keeps report-agent's --message but shows it nowhere (not in
// the sidebar, not in pane get or agent list), so each report is followed
// by the sidebar's words for the state, as a display label:
//
//	"$HERDR_BIN_PATH" pane report-metadata "$HERDR_PANE_ID" --source keepstate
//	    --agent "<name> (KeepState)" --state-label blocked="Needs you: permission"
//	    --seq <unix ms>
//
// herdr shows a label where a sidebar row has its state_text token, in place
// of its own state word, which is how "Needs you: permission" and "Done"
// (herdr itself only knows idle) reach the sidebar. The labels are cleared
// when the stream is left.
package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const herdrTimeout = 2 * time.Second

type herdrReporter struct {
	bin, pane, label string
	resume           []string

	mu      sync.Mutex
	seq     int64
	pending [][]string    // the newest report's calls waiting to run; older ones are dropped
	running chan struct{} // open while a report runs; closed when none does
	closed  bool
}

// herdrPane answers the pane's herdr binary and id when this process runs
// inside herdr, else false.
func herdrPane() (bin, pane string, ok bool) {
	if os.Getenv("HERDR_ENV") != "1" {
		return "", "", false
	}
	bin, pane = os.Getenv("HERDR_BIN_PATH"), os.Getenv("HERDR_PANE_ID")
	if bin == "" || pane == "" {
		return "", "", false
	}
	return bin, pane, true
}

// newHerdrReporter is the reporter for one stream, or nil outside herdr.
// resume is the command that reopens the stream; it is sent only when every
// word is one herdr accepts (a plain command first, no apostrophe and no
// control character).
func newHerdrReporter(name string, resume []string) *herdrReporter {
	bin, pane, ok := herdrPane()
	if !ok {
		return nil
	}
	h := &herdrReporter{bin: bin, pane: pane, label: oscText(name, 64) + " (KeepState)"}
	if herdrResumeOK(resume) {
		h.resume = resume
	}
	return h
}

func herdrResumeOK(argv []string) bool {
	if len(argv) == 0 || len(argv) > 64 || strings.ContainsAny(argv[0], "/\\") {
		return false
	}
	total := 0
	for _, a := range argv {
		total += len(a) + 1
		if a == "" || strings.ContainsRune(a, '\'') || oscText(a, len(a)+1) != a {
			return false
		}
	}
	return total <= 8<<10
}

// herdrState maps a report to herdr's state and message.
func herdrState(state, kind, msg string) (string, string) {
	switch state {
	case "working":
		return "working", msg
	case "blocked":
		words := map[string]string{"permission": "permission", "question": "a question", "auth": "sign-in or key"}[kind]
		head := "Needs you."
		if words != "" {
			head = "Needs you: " + words + "."
		}
		return "blocked", strings.TrimSpace(head + " " + msg)
	case "error":
		return "blocked", strings.TrimSpace("Error: " + msg)
	case "done", "idle":
		return "idle", msg
	}
	return "", ""
}

// herdrLabel is the sidebar's words for a state, the words the agent list
// uses; "" for a state herdr is not told about.
func herdrLabel(state, kind string) string {
	switch state {
	case "working":
		return "Working"
	case "blocked":
		if w := map[string]string{"permission": "permission", "question": "a question", "auth": "sign-in or key"}[kind]; w != "" {
			return "Needs you: " + w
		}
		return "Needs you"
	case "error":
		return "Error"
	case "done":
		return "Done"
	case "idle":
		return "Idle"
	}
	return ""
}

// nextSeq is the report's --seq: the time in milliseconds, and always
// higher than the last one sent.
func (h *herdrReporter) nextSeq() string {
	n := time.Now().UnixMilli()
	if n <= h.seq {
		n = h.seq + 1
	}
	h.seq = n
	return strconv.FormatInt(n, 10)
}

// report queues one state report; it never waits for herdr.
func (h *herdrReporter) report(state, kind, msg string) {
	if h == nil {
		return
	}
	hs, hm := herdrState(state, kind, msg)
	if hs == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	argv := []string{"pane", "report-agent", h.pane, "--source", "keepstate", "--agent", h.label, "--state", hs}
	if hm != "" {
		argv = append(argv, "--message", oscText(hm, oscMaxMsg))
	}
	argv = append(argv, "--seq", h.nextSeq())
	if len(h.resume) > 0 {
		argv = append(append(argv, "--"), h.resume...)
	}
	calls := [][]string{argv}
	if l := herdrLabel(state, kind); l != "" {
		calls = append(calls, []string{"pane", "report-metadata", h.pane, "--source", "keepstate", "--agent", h.label,
			"--state-label", hs + "=" + l, "--seq", h.nextSeq()})
	}
	h.pending = calls
	if h.running == nil {
		h.running = make(chan struct{})
		go h.drain(h.running)
	}
}

// drain runs the newest waiting report until none waits.
func (h *herdrReporter) drain(done chan struct{}) {
	for {
		h.mu.Lock()
		calls := h.pending
		h.pending = nil
		if calls == nil {
			h.running = nil
			close(done)
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		for _, argv := range calls {
			h.run(argv, herdrTimeout)
		}
	}
}

// run calls herdr once, within the time limit; what it says is ignored.
func (h *herdrReporter) run(argv []string, limit time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	_ = cmd.Run()
}

// release ends the stream's hold on the pane: a report still waiting is
// dropped, the one running gets half the time limit to finish (so the
// release lands after it), and the release, then the clearing of the
// sidebar labels, run within the rest. Leaving a stream never waits for
// herdr longer than the time limit.
func (h *herdrReporter) release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.pending = nil
	running := h.running
	h.mu.Unlock()
	start := time.Now()
	if running != nil {
		select {
		case <-running:
		case <-time.After(herdrTimeout / 2):
		}
	}
	h.mu.Lock()
	argv := []string{"pane", "release-agent", h.pane, "--source", "keepstate", "--agent", h.label, "--seq", h.nextSeq()}
	clear := []string{"pane", "report-metadata", h.pane, "--source", "keepstate", "--agent", h.label, "--clear-state-labels", "--seq", h.nextSeq()}
	h.mu.Unlock()
	h.run(argv, herdrTimeout-time.Since(start))
	if left := herdrTimeout - time.Since(start); left > 0 {
		h.run(clear, left)
	}
}
