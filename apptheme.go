package main

// apptheme.go: the app's look. Colour is used for meaning and hierarchy, the
// way a well-made terminal app uses it: one accent (KeepState's claret,
// lifted so it reads on a dark or a light terminal), grey for everything
// secondary, and green, red, amber and a permission colour for states. Text
// is never carried by colour alone: every word is the same with colour off.
//
// The theme is OFF unless the app turns it on at an interactive terminal,
// and stays off for NO_COLOR, TERM=dumb, --plain and KS_COLOR=0, so every
// command's output, every pipe and every test reads exactly as before.

import (
	"fmt"
	"os"
	"strings"
)

type colorDepth int

const (
	colorOff colorDepth = iota
	color16
	color256
	colorTrue
)

type rgb struct {
	r, g, b uint8
	c256    int
	c16     int // the SGR code at 16 colours
}

type appTheme struct {
	depth colorDepth
	light bool
}

var th appTheme

// palette: dark-terminal values; light terminals swap the few that need it.
var (
	palAccent = rgb{215, 95, 120, 168, 35}  // claret, lifted
	palDim    = rgb{138, 138, 138, 245, 90} // secondary text
	palSubtle = rgb{88, 88, 88, 240, 90}    // borders and rules
	palOK     = rgb{78, 186, 101, 71, 32}
	palBad    = rgb{255, 107, 128, 204, 31}
	palWarn   = rgb{229, 181, 103, 179, 33}
	palInfo   = rgb{177, 185, 249, 147, 94} // permission prompts
	lightInfo = rgb{87, 105, 247, 63, 34}
	lightSub  = rgb{170, 170, 170, 248, 37}
)

// enableAppTheme turns colour on for the app when the terminal can show it
// and nobody asked for none.
func enableAppTheme() {
	th = appTheme{}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("KS_COLOR") == "0" || out.plain || !stdoutIsTerminal() {
		return
	}
	term := os.Getenv("TERM")
	if term == "dumb" {
		return
	}
	switch {
	case os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit":
		th.depth = colorTrue
	case strings.Contains(term, "256color"):
		th.depth = color256
	default:
		th.depth = color16
	}
	// COLORFGBG="15;0" is light text on dark; "0;15" dark text on light
	if v := os.Getenv("KS_THEME"); v != "" {
		th.light = v == "light"
	} else if f := os.Getenv("COLORFGBG"); f != "" {
		parts := strings.Split(f, ";")
		bg := parts[len(parts)-1]
		th.light = bg == "7" || bg == "15"
	}
}

func (t appTheme) on() bool { return t.depth != colorOff }

func (t appTheme) sgr(c rgb) string {
	switch t.depth {
	case colorTrue:
		return fmt.Sprintf("38;2;%d;%d;%d", c.r, c.g, c.b)
	case color256:
		return fmt.Sprintf("38;5;%d", c.c256)
	}
	return fmt.Sprint(c.c16)
}

func (t appTheme) paint(s string, attrs ...string) string {
	if !t.on() || s == "" {
		return s
	}
	return "\x1b[" + strings.Join(attrs, ";") + "m" + s + "\x1b[0m"
}

func (t appTheme) color(c rgb, s string) string { return t.paint(s, t.sgr(c)) }

func (t appTheme) accent(s string) string     { return t.color(palAccent, s) }
func (t appTheme) accentBold(s string) string { return t.paint(s, "1", t.sgr(palAccent)) }
func (t appTheme) dim(s string) string        { return t.color(palDim, s) }
func (t appTheme) ok(s string) string         { return t.color(palOK, s) }
func (t appTheme) bad(s string) string        { return t.color(palBad, s) }
func (t appTheme) warn(s string) string       { return t.color(palWarn, s) }
func (t appTheme) bold(s string) string       { return t.paint(s, "1") }

func (t appTheme) subtle(s string) string {
	if t.light {
		return t.color(lightSub, s)
	}
	return t.color(palSubtle, s)
}

func (t appTheme) info(s string) string {
	if t.light {
		return t.color(lightInfo, s)
	}
	return t.color(palInfo, s)
}

func (t appTheme) infoBold(s string) string {
	if t.light {
		return t.paint(s, "1", t.sgr(lightInfo))
	}
	return t.paint(s, "1", t.sgr(palInfo))
}

// keyHints renders "key what · key what": the key plain, what it does grey.
func (t appTheme) keyHints(pairs ...[2]string) string {
	var parts []string
	for _, p := range pairs {
		parts = append(parts, p[0]+" "+t.dim(p[1]))
	}
	return strings.Join(parts, t.dim(" · "))
}

// activityColor colours an agent's activity by what it asks of a person.
func (t appTheme) activityColor(activity, s string) string {
	switch {
	case needsYou(activity):
		return t.warn(s)
	case activity == "working" || activity == "starting" || activity == "running":
		return t.accent(s)
	case activity == "ready" || activity == "idle":
		return t.ok(s)
	}
	return t.dim(s)
}

// spinFrames is the working indicator, cycled while an agent works.
var spinFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

// roundedBox draws lines inside a rounded border of the given width, the
// border in the given colour; a title sits in the top edge.
func roundedBox(lines []string, width int, title string, border func(string) string) []string {
	if width < 12 {
		width = 12
	}
	inner := width - 4
	top := "╭─"
	if title != "" {
		top += " " + title + " "
	}
	fill := width - 1 - displayWidth(top)
	if fill < 0 {
		fill = 0
	}
	rows := []string{border(top[:len("╭─")]) + strings.TrimPrefix(top, "╭─") + border(strings.Repeat("─", fill)+"╮")}
	for _, l := range lines {
		l = fitWidth(l, inner)
		rows = append(rows, border("│")+" "+l+strings.Repeat(" ", max(0, inner-displayWidth(l)))+" "+border("│"))
	}
	rows = append(rows, border("╰"+strings.Repeat("─", width-2)+"╯"))
	return rows
}
