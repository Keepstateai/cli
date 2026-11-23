package main

// ui.go: the shared component kit for one-shot commands. The app's look
// (apptheme.go, appui.go) is opened to every verb through a ui, a writer that
// knows whether it is themed and how wide it is. Four levels, never more:
// a title, facts, detail and the one Next command. Prose is wrapped, table
// cells are cut, a command in a Next line is never wrapped or cut.
//
// A ui is made once per stream by cmdUI. Colour never carries meaning alone:
// with colour off every word is the same. Nothing here touches --json: a
// command's JSON branch never reaches the kit.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// narrowCols: under this width the kit uses its narrow layouts.
const narrowCols = 60

type ui struct {
	w       io.Writer
	t       appTheme
	width   int  // columns to draw in, at most cardMax
	cols    int  // the terminal's own columns: a table may use them all
	pipe    bool // not a terminal and no COLUMNS: a table is not cut to any width
	tty     bool // the stream is a terminal
	animate bool // spinner and bars redraw in place
	in      *bufio.Reader
	inTTY   bool
	now     func() time.Time
	mu      sync.Mutex
}

// uiOut and uiErr are the two streams, made on first use by cmdUI.
var uiOut, uiErr *ui

// cmdUI decides colour, width and motion for both streams once.
func cmdUI() (stdout, stderr *ui) {
	if uiOut == nil || uiErr == nil {
		cols := termColumns()
		uiOut = newUI(os.Stdout, detectTheme(os.Getenv, stdoutIsTerminal(), true), cols, stdoutIsTerminal())
		uiErr = newUI(os.Stderr, detectTheme(os.Getenv, stderrIsTerminal(), true), cols, stderrIsTerminal())
		// into a pipe, with no COLUMNS to say otherwise, a table is whole: a
		// program reading it is never handed a cut cell
		uiOut.pipe = !stdoutIsTerminal() && os.Getenv("COLUMNS") == ""
		uiErr.pipe = !stderrIsTerminal() && os.Getenv("COLUMNS") == ""
		uiErr.in = bufio.NewReader(os.Stdin)
		uiErr.inTTY = stdinIsTerminal()
	}
	return uiOut, uiErr
}

func newUI(w io.Writer, t appTheme, cols int, tty bool) *ui {
	if cols <= 0 {
		cols = 80
	}
	u := &ui{w: w, t: t, width: min(cols, cardMax), cols: cols, tty: tty, now: time.Now}
	// motion needs a terminal that can redraw, a theme (NO_COLOR, TERM=dumb,
	// --plain and CI turn it off) and nobody asking for quiet
	u.animate = tty && t.on() && !out.quiet && !out.plain && os.Getenv("TERM") != "dumb"
	return u
}

func (u *ui) narrow() bool { return u.width < narrowCols }

func (u *ui) printf(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.w, format, a...)
}

func (u *ui) lines(ls ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, l := range ls {
		fmt.Fprintln(u.w, l)
	}
}

// Blank is the one blank line between blocks.
func (u *ui) Blank() { u.lines("") }

// Title is K1: what this screen is, with its outcome icon, in bold. A title
// with no icon is a neutral screen. detail is dim and follows the title.
func (u *ui) Title(icon string, tn tone, text string, detail ...string) {
	head := text
	prefix := ""
	if icon != "" {
		prefix = u.t.toned(tn, icon) + " "
	}
	first := prefix
	rest := strings.Repeat(" ", displayWidth(prefix))
	body := wrapHang(first, rest, u.t.bold(head), u.width)
	if len(detail) > 0 && strings.TrimSpace(detail[0]) != "" {
		last := body[len(body)-1]
		d := "  " + u.t.dim(detail[0])
		if displayWidth(last)+displayWidth(d) <= u.width {
			body[len(body)-1] = last + d
		} else {
			body = append(body, wrapHang(rest, rest, u.t.dim(detail[0]), u.width)...)
		}
	}
	u.lines(body...)
}

