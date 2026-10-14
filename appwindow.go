package main

// appwindow.go: an agent's window -- the cards that open instead of it (an
// agent that needs attention, one steered elsewhere), the conversation, the
// input and its / commands, help, and how events read.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- the agent's window -----------------------------------------------------------

// appSystemLine is how a line the commands print (progress, an outcome, a
// refusal) reads in the app: grey and wrapped, so the conversation stands
// out; a refusal red and the next step amber. A line the app built itself
// (a notice, an error, a card) is already laid out and passes as it is.
func appSystemLine(l string) string {
	if strings.Contains(l, "\x1b[") || strings.HasPrefix(l, "  "+iconDone) || strings.HasPrefix(l, "  "+iconFailed) ||
		strings.HasPrefix(l, "  "+iconWarn) || strings.HasPrefix(l, "  "+iconState) || strings.HasPrefix(l, "  "+iconDenied) {
		return l
	}
	width := min(appTerminalWidth(), cardMax)
	t := strings.TrimSpace(l)
	lead := l[:len(l)-len(strings.TrimLeft(l, " "))]
	wrap := func(paint func(string) string) string {
		var out []string
		for _, w := range wrapHang(lead, lead+"  ", t, width) {
			out = append(out, paint(w))
		}
		return strings.Join(out, "\n")
	}
	switch {
	case strings.HasPrefix(t, "refused:") || strings.HasPrefix(t, "error:"):
		return wrap(th.bad)
	case strings.HasPrefix(t, "Next:"):
		return wrap(th.warn)
	}
	return wrap(th.dim)
}

// appWindowHint is the grey line under the input: what the keys do, the most
// important first, as many as fit.
func appWindowHint() string {
	return keyHintsFit(appTerminalWidth()-2, [2]string{"Enter", "send"}, [2]string{"Esc", "interrupt"},
		[2]string{"Ctrl-C twice", "leave (it keeps working)"}, [2]string{"/help", "commands"})
}

// appWindowHeader opens an agent's conversation: a rule carrying the agent's
// name and, at its end, the session it runs in.
func appWindowHeader(name string, sess inventoryRow, width int) string {
	// every part is fitted, so the rule is exactly as wide as the terminal
	// whatever the names are
	width = max(20, width)
	head := th.accent("──") + " " + th.accentBold(fitWidth(sanitize(name), max(6, width/3))) + " "
	label := fitWidth("session "+sessLabel(sess), max(0, width-displayWidth(head)-8))
	tail := " " + th.dim(label) + " " + th.accent("──")
	if displayWidth(label) < 12 {
		tail = ""
	}
	fill := max(0, width-displayWidth(head)-displayWidth(tail))
	return "\n" + head + th.subtle(strings.Repeat("─", fill)) + tail
}

// appAgent opens one agent from the list: its window, or the card that says
// why there is no window to open (a paused session, an agent that needs
// attention, a window elsewhere in control). It answers true when the person
// asked to leave the app altogether, false to go back home.
func appAgent(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow) bool {
	scr.update(func() { scr.status, scr.spin = th.accent(iconState)+" Opening "+sanitize(a.Name)+"…", true })
	w, err := openAgent(cr, sess, a, false)
	scr.update(func() { scr.status, scr.spin = "", false })
	return appAgentWith(cr, scr, keys, sess, a, w, err)
}

func appAgentWith(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, w *agentWindow, err error) bool {
	if err != nil {
		var ce *cliError
		if errors.As(err, &ce) && ce.Kind == "controller_held" {
			return appControlled(cr, scr, keys, sess, a, ce)
		}
		scr.Print(errorBlock("open "+sanitize(a.Name), err, appTerminalWidth()))
		return false
	}
	if w.Runtime != nil && !w.Runtime.Running {
		return appParked(cr, scr, keys, sess, a, w)
	}
	if w.Recovery != nil {
		return appRecovery(scr, keys, sess, a, w)
	}
	return appWindow(cr, scr, keys, sess, a, w)
}

// showCard puts a card in the live region with nothing to type into, and
// waits for a key.
func showCard(scr *screen, c card) {
	box := c.render(appTerminalWidth())
	scr.update(func() {
		scr.box, scr.prompt, scr.input, scr.cursor, scr.hint, scr.suggest = box, "", nil, 0, "", nil
	})
}

