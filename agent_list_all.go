// agent_list_all.go: ks agent list --all, every agent across your sessions
// with the console rail's words, from the service's status records
// (GET /api/v2/agent-status): what needs you first, then what works, what
// is done, what failed and what rests.
//
//	ks agent list --all                     the list, once
//	ks agent list --all --watch             redrawn every 2 seconds, with
//	                                        terminal status per agent and
//	                                        for all of them together
//	ks agent list --all --watch --notify    also a notification when an
//	                                        agent comes to need you or fails
//
// A notification is OSC 9 with a bell, and inside herdr a herdr
// notification; it fires once per change into needing you or failing,
// never for what the first read already showed.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

type agentStatusCounts struct {
	NeedsYou int `json:"needs_you"`
	Working  int `json:"working"`
	Blocked  int `json:"blocked"`
	Done     int `json:"done"`
	Error    int `json:"error"`
	Idle     int `json:"idle"`
}

type agentStatusList struct {
	Items     []statusRecord    `json:"items"`
	Counts    agentStatusCounts `json:"counts"`
	Truncated bool              `json:"truncated"`
	// Context is the organization the list was read in ("" for personal):
	// its agents that are yours, never your personal ones
	Context string `json:"-"`
}

func fetchAgentStatusList(cr hostedCreds) (agentStatusList, error) {
	var env struct {
		Data agentStatusList `json:"data"`
	}
	err := hostedCall(cr, "GET", inContext(cr, "/api/v2/agent-status"), nil, &env)
	if env.Data.Items == nil {
		env.Data.Items = []statusRecord{}
	}
	env.Data.Context = currentContext(cr)
	sortStatusRecords(env.Data.Items)
	return env.Data, err
}

// statusWord is a record's state in the console rail's words.
func statusWord(rec statusRecord) string {
	switch rec.State {
	case "working":
		return "Working"
	case "blocked":
		switch rec.Kind {
		case "permission":
			return "Needs you: permission"
		case "question":
			return "Needs you: a question"
		case "auth":
			return "Needs you: sign-in or key"
		}
		return "Needs you"
	case "done":
		return "Done"
	case "error":
		return "Error"
	case "idle":
		return "Idle"
	}
	return "Status unavailable"
}

// statusRank orders the list: what needs a person first (blocked, then
// failed), then working, done and idle; a state this client does not know
// last.
var statusRank = map[string]int{"blocked": 0, "error": 1, "working": 2, "done": 3, "idle": 4}

func sortStatusRecords(items []statusRecord) {
	rank := func(s string) int {
		if n, ok := statusRank[s]; ok {
			return n
		}
		return 5
	}
	sort.SliceStable(items, func(i, j int) bool {
		if ri, rj := rank(items[i].State), rank(items[j].State); ri != rj {
			return ri < rj
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].SessionName != items[j].SessionName {
			return items[i].SessionName < items[j].SessionName
		}
		return items[i].AgentID < items[j].AgentID
	})
}

// countRecords counts the list itself, so the first line always agrees with
// the rows under it.
func countRecords(items []statusRecord) agentStatusCounts {
	var c agentStatusCounts
	for _, it := range items {
		// an error the person has read is still listed, and no longer
		// counted as needing them
		if it.NeedsYou || it.State == "blocked" || (it.State == "error" && !it.Read) {
			c.NeedsYou++
		}
		switch it.State {
		case "working":
			c.Working++
		case "blocked":
			c.Blocked++
		case "done":
			c.Done++
		case "error":
			c.Error++
		case "idle":
			c.Idle++
		}
	}
	return c
}

// needYouLine is the list's first line.
func needYouLine(c agentStatusCounts) string {
	head := "Nothing needs you"
	switch {
	case c.NeedsYou == 1:
		head = "1 needs you"
	case c.NeedsYou > 1:
		head = fmt.Sprintf("%d need you", c.NeedsYou)
	}
	var rest []string
	for _, p := range []struct {
		n    int
		word string
	}{{c.Working, "working"}, {c.Done, "done"}, {c.Error, "failed"}, {c.Idle, "idle"}} {
		if p.n > 0 {
			rest = append(rest, fmt.Sprintf("%d %s", p.n, p.word))
		}
	}
	if len(rest) == 0 {
		return head
	}
	return head + " · " + strings.Join(rest, " · ")
}

func recordSession(rec statusRecord) string {
	if rec.SessionName != "" {
		return sanitize(rec.SessionName)
	}
	return sanitize(rec.SessionID)
}

// allAgentsLines is the list as printed.
func allAgentsLines(l agentStatusList) []string {
	out := []string{needYouLine(countRecords(l.Items))}
	if l.Context != "" {
		out = append([]string{"Context: organization " + sanitize(l.Context) + " (its agents that are yours; ks context use personal for your own)"}, out...)
	}
	if len(l.Items) == 0 {
		if l.Context != "" {
			return append(out, "None of your agents in this organization.")
		}
		return append(out, "No agents in your sessions.")
	}
	out = append(out, fmt.Sprintf("%-26s %-16s %-20s %s", "STATE", "AGENT", "SESSION", "WHAT"))
	for _, rec := range l.Items {
		msg := oscText(sanitize(rec.Msg), 200)
		out = append(out, strings.TrimRight(fmt.Sprintf("%-26s %-16s %-20s %s", statusWord(rec), clip(sanitize(rec.Name), 16), clip(recordSession(rec), 20), msg), " "))
		if a := rec.Action; a != nil && (rec.State == "blocked" || rec.State == "error") {
			next := oscText(sanitize(a.Label), 80)
			if v := oscText(sanitize(a.Verb), 200); v != "" {
				next += ": " + v
			}
			if next != "" {
				out = append(out, "    next: "+next)
			}
		}
	}
	if l.Truncated {
		out = append(out, "Only your newest sessions are listed; ks session list shows them all.")
	}
	first := l.Items[0]
	out = append(out, fmt.Sprintf("open one: ks agent open %s --session %s", nameOrPlaceholder(first.Name), nameOrPlaceholder(first.SessionID)))
	return out
}