// Facts is K2: label/value rows. Normal layout aligns the labels (at most
// 14 columns) and wraps values under themselves; narrow puts the label on its
// own line with the value two columns in. Plain shows "label: value".
func (u *ui) Facts(rows ...[2]string) {
	if len(rows) == 0 {
		return
	}
	labelW := 0
	for _, r := range rows {
		labelW = max(labelW, displayWidth(r[0]))
	}
	labelW = min(labelW+2, 18)
	var out []string
	for _, r := range rows {
		label, value := r[0], r[1]
		switch {
		case u.narrow():
			out = append(out, "  "+u.t.dim(label))
			for _, l := range strings.Split(value, "\n") {
				out = append(out, wrapHang("    ", "    ", l, u.width)...)
			}
		case u.t.on():
			pad := strings.Repeat(" ", max(2, labelW-displayWidth(label)))
			first := "  " + u.t.dim(label) + pad
			out = append(out, wrapHang(first, strings.Repeat(" ", displayWidth(first)), value, u.width)...)
		default:
			first := "  " + label + ": "
			out = append(out, wrapHang(first, strings.Repeat(" ", displayWidth(first)), value, u.width)...)
		}
	}
	u.lines(out...)
}

// Note is a dim paragraph under a block, wrapped and indented by two.
func (u *ui) Note(text string) {
	var out []string
	for _, l := range wrapText(text, u.width-2) {
		out = append(out, "  "+u.t.dim(l))
	}
	u.lines(out...)
}

// Col is one table column. Flex columns take what is left and are cut in
// the middle; Right aligns numbers; Second columns move to the second line
// of a row in the narrow layout (the others share its first line).
type Col struct {
	Title  string
	Right  bool
	Flex   bool
	Second bool
	// Wrap marks the last column as prose: it takes what the others leave and
	// is wrapped on word boundaries under itself, never cut
	Wrap bool
}

// Table is K3. Headers are the only capitals; columns are two spaces apart;
// rows never wrap. An empty table prints nothing: say it in a sentence and a
// Next instead.
//
// The table takes the terminal's whole width (a pipe takes none: nothing is
// cut for a program to read). A Flex column is cut in the middle to make it
// fit, down to 24 columns; when the table still does not fit, or under 60
// columns, every row is stacked instead (stackedTable), wrapped on word
// boundaries, so no cell and no id is ever cut to a stub.
func (u *ui) Table(cols []Col, rows [][]string) {
	if len(rows) == 0 || len(cols) == 0 {
		return
	}
	avail := u.cols
	if u.pipe {
		avail = 1 << 30
	}
	if avail <= 0 {
		avail = u.width
	}
	if u.narrow() && !u.pipe {
		u.stackedTable(cols, rows)
		return
	}
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = displayWidth(c.Title)
		for _, r := range rows {
			if i < len(r) {
				w[i] = max(w[i], displayWidth(r[i]))
			}
		}
	}
	flex, wrapCol := -1, -1
	total := 2
	for i, c := range cols {
		if c.Flex && flex < 0 {
			flex = i
		}
		if c.Wrap && i == len(cols)-1 {
			wrapCol = i
		}
		total += w[i] + 2
	}
	total -= 2
	if wrapCol >= 0 {
		// the prose column takes what is left, at least 24 columns; when the
		// other columns leave less, the rows are stacked
		others := total - w[wrapCol]
		room := avail - others
		switch {
		case room >= w[wrapCol]:
		case room >= 24:
			w[wrapCol] = room
		default:
			u.stackedTable(cols, rows)
			return
		}
		total = others + w[wrapCol]
	} else if total > avail {
		if flex < 0 || w[flex]-(total-avail) < min(24, w[flex]) {
			u.stackedTable(cols, rows)
			return
		}
		w[flex] -= total - avail
	}
	line := func(cells []string, head bool) []string {
		var b strings.Builder
		b.WriteString("  ")
		var tail []string
		for i, c := range cols {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			switch {
			case i == wrapCol && !head:
				ws := wrapText(v, w[i])
				v = ws[0]
				tail = ws[1:]
			case i == wrapCol:
				v = fitWidth(v, w[i])
			case i == flex:
				v = cutMiddle(v, w[i])
			default:
				v = fitWidth(v, w[i])
			}
			pad := strings.Repeat(" ", max(0, w[i]-displayWidth(v)))
			switch {
			case c.Right:
				v = pad + v
			case i < len(cols)-1:
				v += pad
			}
			if head {
				v = u.t.dim(v)
			}
			b.WriteString(v)
			if i < len(cols)-1 {
				b.WriteString("  ")
			}
		}
		out := []string{strings.TrimRight(b.String(), " ")}
		if len(tail) > 0 {
			ind := strings.Repeat(" ", total-w[wrapCol])
			for _, t := range tail {
				out = append(out, ind+t)
			}
		}
		return out
	}
	heads := make([]string, len(cols))
	for i, c := range cols {
		heads[i] = c.Title
	}
	out := line(heads, true)
	for _, r := range rows {
		out = append(out, line(r, false)...)
	}
	u.lines(out...)
}

