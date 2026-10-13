package main

// app.go: `ks` with no arguments, at a terminal, is an app you stay in: a
// home screen of your agents, and an agent's window with the conversation
// scrolling above, an input line, inline permission prompts and a status
// line below. Everything it does goes through the same machinery as the
// commands -- the control lease, fresh submission ids recorded before
// sending, the shown-then-decided approval rule, the cancel route for an
// interrupt -- so the app promises nothing the commands would not.
//
// How it looks is one system (appui.go, docs/app-design.md): a screen that
// is a STATE (your agents, a paused session, a session that needs attention,
// a permission request) is a titled card in the live region at the bottom
// while it is current, and goes away when it is not; the scrollback keeps
// what is worth keeping -- the conversation, outcomes, errors.
//
// The files: app.go (the app and its home screen), appnew.go (a new agent),
// appwindow.go (an agent's window), appstates.go (a session that is not
// running, the permission card, the status line), appconfirm.go (confirm
// cards for what changes or removes something), appui.go (the components),
// apptheme.go (colour), appterm.go (keys and the screen), appslash.go (every
// ks command as a / command), appaway.go (while you were away).
//
// Keys: Enter sends; Esc interrupts the instruction in flight; y / n / d
// answer a permission prompt when the input is empty; Ctrl-C clears the
// input, and on an empty input (twice) leaves the agent, which keeps
// working; /help lists the / commands.

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

// appSink, when set, receives every progress and emitted line, so nothing
// printed while the app runs tears its screen.
var appSink func(string)

// appFlash, when set (an agent's window is open), shows a short note under
// the input that goes away by itself, for what is not worth keeping.
var appFlash func(string)

// appWanted: bare ks opens the app only at an interactive terminal, and
// never for a script, a pipe, --json or --no-input.
func appWanted() bool {
	return stdinIsTerminal() && stdoutIsTerminal() && !out.json && !out.noInput
}

type homeRow struct {
	sess  inventoryRow
	agent *agentRow // nil: a plain session, which has no agents
}

type appAction int

const (
	appOpen appAction = iota
	appNew
	appQuit
)

func hostedApp(cr hostedCreds) {
	restore, err := rawTerminal()
	if err != nil {
		fail(&cliError{Code: exitFailed, Kind: "no_terminal", Message: "the app needs an interactive terminal (" + err.Error() + ")", NextAction: "ks --help"})
	}
	enableAppTheme()
	scr := newScreen(os.Stdout)
	appSink = func(l string) { scr.Print(appSystemLine(l)) }
	go scr.spinner()
	exit := func(code int) {
		scr.close()
		appSink = nil
		restore()
		os.Exit(code)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	go func() {
		for s := range sig {
			if s == syscall.SIGWINCH {
				scr.update(func() {})
				continue
			}
			exit(exitOK)
		}
	}()
	keys := make(chan keyEvent, 64)
	go readKeys(os.Stdin, keys)

	scr.Print(strings.Join(appWelcome(cr, appTerminalWidth()), "\n"))
	last := "" // the agent opened last: home selects it again
	for {
		row, act := appHome(cr, scr, keys, last)
		switch act {
		case appQuit:
			scr.Print(dimNotice(iconState, toneDim, "Left the app. Your agents keep working; run ks to come back.", appTerminalWidth()))
			exit(exitOK)
		case appNew:
			r, ok := appNewSession(cr, scr, keys)
			if !ok {
				continue
			}
			row = r
		}
		if row != nil {
			last = row.agent.ID
			if appAgent(cr, scr, keys, row.sess, *row.agent) {
				scr.Print(dimNotice(iconState, toneDim, "Left the app. Your agents keep working; run ks to come back.", appTerminalWidth()))
				exit(exitOK)
			}
		}
	}
}

// ---- home -------------------------------------------------------------------

func loadHome(cr hostedCreds) ([]homeRow, error) {
	inv, err := fetchInventory(cr, "", false)
	if err != nil {
		return nil, err
	}
	var rows []homeRow
	for _, s := range inv {
		if s.RecordID == "" {
			rows = append(rows, homeRow{sess: s})
			continue
		}
		agents, err := fetchAgents(cr, s.RecordID)
		if err != nil || len(agents) == 0 {
			rows = append(rows, homeRow{sess: s})
			continue
		}
		sort.SliceStable(agents, func(i, j int) bool { return agents[i].IsPrimary && !agents[j].IsPrimary })
		for i := range agents {
			a := agents[i]
			rows = append(rows, homeRow{sess: s, agent: &a})
		}
	}
	// agents that need you first, then the other agents, plain sessions last
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].rank() < rows[j].rank() })
	return rows, nil
}

