package main

// listui.go: the one shape of a list screen, on the shared kit (ui.go).
//
//	Title  dim count            a title, and how many
//	                            (lead: one or two wrapped sentences)
//	  Header  Header            a table that takes the terminal's width,
//	  row     row               stacked and wrapped when it does not fit
//
//	  dim notes                 wrapped notes
//	Next: ks ...                one command
//
// An empty list is not an empty table: it is one plain sentence and, when
// there is something to do about it, the Next command.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// listGroup is rows under their own sub-heading, in the screen's columns.
type listGroup struct {
	Heading string // "Ended"; empty for the first, unheaded group
	Lead    []string
	Facts   [][2]string
	Rows    [][]string
	Empty   string // said under the heading when the group has no rows
}

type listScreen struct {
	Title  string
	Detail string   // dim, beside the title: "3 keys"
	Lead   []string // sentences under the title
	Cols   []Col
	Rows   [][]string
	Groups []listGroup // instead of Rows, when the rows fall in groups
	Empty  string      // said, with the Lead, when there are no rows
	Notes  []string    // dim, under the table
	Next   []string
}

// Heading is a screen's title with no icon, and a dim detail beside it.
func (u *ui) Heading(title string, detail ...string) { u.Title("", toneNeutral, title, detail...) }

// Para is wrapped sentences indented by two, one paragraph each.
func (u *ui) Para(texts ...string) {
	var out []string
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "- ") { // a bullet hangs under its text
			for i, l := range wrapProse(t[2:], u.proseWidth()-2) {
				if i == 0 {
					out = append(out, "  - "+l)
				} else {
					out = append(out, "    "+l)
				}
			}
			continue
		}
		for _, l := range wrapProse(t, u.proseWidth()) {
			out = append(out, "  "+l)
		}
	}
	u.lines(out...)
}

// Notes is dim wrapped sentences under a table.
func (u *ui) Notes(texts ...string) {
	var out []string
	for _, t := range texts {
		for _, l := range wrapProse(strings.TrimSpace(t), u.proseWidth()) {
			out = append(out, "  "+u.t.dim(l))
		}
	}
	u.lines(out...)
}

// wrapProse is wrapText that never breaks a word: a path or an address
// longer than the line goes on a line of its own, whole, so a copy of it is
// the thing itself (the terminal soft-wraps it).
func wrapProse(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		var cur string
		curW := 0
		flush := func() {
			if cur != "" {
				out = append(out, cur)
			}
			cur, curW = "", 0
		}
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		for _, w := range words {
			ww := displayWidth(w)
			switch {
			case ww > width:
				flush()
				out = append(out, w)
			case curW == 0:
				cur, curW = w, ww
			case curW+1+ww > width:
				flush()
				cur, curW = w, ww
			default:
				cur += " " + w
				curW += 1 + ww
			}
		}
		flush()
	}
	return out
}

// List draws a list screen. A screen with no rows says its Empty sentence
// and nothing else of a table.
func (u *ui) List(s listScreen) {
	if s.Title != "" {
		u.Heading(s.Title, s.Detail)
	}
	if len(s.Lead) > 0 {
		u.Para(s.Lead...)
	}
	groups := s.Groups
	if len(groups) == 0 && len(s.Rows) > 0 {
		groups = []listGroup{{Rows: s.Rows}}
	}
	have := false
	for _, g := range groups {
		if len(g.Rows) == 0 && g.Empty == "" && len(g.Lead) == 0 && len(g.Facts) == 0 {
			continue
		}
		have = true
		u.Blank()
		if g.Heading != "" {
			u.lines("  " + u.t.bold(g.Heading))
		}
		if len(g.Lead) > 0 {
			u.Para(g.Lead...)
		}
		u.Facts(g.Facts...)
		if len(g.Rows) > 0 {
			u.Table(s.Cols, g.Rows)
		} else if g.Empty != "" {
			u.Para(g.Empty)
		}
	}
	if !have && s.Empty != "" {
		u.Blank()
		u.Para(s.Empty)
	}
	if len(s.Notes) > 0 {
		u.Blank()
		u.Notes(s.Notes...)
	}
	if len(s.Next) > 0 {
		u.Blank()
		u.Next(s.Next...)
	}
}

// listCount is "3 keys" / "1 key" / "no keys".
func listCount(n int, one, many string) string {
	if n == 0 {
		return "no " + many
	}
	return nounCount(n, one, many)
}