// stackedTable is a table too wide for its terminal: each row is its main
// cells on a line (wrapped on word boundaries, hanging under its first word)
// and, dim under it, the Second cells.
func (u *ui) stackedTable(cols []Col, rows [][]string) {
	var out []string
	multi := false
	for _, r := range rows {
		for i, c := range cols {
			if c.Second && i < len(r) && r[i] != "" {
				multi = true
			}
		}
	}
	for n, r := range rows {
		var first, second []string
		for i, c := range cols {
			if i >= len(r) || r[i] == "" {
				continue
			}
			if c.Second {
				second = append(second, r[i])
			} else {
				first = append(first, r[i])
			}
		}
		if len(first) == 0 && len(second) > 0 {
			first, second = second[:1], second[1:]
		}
		if multi && n > 0 {
			out = append(out, "")
		}
		for i, l := range wrapText(strings.Join(first, "  "), u.width-4) {
			if i == 0 {
				out = append(out, "  "+l)
			} else {
				out = append(out, "    "+l)
			}
		}
		if len(second) > 0 {
			for _, l := range wrapText(strings.Join(second, " \u00b7 "), u.width-4) {
				out = append(out, "    "+u.t.dim(l))
			}
		}
	}
	u.lines(out...)
}

// cutMiddle cuts s to w columns from the middle, so a path keeps its start
// and its end: ~/TRMNL/…/BulkPulse.
func cutMiddle(s string, w int) string {
	if displayWidth(s) <= w {
		return s
	}
	if w < 3 {
		return fitWidth(s, w)
	}
	r := []rune(s)
	keepEnd := (w - 1) / 2
	keepStart := w - 1 - keepEnd
	for len(r) > 0 && displayWidth(string(r[:keepStart]))+displayWidth(string(r[len(r)-keepEnd:]))+1 > w {
		if keepEnd > keepStart {
			keepEnd--
		} else {
			keepStart--
		}
	}
	return string(r[:keepStart]) + "…" + string(r[len(r)-keepEnd:])
}

// Next is K4: the command to run next, with at most two choices. A command is
// never wrapped or cut; one too wide for its line goes on a line of its own,
// indented by two, so a triple-click copies it whole.
func (u *ui) Next(cmds ...string) {
	u.nextBlock(cmds, "")
}

// NextWhy is one Next command with a short reason under it.
func (u *ui) NextWhy(cmd, why string) {
	u.nextBlock([]string{cmd}, why)
}

func (u *ui) nextBlock(cmds []string, why string) {
	var out []string
	for i, c := range cmds {
		if i >= 2 {
			break
		}
		label := "Next:"
		if i > 0 {
			label = "  or:"
		}
		out = append(out, u.nextLine(label, c)...)
	}
	if why != "" {
		out = append(out, wrapHang("      ", "      ", u.t.dim(why), u.width)...)
	}
	u.lines(out...)
}

