package main

// app.go: `ks` with no arguments, at a terminal, is an app you stay in
// (phase 1 of the app plan): a home screen of your agents, and an agent's
// window with the conversation scrolling above, an input line, inline
// permission prompts and a status line below. Everything it does goes
// through the same machinery as the commands -- the control lease, fresh
// submission ids recorded before sending, the shown-then-decided approval
// rule, the cancel route for an interrupt -- so the app promises nothing the
// commands would not.
//
// Keys: Enter sends; Esc interrupts the instruction in flight; y / n / d
// answer a permission prompt when the input is empty; Ctrl-C clears the
// input, and on an empty input (twice) leaves the agent, which keeps
// working; /help lists the / commands.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// appSink, when set, receives every progress and emitted line, so nothing
// printed while the app runs tears its screen.
var appSink func(string)

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
	scr := newScreen(os.Stdout)
	appSink = scr.Print
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

	scr.Print("KeepState · " + cr.CTL + " · agents keep working when you leave")
	for {
		row, act := appHome(cr, scr, keys)
		switch act {
		case appQuit:
			exit(exitOK)
		case appNew:
			r, ok := appNewSession(cr, scr, keys)
			if !ok {
				continue
			}
			row = r
		}
		if row != nil && appAgent(cr, scr, keys, row.sess, *row.agent) {
			exit(exitOK)
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
		sort.Slice(agents, func(i, j int) bool { return agents[i].IsPrimary && !agents[j].IsPrimary })
		for i := range agents {
			a := agents[i]
			rows = append(rows, homeRow{sess: s, agent: &a})
		}
	}
	// agents that need you first, then the other agents, plain sessions last
	rank := func(r homeRow) int {
		switch {
		case r.agent != nil && needsYou(r.agent.Activity):
			return 0
		case r.agent != nil:
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i]) < rank(rows[j]) })
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

func homeLine(r homeRow, selected bool, width int) string {
	mark := "  "
	if selected {
		mark = "› "
	}
	var s string
	if r.agent == nil {
		s = fmt.Sprintf("%s%-14s plain session %s · %s · no agent (ks attach %s)", mark, "–", r.sess.ShortID,
			stateLabel("session_runtime", r.sess.RuntimeState), r.sess.ShortID)
	} else {
		age, _ := agentAge(*r.agent, time.Now())
		flag := "  "
		if needsYou(r.agent.Activity) {
			flag = "⚑ "
		}
		s = fmt.Sprintf("%s%s%-14s %s · %s · %s · %s", mark, flag, fitWidth(r.agent.Name, 14), r.sess.ShortID, fitWidth(r.sess.Name, 24),
			stateLabel("agent_activity", r.agent.Activity), age)
	}
	s = fitWidth(s, width)
	if selected {
		return "\x1b[7m" + s + "\x1b[0m"
	}
	return s
}

func appHome(cr hostedCreds, scr *screen, keys <-chan keyEvent) (*homeRow, appAction) {
	rows, err := loadHome(cr)
	if err != nil {
		scr.Print("your sessions could not be read: " + errText(err))
	}
	sel := 0
	for sel < len(rows) && rows[sel].agent == nil {
		sel++
	}
	if sel == len(rows) {
		sel = 0
	}
	render := func() {
		width := appTerminalWidth()
		box := []string{"Your agents:"}
		agents := 0
		for _, r := range rows {
			if r.agent != nil {
				agents++
			}
		}
		if agents == 0 {
			box = append(box, "  none yet: press n to start an agent session")
		}
		for i, r := range rows {
			box = append(box, homeLine(r, i == sel, width))
		}
		scr.update(func() {
			scr.box = box
			scr.prompt = ""
			scr.input, scr.cursor = nil, 0
			scr.hint = ""
			scr.status = "↑/↓ choose · Enter open · n new agent session · r refresh · q quit"
		})
	}
	render()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if fresh, err := loadHome(cr); err == nil {
				rows = fresh
				if sel >= len(rows) {
					sel = len(rows) - 1
				}
				if sel < 0 {
					sel = 0
				}
				render()
			}
		case k, ok := <-keys:
			if !ok {
				return nil, appQuit
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
			case k.kind == keyRune && (k.r == 'q' || k.r == 'Q'), k.kind == keyCtrlC, k.kind == keyCtrlD:
				scr.setBox(nil)
				return nil, appQuit
			case k.kind == keyRune && (k.r == 'n' || k.r == 'N'):
				scr.setBox(nil)
				return nil, appNew
			case k.kind == keyRune && (k.r == 'r' || k.r == 'R'):
				if fresh, err := loadHome(cr); err == nil {
					rows = fresh
					sel = 0
				} else {
					scr.Print("your sessions could not be read: " + errText(err))
				}
			case k.kind == keyEnter:
				if len(rows) == 0 {
					continue
				}
				r := rows[sel]
				if r.agent == nil {
					scr.Print(fmt.Sprintf("%s is a plain session: it has no agents. Its terminal: ks attach %s (outside the app). For an agent: press n.", r.sess.ShortID, r.sess.ShortID))
					continue
				}
				scr.setBox(nil)
				return &r, appOpen
			}
			render()
		}
	}
}

