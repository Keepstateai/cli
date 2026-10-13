package main

// appstates.go: the cards for what stands between a person and an agent's
// window -- a session that is not running (paused, out of credit), resuming
// it -- and the permission card and status line of the window itself.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- a session that is not running -----------------------------------------------

// parkedCard is a session that is not running, in this client's own words
// for the state it knows. The billing disclosure the service attaches to its
// resume action is always on it when resuming is offered, so a person sees
// what resuming costs before pressing r. The service's own note is shown
// when this client does not know the state, and otherwise on d (details).
func parkedCard(sess inventoryRow, a agentRow, w *agentWindow, funds, details bool) (card, bool) {
	rt := w.Runtime
	name := sanitize(a.Name)
	resume, resumable := findAction(rt.Actions, "resume_session")
	notWoken := ""
	if !rt.Woken {
		notWoken = " Opening it did not wake it."
	}
	c := card{tone: toneWarn, icon: iconPaused}
	known := true
	switch {
	case funds:
		c.title = "Out of credit"
		c.head = name + "'s session is paused: your account ran out of credit."
		c.lines = []string{"Its work is saved and nothing is running." + notWoken,
			"Add credit in the KeepState console, then press r to resume. Adding credit alone does not resume it."}
	case rt.State == "parked":
		c.title = "Session paused"
		c.head = name + "'s session is paused." + notWoken
		c.lines = []string{"Its work is saved and nothing is running."}
	case rt.State == "stopping" || rt.State == "checkpointing":
		c.title = "Session stopping"
		c.head = name + "'s session is saving and stopping." + notWoken
	case rt.State == "resuming" || rt.State == "provisioning":
		c.title, c.tone, c.icon = "Session starting", toneNeutral, iconState
		c.head = name + "'s session is starting. Open it again in a moment."
	case rt.State == "failed" || rt.State == "dead" || rt.State == "recovery_required":
		c.title, c.tone, c.icon = "Session not running", toneBad, iconFailed
		c.head = name + "'s session is not running: " + strings.ToLower(sessionWord(rt.State, "")) + "." + notWoken
	default:
		known = false
		c.title, c.tone, c.icon = "Session not running", toneWarn, iconState
		c.head = name + "'s session is not running (" + stateLabel("session_runtime", rt.State) + ")." + notWoken
	}
	c.facts = []cardFact{{"Agent", name}, {"Session", sessLabel(sess)}}
	if funds && a.Activity != "" {
		c.facts = append(c.facts, cardFact{"Last activity", activityWord(a.Activity) + ", as it was when it paused"})
	}
	if rt.LastSavedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, rt.LastSavedAt); err == nil {
			c.facts = append(c.facts, cardFact{"Last saved", shortDuration(max(0, time.Since(t))) + " ago"})
		}
	}
	if resumable {
		if d := strings.TrimSpace(resume.Discloses); d != "" {
			c.facts = append(c.facts, cardFact{"Billing", sentence(sanitize(d))})
		}
	} else {
		c.lines = append(c.lines, "It can't be resumed from here.")
	}
	note := strings.TrimSpace(rt.Note)
	if note != "" && (details || !known) {
		c.facts = append(c.facts, cardFact{"Details", sentence(sanitize(note))})
	}
	if resumable {
		c.actions = append(c.actions, [2]string{"r", "resume (billing starts)"})
	}
	if note != "" && known {
		if details {
			c.actions = append(c.actions, [2]string{"d", "hide details"})
		} else {
			c.actions = append(c.actions, [2]string{"d", "details"})
		}
	}
	c.actions = append(c.actions, [2]string{"Esc", "back"})
	return c, resumable
}

func findAction(actions []openAction, name string) (openAction, bool) {
	for _, ac := range actions {
		if ac.Action == name {
			return ac, true
		}
	}
	return openAction{}, false
}

