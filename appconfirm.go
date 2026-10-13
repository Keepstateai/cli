package main

// appconfirm.go: a step that changes or removes something, asked inside the
// app, is a confirm card -- what will happen in a few plain sentences, then
// y to do it, n to keep things as they are, d for everything the service
// said. Nobody retypes a command with --confirm: on y the app runs the same
// command with the same confirmation the command line takes (the same plan,
// the same confirm token), so the service's rules are unchanged, and nothing
// happens without the y.

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
)

// confirmModal is a confirm card that owns y, n (or Esc) and d while it is up.
type confirmModal struct {
	c       card
	yes     func()
	no      func()
	details func() // d; nil when there is nothing more to show
}

// windowCtx is what a step started from an agent's window needs of it.
type windowCtx struct {
	cr      hostedCreds
	scr     *screen
	sess    inventoryRow
	agent   agentRow
	mu      sync.Mutex
	modal   *confirmModal
	busy    func(string) // show a step in progress on the status line ("" ends it)
	restore func()       // put the window's own live region back
}

func (w *windowCtx) current() *confirmModal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.modal
}

// ask puts a confirm card up in place of the input.
func (w *windowCtx) ask(m *confirmModal) {
	w.mu.Lock()
	w.modal = m
	w.mu.Unlock()
	box := m.c.render(appTerminalWidth())
	w.scr.update(func() {
		w.scr.box, w.scr.prompt, w.scr.hint, w.scr.suggest = box, "", "", nil
		w.scr.status, w.scr.spin = "", false
	})
}

// key gives a key to the card that is up; false when none is.
func (w *windowCtx) key(k keyEvent) bool {
	m := w.current()
	if m == nil {
		return false
	}
	closeIt := func() {
		w.mu.Lock()
		w.modal = nil
		w.mu.Unlock()
		w.restore()
	}
	switch {
	case k.kind == keyRune && (k.r == 'y' || k.r == 'Y'):
		closeIt()
		go m.yes()
	case k.kind == keyRune && (k.r == 'n' || k.r == 'N'), k.kind == keyEsc, k.kind == keyCtrlC:
		closeIt()
		if m.no != nil {
			m.no()
		}
	case k.kind == keyRune && (k.r == 'd' || k.r == 'D') && m.details != nil:
		m.details()
	}
	return true
}

// appIntercept takes the / commands the app asks about itself: deleting a
// session (ks kill, ks session delete without a plan to execute) and stopping
// an agent without --confirm. It answers false for every other command.
func appIntercept(w *windowCtx, c *Command, argv []string) bool {
	if c == nil || hasHelp(argv) {
		return false
	}
	pos := positionals(c, argv[len(c.Path):])
	switch c.Name() {
	case "kill", "session delete":
		if hasFlag(argv, "execute") || len(pos) == 0 {
			return false
		}
		go appDeleteFlow(w, pos[0], c.Name() == "kill", argv)
		return true
	case "agent stop":
		if hasFlag(argv, "confirm") || len(pos) == 0 {
			return false
		}
		appStopFlow(w, pos[0], argv)
		return true
	}
	return false
}

// positionals are the arguments that are not flags or flag values.
func positionals(c *Command, rest []string) []string {
	var out []string
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			if f := c.flagNamed(strings.TrimLeft(rest[i], "-")); f != nil && f.Kind != flagBool && !strings.Contains(rest[i], "=") {
				i++
			}
			continue
		}
		out = append(out, rest[i])
	}
	return out
}

