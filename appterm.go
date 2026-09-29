package main

// appterm.go: the terminal layer of the ks app. Two things and nothing more:
//
//   - keys: standard input in raw mode (golang.org/x/term), decoded into
//     key events, with bracketed paste kept as ONE event so a pasted "q" or
//     "/stop" is text and never a command;
//   - a screen: what has happened is PRINTED into the terminal's own
//     scrollback (so the terminal scrolls, searches and copies it as usual),
//     and a small live region at the bottom -- a prompt box, the input line
//     and the status line -- is erased and redrawn around every print.
//
// Nothing here knows about agents; app.go does.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// ---- keys -----------------------------------------------------------------

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyEsc
	keyCtrlC
	keyCtrlD
	keyCtrlU
	keyCtrlW
	keyTab
	keyPaste
)

type keyEvent struct {
	kind keyKind
	r    rune
	text string // keyPaste
}

// escWait is how long a lone ESC waits for the rest of a sequence: long
// enough for any terminal to send "[A", short enough that Esc feels instant.
const escWait = 40 * time.Millisecond

// readKeys decodes raw bytes into key events until the reader ends.
func readKeys(r io.Reader, out chan<- keyEvent) {
	defer close(out)
	bytesCh := make(chan byte, 256)
	go func() {
		br := bufio.NewReader(r)
		for {
			b, err := br.ReadByte()
			if err != nil {
				close(bytesCh)
				return
			}
			bytesCh <- b
		}
	}()
	next := func(wait time.Duration) (byte, bool) {
		if wait <= 0 {
			b, ok := <-bytesCh
			return b, ok
		}
		select {
		case b, ok := <-bytesCh:
			return b, ok
		case <-time.After(wait):
			return 0, false
		}
	}
	for {
		b, ok := next(0)
		if !ok {
			return
		}
		switch {
		case b == 0x03:
			out <- keyEvent{kind: keyCtrlC}
		case b == 0x04:
			out <- keyEvent{kind: keyCtrlD}
		case b == 0x15:
			out <- keyEvent{kind: keyCtrlU}
		case b == 0x17:
			out <- keyEvent{kind: keyCtrlW}
		case b == 0x01:
			out <- keyEvent{kind: keyHome}
		case b == 0x05:
			out <- keyEvent{kind: keyEnd}
		case b == '\t':
			out <- keyEvent{kind: keyTab}
		case b == '\r' || b == '\n':
			out <- keyEvent{kind: keyEnter}
		case b == 0x7f || b == 0x08:
			out <- keyEvent{kind: keyBackspace}
		case b == 0x1b:
			out <- decodeEscape(next)
		case b < 0x20:
			// other control bytes are ignored rather than typed
		default:
			out <- keyEvent{kind: keyRune, r: decodeRune(b, next)}
		}
	}
}

func decodeRune(first byte, next func(time.Duration) (byte, bool)) rune {
	if first < utf8.RuneSelf {
		return rune(first)
	}
	buf := []byte{first}
	for !utf8.FullRune(buf) && len(buf) < utf8.UTFMax {
		b, ok := next(escWait)
		if !ok {
			break
		}
		buf = append(buf, b)
	}
	r, _ := utf8.DecodeRune(buf)
	return r
}

// decodeEscape reads what follows ESC: a CSI sequence, an SS3 arrow, a
// bracketed paste, or nothing (the Esc key itself).
func decodeEscape(next func(time.Duration) (byte, bool)) keyEvent {
	b, ok := next(escWait)
	if !ok {
		return keyEvent{kind: keyEsc}
	}
	if b == 'O' { // SS3: arrows and Home/End on some terminals
		c, _ := next(escWait)
		return ss3Key(c)
	}
	if b != '[' {
		return keyEvent{kind: keyEsc}
	}
	var params []byte
	for {
		c, ok := next(escWait)
		if !ok {
			return keyEvent{kind: keyEsc}
		}
		if c >= 0x40 && c <= 0x7e { // final byte
			return csiKey(string(params), c, next)
		}
		params = append(params, c)
	}
}