func (u *ui) nextLine(label, cmd string) []string {
	if !strings.HasPrefix(cmd, "ks ") {
		// words, not a command: wrapped like prose
		return wrapHang(u.t.dim(label)+" ", strings.Repeat(" ", displayWidth(label)+1), cmd, u.width)
	}
	if displayWidth(label)+1+displayWidth(cmd) <= u.width {
		return []string{u.t.dim(label) + " " + u.t.accent(cmd)}
	}
	return []string{u.t.dim(label), "  " + u.t.accent(cmd)}
}

// Success is K8: what now exists, one to four facts and the Next command.
func (u *ui) Success(title string, facts [][2]string, next ...string) {
	u.Title(iconDone, toneOK, title)
	u.Facts(facts...)
	if len(next) > 0 {
		u.Blank()
		u.Next(next...)
	}
}

// Warn is K9: what you should know. It goes before the block it belongs to.
func (u *ui) Warn(title string, body ...string) {
	u.Title(iconWarn, toneWarn, title)
	var out []string
	for _, b := range body {
		for _, l := range wrapText(b, u.width-2) {
			out = append(out, "  "+l)
		}
	}
	u.lines(out...)
}

// ErrorBlock is K10: a title in the failure colour, then a left rule with the
// why and what happened, then the Next command. It uses a rule, not a box, so
// it copies cleanly, survives a resize and works at 40 columns. warn lines
// (when work may have started) are drawn in the warning colour.
func (u *ui) ErrorBlock(title string, why []string, warn []string, next ...string) {
	rule := u.t.bad("│") + " "
	out := wrapHang(u.t.bad(iconFailed)+" ", "  ", u.t.bad(title), u.width)
	for _, w := range why {
		for _, l := range wrapText(w, u.width-2) {
			out = append(out, rule+l)
		}
	}
	for _, w := range warn {
		for _, l := range wrapText(w, u.width-2) {
			out = append(out, rule+u.t.warn(l))
		}
	}
	u.lines(out...)
	if len(next) > 0 {
		u.Blank()
		u.Next(next...)
	}
}

// ---- errors ----------------------------------------------------------------------

// splitMessage turns a one-sentence refusal into a title and its reasons at
// its first "; " or ". ", so an error that carries no title of its own still
// reads as a title and a why.
func splitMessage(msg string) (string, []string) {
	msg = strings.TrimSpace(msg)
	cut, skip := -1, 0
	for _, sep := range []string{"; ", ". "} {
		if i := strings.Index(msg, sep); i >= 0 && (cut < 0 || i < cut) {
			cut, skip = i, len(sep)
		}
	}
	if cut < 0 {
		return capFirst(strings.TrimSuffix(msg, ".")), nil
	}
	title := capFirst(strings.TrimSuffix(msg[:cut], "."))
	var why []string
	for _, part := range strings.Split(msg[cut+skip:], "; ") {
		if part = strings.TrimSpace(part); part != "" {
			why = append(why, sentence(part))
		}
	}
	return title, why
}

// errorTitle and errorWhy are the optional words of an error that has them.
// A title that has no Title field is derived from Message.
func errorWords(ce *cliError) (string, []string) {
	title, why := ce.Title, ce.Why
	if title == "" {
		t, w := splitMessage(ce.Message)
		title = t
		if len(why) == 0 {
			why = w
		}
	}
	return title, why
}

// saysNothingHappened: the reasons already say that nothing was sent or
// created, so the block does not say it a second time.
func saysNothingHappened(why []string) bool {
	for _, w := range why {
		if strings.Contains(w, "Nothing was sent") || strings.Contains(w, "Nothing was created") || strings.Contains(w, "Nothing was done") {
			return true
		}
	}
	return false
}

// renderError draws a failure for a person at a terminal. fail() calls it
// when stderr is a terminal with colour on; every other stream gets the
// contract lines (error:, operation:, Remote work started:, Next:), byte for
// byte as before.
func renderError(u *ui, ce *cliError) {
	if d, ok := ce.Detail.(errorDetail); ok {
		u.lines(d.detailLines()...)
	}
	title, why := errorWords(ce)
	var warn []string
	switch ce.WorkStarted {
	case workYes:
		why = append(why, "Some of the work had already started.")
	case workUnknown:
		why = append(why, "It is not known whether the work started.")
		warn = append(warn, "Don't run the same command again blindly.")
	default:
		if ce.Kind != "usage" && !saysNothingHappened(why) {
			why = append(why, "Nothing was sent or created.")
		}
	}
	next := ce.NextAction
	if next == "" && ce.WorkStarted == workUnknown && ce.OperationID != "" {
		next = "ks operation show " + ce.OperationID
	}
	var nexts []string
	if next != "" {
		nexts = []string{next}
	}
	u.ErrorBlock(title, why, warn, nexts...)
}

