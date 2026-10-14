package main

// appnew.go: starting an agent from the app: its name, the key it calls
// the model with when there is a choice, and its start, as one card.

import (
	"fmt"
	"strings"
	"sync"
)

// ---- a new agent ------------------------------------------------------------

// nameRuleText is the service's name rule, as a person reads it.
const nameRuleText = "1 to 48 letters, digits, hyphens or underscores, starting with a letter."

// nameProblem says, in a few words, what makes a new name one the service
// would refuse; empty when it is a name.
func nameProblem(name string) string {
	if newNameRefusal(name, "") == nil {
		return ""
	}
	switch {
	case len(name) > 48:
		return "it is longer than 48 characters"
	case strings.ContainsAny(name, " \t"):
		return "it has a space in it"
	}
	r := []rune(name)
	if len(r) > 0 && !(r[0] >= 'a' && r[0] <= 'z' || r[0] >= 'A' && r[0] <= 'Z') {
		return "it must start with a letter"
	}
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Sprintf("%q is not allowed in a name", string(c))
		}
	}
	return "it does not follow the rule above"
}

func newAgentCard(problem string) card {
	return card{title: "New agent", tone: toneNeutral, icon: iconApp,
		head:  "Start an agent in a new session.",
		lines: []string{"Name it, and press Enter. It gets a machine of its own, and keeps working when you leave."},
		facts: []cardFact{{"Name", nameRuleText + " Leave it empty for main."}},
		err:   problem,
	}
}