// appControlled is an agent another window steers: watch it, or take control.
func appControlled(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, ce *cliError) bool {
	c := card{title: "Controlled elsewhere", tone: toneWarn, icon: iconWarn,
		head:    "Another window is steering " + sanitize(a.Name) + ".",
		lines:   []string{"You can watch it here, or take control. Taking control stops the other window from steering it."},
		facts:   []cardFact{{"Agent", sanitize(a.Name)}, {"Session", sessLabel(sess)}, {"Details", sentence(ce.Message)}},
		actions: [][2]string{{"w", "watch"}, {"t", "take control"}, {"Esc", "back"}}}
	showCard(scr, c)
	for k := range keys {
		switch {
		case k.kind == keyRune && (k.r == 'w' || k.r == 'W'):
			scr.setBox(nil)
			w, err := openAgentView(cr, sess, a)
			return appAgentWith(cr, scr, keys, sess, a, w, err)
		case k.kind == keyRune && (k.r == 't' || k.r == 'T'):
			scr.setBox(nil)
			w, err := openAgent(cr, sess, a, true)
			if err != nil {
				scr.Print(errorBlock("take control of "+sanitize(a.Name), err, appTerminalWidth()))
				return false
			}
			return appAgentWith(cr, scr, keys, sess, a, w, nil)
		case k.kind == keyEsc, k.kind == keyCtrlC, k.kind == keyLeft, k.kind == keyRune && (k.r == 'q' || k.r == 'Q'):
			scr.setBox(nil)
			return false
		}
	}
	return true
}

// appRecovery is an agent that needs a person before it can go on: the
// service's recovery view, as a card. Nothing is opened, retried or decided.
func appRecovery(scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, w *agentWindow) bool {
	name := sanitize(a.Name)
	c := card{title: "Needs attention", tone: toneBad, icon: iconNeedsYou}
	switch w.Recovery.Activity {
	case "recovery_required":
		c.head = name + " stopped in the middle of an instruction."
		c.lines = []string{"It is not ready. What it did is not known, and the instructions after it are held until you decide; nothing is retried by itself."}
	case "failed":
		c.head = name + " has failed."
		c.lines = []string{"It is not ready. Its instructions wait until you decide what happens next."}
	default:
		c.head = name + " needs attention: it reads " + activityWord(w.Recovery.Activity) + "."
	}
	c.facts = []cardFact{{"Agent", name}, {"Session", sessLabel(sess)}}
	if note := strings.TrimSpace(w.Recovery.Note); note != "" {
		c.facts = append(c.facts, cardFact{"Details", sentence(sanitize(note))})
	}
	decide := fmt.Sprintf("ks agent queue show %s --session %s", shellQuote(a.Name), sess.ShortID)
	c.facts = append(c.facts, cardFact{"To decide", decide})
	c.lines = append(c.lines, "Run the command below in a terminal, or as a / command in any agent's window.")
	c.actions = [][2]string{{"Esc", "back"}}
	showCard(scr, c)
	for k := range keys {
		switch {
		case k.kind == keyEsc, k.kind == keyEnter, k.kind == keyLeft, k.kind == keyCtrlC, k.kind == keyRune && (k.r == 'q' || k.r == 'Q'):
			scr.setBox(nil)
			return false
		}
	}
	return true
}

