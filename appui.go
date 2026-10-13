package main

// appui.go: the app's message system. Every screen and line the app shows
// is built from these few parts, so that the same kind of thing always looks
// the same (docs/app-design.md is the spec):
//
//   - a card: a titled, rounded box whose border colour says what kind of
//     screen it is, a one-sentence headline that states the state, aligned
//     label/value facts, wrapped text, and the keys that act on it;
//   - a notice: one line in the conversation, an icon and a sentence
//     ("✓ Resumed api (5335fa).");
//   - an error: "✗ Couldn't <do X>.", then why, then what to do;
//   - key hints: "key what · key what", dropping the least important pairs
//     rather than cutting one off at the edge.
//
// Prose is WRAPPED at word boundaries and never cut; only a table cell is
// cut, with an ellipsis. Colour and bold add emphasis and never carry a
// meaning alone: with colour off every word is the same.

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// ---- wrapping ------------------------------------------------------------------

// wrapText wraps s at word boundaries to lines of at most width columns, as
// the terminal counts them (displayWidth: styles take none, wide characters
// two). A style open at a break is closed at the end of the line and opened
// again at the start of the next, so a wrapped coloured sentence stays
// coloured and nothing bleeds. A word longer than a whole line is broken.
// A newline in s starts a new line.
func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, wrapPara(para, width)...)
	}
	return out
}

func wrapPara(p string, width int) []string {
	words := strings.Fields(p)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	style := "" // the style in force at this point of the text
	flush := func() {
		l := cur.String()
		if style != "" {
			l += "\x1b[0m"
		}
		lines = append(lines, l)
		cur.Reset()
		curW = 0
		cur.WriteString(style)
	}
	for _, w := range words {
		ww := displayWidth(w)
		if curW > 0 && curW+1+ww > width {
			flush()
		}
		if curW > 0 {
			cur.WriteString(" ")
			curW++
		}
		if ww <= width-curW {
			cur.WriteString(w)
			curW += ww
			style = styleAfter(style, w)
			continue
		}
		// a word longer than a line: broken where the line ends
		for i := 0; i < len(w); {
			if n := escLen(w[i:]); n > 0 {
				cur.WriteString(w[i : i+n])
				style = styleAfter(style, w[i:i+n])
				i += n
				continue
			}
			r, size := utf8.DecodeRuneInString(w[i:])
			rw := runeWidth(r)
			if curW+rw > width {
				flush()
			}
			cur.WriteRune(r)
			curW += rw
			i += size
		}
	}
	lines = append(lines, cur.String())
	return lines
}

// styleAfter is the style in force after s, given the one before it.
func styleAfter(style, s string) string {
	for i := 0; i < len(s); {
		n := escLen(s[i:])
		if n == 0 {
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
			continue
		}
		seq := s[i : i+n]
		if strings.HasSuffix(seq, "m") {
			if seq == "\x1b[0m" || seq == "\x1b[m" {
				style = ""
			} else {
				style += seq
			}
		}
		i += n
	}
	return style
}

// wrapHang wraps s to width with the first line after first and every
// following line after rest (a hanging indent), so a wrapped line lines up
// under the text it continues.
func wrapHang(first, rest, s string, width int) []string {
	hang := max(displayWidth(first), displayWidth(rest))
	lines := wrapText(s, width-hang)
	for i := range lines {
		if i == 0 {
			lines[i] = first + strings.Repeat(" ", hang-displayWidth(first)) + lines[i]
		} else {
			lines[i] = rest + strings.Repeat(" ", hang-displayWidth(rest)) + lines[i]
		}
	}
	return lines
}

// sentence is a fragment the service or another part of the client wrote,
// made to read as a sentence here: a capital first letter and a full stop.
// The words are not changed.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = capFirst(s)
	switch {
	case strings.HasSuffix(s, "."), strings.HasSuffix(s, "!"), strings.HasSuffix(s, "?"), strings.HasSuffix(s, "…"):
		return s
	case strings.HasSuffix(s, ";") || strings.HasSuffix(s, ",") || strings.HasSuffix(s, ":"):
		return s[:len(s)-1] + "."
	}
	return s + "."
}

func capFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// cell cuts s to a table cell of w columns, marking a cut with "…", and pads
// it to exactly w. Only cells are cut; prose is wrapped.
func cell(s string, w int) string {
	s = fitWidth(s, w)
	return s + strings.Repeat(" ", max(0, w-displayWidth(s)))
}

