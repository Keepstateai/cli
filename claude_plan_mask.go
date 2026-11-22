package main

// claude_plan_mask.go: the streaming filter between Claude Code's
// setup-token screen and the person's terminal. The child prints a long-lived
// sign-in token once; this filter lets every other byte through, replaces the
// token with a fixed mask and keeps the token in memory for the one request
// that stores it.
//
// It works on bytes as they arrive, so a token split across two reads is
// still hidden: bytes that could be the start of the token's prefix are held
// back until the next read says what they are. A token that the child's
// terminal wrapped onto a second line is followed across the break when the
// first line was full to the terminal's width. Colour codes inside a token are
// passed through and the token goes on; a cursor move, an erase or a break
// that is not a wrap ends the capture, and what follows it is hidden up to a
// space or a blank line, so no piece of a token shows.
//
// The filter fails closed. A token it cannot be sure it read whole (cut by a
// wrap or an erase, longer than claudePlanTokenMaxLen, shorter than a token,
// or a prefix that the output ended on) is hidden and never kept: Result says
// why, and the caller stores nothing. It hides what looks like a token as it
// passes; it cannot hide a token it does not recognise.

import (
	"bytes"
	"regexp"
)

const (
	// claudePlanTokenPrefix starts every token `claude setup-token` prints.
	claudePlanTokenPrefix = "sk-ant-oat"
	// claudePlanTokenMaxLen bounds the token this filter will keep. A longer
	// run is hidden whole and refused, never cut.
	claudePlanTokenMaxLen = 512
	// claudePlanTailSuspect: this many token characters hidden after a break
	// that ended the capture make the capture suspect. A word of the text
	// after the token is shorter.
	claudePlanTailSuspect = 12
	// claudePlanMask is what the person sees where the token was.
	claudePlanMask = "[token hidden]"
)

// claudePlanTokenRE is the whole token, as the service checks it.
var claudePlanTokenRE = regexp.MustCompile(`^sk-ant-oat[0-9]*-[A-Za-z0-9_\-]{16,}$`)

func isTokenByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

type maskPhase int

const (
	maskScan    maskPhase = iota // passing bytes through, holding a possible prefix
	maskToken                    // inside the token's first run: hiding and capturing
	maskTail                     // after a break or a colour code: hiding, not capturing
	maskEscape                   // inside an escape sequence while hiding
	maskEscCSI                   // inside a CSI sequence while hiding
	maskEscOSC                   // inside an OSC sequence while hiding
	maskEscOSC2                  // inside an OSC sequence, after an escape
)

// tokenMasker is not safe for concurrent use.
type tokenMasker struct {
	cols     int // the width of the terminal the child draws for
	phase    maskPhase
	resume   maskPhase // the phase an escape sequence returns to
	hold     []byte    // a possible prefix of the token's prefix, not yet shown
	tok      []byte    // the first complete run of the token, captured
	done     bool      // a token was captured; later ones are only hidden
	runLen   int       // hidden characters in the current run, against the cap
	col      int       // visible columns on the current line
	pending  []byte    // line breaks seen while hiding, shown if the token ended
	decided  bool      // the current break has been judged
	tailRun  int       // token characters hidden since the last break or code
	tailMax  int       // the longest such run after the capture ended
	started  bool      // a token's prefix was seen
	overflow bool      // the run was longer than the bound
	partial  bool      // the output ended on a prefix of the token's prefix
	breaks   int       // consecutive line breaks while hiding
	escBuf   []byte    // the escape sequence being passed through
}

func newTokenMasker(cols int) *tokenMasker {
	return &tokenMasker{cols: cols}
}

// Write takes the next bytes the child wrote and returns the bytes that are
// safe to show.
func (m *tokenMasker) Write(p []byte) []byte {
	var out []byte
	for _, b := range p {
		out = m.step(out, b)
	}
	return out
}

// Flush returns what is still held when the child's output ends: a held
// prefix that never became a token, and any break that followed a token.
func (m *tokenMasker) Flush() []byte {
	var out []byte
	if m.phase == maskScan {
		out = append(out, m.hold...)
	}
	out = append(out, m.pending...)
	if len(m.escBuf) > 0 {
		out = append(out, m.escBuf...)
	}
	if len(m.hold) >= 3 {
		m.partial = true
	}
	m.hold, m.pending, m.escBuf = nil, nil, nil
	m.phase = maskScan
	return out
}

// Result is the token, or nil with the reason it is not kept: "" when no
// token was seen, otherwise "cut", "overflow", "unreadable" or "partial".
// Call it after Flush.
func (m *tokenMasker) Result() ([]byte, string) {
	switch {
	case m.overflow:
		return nil, "overflow"
	case m.started && m.tailMax >= claudePlanTailSuspect:
		// the capture stopped at a break or a code and a long run of token
		// characters followed it: the token was probably cut by a wrap or an
		// erase this filter could not tell from the end of the token
		return nil, "cut"
	case m.started && !claudePlanTokenRE.Match(m.tok):
		return nil, "unreadable"
	case m.started:
		return append([]byte(nil), m.tok...), ""
	case m.partial:
		return nil, "partial"
	}
	return nil, ""
}

// Token returns a copy of the captured token, nil when none is kept.
func (m *tokenMasker) Token() []byte {
	t, _ := m.Result()
	return t
}

