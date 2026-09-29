package main

// appaway.go: "while you were away" (app phase 3). When an agent's window
// opens, what happened since THIS client last showed that agent is summed up
// in one line before the conversation: instructions finished and failed,
// results, and what waits for you now. What was last shown is kept locally
// (per control plane and agent, the journal position only -- never text).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type seenMark struct {
	Seq int64  `json:"seq"`
	At  string `json:"at"`
}

var seenMu sync.Mutex

func seenPath() string { return filepath.Join(configDir(), "app-seen.json") }

func seenKey(cr hostedCreds, agentID string) string { return cr.CTL + " " + agentID }

func loadSeen() map[string]seenMark {
	m := map[string]seenMark{}
	if b, err := os.ReadFile(seenPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// lastSeen answers where this client last left an agent's conversation.
func lastSeen(cr hostedCreds, agentID string) (seenMark, bool) {
	seenMu.Lock()
	defer seenMu.Unlock()
	m, ok := loadSeen()[seenKey(cr, agentID)]
	return m, ok
}

// markSeen records the journal position shown up to. Best effort: a mark
// that cannot be written costs the next summary, nothing else.
func markSeen(cr hostedCreds, agentID string, seq int64) {
	if seq <= 0 {
		return
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	m := loadSeen()
	if prev, ok := m[seenKey(cr, agentID)]; ok && prev.Seq >= seq {
		return
	}
	m[seenKey(cr, agentID)] = seenMark{Seq: seq, At: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(m)
	_ = os.MkdirAll(configDir(), 0o700)
	tmp := seenPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, seenPath())
	}
}

// awaySummary sums up the events after `since` (up to what the window
// resumes from) and what waits now. Empty when nothing happened.
func awaySummary(events []journalEvent, since int64, waiting int, sinceAt string) string {
	finished, failed, results, said := 0, 0, 0, 0
	for _, e := range events {
		if e.StreamSeq <= since {
			continue
		}
		var p struct {
			Type  string `json:"type"`
			State string `json:"state"`
			Kind  string `json:"kind"`
			Name  string `json:"name"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch {
		case p.Type == "task.finished" && p.State == "succeeded":
			finished++
		case p.Type == "task.finished" && p.State != "":
			failed++
		case p.Type == "result.recorded" && p.Name != "ks-changeset.json":
			results++
		case p.Kind == "assistant_text":
			said++
		}
	}
	var parts []string
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	if finished > 0 {
		parts = append(parts, plural(finished, "instruction finished", "instructions finished"))
	}
	if failed > 0 {
		parts = append(parts, plural(failed, "did not finish", "did not finish"))
	}
	if results > 0 {
		parts = append(parts, plural(results, "result", "results")+" (/results)")
	}
	if said > 0 && finished == 0 && failed == 0 {
		parts = append(parts, plural(said, "message from the agent", "messages from the agent"))
	}
	if waiting > 0 {
		parts = append(parts, plural(waiting, "permission request waits for you", "permission requests wait for you"))
	}
	if len(parts) == 0 {
		return ""
	}
	when := ""
	if t, err := time.Parse(time.RFC3339, sinceAt); err == nil {
		when = " (since " + t.Local().Format("Jan 2 15:04") + ")"
	}
	return "While you were away" + when + ": " + strings.Join(parts, " · ")
}