// ask reads one line in the input row with a prompt; Esc or Ctrl-C cancels.
func ask(scr *screen, keys <-chan keyEvent, prompt, status string) (string, bool) {
	scr.update(func() {
		scr.box, scr.prompt, scr.input, scr.cursor, scr.hint, scr.status = nil, prompt, nil, 0, "", status
	})
	for k := range keys {
		switch k.kind {
		case keyEnter:
			return strings.TrimSpace(scr.take()), true
		case keyEsc, keyCtrlC:
			scr.take()
			return "", false
		case keyPaste:
			scr.update(func() {
				for _, r := range strings.ReplaceAll(k.text, "\n", " ") {
					scr.input = append(scr.input, r)
				}
				scr.cursor = len(scr.input)
			})
		default:
			scr.edit(k)
		}
	}
	return "", false
}

func appNewSession(cr hostedCreds, scr *screen, keys <-chan keyEvent) (*homeRow, bool) {
	name, ok := ask(scr, keys, "new agent session · the agent's name [main]: ", "Enter to start · Esc to go back")
	if !ok {
		return nil, false
	}
	if name == "" {
		name = "main"
	}
	if !agentNameSafe.MatchString(name) {
		scr.Print("an agent name is letters, digits, dot, hyphen or underscore; nothing was created")
		return nil, false
	}
	keyArg := ""
	for {
		scr.update(func() { scr.status = "starting a machine for " + name + " … (this takes a few seconds)" })
		rec, p, _, err := startAgentSession(cr, "", name, keyArg, nil)
		if err != nil {
			ce := classify(err)
			scr.Print("not started: " + ce.Message)
			if ce.Kind == "key_ambiguous" {
				k, ok := ask(scr, keys, "the key this agent calls with (id): ", "Enter to use it · Esc to go back")
				if ok && k != "" {
					keyArg = k
					continue
				}
			}
			return nil, false
		}
		if !p.Ready && !p.Supervised {
			scr.Print("the agent did not start: " + p.Note)
			return nil, false
		}
		// the session as ks session list shows it: its id there is the one a
		// person sees everywhere else, never one derived from the record id
		sess := inventoryRow{ID: rec.ID, RecordID: rec.ID, ShortID: rec.ID, Name: rec.ID, RuntimeState: "running"}
		if inv, err := fetchInventory(cr, "", false); err == nil {
			for _, r := range inv {
				if r.RecordID == rec.ID {
					sess = r
					break
				}
			}
		}
		scr.Print(fmt.Sprintf("agent %s is %s in session %s", name, startedWord(p), sess.ShortID))
		a := agentRow{ID: p.AgentID, Name: name, SessionID: rec.ID, IsPrimary: true}
		return &homeRow{sess: sess, agent: &a}, true
	}
}

// ---- the agent's window -----------------------------------------------------------

