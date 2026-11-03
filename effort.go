// effort.go: how hard the model thinks, chosen by the person.
//
//	ks session runner <session> --effort <level|default>   change it, from the next instruction
//	ks run --agent --effort <level>                        choose it for a new agent session
//	ks agent run --effort <level>                          choose it for a child agent
//
// The levels are low, medium, high, xhigh and max; "default" means no choice,
// so the harness uses its own level for the model (the read names it). This
// client checks only that the word is one of those; the service checks it
// against the session's model and harness, and refuses a level the model does
// not take with the levels it does.
package main

import (
	"fmt"
	"strings"
)

// effortLevels, in order from the least thinking to the most
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

var effortNames = map[string]string{"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high", "max": "Max"}

// effortMoney: what a higher level costs, said next to the choice
const effortMoney = "Higher effort thinks longer: more output tokens per instruction at the same per-token price, so it spends more and reaches the session's token budget and spend limit sooner."

// effortWords is a level as a person reads it; an unknown word is shown as sent.
func effortWords(level string) string {
	if n, ok := effortNames[level]; ok {
		return n
	}
	return sanitize(level)
}

// parseEffortFlag reads --effort: a level, or default for no choice ("").
func parseEffortFlag(v string) (string, *cliError) {
	w := strings.ToLower(strings.TrimSpace(v))
	if w == "default" {
		return "", nil
	}
	for _, l := range effortLevels {
		if w == l {
			return l, nil
		}
	}
	return "", &cliError{Code: exitUsage, Kind: "usage",
		Message:    fmt.Sprintf("--effort is low, medium, high, xhigh or max, or default for the harness's own level; got %q. Nothing was sent", sanitize(v)),
		NextAction: "ks cruise models (the levels each model takes)"}
}

// effortFlag reads --effort when it is set: (level, true), default as "".
func effortFlag(inv *Invocation) (string, bool) {
	if !inv.Set("effort") {
		return "", false
	}
	level, err := parseEffortFlag(inv.Str("effort"))
	if err != nil {
		fail(err)
	}
	return level, true
}

// effortLine is the runner read's effort: the level chosen, or the default
// and which level that is, or why the session cannot take one.
func effortLine(rec sessionRunnerRecord) string {
	switch {
	case rec.RunnerEffortUnavailable != "":
		return "default: " + sanitize(rec.RunnerEffortUnavailable)
	case rec.RunnerEffort != "":
		return effortWords(rec.RunnerEffort)
	case rec.RunnerEffortDefault != "":
		return "default (" + effortWords(rec.RunnerEffortDefault) + ")"
	}
	return "default"
}

// harnessEffortLines: each model's levels and its default, or that it takes
// none; a model the catalog says nothing about (an older catalog) is left out.
func harnessEffortLines(h catalogHarness) []string {
	var out []string
	for _, m := range h.Models {
		if m.Efforts == nil {
			continue
		}
		if len(m.Efforts) == 0 {
			out = append(out, sanitize(m.ID)+": takes no effort setting")
			continue
		}
		names := make([]string, 0, len(m.Efforts))
		for _, l := range m.Efforts {
			names = append(names, effortWords(l))
		}
		line := sanitize(m.ID) + ": " + strings.Join(names, ", ")
		if m.DefaultEffort != "" {
			line += " (default " + effortWords(m.DefaultEffort) + ")"
		}
		out = append(out, line)
	}
	return out
}