// Wipe overwrites the captured token and every buffer that may hold a piece.
func (m *tokenMasker) Wipe() {
	wipe(m.tok)
	wipe(m.hold)
	m.tok, m.hold = nil, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (m *tokenMasker) trackColumn(b byte) {
	switch {
	case b == '\n':
		m.col = 0
	case b == '\r':
		m.col = 0
	case b >= 0x20 && b < 0x7f:
		m.col++
	case b >= 0xc0: // the first byte of a multibyte character
		m.col++
	}
}

func (m *tokenMasker) lineFull() bool { return m.cols > 0 && m.col >= m.cols-1 }

func (m *tokenMasker) startRun(out []byte) []byte {
	m.runLen = 0
	m.decided = false
	m.breaks = 0
	m.col += len(claudePlanTokenPrefix) // the prefix was held, not counted
	m.tailRun = 0
	if !m.done {
		m.started = true
		m.tailMax = 0
		m.tok = append(m.tok[:0], claudePlanTokenPrefix...)
	}
	return append(out, claudePlanMask...)
}

func (m *tokenMasker) endRun(out []byte) []byte {
	if len(m.tok) > 0 {
		m.done = true
	}
	m.phase = maskScan
	m.runLen = 0
	out = append(out, m.pending...)
	m.pending = m.pending[:0]
	return out
}

func (m *tokenMasker) step(out []byte, b byte) []byte {
	switch m.phase {
	case maskScan:
		return m.scan(out, b)
	case maskToken, maskTail:
		return m.hiding(out, b)
	case maskEscape:
		m.escBuf = append(m.escBuf, b)
		switch b {
		case '[':
			m.phase = maskEscCSI
		case ']':
			m.phase = maskEscOSC
		default:
			return m.finishEscape(out)
		}
		return out
	case maskEscCSI:
		m.escBuf = append(m.escBuf, b)
		if b >= 0x40 && b <= 0x7e {
			return m.finishEscape(out)
		}
		return out
	case maskEscOSC:
		m.escBuf = append(m.escBuf, b)
		switch b {
		case 0x07:
			return m.finishEscape(out)
		case 0x1b:
			m.phase = maskEscOSC2
		}
		return out
	case maskEscOSC2:
		m.escBuf = append(m.escBuf, b)
		if b == '\\' {
			return m.finishEscape(out)
		}
		m.phase = maskEscOSC
		return out
	}
	return out
}

// finishEscape ends an escape sequence met while hiding. A colour code
// (SGR) in the middle of a token changes nothing: the token goes on. Any other
// sequence (a cursor move, an erase, a link) is a break in what the child drew,
// so the capture ends there and what follows is hidden, not kept.
func (m *tokenMasker) finishEscape(out []byte) []byte {
	sgr := len(m.escBuf) >= 3 && m.escBuf[1] == '[' && m.escBuf[len(m.escBuf)-1] == 'm'
	m.pending = append(m.pending, m.escBuf...)
	m.escBuf = m.escBuf[:0]
	m.phase = m.resume
	if !sgr {
		if m.phase == maskToken {
			m.phase = maskTail
		}
		m.tailRun = 0
	}
	return out
}

func (m *tokenMasker) scan(out []byte, b byte) []byte {
	// a byte that continues what is held is held too; one that does not
	// releases the held bytes and is looked at again
	cand := append(m.hold, b)
	if bytes.HasPrefix([]byte(claudePlanTokenPrefix), cand) {
		m.hold = cand
		if len(m.hold) == len(claudePlanTokenPrefix) {
			m.hold = m.hold[:0]
			m.phase = maskToken
			return m.startRun(out)
		}
		return out
	}
	if len(m.hold) > 0 {
		held := append([]byte(nil), m.hold...)
		m.hold = m.hold[:0]
		for _, h := range held {
			m.trackColumn(h)
		}
		out = append(out, held...)
		// the dropped bytes may hide a new start: 's' of "ssk-ant-oat"
		return m.scan(out, b)
	}
	m.trackColumn(b)
	return append(out, b)
}

func (m *tokenMasker) hiding(out []byte, b byte) []byte {
	switch {
	case b == 0x1b:
		m.resume = m.phase
		m.escBuf = append(m.escBuf[:0], b)
		m.phase = maskEscape
		return out
	case b == '\r' || b == '\n':
		if m.phase == maskToken && !m.decided {
			// the first byte of a break decides: a line full to the width means
			// the child's terminal wrapped the token, which goes on below; a
			// shorter line means the token ended there
			m.decided = true
			if !m.lineFull() {
				m.phase = maskTail
			}
		}
		m.tailRun = 0
		if b == '\n' {
			m.breaks++
		}
		m.pending = append(m.pending, b)
		m.col = 0
		if m.breaks >= 2 {
			return m.endRun(out)
		}
		return out
	case isTokenByte(b):
		m.breaks = 0
		m.decided = false
		m.col++
		m.runLen++
		// the token (or the word after it) goes on: the breaks and codes held
		// back are shown, and the characters are not
		out = append(out, m.pending...)
		m.pending = m.pending[:0]
		if m.phase == maskToken && !m.done {
			if len(m.tok) >= claudePlanTokenMaxLen {
				m.overflow = true
			} else {
				m.tok = append(m.tok, b)
			}
		} else if m.phase == maskTail && !m.done {
			m.tailRun++
			m.tailMax = max(m.tailMax, m.tailRun)
		}
		return out
	default:
		// any other byte ends the token; the breaks and codes held back are
		// shown before it
		out = m.endRun(out)
		m.trackColumn(b)
		return append(out, b)
	}
}