// appParked is an agent whose session is not running: opening never wakes
// it; r resumes it because the person asked, and only then.
func appParked(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow, w *agentWindow) bool {
	funds := a.SessionRuntime.funds() || w.Agent.SessionRuntime.funds() || rowFundsParked(sess)
	details := false
	problem := ""
	draw := func() bool {
		c, resumable := parkedCard(sess, a, w, funds, details)
		c.err = problem
		showCard(scr, c)
		return resumable
	}
	resumable := draw()
	for k := range keys {
		switch {
		case k.kind == keyRune && (k.r == 'r' || k.r == 'R') && resumable:
			c, _ := parkedCard(sess, a, w, funds, details)
			c.actions, c.err = nil, "" // the status line says it is resuming
			showCard(scr, c)
			scr.update(func() { scr.status, scr.spin = th.accent(iconState)+" Resuming "+sessLabel(sess)+"…", true })
			r, err := postResume(cr, agentSessionID(sess))
			scr.update(func() { scr.status, scr.spin = "", false })
			if err != nil {
				ce := classify(err)
				problem = "Couldn't resume the session. " + sentence(ce.Message)
				if ce.NextAction != "" {
					problem += " " + sentence(ce.NextAction)
				}
				resumable = draw()
				continue
			}
			scr.setBox(nil)
			state := fmt.Sprint(firstNonNil(r["runtime_state"], "resuming"))
			scr.Print(notice(iconDone, toneOK, fmt.Sprintf("Resumed %s (%s).", sessLabel(sess), strings.ToLower(sessionWord(state, ""))), appTerminalWidth()))
			return appAwaitRunning(cr, scr, keys, sess, a)
		case k.kind == keyRune && (k.r == 'd' || k.r == 'D'):
			details = !details
			resumable = draw()
		case k.kind == keyEsc, k.kind == keyCtrlC, k.kind == keyLeft, k.kind == keyRune && (k.r == 'q' || k.r == 'Q'):
			scr.setBox(nil)
			return false
		}
	}
	return true
}

func firstNonNil(v any, def string) any {
	if v == nil || v == "" {
		return def
	}
	return v
}

// appAwaitRunning waits for a resumed session to run, then opens the agent's
// window. Opening never wakes anything: it is asked again until the session
// runs (or the person stops waiting, which leaves it resuming).
func appAwaitRunning(cr hostedCreds, scr *screen, keys <-chan keyEvent, sess inventoryRow, a agentRow) bool {
	start := time.Now()
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	status := func() {
		scr.update(func() {
			scr.box, scr.prompt, scr.input, scr.cursor = nil, "", nil, 0
			scr.status = th.accent(iconState) + fmt.Sprintf(" Waiting for %s to start… %s", sessLabel(sess), shortDuration(time.Since(start)))
			scr.spin = true
			scr.hint = keyHintsFit(appTerminalWidth()-2, [2]string{"Esc", "stop waiting (it keeps starting)"})
		})
	}
	stop := func() { scr.update(func() { scr.status, scr.spin, scr.hint = "", false, "" }) }
	status()
	for {
		select {
		case k, ok := <-keys:
			if !ok {
				return true
			}
			if k.kind == keyEsc || k.kind == keyCtrlC || k.kind == keyRune && k.r == 'q' {
				stop()
				scr.Print(dimNotice(iconState, toneDim, sessLabel(sess)+" keeps starting. Open "+sanitize(a.Name)+" again from your agents in a moment.", appTerminalWidth()))
				return false
			}
		case <-tick.C:
			w, err := openAgent(cr, sess, a, false)
			if err != nil || w.Runtime == nil || w.Runtime.Running {
				stop()
				return appAgentWith(cr, scr, keys, sess, a, w, err)
			}
			switch w.Runtime.State {
			case "resuming", "provisioning", "parked", "stopping", "checkpointing":
			default: // it will not run by waiting: say what it is
				stop()
				return appParked(cr, scr, keys, sess, a, w)
			}
			if time.Since(start) > 3*time.Minute {
				stop()
				scr.Print(notice(iconWarn, toneWarn, sessLabel(sess)+" has not started after 3 minutes. It keeps starting; open "+sanitize(a.Name)+" again from your agents later.", appTerminalWidth()))
				return false
			}
			status()
		}
	}
}

// ---- prompt box and status line ---------------------------------------------------

// approvalWants is what a request asks for, as a person reads it.
func approvalWants(kind string) string {
	switch kind {
	case "shell", "bash", "command":
		return "run a command"
	case "tool":
		return "use a tool"
	case "network", "web":
		return "reach the network"
	case "file", "write", "edit":
		return "change files"
	}
	return "do something that needs your permission"
}

// approvalWhen is an expiry as a person reads it: the time, and how far off.
func approvalWhen(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return figure(ts)
	}
	local := t.Local()
	at := local.Format("15:04")
	if local.YearDay() != time.Now().YearDay() || local.Year() != time.Now().Year() {
		at = local.Format("Jan 2 15:04")
	}
	if d := time.Until(t); d > 0 {
		return at + " (in " + shortDuration(d) + ")"
	}
	return at + " (passed)"
}