// appAgent runs one agent's window; it answers true when the person asked to
// leave the app altogether, false to go back home.
func appAgent(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow) bool {
	w, err := openAgent(cr, sess, a, false)
	if err != nil {
		scr.Print("could not open " + a.Name + ": " + errText(err))
		return false
	}
	if w.Runtime != nil && !w.Runtime.Running {
		return appParked(cr, scr, keys, sess, a, w)
	}
	if w.Recovery != nil {
		scr.Print(fmt.Sprintf("agent %s reads %s: %s", a.Name, stateLabel("agent_activity", w.Recovery.Activity), sanitize(w.Recovery.Note)))
		for _, ac := range w.Recovery.Actions {
			scr.Print("  " + sanitize(ac.Label))
		}
		return false
	}
	scr.Print(fmt.Sprintf("── %s · session %s (%s) ──", a.Name, sess.ShortID, sess.Name))
	appBacklog(cr, scr, sess, a, w.Resume.AfterSeq)
	markSeen(cr, a.ID, w.Resume.AfterSeq)
	win := &liveWindow{sess: sess, agent: a, lease: w.Lease, app: true}
	if _, held := win.hold(); !held {
		scr.Print("another window controls this agent: you are watching. /take takes control.")
	}
	go win.renew(cr)

	var leaving atomic.Bool
	var othersNeed atomic.Int64
	go func() { // other agents that need you, every 15 s
		for !leaving.Load() {
			if rows, err := loadHome(cr); err == nil {
				n := int64(0)
				for _, r := range rows {
					if r.agent != nil && r.agent.ID != a.ID && needsYou(r.agent.Activity) {
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
	refresh := func() {
		v, err := fetchLiveView(cr, a.ID)
		pending, perr := fetchPendingApprovals(cr, agentSessionID(sess))
		if leaving.Load() {
			return
		}
		if err == nil {
			_, held := win.hold()
			line := appStatusLine(v, held)
			if n := othersNeed.Load(); n > 0 {
				line = fmt.Sprintf("⚑ %d other agent(s) need you (/switch) · ", n) + line
			}
			scr.setStatus(line)
		}
		if perr == nil {
			mu.Lock()
			shown = pending
			mu.Unlock()
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
					for _, l := range recoveryScreenFor(e, a.Name, sess.ShortID) {
						scr.Print(l)
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
		scr.hint = "Enter sends · Esc interrupts · Ctrl-C twice leaves (the agent keeps working) · /help"
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
		scr.update(func() { scr.box, scr.hint, scr.status = nil, "", "" })
		scr.Print(fmt.Sprintf("left %s; it keeps working", a.Name))
	}
	var history []string
	hpos := 0
	var ctrlCArmed time.Time
	var running atomic.Bool
	var cancel chan struct{}
	for k := range keys {
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
				if len(name) > 0 && name[0] == "switch" {
					if len(name) < 2 {
						scr.Print("/switch <agent> [session]: open another of your agents; /agents lists this session's")
						continue
					}
					hint := ""
					if len(name) > 2 {
						hint = name[2]
					}
					target, err := appFindAgent(cr, sess, name[1], hint)
					if err != nil {
						scr.Print(err.Error())
						continue
					}
					leave()
					return appAgent(cr, scr, keys, target.sess, *target.agent)
				}
				if len(name) > 0 && !windowCommands[name[0]] {
					if running.Load() {
						scr.Print("a command is still running; Ctrl-C stops it")
						continue
					}
					argv, _, err := slashArgv(line, slashContext{sessionID: agentSessionID(sess), agentName: a.Name})
					if err != nil {
						scr.Print(err.Error())
						continue
					}
					cancel = make(chan struct{})
					running.Store(true)
					scr.setHint("running: ks " + strings.Join(argv, " ") + " · Ctrl-C stops it")
					go func(argv []string, c chan struct{}) {
						runSlash(scr, argv, c)
						running.Store(false)
						scr.setHint("Enter sends · Esc interrupts · Ctrl-C twice leaves (the agent keeps working) · /help")
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
			scr.Print(fmt.Sprintf("%-6s %s", "you", strings.ReplaceAll(sanitize(text), "\n", "\n       ")))
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
				scr.Print("  /" + strings.Join(shown, "  /"))
			}
			scr.update(func() { scr.input, scr.cursor = []rune(done), len([]rune(done)) })
		case keyCtrlC, keyCtrlD:
			if running.Load() && cancel != nil {
				close(cancel)
				cancel = nil
				scr.Print("  stopping the command…")
				continue
			}
			if !scr.inputEmpty() {
				scr.take()
				continue
			}
			if time.Since(ctrlCArmed) < 2*time.Second {
				leave()
				return false
			}
			ctrlCArmed = time.Now()
			scr.setHint("press Ctrl-C again to leave " + a.Name + " (it keeps working)")
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
					scr.Print(approvalHuman(first, "y approves it, n denies it"))
					for _, f := range first.Affects {
						scr.Print("  affects: " + sanitize(f))
					}
					if first.CostImplication != "" {
						scr.Print("  cost: " + sanitize(first.CostImplication))
					}
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
	if _, err := openAgent(cr, sess, a, true); err != nil {
		scr.Print("control was not taken: " + errText(err))
		return false
	}
	return appAgent(cr, scr, keys, sess, a)
}

// appParked is an agent whose session is not running: opening never wakes
// it; r resumes it because the person asked.
func appParked(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, w *agentWindow) bool {
	scr.Print(fmt.Sprintf("agent %s: the session is %s and was NOT woken. %s", a.Name, stateLabel("session_runtime", w.Runtime.State), sanitize(w.Runtime.Note)))
	resumable := false
	for _, ac := range w.Runtime.Actions {
		if ac.Action == "resume_session" {
			resumable = true
			if ac.Discloses != "" {
				scr.Print("resuming: " + sanitize(ac.Discloses))
			}
		}
	}
	if !resumable {
		scr.Print("this session cannot be resumed from here")
		return false
	}
	scr.update(func() {
		scr.box, scr.prompt, scr.hint, scr.status = nil, "", "", "r resume the session (billing resumes) · Esc back"
	})
	for k := range keys {
		switch {
		case k.kind == keyRune && (k.r == 'r' || k.r == 'R'):
			r, err := postResume(cr, agentSessionID(sess))
			if err != nil {
				scr.Print("not resumed: " + errText(err))
				return false
			}
			scr.Print(fmt.Sprintf("session %s is resuming (state: %s)", sess.ShortID, figure(r["runtime_state"])))
			time.Sleep(2 * time.Second)
			return appAgent(cr, scr, keys, sess, a)
		case k.kind == keyEsc, k.kind == keyCtrlC, k.kind == keyRune && k.r == 'q':
			return false
		}
	}
	return true
}

// ---- prompt box and status line ---------------------------------------------------

func approvalBox(pending []approvalRow, width int) []string {
	if len(pending) == 0 {
		return nil
	}
	ap := pending[0]
	inner := width - 4
	if inner < 20 {
		inner = 20
	}
	title := "┌ Permission "
	if len(pending) > 1 {
		title = fmt.Sprintf("┌ Permission · 1 of %d waiting ", len(pending))
	}
	rows := []string{title + strings.Repeat("─", max(0, width-displayWidth(title)-1)) + "┐"}
	add := func(s string) {
		rows = append(rows, "│ "+fitWidth(s, inner)+strings.Repeat(" ", max(0, inner-displayWidth(fitWidth(s, inner))))+" │")
	}
	add("the agent wants to: " + sanitize(actionShown(ap)))
	if ap.RequestedForTask != "" {
		add("for instruction " + ap.RequestedForTask + " · expires " + figure(ap.ExpiresAt))
	}
	add("[y] allow once   [n] deny   [d] details")
	rows = append(rows, "└"+strings.Repeat("─", max(0, width-2))+"┘")
	return rows
}

func appStatusLine(v liveViewDoc, held bool) string {
	parts := []string{}
	if v.Status != nil {
		st := "● " + figure(v.Status.Label)
		switch {
		case v.Status.Stale:
			st += " (STALE: last known)"
		case v.Status.AgeSeconds != nil && *v.Status.AgeSeconds > 15:
			st += " · unchanged " + (time.Duration(*v.Status.AgeSeconds) * time.Second).String()
		}
		parts = append(parts, st)
	} else {
		parts = append(parts, "● "+stateLabel("agent_activity", v.Header.Activity))
	}
	if v.Footer.Queued != nil {
		parts = append(parts, fmt.Sprintf("queue %d", *v.Footer.Queued))
	}
	if v.Footer.PendingApprovals != nil && *v.Footer.PendingApprovals > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting for you", *v.Footer.PendingApprovals))
	}
	if v.Header.LastSavedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, v.Header.LastSavedAt); err == nil {
			parts = append(parts, "saved "+time.Since(t).Round(time.Minute).String()+" ago")
		}
	} else {
		parts = append(parts, "not saved yet")
	}
	if u := v.Usage; u != nil {
		var tokens int64
		for _, e := range u.Entries {
			tokens += e.Tokens
		}
		switch {
		case u.ModelSpend != nil:
			parts = append(parts, fmt.Sprintf("model $%.2f", float64(*u.ModelSpend)/1e6))
		case tokens > 0:
			parts = append(parts, fmt.Sprintf("%d tokens", tokens))
		}
	}
	if held {
		parts = append(parts, "you control")
	} else {
		parts = append(parts, "watching")
	}
	parts = append(parts, "/help")
	return strings.Join(parts, " · ")
}

// ---- / commands (phase 1: the window's own; every ks command comes in phase 2) ----

type slashResult int

const (
	slashStay slashResult = iota
	slashHome
	slashQuit
	slashTake
)

func appSlash(cr hostedCreds, scr *screen, win *liveWindow, a agentRow, line string) slashResult {
	f := strings.Fields(line)
	switch f[0] {
	case "/help":
		scr.Print(strings.Join([]string{
			"Enter sends what you typed to " + a.Name + " · Esc interrupts its instruction in flight",
			"y / n / d answer a permission prompt (with an empty input) · ↑/↓ your earlier lines",
			"/status   the agent's full state        /take   take control from another window",
			"/switch <agent> [session]   open another of your agents without going home",
			"/home     back to your agents           /quit   leave the app (agents keep working)",
			"/stop /pause /resume /queue /tasks /results /approvals /advisers /usage /keys /logs /agents",
			"and EVERY ks command: /<command> [options], e.g. /session checkpoints, /result diff <id>, /cruise status <job>",
			"  this session and agent are filled in when the command takes them; /<command> --help shows its options",
			"Tab completes a / command · Ctrl-C stops a running command; on an empty input, twice leaves this agent",
		}, "\n"))
	case "/status":
		if v, err := fetchLiveView(cr, a.ID); err == nil {
			scr.Print(strings.Join(viewLines(v), "\n"))
		} else {
			scr.Print("the agent's state could not be read: " + errText(err))
		}
	case "/home", "/back":
		return slashHome
	case "/quit", "/exit":
		return slashQuit
	case "/take":
		if _, held := win.hold(); held {
			scr.Print("you already control " + a.Name)
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
			return sanitize(fmt.Sprintf("%-*s %s", w, fitWidth(agentName, w), strings.ReplaceAll(text, "\n", "\n"+pad))), true
		case "instruction":
			// this person's own messages were shown when they pressed Enter;
			// an instruction from anyone or anywhere else is shown here
			if t.AuthorType == "human" && (account == "" || t.AuthorID == account) {
				return "", false
			}
			who, _ := attribution(*t)
			return sanitize(fmt.Sprintf("%-6s %s", who, strings.ReplaceAll(strings.TrimRight(t.Text, "\n"), "\n", "\n       "))), true
		case "tool_started":
			s := "  ⎿ " + orUnnamedTool(t.ToolName)
			if d := firstLineOf(t.Text); d != "" {
				s += ": " + d
			}
			return sanitize(s), true
		case "tool_finished":
			mark := "✓"
			if t.Failed {
				mark = "✗"
			}
			s := "  " + mark + " " + orUnnamedTool(t.ToolName)
			if t.Failed {
				if d := firstLineOf(t.Text); d != "" {
					s += ": " + d
				}
			}
			return sanitize(s), true
		default:
			return sanitize(transcriptLine(*t)), true
		}
	}
	var p struct {
		Type    string `json:"type"`
		State   string `json:"state"`
		Summary string `json:"summary"`
		Name    string `json:"name"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	switch p.Type {
	case "task.finished":
		switch p.State {
		case "succeeded":
			return sanitize("  ● finished (" + e.SubjectID + ")"), true
		case "failed":
			return sanitize("  ✗ failed (" + e.SubjectID + "): " + firstLineOf(p.Summary)), true
		default:
			return sanitize("  ● " + figure(p.State) + " (" + e.SubjectID + ")"), true
		}
	case "result.recorded":
		if p.Name == "ks-changeset.json" {
			return "", false // every turn records one; /results lists them
		}
		return sanitize("  ↳ result " + figure(p.Name) + " (/results)"), true
	case "queue.held":
		return sanitize("! the queue is HELD: " + firstLineOf(p.Reason) + " (ks agent queue show " + agentName + ")"), true
	case "session.parked", "session.resumed", "session.stopped":
		return sanitize(agentEventLine(e)), true
	}
	return "", false
}

// appBacklog shows the end of the conversation so far, as the chat renders
// it; a journal that cannot be read says so and is never shown as silence.
func appBacklog(cr hostedCreds, scr *screen, sess inventoryRow, a agentRow, upto int64) {
	agentName := a.Name
	if upto <= 0 {
		return
	}
	var env struct {
		Data struct {
			Items []journalEvent `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess))+"/events?limit=400", nil, &env); err != nil {
		scr.Print("the conversation so far could not be read (that is not the same as nothing having happened): " + errText(err))
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
					s += " (at least: more happened than one page; ks agent logs)"
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
		scr.Print(fmt.Sprintf("… %d earlier lines (ks agent logs)", len(lines)-backlogMax))
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
		return homeRow{}, fmt.Errorf("no agent of yours is named %q; /home lists them, n there starts one", name)
	}
	var where []string
	for _, r := range pick {
		where = append(where, r.sess.ShortID)
	}
	return homeRow{}, fmt.Errorf("%d agents are named %q (sessions %s); name the session: /switch %s <session>", len(pick), name, strings.Join(where, ", "), name)
}
