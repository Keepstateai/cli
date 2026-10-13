package main

// apptheme.go: the app's look. Colour is used for meaning and hierarchy, the
// way a well-made terminal app uses it: one accent (KeepState's claret,
// lifted so it reads on a dark or a light terminal), grey for everything
// secondary, and green, red, amber and a permission colour for states. Text
// is never carried by colour alone: every word is the same with colour off.
//
// The components built from it (cards, notices, key hints, wrapping) are in
// appui.go; docs/app-design.md is the design spec.
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

// palette: each colour has a value for a dark terminal and one for a light
// terminal, chosen so that text in it stays readable on that background.
type swatch struct{ dark, light rgb }

var (
	palAccent = swatch{rgb{215, 95, 120, 168, 35}, rgb{176, 46, 84, 125, 35}}    // claret
	palDim    = swatch{rgb{138, 138, 138, 245, 90}, rgb{102, 102, 102, 242, 90}} // secondary text
	palSubtle = swatch{rgb{88, 88, 88, 240, 90}, rgb{176, 176, 176, 249, 37}}    // borders and rules
	palOK     = swatch{rgb{78, 186, 101, 71, 32}, rgb{26, 127, 55, 28, 32}}
	palBad    = swatch{rgb{255, 107, 128, 204, 31}, rgb{200, 30, 60, 160, 31}}
	palWarn   = swatch{rgb{229, 181, 103, 179, 33}, rgb{168, 98, 0, 130, 33}}
	palInfo   = swatch{rgb{177, 185, 249, 147, 94}, rgb{70, 88, 230, 62, 34}} // permission prompts
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

func (t appTheme) color(c swatch, s string) string { return t.paint(s, t.sgr(t.pick(c))) }

func (t appTheme) pick(c swatch) rgb {
	if t.light {
		return c.light
	}
	return c.dark
}

func (t appTheme) accent(s string) string     { return t.color(palAccent, s) }
func (t appTheme) accentBold(s string) string { return t.paint(s, "1", t.sgr(t.pick(palAccent))) }
func (t appTheme) dim(s string) string        { return t.color(palDim, s) }
func (t appTheme) ok(s string) string         { return t.color(palOK, s) }
func (t appTheme) bad(s string) string        { return t.color(palBad, s) }
func (t appTheme) warn(s string) string       { return t.color(palWarn, s) }
func (t appTheme) bold(s string) string       { return t.paint(s, "1") }
func (t appTheme) subtle(s string) string     { return t.color(palSubtle, s) }
func (t appTheme) info(s string) string       { return t.color(palInfo, s) }
func (t appTheme) infoBold(s string) string   { return t.paint(s, "1", t.sgr(t.pick(palInfo))) }

// tone is what a colour means. Each screen and line has one, and the words
// carry the same meaning with colour off.
type tone int

const (
	toneNeutral tone = iota // the accent: an ordinary screen
	toneWarn                // paused, or waiting on you
	toneBad                 // something failed
	toneInfo                // a permission request
	toneOK                  // done
	toneDim                 // secondary
)

func (t appTheme) toned(tn tone, s string) string {
	switch tn {
	case toneWarn:
		return t.warn(s)
	case toneBad:
		return t.bad(s)
	case toneInfo:
		return t.info(s)
	case toneOK:
		return t.ok(s)
	case toneDim:
		return t.dim(s)
	}
	return t.accent(s)
}

func (t appTheme) tonedBold(tn tone, s string) string {
	c := palAccent
	switch tn {
	case toneWarn:
		c = palWarn
	case toneBad:
		c = palBad
	case toneInfo:
		c = palInfo
	case toneOK:
		c = palOK
	case toneDim:
		c = palDim
	}
	return t.paint(s, "1", t.sgr(t.pick(c)))
}

// The icons. Each has one meaning everywhere in the app, and is never the
// only carrier of it: the words beside it say the same.
const (
	iconDone     = "✓" // done, allowed
	iconFailed   = "✗" // failed, could not
	iconDenied   = "⊘" // denied by a person
	iconPaused   = "⏸" // paused (saved and stopped)
	iconNeedsYou = "⚑" // waiting on you
	iconState    = "●" // a state, or something in progress
	iconNoAgent  = "○" // a session with no agents
	iconResult   = "↳" // a result
	iconWarn     = "!" // a warning
	iconSelected = "❯" // the selected row
	iconApp      = "✻" // KeepState
)