// screenLines draws a screen to lines, for what redraws or joins them (a
// watch, a test). It has the terminal's width and theme.
func screenLines(draw func(u *ui)) []string {
	o, _ := cmdUI()
	var b bytes.Buffer
	u := newUI(&b, o.t, o.cols, o.tty)
	u.pipe = o.pipe
	draw(u)
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// Lines prints lines a verb built as text (a card of "  label   value" rows,
// a sentence, a bullet): one that fits is printed as it is, with its own
// spacing; one that does not is wrapped on word boundaries and hangs under
// its value (or under its text), never in the middle of a word. Into a pipe
// nothing is wrapped.
func (u *ui) Lines(ls ...string) {
	var out []string
	for _, l := range ls {
		out = append(out, u.fitLine(l)...)
	}
	u.lines(out...)
}

// fitLine is one line at the terminal's width.
func (u *ui) fitLine(l string) []string {
	if u.pipe || displayWidth(l) <= u.width || unwrappable(l) {
		return []string{l}
	}
	trimmed := strings.TrimLeft(l, " ")
	lead := len(l) - len(trimmed)
	// "  label     value": the value starts after the first run of two or more
	// spaces that comes soon after the label
	hang := lead
	if i := strings.Index(trimmed, "  "); i > 0 && lead+i < 24 {
		j := i
		for j < len(trimmed) && trimmed[j] == ' ' {
			j++
		}
		if j < len(trimmed) {
			first := l[:lead+j]
			rest := strings.Repeat(" ", displayWidth(first))
			if widestWord(trimmed[j:]) > u.width-displayWidth(first) {
				return []string{l} // a word that cannot fit beside its label is never broken
			}
			return wrapHang(first, rest, trimmed[j:], u.width)
		}
	}
	rest := strings.Repeat(" ", hang)
	switch {
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
		rest += "  "
	case len(trimmed) > 6 && strings.EqualFold(trimmed[:6], "Next: "):
		rest += strings.Repeat(" ", 6)
	}
	if widestWord(trimmed) > u.width-displayWidth(rest) {
		return []string{l}
	}
	return wrapHang(l[:lead], rest, trimmed, u.width)
}

// widestWord is the widest word of s in columns.
func widestWord(s string) int {
	w := 0
	for _, f := range strings.Fields(s) {
		w = max(w, displayWidth(f))
	}
	return w
}

// printLines is Lines on standard output, for the verbs that build their
// text as lines.
func printLines(ls []string) {
	o, _ := cmdUI()
	o.Lines(ls...)
}

// uiOn is a ui that draws on w with the terminal's theme and width, for a
// helper that is handed a writer (a refusal's detail, a test).
func uiOn(w io.Writer) *ui {
	o, _ := cmdUI()
	if w == o.w {
		return o
	}
	u := newUI(w, o.t, o.cols, o.tty)
	u.pipe = o.pipe
	return u
}

// cardf prints one line of a card, wrapped at the terminal's width like any
// line (see Lines). A trailing newline in the format is not needed.
func cardf(format string, a ...any) {
	printLines([]string{strings.TrimRight(fmt.Sprintf(format, a...), "\n")})
}

// unwrappable: a line that is not prose. A command, an address or a long
// token is left to the terminal's own soft wrap, which a copy puts back
// together; a line that moves the cursor is not text at all.
func unwrappable(l string) bool {
	if strings.ContainsAny(l, "\r\a") {
		return true
	}
	for i := 0; i < len(l); i++ {
		if l[i] == 0x1b && i+1 < len(l) && l[i+1] != '[' {
			return true // an OSC or another escape
		}
		if l[i] == 0x1b {
			n := escLen(l[i:])
			if n == 0 || l[i+n-1] != 'm' {
				return true // a cursor move or an erase
			}
		}
	}
	if strings.Contains(l, "://") || strings.HasPrefix(strings.TrimLeft(l, " "), "ks ") || strings.Contains(l, "&&") {
		return true
	}
	// a line indented four or more is a shell line or a block, never prose
	if len(l)-len(strings.TrimLeft(l, " ")) >= 4 {
		return true
	}
	for _, w := range strings.Fields(l) {
		if displayWidth(w) >= 48 {
			return true
		}
	}
	// "... ks verb ...": a command inside a sentence
	for i := 0; i+4 < len(l); i++ {
		if l[i:i+3] == "ks " && (i == 0 || l[i-1] == ' ' || l[i-1] == '`' || l[i-1] == '(') && l[i+3] >= 'a' && l[i+3] <= 'z' {
			return true
		}
	}
	return false
}

// outf and outln are fmt.Printf and fmt.Println for a verb's human text. A
// complete line wider than the terminal is wrapped on word boundaries under
// its text (see Lines); into a pipe, and for anything that is not whole
// lines, they are fmt itself.
func outf(format string, a ...any) { outs(fmt.Sprintf(format, a...)) }
func outln(a ...any)               { outs(fmt.Sprintln(a...)) }

func outs(s string) {
	o, _ := cmdUI()
	if o.pipe || !strings.HasSuffix(s, "\n") {
		fmt.Print(s)
		return
	}
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		for _, f := range o.fitLine(l) {
			b.WriteString(f)
			b.WriteByte('\n')
		}
	}
	fmt.Print(b.String())
}

// proseWidth is the width a paragraph wraps at: the card's, less its indent;
// into a pipe, no width at all.
func (u *ui) proseWidth() int {
	if u.pipe {
		return 1 << 20
	}
	return u.width - 2
}

// padTo pads s with spaces on the right to n columns (never cuts it).
func padTo(s string, n int) string {
	if w := displayWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// outRaw and outRawln print verbatim content (what an agent said, a log, a
// consultation's words, a brief, a file): nothing in it is reflowed, because
// spaces in it can be code.
func outRaw(s string)   { fmt.Print(s) }
func outRawln(s string) { fmt.Println(s) }
