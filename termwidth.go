// termwidth.go: display width for the 80x24 window (KS-094, F11 matrix).
// A line is fitted to the terminal by the columns it OCCUPIES, not by its
// rune count: a wide (East Asian or emoji) character takes two columns and
// a combining mark or zero-width character takes none, so Japanese text or
// an emoji can no longer push a fitted line past the edge and wrap it into
// the line below. No dependency: the ranges are the common wide and
// zero-width blocks.
package main

import (
	"os"
	"unicode"
)

func runeWidth(r rune) int {
	switch {
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0xFEFF:
		return 0 // zero-width space, joiners, word joiner, BOM
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0 // combining marks
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals .. symbols
		r >= 0x3041 && r <= 0x33FF, // kana .. CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // symbols and pictographs, emoticons
		r >= 0x1F900 && r <= 0x1F9FF, // supplemental symbols and pictographs
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B..
		return 2
	}
	return 1
}

// displayWidth is the number of terminal columns s occupies.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// fitWidth cuts s to at most cols columns, marking a cut with "…".
func fitWidth(s string, cols int) string {
	if displayWidth(s) <= cols {
		return s
	}
	w := 0
	out := []rune{}
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > cols-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}

// stdoutIsTerminal: output goes to a terminal (not a pipe, file or null).
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if dn, derr := os.Stat(os.DevNull); derr == nil && os.SameFile(fi, dn) {
		return false
	}
	return true
}