func nameOrPlaceholder(s string) string { return commandWord(s) }

// aggregateStatus is the root record of the watch: the most urgent state
// of all the agents, and the counts in words.
func aggregateStatus(items []statusRecord) oscStatus {
	c := countRecords(items)
	r := oscStatus{App: statusApp, Title: "KeepState agents", Msg: needYouLine(c)}
	switch {
	case c.Blocked > 0:
		r.State = "blocked"
		for _, it := range items {
			if it.State == "blocked" {
				r.Kind = it.Kind
				break
			}
		}
	case unreadErrors(items) > 0:
		r.State = "error"
	case c.Working > 0:
		r.State = "working"
	case c.Done > 0:
		r.State = "done"
	default:
		r.State = "idle"
	}
	return r
}

// unreadErrors counts the errors the person has not read yet.
func unreadErrors(items []statusRecord) int {
	n := 0
	for _, it := range items {
		if it.State == "error" && !it.Read {
			n++
		}
	}
	return n
}

// notifyText is a notification's words: who, and what it needs.
func notifyText(rec statusRecord) string {
	word := statusWord(rec)
	if rec.State == "error" {
		word = "failed"
	}
	t := "KeepState: " + sanitize(rec.Name) + " " + strings.ToLower(word[:1]) + word[1:]
	if m := sanitize(rec.Msg); m != "" {
		t += ". " + m
	}
	return oscText(t, 200)
}

// notifyOSC is a terminal notification (OSC 9) and a bell.
func notifyOSC(text string) string {
	return "\x1b]9;" + oscText(text, 200) + "\x1b\\\a"
}

// herdrNotify shows a notification in herdr, in the background, within the
// time limit; a failure is ignored.
func herdrNotify(title, body, sound string) {
	bin, _, ok := herdrPane()
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), herdrTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "notification", "show", oscText(title, 120), "--body", oscText(body, 400), "--sound", sound)
		_ = cmd.Run()
	}()
}

// hostedAgentListAll is ks agent list --all [--watch [--notify]].
func hostedAgentListAll(cr hostedCreds, inv *Invocation) {
	if inv.Set("session") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--all lists the agents of every session; --session names one, so they do not go together", NextAction: "ks agent list --all"})
	}
	l, err := fetchAgentStatusList(cr)
	if err != nil {
		if statusUnsupported(err) {
			fail(&cliError{Code: exitFailed, Kind: "status_unsupported", Message: "this control plane does not serve the status of every agent yet; nothing was read", NextAction: "ks agent list --session <session>"})
		}
		die(err)
	}
	if !inv.Bool("watch") {
		doc := map[string]any{"items": l.Items, "counts": countRecords(l.Items), "truncated": l.Truncated}
		if l.Context != "" {
			doc["context"] = l.Context
		}
		emit(doc, func() {
			for _, line := range allAgentsLines(l) {
				fmt.Println(line)
			}
		})
		return
	}
	watchAllAgents(cr, l, inv.Bool("notify"))
}

func watchAllAgents(cr hostedCreds, l agentStatusList, notify bool) {
	status := newStatusEmitter(nil, "Agents", []string{"ks", "agent", "list", "--all", "--watch"})
	defer status.leave()
	oscOn, _ := statusOSCMode()
	redraw := stdoutIsTerminal() && !out.json && !out.plain
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	shown := ""
	seen := map[string]string{} // agent id -> its state at the last read
	first := true
	show := func(l agentStatusList) {
		lines := strings.Join(allAgentsLines(l), "\n")
		if out.json {
			if lines != shown {
				doc := map[string]any{"kind": "agent_status", "items": l.Items, "counts": countRecords(l.Items), "truncated": l.Truncated}
				if l.Context != "" {
					doc["context"] = l.Context
				}
				emitLine(doc, "")
			}
		} else if redraw {
			fmt.Print("\x1b[H\x1b[2J" + lines + "\n")
		} else if lines != shown {
			if shown != "" {
				fmt.Println()
			}
			fmt.Println(lines)
		}
		shown = lines
		// terminal status: one record per agent, and all of them at the root
		now := map[string]string{}
		for _, rec := range l.Items {
			id := statusID(rec.AgentID)
			now[id] = rec.State
			if r, ok := recordStatus(rec, id, statusTitle(sanitize(rec.Name), recordSession(rec))); ok {
				status.report(r)
			}
			was, known := seen[id]
			if notify && !first && (!known || was != rec.State) && (rec.State == "blocked" || rec.State == "error") {
				if oscOn {
					fmt.Print(notifyOSC(notifyText(rec)))
				}
				herdrNotify("KeepState: "+sanitize(rec.Name), notifyText(rec), "request")
			}
		}
		for id := range seen {
			if _, still := now[id]; !still {
				status.forget(id)
			}
		}
		status.report(aggregateStatus(l.Items))
		seen, first = now, false
	}
	show(l)
	unit := watchSecond()
	failing := false
	for {
		select {
		case <-stop:
			progress("stopped watching; nothing on any agent changed")
			return
		case <-time.After(2 * unit):
		}
		fresh, err := fetchAgentStatusList(cr)
		if err != nil {
			if !failing {
				progress("the agents could not be read (%s); the last list above is kept", sanitize(errText(err)))
			}
			failing = true
			continue
		}
		failing = false
		show(fresh)
	}
}
