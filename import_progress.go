// import_progress.go: the progress an import shows while it works, with
// numbers that are true.
//
// Three phases, one look:
//
//	Cleaning   ████████░░░░░░░░  51%  70 MB of 137 MB
//	Uploading  ████████░░░░░░░░  64%  880 MB of 1.37 GB  12 MB/s  1m 10s left
//	Placing    ⠹  the service is checking and placing it
//
// Reading is counted in bytes read out of the file. Uploading is counted in
// bytes the service acknowledged, never bytes written to the socket, so the
// bar does not run ahead of what would survive a failure, and 100% is shown
// only when the service has acknowledged the last byte. Placing has no
// percentage: nothing in it can be measured from here, so it is a spinner
// until the service says it is done.
//
// On a terminal the line is redrawn in place and the last line stays on the
// screen. With --plain, or without a terminal, one line is printed at each
// 10%. With --quiet or --json nothing is printed.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const barWidth = 16

// barEvent is one thing the bar showed, kept when a test asks for it.
type barEvent struct {
	Done, Total int64
	Percent     int
	Rate        float64 // bytes a second, 0 when not yet known
	ETA         time.Duration
	Final       bool
}

type xferBar struct {
	label string // "Uploading"
	noun  string // "uploading", in the plain lines
	total int64
	mode  string // "tty", "plain" or "off"
	w     io.Writer
	now   func() time.Time

	start    time.Time
	base     int64 // what was already there when the bar began (a resume)
	done     int64
	lastTick time.Time
	lastDec  int // the last tenth printed in plain mode
	window   []barSample
	drawn    bool
	events   *[]barEvent
	scale    int64 // the count is this many times the bytes shown (a file read in passes)
}

type barSample struct {
	at   time.Time
	done int64
}

// barMode is how progress is shown for this run.
func barMode() string {
	switch {
	case out.json || out.quiet:
		return "off"
	case stderrIsTerminal() && !out.plain && os.Getenv("NO_COLOR") == "":
		return "tty"
	}
	return "plain"
}

func newXferBar(label, noun string, total, already int64) *xferBar {
	b := &xferBar{label: label, noun: noun, total: total, mode: barMode(), w: os.Stderr, now: time.Now, lastDec: -1, scale: 1}
	b.begin(already)
	return b
}

func (b *xferBar) begin(already int64) {
	b.start, b.base, b.done = b.now(), already, already
	b.window = []barSample{{b.start, already}}
	if b.total > 0 {
		b.lastDec = int(already * 10 / b.total)
	}
}

// percent is whole percent done, and 100 only when everything is done.
func (b *xferBar) percent() int {
	if b.total <= 0 || b.done >= b.total {
		return 100
	}
	p := int(b.done * 100 / b.total)
	if p > 99 {
		p = 99
	}
	return p
}

// rate is bytes a second over the last ten seconds, or since the start while
// that is shorter; 0 while it is not yet known.
func (b *xferBar) rate() float64 {
	if len(b.window) < 2 {
		return 0
	}
	first, last := b.window[0], b.window[len(b.window)-1]
	dt := last.at.Sub(first.at).Seconds()
	if dt < 0.5 || last.done <= first.done {
		return 0
	}
	return float64(last.done-first.done) / dt
}

func (b *xferBar) eta() time.Duration {
	r := b.rate()
	if r <= 0 || b.done >= b.total {
		return 0
	}
	return time.Duration(float64(b.total-b.done) / r * float64(time.Second))
}

// update is told how much is now done. It never goes backwards by itself:
// a smaller number is ignored (reset is for the one case where it is true).
func (b *xferBar) update(done int64) {
	if done < b.done {
		return
	}
	if done > b.total {
		done = b.total
	}
	b.done = done
	t := b.now()
	b.window = append(b.window, barSample{t, done})
	for len(b.window) > 2 && t.Sub(b.window[0].at) > 10*time.Second {
		b.window = b.window[1:]
	}
	if done >= b.total {
		return // the last byte is shown by finish, once it is acknowledged and the caller says so
	}
	b.show(false)
}

// reset sets the count to what the service says it holds, down or up.
func (b *xferBar) reset(done int64) {
	b.begin(done)
	b.show(false)
}

func (b *xferBar) record(final bool) {
	if b.events != nil {
		*b.events = append(*b.events, barEvent{Done: b.done, Total: b.total, Percent: b.percent(), Rate: b.rate(), ETA: b.eta(), Final: final})
	}
}