// ---- cards ---------------------------------------------------------------------

// cardMax is the widest a card is drawn: wider than this, lines of prose get
// too long to read comfortably.
const cardMax = 96

func cardWidth(width int) int { return max(20, min(width, cardMax)) }

type cardFact struct{ label, value string }

// card is one screen's identity and content. Every part is optional except
// the title.
type card struct {
	title   string      // in the top border: what screen this is
	tone    tone        // the border's meaning
	icon    string      // before the headline
	head    string      // one sentence: the state
	lines   []string    // short sentences under the headline
	facts   []cardFact  // aligned label / value rows
	rows    []string    // a list, already laid out (each fitted by the caller)
	err     string      // a problem with the last thing tried, in the failure colour
	busy    string      // something in progress, with the working indicator
	actions [][2]string // the keys that act on this screen
}

// render draws the card to fit width columns.
func (c card) render(width int) []string {
	w := cardWidth(width)
	inner := w - 4
	var body []string
	gap := func() {
		if len(body) > 0 && body[len(body)-1] != "" {
			body = append(body, "")
		}
	}
	if c.head != "" {
		icon := c.icon
		if icon == "" {
			icon = iconState
		}
		body = append(body, wrapHang(th.toned(c.tone, icon)+" ", "  ", th.bold(c.head), inner)...)
	}
	for _, l := range c.lines {
		body = append(body, wrapHang("  ", "  ", l, inner)...)
	}
	if len(c.facts) > 0 {
		gap()
		lw := 0
		for _, f := range c.facts {
			lw = max(lw, displayWidth(f.label))
		}
		for _, f := range c.facts {
			label := "  " + th.dim(f.label) + strings.Repeat(" ", lw-displayWidth(f.label)+3)
			body = append(body, wrapHang(label, strings.Repeat(" ", lw+5), f.value, inner)...)
		}
	}
	if len(c.rows) > 0 {
		gap()
		body = append(body, c.rows...)
	}
	if c.err != "" {
		gap()
		body = append(body, wrapHang("  "+th.bad(iconFailed)+" ", "    ", th.bad(c.err), inner)...)
	}
	if c.busy != "" {
		gap()
		body = append(body, wrapHang("  "+th.accent(iconState)+" ", "    ", c.busy, inner)...)
	}
	if len(c.actions) > 0 {
		gap()
		body = append(body, "  "+keyHintsFit(inner-2, c.actions...))
	}
	return roundedBox(body, w, th.tonedBold(c.tone, c.title), func(s string) string { return th.toned(c.tone, s) })
}

// roundedBox draws lines inside a rounded border of the given width, the
// border in the given colour; a title sits in the top edge. Lines are laid
// out to fit by the caller (a card wraps them); one that does not is cut
// here as a last resort rather than breaking the border.
func roundedBox(lines []string, width int, title string, border func(string) string) []string {
	if width < 12 {
		width = 12
	}
	inner := width - 4
	top := border("╭─")
	used := 2
	if title != "" {
		title = fitWidth(title, width-6)
		top += " " + title + " "
		used += displayWidth(title) + 2
	}
	top += border(strings.Repeat("─", max(0, width-1-used)) + "╮")
	rows := []string{top}
	for _, l := range lines {
		l = fitWidth(l, inner)
		rows = append(rows, border("│")+" "+l+strings.Repeat(" ", max(0, inner-displayWidth(l)))+" "+border("│"))
	}
	rows = append(rows, border("╰"+strings.Repeat("─", width-2)+"╯"))
	return rows
}

// ---- hints, notices and errors -------------------------------------------------

// keyHints renders "key what · key what": the key plain, what it does grey.
func (t appTheme) keyHints(pairs ...[2]string) string {
	var parts []string
	for _, p := range pairs {
		parts = append(parts, p[0]+" "+t.dim(p[1]))
	}
	return strings.Join(parts, t.dim(" · "))
}

// keyHintsFit is keyHints within width columns: the pairs are given most
// important first, and the ones that do not fit are left off whole.
func keyHintsFit(width int, pairs ...[2]string) string {
	for n := len(pairs); n > 0; n-- {
		if s := th.keyHints(pairs[:n]...); displayWidth(s) <= width {
			return s
		}
	}
	if len(pairs) == 0 {
		return ""
	}
	return fitWidth(th.keyHints(pairs[0]), width)
}