// appWindow runs one agent's window; it answers true when the person asked
// to leave the app altogether, false to go back home.
func appWindow(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, w *agentWindow) bool {
	scr.Print(appWindowHeader(a.Name, sess, appTerminalWidth()))
	appBacklog(cr, scr, sess, a, w.Resume.AfterSeq)
	markSeen(cr, a.ID, w.Resume.AfterSeq)
	win := &liveWindow{sess: sess, agent: a, lease: w.Lease, app: true}
	if _, held := win.hold(); !held {
		scr.Print(notice(iconWarn, toneWarn, "You are watching "+sanitize(a.Name)+": another window controls it, so nothing you type is sent. /take takes control.", appTerminalWidth()))
	}
	go win.renew(cr)

	var leaving atomic.Bool
	var othersNeed atomic.Int64
	go func() { // other agents that need you, every 15 s
		for !leaving.Load() {
			if rows, err := loadHome(cr); err == nil {
				n := int64(0)
				for _, r := range rows {
					if r.agent != nil && r.agent.ID != a.ID && r.rowNeedsYou() {
						n++
					}
				}
				othersNeed.Store(n)
			}
			for i := 0; i < 15 && !leaving.Load(); i++ {
				time.Sleep(time.Second)
			}
		}
	}()
	var mu sync.Mutex
	var shown []approvalRow // the prompt on screen, as recorded shown
	var running atomic.Bool
	wctx := &windowCtx{cr: cr, scr: scr, sess: sess, agent: a}
	var refresh func()
	wctx.busy = func(t string) {
		if t == "" {
			running.Store(false)
			scr.update(func() { scr.status, scr.spin, scr.hint = "", false, appWindowHint() })
			go refresh()
			return
		}
		running.Store(true)
		scr.update(func() { scr.status, scr.spin = th.accent(iconState)+" "+t, true })
	}
	wctx.restore = func() {
		scr.update(func() {
			scr.box, scr.prompt, scr.hint = nil, "> ", appWindowHint()
			scr.suggest = func(in string) []string { return slashSuggestions(in, 6) }
		})
		go refresh()
	}
	// a short note that goes away by itself (Esc with nothing to interrupt)
	var flashes atomic.Int64
	appFlash = func(t string) {
		n := flashes.Add(1)
		scr.setHint(t)
		go func() {
			time.Sleep(3 * time.Second)
			if flashes.Load() == n && !leaving.Load() && !running.Load() && wctx.current() == nil {
				scr.setHint(appWindowHint())
			}
		}()
	}
	defer func() { appFlash = nil }()
	refresh = func() {
		v, err := fetchLiveView(cr, a.ID)
		pending, perr := fetchPendingApprovals(cr, agentSessionID(sess))
		if leaving.Load() {
			return
		}
		if err == nil && !running.Load() && wctx.current() == nil {
			_, held := win.hold()
			parts := appStatusParts(v, held)
			if n := othersNeed.Load(); n > 0 {
				others := th.warn(iconNeedsYou + " " + plural(int(n), "other agent needs you", "other agents need you") + ": /switch")
				parts = append(parts[:1], append([]string{others}, parts[1:]...)...)
			}
			scr.setStatus(joinFit(parts, appTerminalWidth()))
			scr.setSpin(v.Header.Activity == "working" || v.Header.Activity == "starting" || v.Header.Activity == "running_tool" || v.Header.Activity == "waiting_provider")
		}
		if perr == nil {
			mu.Lock()
			shown = pending
			mu.Unlock()
			if wctx.current() != nil {
				return // a confirm card is up; the prompt waits under it
			}
			if len(pending) > 0 {
				rememberShown(cr, pending) // what is on screen is what a decision must match
			}
			scr.setBox(approvalBox(pending, appTerminalWidth()))
		}
	}
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		refresh()
		for range t.C {
			if leaving.Load() {
				return
			}
			refresh()
		}
	}()
	go func() {
		_ = streamEvents(cr, agentSessionID(sess), w.Resume.AfterSeq, func(kind string, data []byte) bool {
			if leaving.Load() {
				return true
			}
			if kind == "event" {
				var e journalEvent
				if json.Unmarshal(data, &e) == nil {
					markSeen(cr, a.ID, e.StreamSeq)
					if line, ok := appEventLine(e, a.Name, cr.AccountID); ok {
						scr.Print(line)
					}
					if c := appRecoveryCard(e, a.Name, sess.ShortID); c != nil {
						scr.Print(strings.Join(c, "\n"))
					}
					if isApprovalEvent(e) {
						go refresh()
					}
				}
			}
			return kind == "end"
		})
	}()

	scr.update(func() {
		scr.prompt, scr.input, scr.cursor = "> ", nil, 0
		scr.hint = appWindowHint()
		scr.suggest = func(in string) []string { return slashSuggestions(in, 6) }
	})
	leave := func() {
		leaving.Store(true)
		// stop the renewal loop FIRST (lose), then release: a renewal racing
		// the release would otherwise report control lost after leaving
		l, held := win.hold()
		win.lose("left the window")
		if held {
			_ = hostedMutate(cr, "DELETE", "/api/v2/control-leases/"+url.PathEscape(l.ID), nil, nil)
		}
		scr.update(func() { scr.box, scr.hint, scr.status, scr.suggest, scr.spin = nil, "", "", nil, false })
		scr.Print(dimNotice(iconState, toneDim, "Left "+sanitize(a.Name)+". It keeps working.", appTerminalWidth()))
	}
	var history []string
	hpos := 0
	heldKs := ""                // a ks command typed as an instruction, held back once
	var ctrlCArmed atomic.Int64 // when Ctrl-C was pressed once on an empty input
	var cancel chan struct{}
	for k := range keys {
		width := appTerminalWidth()
		if wctx.key(k) {
			continue
		}
		switch k.kind {
		case keyEnter:
			text := scr.take()
			if strings.TrimSpace(text) == "" {
				continue
			}
			history = append(history, text)
			hpos = len(history)
			if strings.HasPrefix(strings.TrimSpace(text), "/") {
				line := strings.TrimSpace(text)
				name := strings.Fields(strings.TrimPrefix(line, "/"))
				if len(name) > 0 { // /Stop is /stop; arguments keep their case
					name[0] = strings.ToLower(name[0])
					rest := strings.TrimPrefix(line, "/")
					i := strings.IndexAny(rest, " \t")
					if i < 0 {
						i = len(rest)
					}
					line = "/" + strings.ToLower(rest[:i]) + rest[i:]
				}
				if len(name) > 1 && name[0] == "help" && !strings.HasPrefix(name[1], "-") {
					// /help agent stop is /agent stop --help
					line = "/" + strings.Join(name[1:], " ") + " --help"
					name = append(name[1:], "--help")
				}
				if len(name) > 0 && windowCommands[name[0]] && hasHelp(name[1:]) {
					scr.Print(strings.Join(wrapHang("  ", "  ", sentence(windowCommandHelp(name[0])), min(width, cardMax)), "\n"))
					continue
				}
				if len(name) > 0 && name[0] == "switch" {
					if len(name) < 2 {
						scr.Print(errorLines("switch", "name the agent to open", "/switch <agent> [session]. /agents lists this session's agents.", width))
						continue
					}
					hint := ""
					if len(name) > 2 {
						hint = name[2]
					}
					target, err := appFindAgent(cr, sess, name[1], hint)
					if err != nil {
						why, next, _ := strings.Cut(err.Error(), "; ")
						scr.Print(errorLines("switch to "+sanitize(name[1]), why, sentence(next), width))
						continue
					}
					leave()
					return appAgent(cr, scr, keys, target.sess, *target.agent)
				}
				if len(name) > 0 && !windowCommands[name[0]] {
					if running.Load() {
						scr.Print(notice(iconWarn, toneWarn, "A command is still running. Wait for it to finish, or press Ctrl-C to stop it.", width))
						continue
					}
					argv, cmd, err := slashArgv(line, slashContext{sessionID: agentSessionID(sess), agentName: a.Name})
					if err != nil {
						scr.Print(errorLines("run "+strings.Fields(line)[0], err.Error(), "", width))
						continue
					}
					if appIntercept(wctx, cmd, argv) {
						continue
					}
					cancel = make(chan struct{})
					running.Store(true)
					scr.update(func() {
						scr.status, scr.spin = th.accent(iconState)+" Running /"+strings.Join(quoteWords(argv), " ")+"…", true
						scr.hint = keyHintsFit(width-2, [2]string{"Ctrl-C", "stop it"})
					})
					go func(argv []string, c chan struct{}) {
						_, confirm := runSlash(scr, argv, c)
						running.Store(false)
						scr.update(func() { scr.status, scr.spin, scr.hint = "", false, appWindowHint() })
						if confirm != nil {
							appConfirmFromCommand(wctx, argv, confirm)
							return
						}
						go refresh()
					}(argv, cancel)
					continue
				}
				switch appSlash(cr, scr, win, a, line) {
				case slashHome:
					leave()
					return false
				case slashQuit:
					leave()
					return true
				case slashTake:
					leave()
					return appAgentTake(cr, scr, keys, sess, a)
				}
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(text), "@") {
				// a question to a connected adviser: it answers here, and it
				// is never an instruction to this agent
				heldKs = ""
				scr.Print("\n" + strings.Join(wrapHang(th.bold(fmt.Sprintf("%-6s", "you"))+" ", "       ", sanitize(text), width), "\n"))
				go win.askAdviser(cr, strings.TrimSpace(text))
				continue
			}
			if slash, ok := looksLikeKsCommand(text); ok && text != heldKs {
				heldKs = text
				scr.Print(notice(iconWarn, toneInfo, fmt.Sprintf("%q is a ks command, so it was not sent to %s. Run it here as %s, or press Enter again to send it to %s as written.",
					strings.TrimSpace(text), sanitize(a.Name), slash, sanitize(a.Name)), width))
				scr.update(func() { scr.input, scr.cursor = []rune(text), len([]rune(text)) })
				continue
			}
			heldKs = ""
			scr.Print("\n" + strings.Join(wrapHang(th.bold(fmt.Sprintf("%-6s", "you"))+" ", "       ", sanitize(text), width), "\n"))
			go win.submitFresh(cr, text)
		case keyPaste:
			scr.update(func() {
				for _, r := range k.text {
					scr.input = append(scr.input[:scr.cursor], append([]rune{r}, scr.input[scr.cursor:]...)...)
					scr.cursor++
				}
			})
		case keyEsc:
			go win.interrupt(cr)
		case keyTab:
			in := ""
			scr.mu.Lock()
			in = string(scr.input)
			scr.mu.Unlock()
			done, cands := completeSlash(in)
			if len(cands) > 1 {
				shown := cands
				if len(shown) > 24 {
					shown = append(shown[:24:24], fmt.Sprintf("… %d more", len(cands)-24))
				}
				scr.Print(strings.Join(wrapHang("  ", "  ", th.dim("/"+strings.Join(shown, "  /")), min(width, cardMax)), "\n"))
			}
			scr.update(func() { scr.input, scr.cursor = []rune(done), len([]rune(done)) })
		case keyCtrlC, keyCtrlD:
			if running.Load() && cancel != nil {
				close(cancel)
				cancel = nil
				scr.update(func() { scr.status = th.accent(iconState) + " Stopping the command…" })
				continue
			}
			if !scr.inputEmpty() {
				scr.take()
				continue
			}
			if time.Since(time.Unix(0, ctrlCArmed.Load())) < 2*time.Second {
				leave()
				return false
			}
			at := time.Now().UnixNano()
			ctrlCArmed.Store(at)
			scr.setHint(th.warn("Press Ctrl-C again to leave " + sanitize(a.Name) + ". It keeps working."))
			go func() { // the hint goes back once a second press would no longer count
				time.Sleep(2 * time.Second)
				if ctrlCArmed.Load() == at && !leaving.Load() && !running.Load() {
					scr.setHint(appWindowHint())
				}
			}()
		case keyUp:
			if hpos > 0 {
				hpos--
				h := history[hpos]
				scr.update(func() { scr.input, scr.cursor = []rune(h), len([]rune(h)) })
			}
		case keyDown:
			if hpos < len(history)-1 {
				hpos++
				h := history[hpos]
				scr.update(func() { scr.input, scr.cursor = []rune(h), len([]rune(h)) })
			} else {
				hpos = len(history)
				scr.take()
			}
		case keyRune:
			mu.Lock()
			waiting := len(shown)
			first := approvalRow{}
			if waiting > 0 {
				first = shown[0]
			}
			mu.Unlock()
			if waiting > 0 && scr.inputEmpty() {
				switch k.r {
				case 'y', 'Y':
					go func() { win.decide(cr, "approve", first.ID); refresh() }()
					continue
				case 'n', 'N':
					go func() { win.decide(cr, "deny", first.ID); refresh() }()
					continue
				case 'd', 'D':
					scr.Print(strings.Join(approvalDetails(first, width), "\n"))
					continue
				}
			}
			scr.edit(k)
		default:
			scr.edit(k)
		}
	}
	leave()
	return true
}