// needsYou: an agent that is waiting on a person, or stopped needing one.
func needsYou(activity string) bool {
	switch activity {
	case "waiting_approval", "recovery_required", "failed":
		return true
	}
	return false
}

// sessionState is the row's session as far as anything says it is not
// running: the agent's own session_runtime first (it is present exactly when
// the session is not running), then the inventory's word. running is true
// only when nothing says otherwise.
func (r homeRow) sessionState() (state string, funds, running bool) {
	if r.agent != nil && r.agent.SessionRuntime != nil {
		rt := r.agent.SessionRuntime
		return rt.State, rt.funds(), rt.State == "running"
	}
	if rowFundsParked(r.sess) {
		return "parked", true, false
	}
	st := r.sess.RuntimeState
	return st, false, st == "running" || st == ""
}

// rowNeedsYou: the agent waits on a person, or its session is paused because
// the account ran out of credit (adding credit is for a person to do).
func (r homeRow) rowNeedsYou() bool {
	if r.agent == nil {
		return false
	}
	_, funds, running := r.sessionState()
	return funds || (running && needsYou(r.agent.Activity))
}

func (r homeRow) rank() int {
	switch {
	case r.rowNeedsYou():
		return 0
	case r.agent != nil:
		return 1
	}
	return 2
}

// sessLabel names a session as the list does: its name and its listed id.
func sessLabel(s inventoryRow) string {
	name := sanitize(s.Name)
	if name == "" || name == s.ShortID {
		return s.ShortID
	}
	return name + " · " + s.ShortID
}

// homeState is a row's state cell: an icon and words that carry the meaning
// without colour (a paused session reads Paused, never its agent's frozen
// last word), and the tone it is drawn in.
func homeState(r homeRow) (icon string, word string, tn tone) {
	state, funds, running := r.sessionState()
	switch {
	case r.agent == nil:
		return iconNoAgent, sessionWord(state, ""), toneDim
	case funds:
		return iconPaused, "Out of credit", toneWarn
	case !running && (state == "parked" || state == "stopping" || state == "checkpointing"):
		return iconPaused, sessionWord(state, ""), toneWarn
	case !running && (state == "failed" || state == "dead" || state == "recovery_required"):
		return iconFailed, "Session " + strings.ToLower(sessionWord(state, "")), toneBad
	case !running:
		return iconState, "Session " + strings.ToLower(sessionWord(state, "")), toneDim
	case r.agent.Activity == "failed":
		return iconFailed, activityWord(r.agent.Activity), toneBad
	case r.agent.Activity == "recovery_required": // it stopped mid-way: a failure that waits on you
		return iconNeedsYou, activityWord(r.agent.Activity), toneBad
	case needsYou(r.agent.Activity):
		return iconNeedsYou, activityWord(r.agent.Activity), toneWarn
	}
	a := r.agent.Activity
	switch {
	case a == "working" || a == "starting" || a == "running_tool" || a == "waiting_provider":
		return iconState, activityWord(a), toneNeutral
	case a == "ready":
		return iconState, activityWord(a), toneOK
	}
	return iconState, activityWord(a), toneDim
}

// homeColumns are the list's column widths for a card of inner width.
func homeColumns(inner int) (name, sess, state int) {
	if inner >= 88 {
		return 14, 26, 24
	}
	return 12, 20, 22
}