// askCard shows a card with an input line under it and reads one line; Esc
// or Ctrl-C cancels. initial is the text the input starts with.
func askCard(scr *screen, keys <-chan keyEvent, box []string, prompt, initial string, hint string) (string, bool) {
	scr.update(func() {
		scr.box, scr.prompt, scr.hint, scr.status, scr.spin, scr.suggest = box, prompt, hint, "", false, nil
		scr.input = []rune(initial)
		scr.cursor = len(scr.input)
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
	hint := keyHintsFit(appTerminalWidth()-2, [2]string{"Enter", "start"}, [2]string{"Esc", "back"})
	name, problem := "", ""
	for {
		typed, ok := askCard(scr, keys, newAgentCard(problem).render(appTerminalWidth()), "Name  ", name, hint)
		if !ok {
			scr.update(func() { scr.box, scr.prompt, scr.hint = nil, "", "" })
			return nil, false
		}
		name = typed
		if name == "" {
			name = "main"
		}
		if why := nameProblem(name); why != "" {
			problem = fmt.Sprintf("%q can't be used as a name: %s.", sanitize(name), why)
			continue
		}
		break
	}
	keyArg := ""
	for {
		// what starting reports becomes the card's steps, never the
		// service's step log in the scrollback
		var mu sync.Mutex
		st := map[string]string{} // step -> "", "busy", "done", "failed"; key -> its last four
		show := func() {
			mu.Lock()
			c := card{title: "New agent", tone: toneNeutral, icon: iconState,
				head:  "Starting " + name + "…",
				lines: []string{"A session and a machine are being prepared for it. This takes a few seconds."}}
			step := func(state, busy, done string) {
				switch state {
				case "done":
					c.rows = append(c.rows, "  "+th.ok(iconDone)+" "+done)
				case "failed":
					c.rows = append(c.rows, "  "+th.bad(iconFailed)+" "+th.bad(done+": not done"))
				case "busy":
					c.rows = append(c.rows, "  "+th.accent(iconState)+" "+th.dim(busy))
				default:
					c.rows = append(c.rows, "  "+th.dim("· "+busy))
				}
			}
			sessState := st["session"]
			if sessState == "" {
				sessState = "busy"
			}
			step(sessState, "Creating the session…", "Session created")
			if k := st["key"]; k != "" {
				step("done", "", "Model calls use your "+providerName(runnerProvider)+" key ending "+k)
			}
			step(st["machine"], "Starting a machine…", "Machine started")
			step(st["agent"], "Starting the agent…", "Agent started")
			mu.Unlock()
			box := c.render(appTerminalWidth())
			scr.update(func() {
				scr.box, scr.prompt, scr.input, scr.cursor, scr.hint = box, "", nil, 0, ""
				scr.status, scr.spin = th.accent(iconState)+" Starting "+name+"…", true
			})
		}
		show()
		startObserver = func(step, detail string) {
			mu.Lock()
			switch {
			case step == "session":
				st["session"] = "done"
			case step == "key":
				st["key"] = detail
			case step == "machine":
				st["machine"] = "busy"
			case strings.HasPrefix(step, "phase:"):
				state := "done"
				if detail != "true" {
					state = "failed"
				}
				switch strings.TrimPrefix(step, "phase:") {
				case "create_guest", "machine":
					st["machine"] = state
				case "start_agent", "supervisor", "agent":
					st["agent"] = state
				}
			}
			mu.Unlock()
			show()
		}
		saved := appSink
		appSink = func(string) {} // the observer has it; anything else is the step log
		rec, p, _, err := startAgentSession(cr, "", name, keyArg, nil, "")
		appSink, startObserver = saved, nil
		scr.update(func() { scr.box, scr.status, scr.spin = nil, "", false })
		width := appTerminalWidth()
		if err != nil {
			ce := classify(err)
			if ce.Kind == "key_ambiguous" && keyArg == "" {
				k, ok := chooseKey(cr, scr, keys, name)
				if ok && k != "" {
					keyArg = k
					continue
				}
				return nil, false
			}
			next := ce.NextAction
			if rec.ID != "" {
				next = "Session " + rec.ID + " was created before this happened; it is in your list. " + next
			}
			scr.Print(errorLines("start "+name, ce.Message, strings.TrimSpace(next), width))
			return nil, false
		}
		if !p.Ready && !p.Supervised {
			scr.Print(errorLines("start "+name, "the agent did not start: "+p.Note, "Session "+rec.ID+" was created; it is in your list.", width))
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
		outcome := fmt.Sprintf("%s is ready in session %s.", name, sessLabel(sess))
		if !p.Ready {
			outcome = fmt.Sprintf("%s is running in session %s and waits for its first instruction.", name, sessLabel(sess))
		}
		scr.Print(notice(iconDone, toneOK, outcome, width))
		a := agentRow{ID: p.AgentID, Name: name, SessionID: rec.ID, IsPrimary: true}
		return &homeRow{sess: sess, agent: &a}, true
	}
}

// providerName is a key provider as a person writes it.
func providerName(p string) string {
	switch p {
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	}
	return p
}

// chooseKey asks which of the account's usable keys a new agent calls with,
// as a list; when the keys cannot be listed, it asks for the key's id. A key
// is never chosen for the person.
func chooseKey(cr hostedCreds, scr *screen, keys <-chan keyEvent, name string) (string, bool) {
	all, err := fetchKeys(cr)
	var usable []customerKey
	for _, k := range all {
		if k.Provider == runnerProvider && k.Enabled {
			usable = append(usable, k)
		}
	}
	c := card{title: "New agent", tone: toneNeutral, icon: iconState,
		head: "Which key should " + name + " call the model with?",
		lines: []string{fmt.Sprintf("You have %s, so you choose. Nothing has been created yet.",
			plural(len(usable), "enabled "+providerName(runnerProvider)+" key", "enabled "+providerName(runnerProvider)+" keys"))}}
	if err != nil || len(usable) < 2 {
		c.lines = []string{"Type the key's id (ks key list shows them). Nothing has been created yet."}
		hint := keyHintsFit(appTerminalWidth()-2, [2]string{"Enter", "use it"}, [2]string{"Esc", "back"})
		return askCard(scr, keys, c.render(appTerminalWidth()), "Key id  ", "", hint)
	}
	sel := 0
	render := func() {
		c.rows = nil
		for i, k := range usable {
			mark := "  "
			label := cell(sanitize(firstNonEmpty(k.Alias, k.ID)), 16)
			if i == sel {
				mark, label = th.accent(iconSelected)+" ", th.accentBold(label)
			}
			c.rows = append(c.rows, mark+label+"  "+th.dim(cell(k.ID, 22))+"  "+th.dim("ends in "+k.Last4))
		}
		c.actions = [][2]string{{"↑↓", "choose"}, {"Enter", "use this key"}, {"Esc", "back"}}
		box := c.render(appTerminalWidth())
		scr.update(func() { scr.box, scr.prompt, scr.hint, scr.status, scr.spin = box, "", "", "", false })
	}
	render()
	for k := range keys {
		switch {
		case k.kind == keyUp || k.kind == keyRune && k.r == 'k':
			sel = max(0, sel-1)
		case k.kind == keyDown || k.kind == keyRune && k.r == 'j':
			sel = min(len(usable)-1, sel+1)
		case k.kind == keyEnter:
			scr.setBox(nil)
			return usable[sel].ID, true
		case k.kind == keyEsc, k.kind == keyCtrlC, k.kind == keyRune && k.r == 'q':
			scr.setBox(nil)
			return "", false
		}
		render()
	}
	return "", false
}