// appDeleteFlow plans a session's deletion (which changes nothing), shows the
// plan as a confirm card, and on y executes exactly that plan.
func appDeleteFlow(w *windowCtx, target string, kill bool, argv []string) {
	width := appTerminalWidth()
	w.busy("Planning the deletion of session " + target + "…")
	sess := w.sess
	if target != agentSessionID(w.sess) && target != w.sess.ShortID && target != w.sess.ID {
		r, err := resolveSession(w.cr, target)
		if err != nil {
			w.busy("")
			w.scr.Print(errorBlock("delete session "+target, err, width))
			return
		}
		sess = r
	}
	label := sessLabel(sess)
	if sess.RecordID == "" {
		w.busy("")
		if !kill {
			w.scr.Print(errorLines("delete session "+label, "it is a plain session, with no record to delete", "/kill "+sess.ShortID+" destroys its machine.", width))
			return
		}
		w.ask(&confirmModal{
			c: card{title: "Kill session", tone: toneBad, icon: iconWarn,
				head:    "Kill session " + label + "?",
				lines:   []string{"Its machine is destroyed and its session time stops. Without a saved checkpoint it is refused, unless you add --force.", "This cannot be undone."},
				actions: [][2]string{{th.bad("y"), "kill it"}, {"n", "keep it"}}},
			yes: func() { appRunConfirmed(w, argv) },
			no: func() {
				w.scr.Print(dimNotice(iconState, toneDim, "Kept session "+label+". Nothing was destroyed.", width))
			},
		})
		return
	}
	var env struct {
		Data struct {
			ID        string         `json:"id"`
			ExpiresAt string         `json:"expires_at"`
			Plan      map[string]any `json:"plan"`
		} `json:"data"`
	}
	err := hostedCall(w.cr, "POST", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/deletion-plan", map[string]any{}, &env)
	w.busy("")
	if err != nil {
		w.scr.Print(errorBlock("plan the deletion of session "+label, err, width))
		return
	}
	d := env.Data
	c := card{title: "Delete session", tone: toneBad, icon: iconWarn,
		head:    "Delete session " + label + "?",
		lines:   deletionSummary(d.Plan),
		actions: [][2]string{{th.bad("y"), "delete"}, {"n", "keep it"}, {"d", "whole plan"}}}
	if cost := planCost(d.Plan); cost != "" {
		c.facts = append(c.facts, cardFact{"Cost", sentence(cost)})
	}
	if d.ExpiresAt != "" {
		c.facts = append(c.facts, cardFact{"Plan expires", approvalWhen(d.ExpiresAt)})
	}
	here := sess.RecordID == agentSessionID(w.sess)
	w.ask(&confirmModal{c: c,
		yes: func() {
			w.busy("Deleting session " + label + "…")
			data, ce := runSlashJSON([]string{"session", "delete", sess.RecordID, "--execute", d.ID, "--confirm", sess.RecordID})
			w.busy("")
			if ce != nil {
				w.scr.Print(errorLines("delete session "+label, ce.Message, asSlash(ce.NextAction), width))
				return
			}
			w.scr.Print(deletedLines(label, data, width))
			if here {
				w.scr.Print(dimNotice(iconState, toneDim, "This window's session is gone. /home goes back to your agents.", width))
			}
		},
		no: func() {
			w.scr.Print(dimNotice(iconState, toneDim, "Kept session "+label+". Nothing was deleted.", width))
		},
		details: func() {
			var b bytes.Buffer
			printDeletionPlan(&b, d.ID, sess.ShortID, d.ExpiresAt, d.Plan)
			inner := cardWidth(width) - 4
			var rows []string
			for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
				lead := l[:len(l)-len(strings.TrimLeft(l, " "))]
				rows = append(rows, wrapHang(lead, strings.Repeat(" ", 13), th.dim(strings.TrimLeft(l, " ")), inner)...)
			}
			w.scr.Print("\n" + strings.Join(card{title: "Whole deletion plan", tone: toneDim, rows: rows}.render(width), "\n"))
		},
	})
}

// deletedLines is a deletion's answer in the app: what was removed, then the
// machine's stop and the saved content's standing, each as the service
// reported it (a stop it did not report is not assumed).
func deletedLines(label string, data json.RawMessage, width int) string {
	var d struct {
		RuntimeCleanup json.RawMessage `json:"runtime_cleanup"`
		Retention      map[string]any  `json:"retention"`
		TasksCancelled int64           `json:"tasks_cancelled"`
		AgentsRemoved  int64           `json:"agents_removed"`
		ResultsDeleted int64           `json:"results_deleted"`
	}
	_ = json.Unmarshal(data, &d)
	var cleanup struct {
		RuntimeStop *struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"runtime_stop"`
		ContentPurge *struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"content_purge"`
	}
	_ = json.Unmarshal(d.RuntimeCleanup, &cleanup)
	var parts []string
	if d.AgentsRemoved > 0 {
		parts = append(parts, plural(int(d.AgentsRemoved), "agent", "agents")+" removed")
	}
	if d.TasksCancelled > 0 {
		parts = append(parts, plural(int(d.TasksCancelled), "instruction", "instructions")+" cancelled")
	}
	if d.ResultsDeleted > 0 {
		parts = append(parts, plural(int(d.ResultsDeleted), "result", "results")+" deleted")
	}
	head := "Deleted session " + label + "."
	if len(parts) > 0 {
		head = "Deleted session " + label + ": " + joinAnd(parts) + "."
	}
	lines := []string{notice(iconDone, toneOK, head, width)}
	sub := func(label, text string) {
		lines = append(lines, strings.Join(wrapHang("    "+th.dim(label)+" ", strings.Repeat(" ", 4+displayWidth(label)+1), text, min(width, cardMax)), "\n"))
	}
	said := func(state, detail string) string {
		if strings.TrimSpace(detail) != "" {
			return sentence(sanitize(detail))
		}
		return sentence(sanitize(strings.ReplaceAll(figure(state), "_", " ")))
	}
	if r := cleanup.RuntimeStop; r != nil {
		sub("Machine:", said(r.State, r.Detail))
	} else {
		sub("Machine:", "The service did not say whether it stopped, so it is not assumed stopped.")
	}
	if c := cleanup.ContentPurge; c != nil {
		sub("Saved content:", said(c.State, c.Detail))
	} else {
		sub("Saved content:", "The service did not report it, so it is not assumed erased.")
	}
	return strings.Join(lines, "\n")
}