// ---- steps -----------------------------------------------------------------------

// stepList is K5. In a terminal the running step shows a spinner and its
// elapsed time and is replaced in place when it ends. Everywhere else a step
// prints one line when it starts and one when it ends; nothing redraws.
type stepList struct {
	u       *ui
	name    string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
}

func (u *ui) Steps() *stepList { return &stepList{u: u} }

// Start begins a step. Say what is happening, in the present tense.
func (s *stepList) Start(name string) {
	s.end()
	s.name = name
	s.started = s.u.now()
	if !s.u.animate {
		s.u.lines("  " + s.u.t.dim("•") + " " + name + "...")
		return
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	s.draw(0)
	go func(stop, done chan struct{}) {
		defer close(done)
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
				s.draw(i)
			}
		}
	}(s.stop, s.done)
}

// spinnerLine is the running step's line at a frame and an elapsed time.
func (u *ui) spinnerLine(name string, frame int, elapsed time.Duration) string {
	l := "  " + u.t.accent(spinFrames[frame%len(spinFrames)]) + " " + name + "…"
	if elapsed >= 3*time.Second {
		l += " " + u.t.dim(kitDuration(elapsed))
	}
	return l
}

func (s *stepList) draw(frame int) {
	s.u.printf("\r\x1b[2K%s", fitWidth(s.u.spinnerLine(s.name, frame, s.u.now().Sub(s.started)), s.u.width))
}

// end stops the spinner and clears its line; the caller prints the step's
// closing line in its place.
func (s *stepList) end() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
	s.u.printf("\r\x1b[2K")
}

func (s *stepList) finish(mark, name, detail string, bad bool) {
	was := s.name != ""
	dur := time.Duration(0)
	if was {
		dur = s.u.now().Sub(s.started)
	}
	s.end()
	s.name = ""
	s.u.lines(s.u.stepLine(mark, name, detail, dur, bad))
	_ = was
}

// stepLine is a step that has ended: a mark, its words, its detail and time.
func (u *ui) stepLine(mark, name, detail string, dur time.Duration, bad bool) string {
	parts := []string{}
	if detail != "" {
		parts = append(parts, detail)
	}
	if dur >= 100*time.Millisecond {
		parts = append(parts, kitDuration(dur))
	}
	if !u.t.on() || u.narrow() {
		l := name
		if len(parts) > 0 {
			l += " (" + strings.Join(parts, ", ") + ")"
		}
		return strings.Join(wrapHang("  "+mark+" ", "    ", l, u.width), "\n")
	}
	tone := toneOK
	switch mark {
	case iconFailed:
		tone = toneBad
	case "–":
		tone = toneDim
	}
	head := "  " + u.t.toned(tone, mark) + " " + name
	if bad {
		head = "  " + u.t.toned(tone, mark) + " " + u.t.bad(name)
	}
	right := u.t.dim(strings.Join(parts, " · "))
	gap := u.width - displayWidth(head) - displayWidth(right)
	if len(parts) == 0 {
		return head
	}
	return head + strings.Repeat(" ", max(2, gap)) + right
}

// Done ends the running step as done. name is its past-tense words.
func (s *stepList) Done(name, detail string) { s.finish(iconDone, name, detail, false) }

// Fail ends the running step as failed.
func (s *stepList) Fail(name, detail string) { s.finish(iconFailed, name, detail, true) }

// Skip ends a step that was not needed, with the reason.
func (s *stepList) Skip(name, reason string) { s.finish("–", name, reason, false) }

// Close stops a spinner still running (a Ctrl-C, a panic path).
func (s *stepList) Close() { s.end() }