// homeLine is one row of the list: the selection mark, the state's icon, the
// agent's name, its session, its state in words and how old that report is.
func homeLine(r homeRow, selected bool, width int) string {
	inner := cardWidth(width) - 4
	nw, sw, stw := homeColumns(inner)
	mark := "  "
	if selected {
		mark = th.accent(iconSelected) + " "
	}
	icon, word, tn := homeState(r)
	if r.agent == nil {
		name := cell(sanitize(firstNonEmpty(r.sess.Name, r.sess.ShortID)), nw)
		sess := ""
		if r.sess.Name != "" && r.sess.Name != r.sess.ShortID {
			sess = r.sess.ShortID
		}
		if selected {
			name = th.accentBold(name)
		}
		return mark + th.dim(icon) + " " + name + "  " + th.dim(cell(sess, sw)) + "  " + th.dim(cell(word, stw))
	}
	name := cell(sanitize(r.agent.Name), nw)
	if selected {
		name = th.accentBold(name)
	} else {
		name = th.bold(name)
	}
	age := ""
	if t, err := time.Parse(time.RFC3339Nano, r.agent.ObservedAt); err == nil {
		age = shortDuration(max(0, time.Since(t))) + " ago"
	}
	line := mark + th.toned(tn, icon) + " " + name + "  " + th.dim(cell(sessLabel(r.sess), sw)) + "  " +
		th.toned(tn, cell(word, stw)) + "  " + th.dim(age)
	return fitWidth(line, inner)
}

// homeList lays the rows out in groups -- needs you, agents, sessions
// without agents -- and answers the list's lines and the line the selected
// row is on.
func homeList(rows []homeRow, sel, width int) ([]string, int) {
	var out []string
	selLine := 0
	group := -1
	for i, r := range rows {
		if g := r.rank(); g != group {
			if group >= 0 {
				out = append(out, "")
			}
			group = g
			switch g {
			case 0:
				out = append(out, th.tonedBold(toneWarn, "Needs you"))
			case 1:
				out = append(out, th.bold("Agents"))
			default:
				out = append(out, th.dim("Sessions without agents"))
			}
		}
		if i == sel {
			selLine = len(out)
		}
		out = append(out, homeLine(r, i == sel, width))
	}
	return out, selLine
}

// homeCard is the home screen: the list, kept within the terminal's height
// (a long list scrolls with the selection, saying how much is above and
// below), and the keys.
func homeCard(rows []homeRow, sel int, problem string, width, height int) card {
	c := card{title: "Your agents", tone: toneNeutral,
		actions: [][2]string{{"↑↓", "choose"}, {"Enter", "open"}, {"n", "new agent"}, {"r", "refresh"}, {"q", "quit"}}}
	agents := 0
	for _, r := range rows {
		if r.agent != nil {
			agents++
		}
	}
	if agents == 0 && problem == "" {
		c.head = "You have no agents yet."
		c.lines = []string{"Press n to start one. It runs in a session of its own and keeps working when you leave."}
	}
	list, selLine := homeList(rows, sel, width)
	room := max(4, height-9) // the card's border, actions and the status line
	if len(list) > room {
		start := max(0, min(selLine-room/2, len(list)-room))
		above, below := start, len(list)-start-room
		list = list[start : start+room]
		if above > 0 {
			list = append([]string{th.dim(fmt.Sprintf("  ↑ %d more", above))}, list[1:]...)
		}
		if below > 0 {
			list = append(list[:len(list)-1], th.dim(fmt.Sprintf("  ↓ %d more", below)))
		}
	}
	c.rows = list
	c.err = problem
	return c
}

// appWelcome is the app's first screen: what it is, and which service it
// talks to.
func appWelcome(cr hostedCreds, width int) []string {
	w := cardWidth(width)
	lines := []string{
		th.accent(iconApp) + " " + th.bold("Welcome to KeepState"),
		"  Your agents keep working when you leave.",
		"",
		"  " + th.dim("Service") + "   " + sanitize(cr.CTL),
	}
	return append(roundedBox(lines, w, "", th.accent), "")
}