// approvalBox is the permission card: what the agent wants, the exact action
// (every part that makes it a different action: its kind, its words and its
// arguments), what it is for, and the three keys. What is shown here is what
// a decision is compared against.
func approvalBox(pending []approvalRow, width int) []string {
	if len(pending) == 0 {
		return nil
	}
	ap := pending[0]
	title := "Permission needed"
	if len(pending) > 1 {
		title += fmt.Sprintf(" · 1 of %d waiting", len(pending))
	}
	c := card{title: title, tone: toneInfo, icon: iconNeedsYou,
		head: "The agent wants to " + approvalWants(ap.Kind) + ". It waits until you decide."}
	c.facts = []cardFact{{"Action", sanitize(figure(ap.Kind) + ": " + figure(ap.Summary))}}
	if args := sanitize(compactPayload(json.RawMessage(ap.ArgumentsJSON))); args != "" {
		inner := cardWidth(width) - 4
		lines := wrapText(args, max(20, inner-16))
		if len(lines) > 3 {
			args = strings.Join(lines[:3], " ") + " … (d shows all of it)"
		}
		c.facts = append(c.facts, cardFact{"Arguments", args})
	}
	if ap.ExpiresAt != "" {
		c.facts = append(c.facts, cardFact{"Expires", approvalWhen(ap.ExpiresAt)})
	}
	c.actions = [][2]string{{th.ok("y"), "allow once"}, {th.bad("n"), "deny"}, {"d", "details"}}
	return c.render(width)
}

// approvalDetails is d on a permission request: everything the service says
// about it, kept in the conversation.
func approvalDetails(ap approvalRow, width int) []string {
	c := card{title: "Permission details", tone: toneInfo, icon: iconNeedsYou,
		head: "Request " + ap.ID + ": the agent wants to " + approvalWants(ap.Kind) + "."}
	c.facts = []cardFact{{"Kind", sanitize(figure(ap.Kind))}, {"Action", sanitize(figure(ap.Summary))}}
	if args := sanitize(compactPayload(json.RawMessage(ap.ArgumentsJSON))); args != "" {
		c.facts = append(c.facts, cardFact{"Arguments", args})
	}
	for _, f := range ap.Affects {
		c.facts = append(c.facts, cardFact{"Affects", sanitize(f)})
	}
	if ap.CostImplication != "" {
		c.facts = append(c.facts, cardFact{"Cost", sentence(sanitize(ap.CostImplication))})
	}
	if ap.RequestedForTask != "" {
		c.facts = append(c.facts, cardFact{"For", "instruction " + ap.RequestedForTask})
	}
	c.facts = append(c.facts, cardFact{"Expires", approvalWhen(ap.ExpiresAt)})
	c.lines = []string{"y allows it once, n denies it. Nothing is decided until you press one."}
	return append([]string{""}, c.render(width)...)
}

// appStatusParts is the status line's parts, the most important first: the
// state (as the service labels it, with its age; stale only when the
// service says so), what waits for you, who controls, the queue, the last
// save and the spend (never $0 for a figure that is unavailable).
func appStatusParts(v liveViewDoc, held bool) []string {
	var st string
	if v.Status != nil {
		st = iconState + " " + sanitize(figure(v.Status.Label))
		switch {
		case v.Status.Stale:
			st += " (stale: the last known status)"
		case v.Status.AgeSeconds != nil && *v.Status.AgeSeconds > 15:
			st += ", unchanged for " + shortDuration(time.Duration(*v.Status.AgeSeconds)*time.Second)
		}
	} else {
		st = iconState + " " + activityWord(v.Header.Activity)
	}
	if v.Status != nil && v.Status.Stale {
		st = th.bad(st)
	} else {
		st = th.activityColor(v.Header.Activity, st)
	}
	parts := []string{st}
	if v.Footer.PendingApprovals != nil && *v.Footer.PendingApprovals > 0 {
		parts = append(parts, th.warn(plural(*v.Footer.PendingApprovals, "request waits for you", "requests wait for you")))
	}
	if held {
		parts = append(parts, th.dim("you control"))
	} else {
		parts = append(parts, th.warn("watching"))
	}
	if v.Footer.Queued != nil && *v.Footer.Queued > 0 {
		parts = append(parts, th.dim(fmt.Sprintf("%d queued", *v.Footer.Queued)))
	}
	if v.Header.LastSavedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, v.Header.LastSavedAt); err == nil {
			parts = append(parts, th.dim("saved "+shortDuration(time.Since(t))+" ago"))
		}
	} else {
		parts = append(parts, th.dim("not saved yet"))
	}
	if u := v.Usage; u != nil {
		var tokens int64
		for _, e := range u.Entries {
			tokens += e.Tokens
		}
		switch {
		case u.ModelSpend != nil:
			parts = append(parts, th.dim(fmt.Sprintf("model $%.2f", float64(*u.ModelSpend)/1e6)))
		case tokens > 0:
			parts = append(parts, th.dim(commas(tokens)+" tokens"))
		}
	}
	return parts
}

func appStatusLine(v liveViewDoc, held bool) string {
	return strings.Join(appStatusParts(v, held), th.dim(" · "))
}