// ---- progress bar ----------------------------------------------------------------

// bar is K6: a transfer of a known size. It shows bytes, percent, rate and
// time left in a terminal, and one line at each 10% everywhere else.
type bar struct {
	u       *ui
	label   string
	total   int64
	done    int64
	start   time.Time
	last    time.Time
	drawn   bool
	tenth   int
	samples []barSample
}

type barSample struct {
	at time.Time
	n  int64
}

func (u *ui) Progress(label string, total int64) *bar {
	now := u.now()
	return &bar{u: u, label: label, total: total, start: now, samples: []barSample{{now, 0}}}
}

// Set records the bytes moved so far. A terminal redraws at most every 100 ms.
func (b *bar) Set(done int64) {
	now := b.u.now()
	b.done = done
	b.samples = append(b.samples, barSample{now, done})
	for len(b.samples) > 2 && now.Sub(b.samples[0].at) > 5*time.Second {
		b.samples = b.samples[1:]
	}
	if !b.u.animate {
		if b.total > 0 {
			for t := int(done * 10 / b.total); b.tenth < t && b.tenth < 10; {
				b.tenth++
				b.u.lines(wrapHang("  ", "    ", b.plainLine(b.tenth*10), b.u.width)...)
			}
		}
		return
	}
	if b.drawn && now.Sub(b.last) < 100*time.Millisecond && done < b.total {
		return
	}
	b.last = now
	b.redraw()
}

func (b *bar) percent() int {
	if b.total <= 0 {
		return 0
	}
	return int(min(100, b.done*100/b.total))
}

