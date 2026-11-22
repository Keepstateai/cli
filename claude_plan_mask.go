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
// first line was full to the terminal's width, and everything after a token
// is hidden up to a space, a blank line or claudePlanTokenMaxLen characters,
// whichever comes first, so a break or a colour code inside a token can show
// at worst a harmless word that follows it, never a piece of the token.

import (
	"bytes"
	"regexp"
)

const (
	// claudePlanTokenPrefix starts every token `claude setup-token` prints.
	claudePlanTokenPrefix = "sk-ant-oat"
	// claudePlanTokenMaxLen caps how many characters one masked run can take.
	claudePlanTokenMaxLen = 200
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
	cols    int // the width of the terminal the child draws for
	phase   maskPhase
	resume  maskPhase // the phase an escape sequence returns to
	hold    []byte    // a possible prefix of the token's prefix, not yet shown
	tok     []byte    // the first complete run of the token, captured
	done    bool      // a token was captured; later ones are only hidden
	runLen  int       // hidden characters in the current run, against the cap
	col     int       // visible columns on the current line
	pending []byte    // line breaks seen while hiding, shown if the token ended
	decided bool      // the current break has been judged
	breaks  int       // consecutive line breaks while hiding
	escBuf  []byte    // the escape sequence being passed through
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
	m.hold, m.pending, m.escBuf = nil, nil, nil
	m.phase = maskScan
	return out
}

// Token returns a copy of the captured token, or nil when none was seen or
// what was seen is not a whole token.
func (m *tokenMasker) Token() []byte {
	if !claudePlanTokenRE.Match(m.tok) {
		return nil
	}
	return append([]byte(nil), m.tok...)
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
	if !m.done {
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

// finishEscape ends an escape sequence met while hiding. The sequence is
// shown if the token ended, and hiding goes on as it was: a colour change in
// the middle of a token does not end its capture.
func (m *tokenMasker) finishEscape(out []byte) []byte {
	m.pending = append(m.pending, m.escBuf...)
	m.escBuf = m.escBuf[:0]
	m.phase = m.resume
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
			m.tok = append(m.tok, b)
		}
		if m.runLen >= claudePlanTokenMaxLen {
			return m.endRun(out)
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