func ss3Key(c byte) keyEvent {
	switch c {
	case 'A':
		return keyEvent{kind: keyUp}
	case 'B':
		return keyEvent{kind: keyDown}
	case 'C':
		return keyEvent{kind: keyRight}
	case 'D':
		return keyEvent{kind: keyLeft}
	case 'H':
		return keyEvent{kind: keyHome}
	case 'F':
		return keyEvent{kind: keyEnd}
	}
	return keyEvent{kind: keyEsc}
}

func csiKey(params string, final byte, next func(time.Duration) (byte, bool)) keyEvent {
	switch final {
	case 'A':
		return keyEvent{kind: keyUp}
	case 'B':
		return keyEvent{kind: keyDown}
	case 'C':
		return keyEvent{kind: keyRight}
	case 'D':
		return keyEvent{kind: keyLeft}
	case 'H':
		return keyEvent{kind: keyHome}
	case 'F':
		return keyEvent{kind: keyEnd}
	case '~':
		switch params {
		case "3":
			return keyEvent{kind: keyDelete}
		case "1", "7":
			return keyEvent{kind: keyHome}
		case "4", "8":
			return keyEvent{kind: keyEnd}
		case "200":
			return readPaste(next)
		}
	}
	return keyEvent{kind: keyEsc}
}

// readPaste collects a bracketed paste up to ESC [ 201 ~ as one event.
func readPaste(next func(time.Duration) (byte, bool)) keyEvent {
	const end = "\x1b[201~"
	var b strings.Builder
	for {
		c, ok := next(0)
		if !ok {
			break
		}
		b.WriteByte(c)
		if strings.HasSuffix(b.String(), end) {
			s := strings.TrimSuffix(b.String(), end)
			return keyEvent{kind: keyPaste, text: strings.ReplaceAll(s, "\r\n", "\n")}
		}
	}
	return keyEvent{kind: keyPaste, text: b.String()}
}

// ---- raw mode ----------------------------------------------------------------

// rawTerminal puts standard input in raw mode and turns on bracketed paste;
// the function it returns puts everything back, once, on every way out.
func rawTerminal() (func(), error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	fmt.Fprint(os.Stdout, "\x1b[?2004h")
	var once sync.Once
	return func() {
		once.Do(func() {
			fmt.Fprint(os.Stdout, "\x1b[?2004l")
			_ = term.Restore(fd, old)
		})
	}, nil
}

func appTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w >= 20 {
		return w
	}
	return 80
}

// ---- screen -----------------------------------------------------------------

// screen owns the bottom of the terminal. Every write goes through it, so
// the live region is never torn by a line printed from another goroutine.
type screen struct {
	mu    sync.Mutex
	w     io.Writer
	width func() int

	// the live region, top to bottom: box, rule, input, status
	box    []string
	prompt string
	input  []rune
	cursor int
	status string
	hint   string

	drawn     int // rows of the live region currently on screen
	cursorRow int // the row of the live region the cursor sits on
	hidden    bool
}

func newScreen(w io.Writer) *screen {
	return &screen{w: w, width: appTerminalWidth, prompt: "> "}
}

// erase removes the live region, leaving the cursor at the start of where
// it began. Caller holds mu.
func (s *screen) erase() {
	if s.drawn == 0 {
		return
	}
	fmt.Fprint(s.w, "\r")
	if s.cursorRow > 0 {
		fmt.Fprintf(s.w, "\x1b[%dA", s.cursorRow)
	}
	fmt.Fprint(s.w, "\x1b[J")
	s.drawn, s.cursorRow = 0, 0
}