func appHome(cr hostedCreds, scr *screen, keys <-chan keyEvent, last string) (*homeRow, appAction) {
	scr.update(func() {
		scr.box, scr.prompt, scr.input, scr.cursor, scr.hint = nil, "", nil, 0, ""
		scr.status, scr.spin = th.accent(iconState)+" Reading your agents…", true
	})
	rows, err := loadHome(cr)
	problem := ""
	if err != nil {
		problem = "Couldn't read your agents. " + sentence(errText(err)) + " Press r to try again."
	}
	sel := -1
	for i, r := range rows {
		if r.agent != nil && r.agent.ID == last {
			sel = i
		}
	}
	if sel < 0 { // the first agent, or the first row
		sel = 0
		for sel < len(rows) && rows[sel].agent == nil {
			sel++
		}
		if sel == len(rows) {
			sel = 0
		}
	}
	plain := false // the selected plain session's card is showing
	render := func() {
		width, height := appTerminalWidth(), appTerminalHeight()
		var box []string
		if plain && sel < len(rows) {
			box = plainSessionCard(rows[sel].sess).render(width)
		} else {
			box = homeCard(rows, sel, problem, width, height).render(width)
		}
		scr.update(func() {
			scr.box, scr.prompt, scr.input, scr.cursor, scr.hint = box, "", nil, 0, ""
			scr.status, scr.spin = "", false
		})
	}
	reload := func(spin bool) {
		if spin {
			scr.update(func() { scr.status, scr.spin = th.accent(iconState)+" Refreshing…", true })
		}
		fresh, err := loadHome(cr)
		if err != nil {
			if spin {
				problem = "Couldn't refresh your agents. " + sentence(errText(err)) + " Press r to try again."
			}
			return
		}
		keep := ""
		if sel < len(rows) && rows[sel].agent != nil {
			keep = rows[sel].agent.ID
		}
		rows, problem = fresh, ""
		sel = min(sel, max(0, len(rows)-1))
		for i, r := range rows {
			if keep != "" && r.agent != nil && r.agent.ID == keep {
				sel = i
			}
		}
	}
	render()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if !plain {
				reload(false)
				render()
			}
		case k, ok := <-keys:
			if !ok {
				return nil, appQuit
			}
			if plain { // the plain session's card: n starts an agent, anything else goes back
				switch {
				case k.kind == keyRune && (k.r == 'n' || k.r == 'N'):
					scr.setBox(nil)
					return nil, appNew
				case k.kind == keyCtrlC, k.kind == keyCtrlD:
					scr.setBox(nil)
					return nil, appQuit
				case k.kind == keyEsc, k.kind == keyEnter, k.kind == keyLeft, k.kind == keyRune && (k.r == 'q' || k.r == 'Q'):
					plain = false
				}
				render()
				continue
			}
			switch {
			case k.kind == keyUp || (k.kind == keyRune && k.r == 'k'):
				if sel > 0 {
					sel--
				}
			case k.kind == keyDown || (k.kind == keyRune && k.r == 'j'):
				if sel < len(rows)-1 {
					sel++
				}
			case k.kind == keyHome:
				sel = 0
			case k.kind == keyEnd:
				sel = max(0, len(rows)-1)
			case k.kind == keyRune && (k.r == 'q' || k.r == 'Q'), k.kind == keyCtrlC, k.kind == keyCtrlD:
				scr.setBox(nil)
				return nil, appQuit
			case k.kind == keyRune && (k.r == 'n' || k.r == 'N'):
				scr.setBox(nil)
				return nil, appNew
			case k.kind == keyRune && (k.r == 'r' || k.r == 'R'):
				reload(true)
			case k.kind == keyEnter:
				if len(rows) == 0 {
					continue
				}
				r := rows[sel]
				if r.agent == nil {
					plain = true
					break
				}
				scr.setBox(nil)
				return &r, appOpen
			}
			render()
		}
	}
}

// plainSessionCard says what a plain session is and how to reach it.
func plainSessionCard(s inventoryRow) card {
	return card{title: "Plain session", tone: toneNeutral, icon: iconNoAgent,
		head: "Session " + s.ShortID + " has no agents.",
		lines: []string{"It was started without one (ks run without --agent), so there is nothing to open in the app. " +
			"Its terminal opens outside the app with ks attach."},
		facts: []cardFact{
			{"Session", sessLabel(s)},
			{"State", sessionWord(s.RuntimeState, s.ParkReason)},
			{"Terminal", "ks attach " + s.ShortID},
		},
		actions: [][2]string{{"n", "start an agent in a new session"}, {"Esc", "back"}},
	}
}