// appStopFlow asks before stopping an agent, in the words of the command's
// own dialog, and on y stops it by name, as --confirm would.
func appStopFlow(w *windowCtx, name string, argv []string) {
	width := appTerminalWidth()
	w.ask(&confirmModal{
		c: card{title: "Stop agent", tone: toneWarn, icon: iconWarn,
			head: "Stop " + sanitize(name) + "?",
			lines: []string{
				"Its current instruction is asked to stop at the next safe point, and nothing more from its queue starts until you release the hold.",
				"The session keeps running, so it may still be charged. To save and pause the whole session instead, use /pause.",
			},
			actions: [][2]string{{th.bad("y"), "stop it"}, {"n", "keep it working"}}},
		yes: func() {
			w.busy("Stopping " + sanitize(name) + "…")
			data, ce := runSlashJSON(append(append([]string{}, argv...), "--confirm", name))
			w.busy("")
			if ce != nil {
				w.scr.Print(errorLines("stop "+sanitize(name), ce.Message, asSlash(ce.NextAction), width))
				return
			}
			w.scr.Print(stoppedLines(name, data, width))
		},
		no: func() {
			w.scr.Print(dimNotice(iconState, toneDim, "Not stopped. "+sanitize(name)+" keeps working.", width))
		},
	})
}

// stoppedLines is an agent stop's answer in the app.
func stoppedLines(name string, data json.RawMessage, width int) string {
	var d struct {
		Interrupted  string   `json:"interrupt_requested_task"`
		HeldTasks    []string `json:"held_tasks"`
		SessionState string   `json:"session_runtime_state"`
		Charges      string   `json:"charges"`
		Resume       string   `json:"resume"`
	}
	_ = json.Unmarshal(data, &d)
	var body []string
	if d.Interrupted != "" {
		body = append(body, "Its current instruction is stopping; it is not confirmed stopped yet.")
	} else {
		body = append(body, "No instruction was running.")
	}
	body = append(body, "Its queue is held: "+plural(len(d.HeldTasks), "instruction waits", "instructions wait")+", and nothing more starts.")
	if d.Charges != "" {
		body = append(body, strings.TrimSuffix(sentence(sanitize(d.Charges)), ".")+". /pause saves and pauses it.")
	}
	if d.Resume != "" {
		body = append(body, "To continue: "+asSlash(sanitize(d.Resume)))
	}
	lines := []string{notice(iconDone, toneOK, "Stop requested for "+sanitize(name)+".", width)}
	for _, b := range body {
		lines = append(lines, strings.Join(wrapHang("    ", "    ", th.dim(sentence(b)), min(width, cardMax)), "\n"))
	}
	return strings.Join(lines, "\n")
}

// appConfirmFromCommand is the confirm card for any other command that
// refused for want of a confirmation: what it does, and on y the command
// that confirms it, run as the command line would run it.
func appConfirmFromCommand(w *windowCtx, argv []string, sc *slashConfirm) {
	next := confirmedArgv(argv, sc.next)
	if next == nil {
		w.scr.Print(errorLines("run /"+strings.Join(argv, " "), sc.message, asSlash(sc.next), appTerminalWidth()))
		return
	}
	what := "/" + strings.Join(quoteWords(argv[:min(len(argv), 2)]), " ")
	summary := ""
	if c, _, _ := lookup(registry, argv); c != nil {
		what = "/" + c.Name()
		summary = c.Summary
	}
	c := card{title: "Confirm", tone: toneWarn, icon: iconWarn,
		head:    what + " needs your yes.",
		lines:   []string{"It changes something, so it runs only when you confirm it. Nothing has been done yet."},
		actions: [][2]string{{"y", "run it"}, {"n", "cancel"}}}
	if summary != "" {
		c.facts = append(c.facts, cardFact{"What it does", sentence(summary)})
	}
	c.facts = append(c.facts, cardFact{"Runs", "/" + strings.Join(quoteWords(next), " ")})
	w.ask(&confirmModal{c: c,
		yes: func() { appRunConfirmed(w, next) },
		no: func() {
			w.scr.Print(dimNotice(iconState, toneDim, "Cancelled. Nothing was done.", appTerminalWidth()))
		},
	})
}

// confirmedArgv is the command that confirms: the command a refusal names in
// full ("ks result apply r1 --confirm 3f2a"), or the original with the flags
// it names ("--confirm main").
func confirmedArgv(argv []string, next string) []string {
	next = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(next), "pass "))
	words, err := splitWords(next)
	if err != nil || len(words) == 0 {
		return nil
	}
	if words[0] == "ks" {
		return words[1:]
	}
	if strings.HasPrefix(words[0], "--") {
		return append(append([]string{}, argv...), words...)
	}
	return nil
}

// appRunConfirmed runs a confirmed command, streaming it like any / command.
func appRunConfirmed(w *windowCtx, argv []string) {
	w.busy("Running /" + strings.Join(quoteWords(argv), " ") + "…")
	runSlash(w.scr, argv, make(chan struct{}))
	w.busy("")
}
