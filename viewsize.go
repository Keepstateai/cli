// viewsize.go: the window holding control reports its terminal size on the
// lease's control channel (KS-033): POST /api/v2/control-leases/{id}/view.
//
// Once when the window opens, and again when the terminal is resized
// (SIGWINCH), debounced so a drag across the screen is one report and not
// a hundred. A size is never agent text and never a task. A lease that is
// released, expired or taken over stops the reports (the service refuses
// them); detaching sends nothing at all, and certainly no cancel.
package main

import (
	"errors"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const viewDebounce = 250 * time.Millisecond

// C04/KS-033 bounds the service accepts.
func viewSizeValid(cols, rows int) bool {
	return cols >= 20 && cols <= 1000 && rows >= 10 && rows <= 500
}

// sizeReporter sends the latest size, debounced; it stops for good once the
// service says the lease no longer controls the agent.
type sizeReporter struct {
	mu      sync.Mutex
	send    func(cols, rows int) error
	measure func() (int, int)
	timer   *time.Timer
	stopped bool
	last    [2]int
	delay   time.Duration
}

func (s *sizeReporter) now() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	cols, rows := s.measure()
	if !viewSizeValid(cols, rows) {
		return
	}
	s.mu.Lock()
	if s.last == [2]int{cols, rows} {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	err := s.send(cols, rows)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.last = [2]int{cols, rows}
		return
	}
	var he *hostedErr
	if errors.As(err, &he) && (he.Type == "ks_lease_gone" || he.Type == "ks_lease_expired") {
		s.stopped = true
	}
}

// resized schedules one report after the resizing settles.
func (s *sizeReporter) resized() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(s.delay, s.now)
}

func (s *sizeReporter) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
}

// startSizeReports reports this window's size for the lease it holds, at
// open and on every settled resize. It does nothing without a terminal or
// without a lease. The returned func stops it.
func startSizeReports(cr hostedCreds, win *liveWindow) func() {
	if _, ok := win.hold(); !ok || !stdinIsTerminal() {
		return func() {}
	}
	r := &sizeReporter{delay: viewDebounce, measure: termSize, send: func(cols, rows int) error {
		lease, ok := win.hold()
		if !ok {
			return &hostedErr{Type: "ks_lease_gone"}
		}
		return hostedCall(cr, "POST", "/api/v2/control-leases/"+url.PathEscape(lease.ID)+"/view", map[string]any{"cols": cols, "rows": rows}, nil)
	}}
	go r.now()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				r.resized()
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done); r.stop() }
}

// saveTerminal records the terminal's settings so every way out of the
// window puts them back as they were; a no-op without a terminal.
func saveTerminal() func() {
	saved, err := sttyOut("-g")
	if err != nil || saved == "" {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(func() { _ = sttyRun(saved) }) }
}