func appAgentTake(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow) bool {
	w, err := openAgent(cr, sess, a, true)
	if err != nil {
		scr.Print(errorBlock("take control of "+sanitize(a.Name), err, appTerminalWidth()))
		return false
	}
	return appAgentWith(cr, scr, keys, sess, a, w, nil)
}

// ---- / commands --------------------------------------------------------------------

// appHelp is /help: what the keys do, the window's own commands, the
// shortcuts, and how every ks command runs here, as one card.
func appHelp(agent string, width int) string {
	inner := cardWidth(width) - 4
	kw := 27
	if inner < 70 {
		kw = 22
	}
	row := func(k, what string) []string {
		return wrapHang("  "+th.accent(cell(k, kw))+" ", strings.Repeat(" ", kw+3), what, inner)
	}
	var rows []string
	section := func(title string) {
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, th.bold(title))
	}
	section("Keys")
	rows = append(rows, row("Enter", "Send what you typed to "+agent+".")...)
	rows = append(rows, row("@<adviser> <question>", "Ask a connected agent (/advisers lists them). It answers here and never works in "+agent+"'s workspace.")...)
	rows = append(rows, row("Esc", "Interrupt the instruction it is working on.")...)
	rows = append(rows, row("y / n / d", "Allow, deny or show a permission request (with an empty input).")...)
	rows = append(rows, row("↑ / ↓", "Your earlier lines.")...)
	rows = append(rows, row("Tab", "Complete a / command.")...)
	rows = append(rows, row("Ctrl-C", "Stop a running command. Twice on an empty input leaves; "+agent+" keeps working.")...)
	section("This window")
	rows = append(rows, row("/status", "The agent's full state.")...)
	rows = append(rows, row("/take", "Take control from another window.")...)
	rows = append(rows, row("/switch <agent> [session]", "Open another of your agents.")...)
	rows = append(rows, row("/home", "Back to your agents.")...)
	rows = append(rows, row("/quit", "Leave the app. Your agents keep working.")...)
	section("Shortcuts")
	rows = append(rows, wrapHang("  ", "  ", th.accent("/stop /pause /resume /queue /tasks /results /approvals /advisers /usage /keys /logs /agents /checkpoints /restore /diff /apply"), inner)...)
	section("Every ks command")
	rows = append(rows, row("/<command> [options]", "Runs here, for example /session checkpoints or /result diff <id>.")...)
	rows = append(rows, row("/<command> --help", "Its options (also /help <command>). This session and agent are filled in for you.")...)
	c := card{title: "Help", tone: toneNeutral, rows: rows}
	return "\n" + strings.Join(c.render(width), "\n")
}