func (b *bar) plainLine(pct int) string {
	return fmt.Sprintf("%s: %d%% (%s of %s)", lowerFirst(b.label), pct, kitBytes(b.total*int64(pct)/100), kitBytes(b.total))
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// rate is the 5 second moving average in bytes per second, and the time left
// once there are 2 seconds of data.
func (b *bar) rate() (float64, time.Duration, bool) {
	first, lastS := b.samples[0], b.samples[len(b.samples)-1]
	span := lastS.at.Sub(first.at)
	if span <= 0 || lastS.n <= first.n {
		return 0, 0, false
	}
	r := float64(lastS.n-first.n) / span.Seconds()
	if lastS.at.Sub(b.start) < 2*time.Second || b.total <= 0 {
		return r, 0, false
	}
	return r, time.Duration(float64(b.total-b.done) / r * float64(time.Second)), true
}

// barLines is the bar at a width: two lines in a terminal, one under 40
// columns with no bar at all.
func (b *bar) barLines() []string {
	u := b.u
	pct := b.percent()
	r, left, haveLeft := b.rate()
	if u.width < 40 {
		return []string{fitWidth(fmt.Sprintf("%s %d%% · %s of %s", b.label, pct, kitBytes(b.done), kitBytes(b.total)), u.width)}
	}
	// the bar shrinks to fit, at least 10 cells
	fixed := 2 + 1 + 1 + displayWidth(b.label) + 2 + 2 + 4
	cells := max(10, min(24, u.width-fixed))
	filled := cells * pct / 100
	head := "  " + u.t.accent(spinFrames[int(b.u.now().UnixMilli()/100)%len(spinFrames)]) + " " + b.label + "  " +
		u.t.accent(strings.Repeat("━", filled)) + u.t.subtle(strings.Repeat("─", cells-filled)) + fmt.Sprintf(" %3d%%", pct)
	detail := []string{kitBytes(b.done) + " of " + kitBytes(b.total)}
	if r > 0 {
		detail = append(detail, kitBytes(int64(r))+"/s")
	}
	if haveLeft {
		detail = append(detail, "about "+kitDuration(left)+" left")
	}
	return []string{head, "    " + u.t.dim(fitWidth(strings.Join(detail, " · "), u.width-4))}
}

func (b *bar) redraw() {
	ls := b.barLines()
	b.u.mu.Lock()
	defer b.u.mu.Unlock()
	if b.drawn {
		fmt.Fprintf(b.u.w, "\x1b[%dA", len(ls))
	}
	for _, l := range ls {
		fmt.Fprintf(b.u.w, "\r\x1b[2K%s\n", l)
	}
	b.drawn = true
}

// Done replaces the bar by one step line that stays in the scrollback.
func (b *bar) Done(doneLabel string) { b.DoneWith(doneLabel, kitBytes(b.total)) }

// DoneWith is Done with the words for the right-hand column in place of the
// bar's own size, for a step whose result is not its input ("537 MB down to
// 2.4 MB"). The time follows.
func (b *bar) DoneWith(doneLabel, detail string) {
	dur := b.u.now().Sub(b.start)
	if b.drawn {
		b.u.mu.Lock()
		fmt.Fprintf(b.u.w, "\x1b[%dA", len(b.barLines()))
		for range b.barLines() {
			fmt.Fprint(b.u.w, "\r\x1b[2K\n")
		}
		fmt.Fprintf(b.u.w, "\x1b[%dA", len(b.barLines()))
		b.u.mu.Unlock()
	}
	b.drawn = false
	b.u.lines(b.u.stepLine(iconDone, doneLabel, detail+" in "+kitDuration(dur), 0, false))
}

// kitBytes is a size as people say it: 137 MB, 3.1 MB, 61.2 MB.
func kitBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d B", n)
	case f >= 100:
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// kitDuration is a time as people read it: 0.8s, 14s, 1m05s.
func kitDuration(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ---- confirmation ----------------------------------------------------------------

// Confirm is K7, the one yes/no question. --yes answers it. With no terminal
// to answer on, or --no-input, it never waits: it returns a refusal (exit 2)
// that names the flag to pass. An answer of no, an empty answer to a [y/N]
// prompt or end of input is declined: nothing is changed and the refusal
// carries the command's own "nothing" line.
func (u *ui) Confirm(q string, def bool, yesFlag, nothing string) (bool, error) {
	if out.yes {
		return true, nil
	}
	if out.noInput || u.in == nil || !u.inTTY {
		return false, &cliError{Code: exitUsage, Kind: "confirmation_unavailable",
			Message:    "confirmation needed and this terminal can't answer: add " + yesFlag + ". " + nothing,
			NextAction: "", WorkStarted: workNo}
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	u.printf("%s %s %s ", u.t.info("?"), u.t.bold(q), u.t.dim(hint))
	line, err := u.in.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	yes := def
	switch ans {
	case "y", "yes":
		yes = true
	case "n", "no":
		yes = false
	case "":
		yes = def && err == nil
	default:
		yes = false
	}
	if yes {
		return true, nil
	}
	u.lines(nothing)
	return false, &cliError{Code: exitUsage, Kind: "confirmation_declined", Message: nothing, WorkStarted: workNo}
}

// ConfirmName is the typed-name confirmation for a step that destroys
// something: only typing its name goes ahead; Enter keeps it. --yes never
// answers it.
func (u *ui) ConfirmName(q, name, nothing string) (bool, error) {
	if out.noInput || u.in == nil || !u.inTTY {
		return false, &cliError{Code: exitUsage, Kind: "confirmation_unavailable",
			Message: "this is confirmed by naming it and this terminal can't answer: add --confirm " + name + ". " + nothing, WorkStarted: workNo}
	}
	u.lines(u.t.bad(iconWarn) + " " + u.t.bold(q))
	u.printf("  Type %s to go ahead, or press Enter to keep it: ", u.t.accent(name))
	line, _ := u.in.ReadString('\n')
	if strings.TrimSpace(line) == name {
		return true, nil
	}
	u.lines(nothing)
	return false, &cliError{Code: exitUsage, Kind: "confirmation_declined", Message: nothing, WorkStarted: workNo}
}

// phaseWords says the service's start phases in a person's words; an
// unknown phase is shown as the service sent it.
var phaseWords = map[string]string{
	"create_guest": "Creating the machine",
	"start_agent":  "Starting the agent",
	"cleanup":      "Cleaning up",
}

func phaseWord(p string) string { return wordFor(phaseWords, p) }
