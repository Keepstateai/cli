// ks agent read: the last N lines of the conversation as plain text.
package main

import (
	"strings"
	"testing"
)

func TestAgentReadPrintsExactlyTheLastLines(t *testing.T) {
	c := newInteropCtl()
	row := func(seq int, kind, text string) map[string]any {
		return map[string]any{"stream_seq": seq, "subject_type": "agent", "subject_id": "agent_1", "observed_at": "2026-11-03T10:00:00Z",
			"payload": map[string]any{"type": "agent.transcript", "kind": kind, "text": text}}
	}
	c.journal["session_1"] = []map[string]any{
		row(1, "assistant_text", "line one\nline two"),
		row(2, "assistant_text", "line three\nline four\nline \x1b[31mfive"),
	}
	_, _, run := interopEnv(t, c)
	if _, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "run", "--bg", "--outlive-parent", "--name", "review", "first"); code != 0 {
		t.Fatalf("run: %d %s", code, errs)
	}
	out, errs, code := run([]string{"KS_CALLER_ID=lead"}, "agent", "read", "review", "--lines", "3")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasSuffix(lines[0], "line three") || !strings.HasSuffix(lines[2], "five") || strings.Contains(out, "\x1b") {
		t.Fatalf("read --lines 3: %d %q\n%s", code, lines, errs)
	}
	out, _, _ = run([]string{"KS_CALLER_ID=lead"}, "agent", "read", "review", "--json")
	if n, _ := jsonData(t, out)["count"].(float64); n != 5 {
		t.Fatalf("read --json: %s", out)
	}
}