type slashResult int

const (
	slashStay slashResult = iota
	slashHome
	slashQuit
	slashTake
)

func appSlash(cr hostedCreds, scr *screen, win *liveWindow, a agentRow, line string) slashResult {
	f := strings.Fields(line)
	width := appTerminalWidth()
	switch f[0] {
	case "/help":
		scr.Print(appHelp(sanitize(a.Name), width))
	case "/status":
		v, err := fetchLiveView(cr, a.ID)
		if err != nil {
			scr.Print(errorBlock("read "+sanitize(a.Name)+"'s state", err, width))
			break
		}
		inner := cardWidth(width) - 4
		var rows []string
		for _, l := range viewLines(v) {
			lead := l[:len(l)-len(strings.TrimLeft(l, " "))]
			rows = append(rows, wrapHang(lead, lead+"  ", l, inner)...)
		}
		c := card{title: "Status of " + sanitize(a.Name), tone: toneNeutral, rows: rows}
		scr.Print("\n" + strings.Join(c.render(width), "\n"))
	case "/home", "/back":
		return slashHome
	case "/quit", "/exit":
		return slashQuit
	case "/take":
		if _, held := win.hold(); held {
			scr.Print(notice(iconDone, toneOK, "You already control "+sanitize(a.Name)+".", width))
			return slashStay
		}
		return slashTake
	}
	return slashStay
}