// joinFit joins status parts with " · " within width columns, leaving off
// whole parts from the end (the least important) rather than cutting one.
func joinFit(parts []string, width int) string {
	sep := th.dim(" · ")
	for n := len(parts); n > 0; n-- {
		if s := strings.Join(parts[:n], sep); displayWidth(s) <= width {
			return s
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fitWidth(parts[0], width)
}

// notice is one line in the conversation: an icon and a sentence, indented
// by two and wrapped under itself.
func notice(icon string, tn tone, text string, width int) string {
	return strings.Join(wrapHang("  "+th.toned(tn, icon)+" ", "    ", text, min(width, cardMax)), "\n")
}

// dimNotice is a notice whose words are secondary.
func dimNotice(icon string, tn tone, text string, width int) string {
	return notice(icon, tn, th.dim(text), width)
}

// errorBlock is an error as the app shows it: "✗ Couldn't <what>." in the
// failure colour, then why (the error's own message, as a sentence), then
// what to do (its next action), each on its own wrapped line.
func errorBlock(what string, err error, width int) string {
	ce := classify(err)
	return errorLines(what, ce.Message, ce.NextAction, width)
}

func errorLines(what, why, next string, width int) string {
	w := min(width, cardMax)
	var lines []string
	head := "Couldn't " + strings.TrimSuffix(what, ".") + "."
	if what == "" {
		head, why = sentence(why), ""
	}
	lines = append(lines, wrapHang("  "+th.bad(iconFailed)+" ", "    ", th.bad(head), w)...)
	if why != "" {
		lines = append(lines, wrapHang("    ", "    ", sentence(why), w)...)
	}
	if next != "" {
		lines = append(lines, wrapHang("    "+th.dim("Next:")+" ", "          ", next, w)...)
	}
	return strings.Join(lines, "\n")
}

// ---- words for states ------------------------------------------------------------

// The states in a person's words. A state this client does not know is shown
// as the service sent it, "Unknown (<word>)", never as the nearest known one.
var activityWords = map[string]string{
	"starting": "Starting", "ready": "Ready", "working": "Working",
	"waiting_provider": "Waiting for the model", "running_tool": "Running a tool",
	"waiting_approval": "Needs your permission", "waiting_advice": "Waiting for advice",
	"paused": "Paused", "stopped": "Stopped", "failed": "Failed", "unknown": "Unknown",
	"removed": "Removed", "recovery_required": "Needs attention", "unavailable": "Unavailable",
}

var sessionWords = map[string]string{
	"provisioning": "Starting up", "running": "Running", "checkpointing": "Saving",
	"stopping": "Stopping", "parked": "Paused", "resuming": "Resuming", "failed": "Failed",
	"recovery_required": "Needs attention", "deleting": "Being deleted", "deleted": "Deleted",
	"dead": "Ended", "unavailable": "Unavailable",
}

var taskWords = map[string]string{
	"queued": "Queued", "held": "Held", "claimed": "Starting", "running": "Running",
	"cancelling": "Stopping", "cancelled": "Cancelled", "succeeded": "Finished",
	"failed": "Failed", "reconciliation_required": "Needs attention", "unavailable": "Unavailable",
}

func wordFor(words map[string]string, s string) string {
	if s == "" {
		return "Unavailable"
	}
	if w, ok := words[s]; ok {
		return w
	}
	return "Unknown (" + sanitize(s) + ")"
}

func activityWord(a string) string { return wordFor(activityWords, a) }
func taskWord(s string) string     { return wordFor(taskWords, s) }

// sessionWord is a session's state in a person's words; a funds park reads
// "Out of credit", whatever the fleet's own word is.
func sessionWord(state, reason string) string {
	if fundsParked(state, reason) {
		return "Out of credit"
	}
	return wordFor(sessionWords, state)
}

// activityColor colours an agent's activity by what it asks of a person.
func (t appTheme) activityColor(activity, s string) string {
	switch {
	case needsYou(activity):
		return t.warn(s)
	case activity == "working" || activity == "starting" || activity == "running_tool" || activity == "waiting_provider":
		return t.accent(s)
	case activity == "ready":
		return t.ok(s)
	}
	return t.dim(s)
}

// spinFrames is the working indicator, cycled while something is in progress.
var spinFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

// plural is "1 thing" or "n things".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func appTerminalHeight() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h >= 8 {
		return h
	}
	return 24
}
