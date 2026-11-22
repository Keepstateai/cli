package main

import (
	"path/filepath"
	"testing"
)

func rec(t *testing.T, s string) map[string]any {
	t.Helper()
	m, err := decodeImportLine([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Known counts, worked by hand: tokens = ceil(bytes / 3) + 6 a record + 15,000.
func TestEstimateCountsTheTextTheModelReads(t *testing.T) {
	e := newTokenEstimator("claude-code")
	e.add(rec(t, `{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"abcdefghi"}}`))
	if got := e.Tokens(); got != 3+6+15000 { // 9 bytes
		t.Fatalf("one message of 9 bytes: %d", got)
	}
	e.add(rec(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	// 9 + (5 + 4 + len({"command":"ls"}) = 16) = 34 bytes -> 12 tokens; 2 records -> 12
	if got := e.Tokens(); got != 12+12+15000 {
		t.Fatalf("two messages, 34 bytes: %d", got)
	}
	e.add(rec(t, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"abc"},{"type":"image","source":"x"}]}}`))
	// +3 bytes (the image counts nothing) = 37 bytes -> 13; 3 records -> 18
	if got := e.Tokens(); got != 13+18+15000 {
		t.Fatalf("a tool result and an image: %d", got)
	}
	if e.compacted {
		t.Fatal("no boundary was seen")
	}
}

func TestEstimateStartsAtTheLastCompactionBoundary(t *testing.T) {
	big := `{"type":"user","message":{"role":"user","content":"` + string(make([]byte, 0)) + `xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`
	e := newTokenEstimator("claude-code")
	for i := 0; i < 5; i++ {
		e.add(rec(t, big))
	}
	before := e.Tokens()
	e.add(rec(t, `{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"manual"}}`))
	e.add(rec(t, `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"SUMMARY"}}`)) // 7 bytes
	e.add(rec(t, `{"type":"user","message":{"role":"user","content":"xx"}}`))                              // 2 bytes
	if got := e.Tokens(); got != 3+12+15000 || !e.compacted || got >= before {
		t.Fatalf("after a boundary only the summary and what followed count: %d (before %d)", got, before)
	}
	// a second boundary cuts again
	e.add(rec(t, `{"type":"system","subtype":"compact_boundary"}`))
	e.add(rec(t, `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"S2"}}`))
	if got := e.Tokens(); got != 1+6+15000 {
		t.Fatalf("after the second boundary: %d", got)
	}
}

func TestEstimateCodexStartsAtItsLastCompactedRecord(t *testing.T) {
	e := newTokenEstimator("codex")
	e.add(rec(t, `{"type":"session_meta","payload":{"id":"x"}}`))
	e.add(rec(t, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
	e.add(rec(t, `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}"}}`))
	e.add(rec(t, `{"type":"response_item","payload":{"type":"function_call_output","output":"file.txt"}}`))
	if got := e.Tokens(); got != (73+5+13+8+2)/3+3*6+15000 { // text 73, "shell" 5 + 12 argument bytes... computed below
		// the argument string {"cmd":"ls"} is 12 bytes: 73 + 5 + 12 + 8 = 98 -> 33; 3 records -> 18
		if got != 33+18+15000 {
			t.Fatalf("a Codex session: %d", got)
		}
	}
	e.add(rec(t, `{"type":"compacted","payload":{"message":"Summary"}}`)) // 7 bytes
	e.add(rec(t, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}`))
	if got := e.Tokens(); got != 3+12+15000 || !e.compacted { // 9 bytes, 2 records
		t.Fatalf("after compacted: %d", got)
	}
}

// The cleaned fixtures. The Claude Code one ends in a compaction, so what the
// first reply reads is far less than the whole file. The cleaned Codex rollout
// carries no `compacted` record (the cleaning drops it, with the encrypted
// compaction item), so the harness is given every message and every one counts.
func TestEstimateOnTheFixtures(t *testing.T) {
	for _, tc := range []struct {
		tool, file string
		compacted  bool
		min, max   int64
	}{
		{"claude-code", "claude-code-2.1.312.jsonl", true, estimateOverheadToken, estimateOverheadToken + 400},
		{"codex", "codex-0.167.0-alpha.3.jsonl", false, estimateOverheadToken + 20, estimateOverheadToken + 2000},
	} {
		dir := t.TempDir()
		info := transcriptInfo{Tool: tc.tool, Path: filepath.Join("testdata", "import", tc.file)}
		light, err := scanTranscript(tc.tool, info.Path)
		if err != nil {
			t.Fatal(err)
		}
		rw := newPathRewriter(nil)
		res, err := sanitizeTranscript(light, rw, newSummaryBuilder(rw.apply), filepath.Join(dir, "o.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if res.EstCompacted != tc.compacted || res.EstTokens < tc.min || res.EstTokens > tc.max {
			t.Fatalf("%s: %d tokens, compacted %v", tc.tool, res.EstTokens, res.EstCompacted)
		}
		t.Logf("%s: %d tokens (the harness's own 15,000 included)", tc.tool, res.EstTokens)
	}
}