// ---- the conversation, as a chat -------------------------------------------------

// appEventLine renders one journal event for the app's conversation. The
// agent's own words are shown whole; a tool step is one short line; the
// events the status line and the permission prompt already carry are not
// repeated. ok false means: nothing to print. The words stay inert -- they
// are rendered, never read as commands (the same rule as the window).
func appEventLine(e journalEvent, agentName, account string) (string, bool) {
	width := min(appTerminalWidth(), cardMax+20)
	if t := transcriptOf(e); t != nil {
		switch t.Kind {
		case "assistant_text":
			text := strings.TrimRight(t.Text, "\n")
			if text == "" {
				return "", false
			}
			if t.Clipped {
				text += " […clipped by the service]"
			}
			w := max(6, min(12, displayWidth(agentName)))
			pad := strings.Repeat(" ", w+1)
			return "\n" + strings.Join(wrapHang(th.accentBold(sanitize(fmt.Sprintf("%-*s", w, fitWidth(agentName, w))))+" ", pad, sanitize(text), width), "\n"), true
		case "instruction":
			// this person's own messages were shown when they pressed Enter;
			// an instruction from anyone or anywhere else is shown here
			if t.AuthorType == "human" && (account == "" || t.AuthorID == account) {
				return "", false
			}
			who, _ := attribution(*t)
			return "\n" + strings.Join(wrapHang(th.info(sanitize(fmt.Sprintf("%-6s", who)))+" ", "       ", sanitize(strings.TrimRight(t.Text, "\n")), width), "\n"), true
		case "tool_started":
			s := orUnnamedTool(t.ToolName)
			if d := firstLineOf(t.Text); d != "" {
				s += ": " + d
			}
			return strings.Join(wrapHang("  "+th.dim("⎿")+" ", "    ", th.dim(sanitize(s)), width), "\n"), true
		case "tool_finished":
			mark := th.ok(iconDone)
			if t.Failed {
				mark = th.bad(iconFailed)
			}
			s := orUnnamedTool(t.ToolName)
			if t.Failed {
				if d := firstLineOf(t.Text); d != "" {
					s += ": " + d
				}
			}
			return strings.Join(wrapHang("  "+mark+" ", "    ", th.dim(sanitize(s)), width), "\n"), true
		default:
			return strings.Join(wrapHang("  ", "  ", th.dim(sanitize(transcriptLine(*t))), width), "\n"), true
		}
	}
	var p struct {
		Type    string `json:"type"`
		State   string `json:"state"`
		Summary string `json:"summary"`
		Name    string `json:"name"`
		Reason  string `json:"reason"`
		Runtime string `json:"runtime_state"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	switch p.Type {
	case "task.finished": // the instruction's id is in /tasks, not in the conversation
		switch {
		case p.State == "succeeded":
			return dimNotice(iconDone, toneOK, "Finished.", width), true
		case p.State == "failed":
			why := ""
			if s := firstLineOf(p.Summary); s != "" {
				why = ": " + sanitize(s)
			}
			return notice(iconFailed, toneBad, th.bad(sentence("Failed"+why)), width), true
		case runnerStopped(p.State):
			return "", false // the needs-attention card says it, in full
		default:
			return dimNotice(iconState, toneDim, taskWord(p.State)+".", width), true
		}
	case "result.recorded":
		if p.Name == "ks-changeset.json" {
			return "", false // every turn records one; /results lists them
		}
		return notice(iconResult, toneNeutral, "Result: "+sanitize(figure(p.Name))+th.dim(". /results lists them."), width), true
	case "queue.held":
		why := ""
		if s := firstLineOf(p.Reason); s != "" {
			why = ": " + sanitize(s)
		}
		return notice(iconWarn, toneWarn, th.warn(sentence(sanitize(agentName)+"'s queue is held"+why))+th.dim(" /queue shows it."), width), true
	case "session.parked":
		if fundsParked(firstNonEmpty(p.Runtime, "parked"), p.Reason) {
			return notice(iconPaused, toneWarn, th.warn("The session is paused: your account ran out of credit. Its work is saved and nothing runs. Add credit, then resume."), width), true
		}
		return notice(iconPaused, toneWarn, "The session is paused. Its work is saved and nothing runs until it is resumed.", width), true
	case "session.resumed":
		return dimNotice(iconState, toneNeutral, "The session is running again.", width), true
	case "session.stopped":
		return dimNotice(iconState, toneDim, "The session stopped.", width), true
	}
	return "", false
}

// appRecoveryCard is the needs-attention card an event calls for, in the
// conversation, or nil: an instruction the runner stopped in the middle of,
// or the agent reading recovery_required. It never reads as ready or
// finished, and names how to decide.
func appRecoveryCard(e journalEvent, agentName, sessShort string) []string {
	name := sanitize(agentName)
	decide := fmt.Sprintf("/queue (ks agent queue show %s --session %s)", shellQuote(agentName), sessShort)
	width := appTerminalWidth()
	c := card{title: "Needs attention", tone: toneBad, icon: iconNeedsYou}
	switch e.kind() {
	case "agent.activity":
		var p struct {
			Activity string `json:"activity"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.Activity != "recovery_required" {
			return nil
		}
		c.head = name + " stopped, and is not ready."
		c.lines = []string{"Its runner stopped and what it was doing is not known. The instructions after it are held until you decide; nothing is retried by itself."}
		c.facts = []cardFact{{"To decide", decide}}
	case "task.finished":
		f := crashFacts{TaskID: e.SubjectID, ModeNotShown: modeNotShown}
		if e.TaskID != "" {
			f.TaskID = e.TaskID
		}
		finishedFact(&f, e)
		if !runnerStopped(f.State) {
			return nil
		}
		c.head = name + " stopped in the middle of an instruction. It is not ready, and the instruction did not finish."
		c.lines = []string{"What it did is not known. The instructions after it are held until you decide; nothing is retried by itself."}
		c.facts = []cardFact{{"Instruction", sanitize(f.TaskID) + " · " + taskWord(f.State)}}
		reported := "Nothing was recorded."
		if strings.TrimSpace(f.Summary) != "" {
			reported = sentence(sanitize(f.Summary))
		}
		c.facts = append(c.facts, cardFact{"It reported", reported})
		if o := f.Outcome; o != nil {
			c.facts = append(c.facts, cardFact{"Outcome", fmt.Sprintf("runner turn %s · work %s · acceptance %s", sanitize(figure(o.RunnerTurn)), sanitize(figure(o.Work)), sanitize(figure(o.Acceptance)))})
			for _, u := range o.Unsettled {
				c.facts = append(c.facts, cardFact{"Unsettled", fmt.Sprintf("%s %s: %s", sanitize(figure(u.Kind)), sanitize(figure(u.ID)), sanitize(figure(u.Detail)))})
			}
		} else {
			c.facts = append(c.facts, cardFact{"Outcome", "The close recorded no outcome."})
		}
		c.facts = append(c.facts, cardFact{"To decide", decide})
	default:
		return nil
	}
	return append([]string{""}, c.render(width)...)
}