// draw paints the live region and parks the cursor in the input. Caller
// holds mu.
func (s *screen) draw() {
	if s.hidden {
		return
	}
	width := s.width()
	var rows []string
	rows = append(rows, s.box...)
	rows = append(rows, strings.Repeat("─", width))
	inputRow := len(rows)
	line, col := s.inputLine(width)
	rows = append(rows, line)
	if s.hint != "" {
		rows = append(rows, fitWidth(s.hint, width))
	}
	rows = append(rows, fitWidth(s.status, width))
	for i, r := range rows {
		if i > 0 {
			fmt.Fprint(s.w, "\r\n")
		}
		fmt.Fprint(s.w, r)
	}
	// back up to the input row, and to the cursor's column in it
	up := len(rows) - 1 - inputRow
	if up > 0 {
		fmt.Fprintf(s.w, "\x1b[%dA", up)
	}
	fmt.Fprintf(s.w, "\r\x1b[%dC", col)
	if col == 0 {
		fmt.Fprint(s.w, "\r")
	}
	s.drawn, s.cursorRow = len(rows), inputRow
}

// inputLine renders the prompt and input within one row, scrolling the text
// sideways so the cursor stays visible, and answers the cursor's column.
func (s *screen) inputLine(width int) (string, int) {
	room := width - displayWidth(s.prompt) - 1
	if room < 10 {
		room = 10
	}
	text := s.input
	if s.cursor > len(text) {
		s.cursor = len(text)
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	start := 0
	for displayWidth(string(text[start:s.cursor])) > room {
		start++
	}
	end := start
	for end < len(text) && displayWidth(string(text[start:end+1])) <= room {
		end++
	}
	shown := strings.ReplaceAll(string(text[start:end]), "\n", "↵")
	col := displayWidth(s.prompt) + displayWidth(string(text[start:s.cursor]))
	return s.prompt + shown, col
}

// Print writes lines above the live region, into the scrollback.
func (s *screen) Print(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erase()
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprint(s.w, l+"\x1b[K\r\n")
	}
	s.draw()
}

// update changes the live region under the lock and redraws it.
func (s *screen) update(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erase()
	f()
	s.draw()
}

// suspend clears the live region and stops drawing it (for a full-screen
// step such as the home list); resume draws it again.
func (s *screen) suspend() { s.update(func() { s.hidden = true }) }
func (s *screen) resume()  { s.update(func() { s.hidden = false }) }

// close removes the live region for good, leaving the scrollback as it is.
func (s *screen) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erase()
	s.hidden = true
}

// ---- line editing -------------------------------------------------------------

// edit applies one key to the input; it answers true when the key was an
// edit (and so is consumed), false for keys the caller acts on.
func (s *screen) edit(k keyEvent) bool {
	consumed := true
	s.update(func() {
		switch k.kind {
		case keyRune:
			s.input = append(s.input[:s.cursor], append([]rune{k.r}, s.input[s.cursor:]...)...)
			s.cursor++
		case keyBackspace:
			if s.cursor > 0 {
				s.input = append(s.input[:s.cursor-1], s.input[s.cursor:]...)
				s.cursor--
			}
		case keyDelete:
			if s.cursor < len(s.input) {
				s.input = append(s.input[:s.cursor], s.input[s.cursor+1:]...)
			}
		case keyLeft:
			if s.cursor > 0 {
				s.cursor--
			}
		case keyRight:
			if s.cursor < len(s.input) {
				s.cursor++
			}
		case keyHome:
			s.cursor = 0
		case keyEnd:
			s.cursor = len(s.input)
		case keyCtrlU:
			s.input, s.cursor = s.input[s.cursor:], 0
		case keyCtrlW:
			i := s.cursor
			for i > 0 && s.input[i-1] == ' ' {
				i--
			}
			for i > 0 && s.input[i-1] != ' ' {
				i--
			}
			s.input = append(s.input[:i], s.input[s.cursor:]...)
			s.cursor = i
		default:
			consumed = false
		}
	})
	return consumed
}

// take returns the input and clears it.
func (s *screen) take() string {
	var t string
	s.update(func() {
		t = string(s.input)
		s.input, s.cursor = nil, 0
	})
	return t
}

func (s *screen) inputEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.input) == 0
}

func (s *screen) setStatus(t string) { s.update(func() { s.status = t }) }
func (s *screen) setHint(t string)   { s.update(func() { s.hint = t }) }
func (s *screen) setBox(b []string)  { s.update(func() { s.box = b }) }
