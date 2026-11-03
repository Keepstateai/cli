// herdr_pane.go: the KeepState plugin's panes in herdr. Each is ks itself
// in a herdr pane, drawn in the app's card style.
//
//	ks herdr pane agents     your sessions and their agents with each one's
//	                         status: Enter opens the agent's window in a new
//	                         herdr tab (tab create, then pane run), c the
//	                         console at the agent, s the session's saved
//	                         points, r the receipts, q closes
//	ks herdr pane receipts   your recent Cruise jobs, the running ones
//	                         followed: when one finishes, its receipt (or
//	                         its error card) is shown and herdr is asked to
//	                         notify; Enter shows a job's card
//
// Every word the service sends is shown through sanitize; the commands typed
// into a new tab are built only from names safe to type as they are.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

type paneScreen struct {
	scr  *screen
	keys chan keyEvent
	exit func(int)
}

func startPane() *paneScreen {
	restore, err := rawTerminal()
	if err != nil {
		fail(&cliError{Code: exitFailed, Kind: "no_terminal", Message: "a herdr pane needs an interactive terminal (" + err.Error() + ")", NextAction: "ks agent list --all"})
	}
	enableAppTheme()
	p := &paneScreen{scr: newScreen(os.Stdout), keys: make(chan keyEvent, 64)}
	p.exit = func(code int) {
		runExitHooks()
		p.scr.close()
		restore()
		os.Exit(code)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	go func() {
		for s := range sig {
			if s == syscall.SIGWINCH {
				p.scr.update(func() {})
				continue
			}
			p.exit(exitOK)
		}
	}()
	go readKeys(os.Stdin, p.keys)
	return p
}

func (p *paneScreen) show(c card) {
	box := c.render(appTerminalWidth())
	p.scr.update(func() {
		p.scr.box, p.scr.prompt, p.scr.input, p.scr.cursor, p.scr.hint = box, "", nil, 0, ""
		p.scr.status, p.scr.spin = "", false
	})
}

func isQuit(k keyEvent) bool {
	return k.kind == keyCtrlC || k.kind == keyCtrlD || (k.kind == keyRune && (k.r == 'q' || k.r == 'Q'))
}

func isBack(k keyEvent) bool {
	return k.kind == keyEsc || k.kind == keyLeft
}

// ---- the agents pane ----------------------------------------------------------

func herdrPaneAgents(cr hostedCreds, inv *Invocation) {
	p := startPane()
	paneAgents(cr, p)
	p.exit(exitOK)
}

func paneAgents(cr hostedCreds, p *paneScreen) {
	rows, err := loadHome(cr)
	problem, note := "", ""
	if err != nil {
		problem = "Couldn't read your agents. " + sentence(errText(err)) + " It is read again every few seconds."
	}
	sel := 0
	for sel < len(rows) && rows[sel].agent == nil {
		sel++
	}
	if sel == len(rows) {
		sel = 0
	}
	render := func() {
		width, height := appTerminalWidth(), appTerminalHeight()
		c := homeCard(rows, sel, problem, width, height)
		c.title = "KeepState"
		c.actions = [][2]string{{"↑↓", "choose"}, {"Enter", "open in a new tab"}, {"c", "console"}, {"s", "saved points"}, {"r", "receipts"}, {"q", "close"}}
		if note != "" {
			c.lines = append(c.lines, note)
		}
		p.show(c)
	}
	reload := func() {
		fresh, err := loadHome(cr)
		if err != nil {
			problem = "Couldn't read your agents. " + sentence(errText(err)) + " It is read again every few seconds."
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
	tick := time.NewTicker(5 * watchSecond())
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			reload()
			render()
		case k, ok := <-p.keys:
			if !ok || isQuit(k) {
				return
			}
			note = ""
			switch {
			case k.kind == keyUp || (k.kind == keyRune && k.r == 'k'):
				sel = max(0, sel-1)
			case k.kind == keyDown || (k.kind == keyRune && k.r == 'j'):
				sel = min(max(0, len(rows)-1), sel+1)
			case k.kind == keyEnter && sel < len(rows):
				note = paneOpenAgent(rows[sel])
			case k.kind == keyRune && k.r == 'c' && sel < len(rows):
				note = paneConsole(cr, rows[sel])
			case k.kind == keyRune && k.r == 's' && sel < len(rows):
				if paneSaved(cr, p, rows[sel].sess) {
					return
				}
			case k.kind == keyRune && k.r == 'r':
				if paneReceipts(cr, p, true) {
					return
				}
			}
			render()
		}
	}
}

// paneOpenAgent opens the selected agent's window in a new herdr tab, and
// answers what happened in a sentence.
func paneOpenAgent(r homeRow) string {
	if r.agent == nil {
		return th.dim("This session has no agent to open. Start one with ks run --agent.")
	}
	bin, _, inHerdr := herdrPane()
	cmd := "ks agent open " + r.agent.Name + " --session " + r.sess.ShortID
	if !inHerdr {
		return th.warn("This pane is not inside herdr, so it cannot open a tab. In a terminal: " + sanitize(cmd))
	}
	if _, err := openAgentInHerdrTab(bin, r.agent.Name, r.sess.ShortID); err != nil {
		return th.bad(sentence(sanitize(err.Error())))
	}
	return th.ok(iconDone + " Opened " + sanitize(r.agent.Name) + " in a new tab.")
}

func paneConsole(cr hostedCreds, r homeRow) string {
	frag := ""
	if r.agent != nil {
		frag = "#agent/" + agentSessionID(r.sess) + "/" + r.agent.ID
	}
	u := consoleAgentsURL(cr, frag)
	if openBrowser(u) {
		return th.ok(iconDone + " Opened the console in your browser.")
	}
	return th.warn("Open the console: " + u)
}

// paneSaved shows a session's saved points until Esc; true when the person
// asked to close the pane.
func paneSaved(cr hostedCreds, p *paneScreen, sess inventoryRow) bool {
	c := card{title: "Saved points · " + sessLabel(sess), tone: toneNeutral, actions: [][2]string{{"Esc", "back"}, {"q", "close"}}}
	if sess.RecordID == "" {
		c.head = "This session keeps no saved points a restore or fork can name."
	} else if cps, err := fetchCheckpoints(cr, agentSessionID(sess)); err != nil {
		c.err = "Couldn't read the saved points. " + sentence(errText(err))
	} else if len(cps) == 0 {
		c.head = "No saved points yet."
		c.lines = []string{"A session is saved when it pauses, and when you save it: ks checkpoint " + sess.ShortID + "."}
	} else {
		sort.SliceStable(cps, func(i, j int) bool { return cps[i].CreatedAt > cps[j].CreatedAt })
		if len(cps) > 15 {
			cps = cps[:15]
		}
		c.head = plural(len(cps), "saved point", "saved points") + ", newest first"
		inner := cardWidth(appTerminalWidth()) - 4
		for _, cp := range cps {
			use, tn := "usable", toneOK
			if !cp.usable() {
				use, tn = "not usable now", toneDim
			}
			when := sanitize(figure(cp.CreatedAt))
			if t, err := time.Parse(time.RFC3339Nano, cp.CreatedAt); err == nil {
				when = t.UTC().Format("2006-01-02 15:04 UTC")
			}
			line := "  " + th.bold(cell(sanitize(cp.ID), 14)) + "  " + cell(when, 22) + "  " + th.toned(tn, cell(use, 15)) + "  " + th.dim(sanitize(notRecorded(cp.Boundary)))
			c.rows = append(c.rows, fitWidth(line, inner))
		}
		c.lines = []string{"Their details: ks session checkpoints " + sess.ShortID + "."}
	}
	p.show(c)
	for k := range p.keys {
		if isQuit(k) {
			return true
		}
		if isBack(k) || k.kind == keyEnter {
			return false
		}
	}
	return true
}

// ---- the receipts pane --------------------------------------------------------

type paneJob struct {
	ID, State, Label, Goal, Created string
	status                          *jobLiveStatus
}

func fetchPaneJobs(cr hostedCreds) ([]paneJob, error) {
	var jobs []map[string]any
	if err := hostedCall(cr, "GET", inContext(cr, "/api/jobs"), nil, &jobs); err != nil {
		return nil, err
	}
	var out []paneJob
	for _, j := range jobs {
		id := jstr(j, "id")
		if !agentNameSafe.MatchString(id) {
			continue
		}
		out = append(out, paneJob{ID: id, State: jstr(j, "state"), Goal: jstr(j, "goal"), Created: jstr(j, "created_at")})
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Created > out[k].Created })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

// receiptsFollow is what the pane knows of each job between reads: its
// state at the last read, so a job seen to finish is noticed once, and a
// job that had already finished at the first read never is.
type receiptsFollow struct {
	seen  map[string]string
	first bool
}

func newReceiptsFollow() *receiptsFollow {
	return &receiptsFollow{seen: map[string]string{}, first: true}
}

// finished answers the jobs this read shows ending that the last read
// showed still going.
func (f *receiptsFollow) finished(jobs []paneJob) []string {
	var out []string
	for _, j := range jobs {
		was, known := f.seen[j.ID]
		if !f.first && jobEnded(j.State) && (!known || !jobEnded(was)) {
			out = append(out, j.ID)
		}
		f.seen[j.ID] = j.State
	}
	f.first = false
	return out
}

func herdrPaneReceipts(cr hostedCreds, inv *Invocation) {
	p := startPane()
	paneReceipts(cr, p, false)
	p.exit(exitOK)
}

// paneJobCard is a job's card in the pane: the receipt of an accepted job
// in short (the whole of it is ks cruise result), the card of one that
// failed, waits for review or was cancelled, and the live state of one
// still going. Every figure the service did not give reads unavailable.
func paneJobCard(st jobLiveStatus) card {
	id := st.JobID
	c := card{title: "Cruise job " + id, tone: toneNeutral, actions: [][2]string{{"Esc", "back to the list"}, {"q", "close"}}}
	spend := strings.ReplaceAll(moneyText(st.Spend.Model), " \u2014 ", "; ")
	switch r := st.Result; {
	case r != nil:
		c.icon, c.tone = iconDone, toneOK
		c.head = "Receipt: " + jobLabel(st)
		if r.Badge != badgeVerified {
			c.icon, c.tone = iconWarn, toneWarn
		}
		ch := pMap(r.Provenance, "checks")
		counts := pMap(ch, "counts")
		c.facts = []cardFact{
			{"Badge", paneBadge(r.Badge)},
			{"Checks", pStr(ch, "outcome") + ": " + pStr(counts, "passed") + " of " + pStr(counts, "collected") + " passed, " + pStr(counts, "failed") + " failed, " + pStr(counts, "skipped") + " skipped"},
			{"Attempt", pStr(r.Provenance, "attempt_id") + " on " + pStr(r.Provenance, "model")},
		}
		if cs := r.Changeset; cs != nil {
			c.facts = append(c.facts, cardFact{"Changes", fmt.Sprintf("%s (sha256 %s)", plural(cs.Changes, "file", "files"), short(notRecorded(bareHex(cs.SHA256))))})
		} else {
			c.facts = append(c.facts, cardFact{"Changes", "not served for this result"})
		}
		c.facts = append(c.facts, cardFact{"Artifact", fmt.Sprintf("%s bytes (sha256 %s)", commas(r.ArtifactBytes), short(notRecorded(r.ArtifactSHA)))}, cardFact{"Model spend", spend})
		for i, l := range r.Limitations {
			label := ""
			if i == 0 {
				label = "Limitations"
			}
			c.facts = append(c.facts, cardFact{label, sanitize(l)})
		}
		pr := pullRequestLine(id, r.PullRequest)
		var next []string
		if r.PullRequest == nil {
			c.facts = append(c.facts, cardFact{"Pull request", "none yet; KeepState opens pull requests once GitHub is connected"})
			next = []string{"Get the change now: ks cruise apply " + id, "Download the files: ks cruise result " + id + " --download"}
		} else {
			c.facts = append(c.facts, cardFact{"Pull request", strings.TrimPrefix(strings.TrimPrefix(pr, "Pull request"), ": ")})
			next = []string{"Download the files: ks cruise result " + id + " --download"}
		}
		next = append(next, th.dim("The whole receipt, with its provenance: ks cruise result "+id))
		for _, n := range next {
			c.rows = append(c.rows, "  "+n)
		}
	case st.State == "failed":
		_, _, words := jobStatusWords(st)
		c.icon, c.tone, c.head = iconFailed, toneBad, "Failed: "+words
		c.facts = []cardFact{{"Model spend", spend}}
		c.lines = []string{"Read why: ks cruise logs " + id}
	case st.State == "review":
		c.icon, c.tone, c.head = iconNeedsYou, toneWarn, "Waiting for your review: nothing runs until you decide"
		c.facts = []cardFact{{"State", jobLabel(st)}, {"Model spend", spend}}
		c.lines = []string{"Review it: ks cruise review " + id}
	case st.State == "cancelled":
		c.icon, c.tone, c.head = iconState, toneDim, "Cancelled; it has no result"
		c.facts = []cardFact{{"Model spend", spend}}
	default:
		_, _, words := jobStatusWords(st)
		if words == "" {
			words = "Status unavailable."
		}
		c.head = words
		c.facts = []cardFact{{"State", jobLabel(st)}, {"Model spend", spend}}
		c.lines = []string{"It is followed here; its receipt shows when it finishes."}
	}
	return c
}

// paneBadge is the badge as served and what it means, in the pane's words.
func paneBadge(badge string) string {
	if badge == badgeVerified {
		return "verified: the checks passed on exactly these bytes"
	}
	return badgeLine(badge)
}

// jobFinishedWords is a finished job's notification body.
func jobFinishedWords(st jobLiveStatus) string {
	_, _, words := jobStatusWords(st)
	return words
}

// jobStateWord is a job state in words, for a job whose status was not read.
func jobStateWord(state string) string {
	w := map[string]string{"queued": "Waiting to start", "running": "Working on it", "verifying": "Checking the result", "escalating": "Trying the next model",
		"review": "Waiting for your review", "accepted": "Accepted", "failed": "Failed", "cancelled": "Cancelled"}[state]
	if w == "" {
		return "Status unavailable"
	}
	return w
}

// paneReceipts runs the receipts list until q (true: close the pane) or,
// when it was opened from the agents pane, Esc (false: back).
func paneReceipts(cr hostedCreds, p *paneScreen, fromAgents bool) bool {
	status := newStatusEmitter(p.scr.raw, "", nil)
	defer status.leave()
	follow := newReceiptsFollow()
	var jobs []paneJob
	problem := ""
	sel := 0
	shown := "" // the job whose card is up, or none
	var shownCard card

	read := func() {
		fresh, err := fetchPaneJobs(cr)
		if err != nil {
			problem = "Couldn't read your jobs. " + sentence(errText(err)) + " They are read again every few seconds."
			return
		}
		problem = ""
		old := map[string]*jobLiveStatus{}
		for _, j := range jobs {
			old[j.ID] = j.status
		}
		for i := range fresh {
			j := &fresh[i]
			j.status = old[j.ID]
			// a job still going is followed; an ended one is read once
			_, known := follow.seen[j.ID]
			if !jobEnded(j.State) || (j.status != nil && !jobEnded(j.status.State)) || (!follow.first && !known) {
				if st, err := fetchJobStatus(cr, j.ID); err == nil {
					j.status = &st
					if st.State != "" {
						j.State = st.State
					}
				}
			}
			if j.status != nil {
				j.Label = j.status.StateLabel
				if state, kind, msg := jobStatusWords(*j.status); state != "" {
					status.report(oscStatus{State: state, ID: statusID(j.ID), Kind: kind, App: statusApp, Title: "Cruise job " + j.ID, Msg: msg})
				}
			}
		}
		jobs = fresh
		sel = min(sel, max(0, len(jobs)-1))
		for _, id := range follow.finished(jobs) {
			for _, j := range jobs {
				if j.ID != id || j.status == nil {
					continue
				}
				shown, shownCard = id, paneJobCard(*j.status)
				herdrNotify("Cruise job "+id+" finished", jobFinishedWords(*j.status), "done")
			}
		}
	}
	render := func() {
		width := appTerminalWidth()
		if shown != "" {
			p.show(shownCard)
			return
		}
		c := card{title: "KeepState receipts", tone: toneNeutral, err: problem}
		c.actions = [][2]string{{"↑↓", "choose"}, {"Enter", "its card"}}
		if fromAgents {
			c.actions = append(c.actions, [2]string{"Esc", "agents"})
		}
		c.actions = append(c.actions, [2]string{"q", "close"})
		if len(jobs) == 0 && problem == "" {
			c.head = "No Cruise jobs yet."
			c.lines = []string{"Start one with ks cruise run; this pane follows it and shows its receipt when it finishes."}
		}
		inner := cardWidth(width) - 4
		for i, j := range jobs {
			mark := "  "
			if i == sel {
				mark = th.accent(iconSelected) + " "
			}
			icon, tn := iconState, toneNeutral
			if j.status != nil {
				switch s, _, _ := jobStatusWords(*j.status); s {
				case "done":
					icon, tn = iconDone, toneOK
				case "error":
					icon, tn = iconFailed, toneBad
				case "blocked":
					icon, tn = iconNeedsYou, toneWarn
				case "idle":
					icon, tn = iconState, toneDim
				}
			}
			label := j.Label
			if label == "" {
				label = jobStateWord(j.State)
			}
			line := mark + th.toned(tn, icon) + " " + th.bold(cell(j.ID, 18)) + "  " + th.toned(tn, cell(sanitize(label), 30)) + "  " + th.dim(sanitize(j.Goal))
			c.rows = append(c.rows, fitWidth(line, inner))
		}
		p.show(c)
	}
	read()
	render()
	tick := time.NewTicker(5 * watchSecond())
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			read()
			render()
		case k, ok := <-p.keys:
			if !ok || isQuit(k) {
				return true
			}
			switch {
			case shown != "":
				if isBack(k) || k.kind == keyEnter {
					shown = ""
				}
			case isBack(k) && fromAgents:
				return false
			case k.kind == keyUp || (k.kind == keyRune && k.r == 'k'):
				sel = max(0, sel-1)
			case k.kind == keyDown || (k.kind == keyRune && k.r == 'j'):
				sel = min(max(0, len(jobs)-1), sel+1)
			case k.kind == keyEnter && sel < len(jobs):
				st, err := fetchJobStatus(cr, jobs[sel].ID)
				if err != nil {
					problem = "Couldn't read job " + jobs[sel].ID + ". " + sentence(errText(err))
				} else {
					shown, shownCard = jobs[sel].ID, paneJobCard(st)
				}
			}
			render()
		}
	}
}