// appBacklog shows the end of the conversation so far, as the chat renders
// it; a journal that cannot be read says so and is never shown as silence.
func appBacklog(cr hostedCreds, scr *screen, sess inventoryRow, a agentRow, upto int64) {
	agentName := a.Name
	width := appTerminalWidth()
	if upto <= 0 {
		return
	}
	var env struct {
		Data struct {
			Items []journalEvent `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess))+"/events?limit=400", nil, &env); err != nil {
		scr.Print(errorLines("read the conversation so far", errText(err), "That is not the same as nothing having happened. /logs shows the journal.", width))
		return
	}
	if mark, ok := lastSeen(cr, a.ID); ok && mark.Seq < upto {
		waiting := 0
		if pending, err := fetchPendingApprovals(cr, agentSessionID(sess)); err == nil {
			waiting = len(pending)
		}
		var away struct {
			Data struct {
				Items []journalEvent `json:"items"`
			} `json:"data"`
		}
		q := fmt.Sprintf("/api/v2/sessions/%s/events?after_seq=%d&limit=400", url.PathEscape(agentSessionID(sess)), mark.Seq)
		if err := hostedCall(cr, "GET", q, nil, &away); err == nil {
			if s := awaySummary(away.Data.Items, mark.Seq, waiting, mark.At); s != "" {
				if len(away.Data.Items) >= 400 {
					s += "\n" + th.dim("  At least this much: more happened than one page holds. /logs shows all of it.")
				}
				scr.Print(s)
			}
		}
	}
	var lines []string
	for _, e := range env.Data.Items {
		if e.StreamSeq > upto {
			continue
		}
		if l, ok := appEventLine(e, agentName, cr.AccountID); ok {
			lines = append(lines, l)
		}
	}
	if len(lines) > backlogMax {
		scr.Print(dimNotice("…", toneDim, fmt.Sprintf("%d earlier lines are not shown here. /logs shows them.", len(lines)-backlogMax), width))
		lines = lines[len(lines)-backlogMax:]
	}
	for _, l := range lines {
		scr.Print(l)
	}
}

// appFindAgent resolves /switch: this session first, then every session;
// several agents of one name are listed, never chosen between.
func appFindAgent(cr hostedCreds, here inventoryRow, name, sessHint string) (homeRow, error) {
	rows, err := loadHome(cr)
	if err != nil {
		return homeRow{}, fmt.Errorf("your agents could not be read: %s", errText(err))
	}
	var local, all []homeRow
	for _, r := range rows {
		if r.agent == nil || (r.agent.Name != name && r.agent.ID != name) {
			continue
		}
		if sessHint != "" && r.sess.ShortID != sessHint && r.sess.ID != sessHint && r.sess.RecordID != sessHint {
			continue
		}
		all = append(all, r)
		if r.sess.RecordID == agentSessionID(here) || r.sess.ID == here.ID {
			local = append(local, r)
		}
	}
	pick := all
	if len(local) > 0 && sessHint == "" {
		pick = local
	}
	switch len(pick) {
	case 1:
		return pick[0], nil
	case 0:
		return homeRow{}, fmt.Errorf("you have no agent named %q; /home lists your agents, and n there starts one", name)
	}
	var where []string
	for _, r := range pick {
		where = append(where, r.sess.ShortID)
	}
	return homeRow{}, fmt.Errorf("%d agents are named %q (sessions %s); name the session: /switch %s <session>", len(pick), name, strings.Join(where, ", "), name)
}

// shortDuration is a duration as a person reads it on a status line: 45s,
// 2m, 1h 5m, 3d 2h -- never Go's 2m0s.
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%dh", m/60)
		}
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	h := m / 60
	if h%24 == 0 {
		return fmt.Sprintf("%dd", h/24)
	}
	return fmt.Sprintf("%dd %dh", h/24, h%24)
}