func (b *xferBar) show(final bool) {
	if b.mode == "off" {
		b.record(final)
		return
	}
	if b.mode == "plain" {
		dec := 10
		if b.done < b.total && b.total > 0 {
			dec = int(b.done * 10 / b.total)
		}
		if dec > b.lastDec || (final && b.done >= b.total && b.lastDec < 10) {
			b.lastDec = dec
			b.record(final)
			fmt.Fprintf(b.w, "%s: %d%% (%s of %s)\n", b.noun, b.percent(), barBytes(b.done/b.scale), barBytes(b.total/b.scale))
		}
		return
	}
	if !final && b.now().Sub(b.lastTick) < 100*time.Millisecond && b.done < b.total {
		return
	}
	b.lastTick = b.now()
	b.record(final)
	b.drawn = true
	end := "\r\x1b[K"
	fmt.Fprint(b.w, end+b.line(final))
	if final {
		fmt.Fprintln(b.w)
		b.drawn = false
	}
}

func (b *xferBar) line(final bool) string {
	p := b.percent()
	n := p * barWidth / 100
	bar := strings.Repeat("█", n) + strings.Repeat("░", barWidth-n)
	s := fmt.Sprintf("%-10s %s %3d%%  %s of %s", b.label, bar, p, barBytes(b.done/b.scale), barBytes(b.total/b.scale))
	if final {
		if el := b.now().Sub(b.start); b.done > b.base && el >= time.Second {
			s += fmt.Sprintf("  %s in %s", barBytes((b.done-b.base)/b.scale), barDuration(el))
		}
		return s
	}
	if r := b.rate(); r > 0 {
		s += fmt.Sprintf("  %s/s", barBytes(int64(r)/b.scale))
		if e := b.eta(); e > 0 {
			s += "  " + barDuration(e) + " left"
		}
	}
	return s
}

// finish ends the bar, leaving its last line on the screen. Called when the
// whole count is done (the service acknowledged the last byte).
func (b *xferBar) finish() {
	if b.done < b.total {
		b.done = b.total
	}
	b.show(true)
}

// leave ends the bar early (a stop or a failure): the line stays as it was,
// on a line of its own, so the words that follow do not land on it.
func (b *xferBar) leave() {
	if b.mode == "tty" && b.drawn {
		fmt.Fprintln(b.w)
		b.drawn = false
	}
}

// barBytes is a size for a moving count: 880 MB, 1.37 GB.
func barBytes(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.0f KB", float64(n)/1e3)
	case n < 1_000_000_000:
		if n >= 10_000_000 {
			return fmt.Sprintf("%.0f MB", float64(n)/1e6)
		}
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.2f GB", float64(n)/1e9)
}

func barDuration(d time.Duration) string {
	s := int((d + 500*time.Millisecond) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
}

// ---- the spinner (placing on the service) ---------------------------------------

var importSpin = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner shows that something is going on that cannot be measured.
type spinner struct {
	label, noun string
	mode        string
	w           io.Writer
	stop        chan struct{}
	done        chan struct{}
	start       time.Time
}

func startSpinner(label, noun string) *spinner {
	s := &spinner{label: label, noun: noun, mode: barMode(), w: os.Stderr, stop: make(chan struct{}), done: make(chan struct{}), start: time.Now()}
	switch s.mode {
	case "off":
		close(s.done)
	case "plain":
		fmt.Fprintf(s.w, "%s...\n", noun)
		close(s.done)
	default:
		go func() {
			defer close(s.done)
			t := time.NewTicker(120 * time.Millisecond)
			defer t.Stop()
			for i := 0; ; i++ {
				fmt.Fprintf(s.w, "\r\x1b[K%-10s %s  %s  %s", s.label, importSpin[i%len(importSpin)], "the service is checking and placing it", barDuration(time.Since(s.start)))
				select {
				case <-s.stop:
					return
				case <-t.C:
				}
			}
		}()
	}
	return s
}

// end stops the spinner; ok says whether the service confirmed. The last line
// stays on the screen.
func (s *spinner) end(ok bool) {
	if s.mode == "off" {
		return
	}
	if s.mode == "plain" {
		if ok {
			fmt.Fprintf(s.w, "%s: done\n", s.noun)
		}
		return
	}
	close(s.stop)
	<-s.done
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Fprintf(s.w, "\r\x1b[K%-10s %s  %s\n", s.label, mark, barDuration(time.Since(s.start)))
}
