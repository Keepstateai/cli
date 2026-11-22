//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 1. Codex: a trim keeps a compaction only when its summary can travel ----------------

// codexRealShape is a Codex rollout as Codex writes it: the compacted record's
// message is empty, and the summary exists only in an encrypted compaction item inside
// the replacement history (record types and field presence read from real sessions).
func codexRealShape(compactions int) []string {
	msg := func(role, kind, text string) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":%q,"text":%q}]}}`, role, kind, text)
	}
	out := []string{fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":"2026-11-12T09:00:00.000Z","cwd":"/Users/alice/projects/shop","cli_version":"0.162.1"}}`, trimSID)}
	for c := 0; c < compactions; c++ {
		out = append(out, msg("user", "input_text", fmt.Sprintf("EARLY-%d question %s", c, strings.Repeat("x", 300))))
		out = append(out, msg("assistant", "output_text", fmt.Sprintf("EARLY-%d answer", c)))
		out = append(out, `{"type":"compacted","payload":{"message":"","replacement_history":[`+
			`{"type":"message","role":"developer","content":[{"type":"input_text","text":"DEVELOPER"}]},`+
			fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"KEPT-USER-%d"}]},`, c)+
			`{"type":"compaction","encrypted_content":"ENCRYPTED-SUMMARY"}],"retained_context":{},"window_id":"w"}}`)
	}
	return append(out, msg("user", "input_text", "LATE question FACT-BRAVO"), msg("assistant", "output_text", "LATE answer"))
}

func TestCodexTrimIsNotMadeWhenTheSummaryCannotTravel(t *testing.T) {
	src := writeLines(t, t.TempDir(), "rollout.jsonl", codexRealShape(2))
	full, _, _, _ := cleanBoth(t, "codex", src)
	if full.Boundary != -1 || !strings.Contains(full.NoTrimWhy, "encrypted") {
		t.Fatalf("an empty message with an encrypted summary is a boundary to trim to (boundary %d, why %q)", full.Boundary, full.NoTrimWhy)
	}
	// older Codex: a compaction with no replacement history is rebuilt from the earlier user messages
	old := codexLines(t, fakeValue(t, "github_token"))
	for i, l := range old {
		if strings.Contains(l, `"compacted"`) {
			old[i] = `{"type":"compacted","payload":{"message":"SUMMARY but no history"}}`
		}
	}
	full2, _, _, _ := cleanBoth(t, "codex", writeLines(t, t.TempDir(), "rollout.jsonl", old))
	if full2.Boundary != -1 || !strings.Contains(full2.NoTrimWhy, "replacement history") {
		t.Fatalf("a compaction with no replacement history (boundary %d, why %q)", full2.Boundary, full2.NoTrimWhy)
	}
}

// Over the limit with such a compaction: not trimmed, refused with the honest reason, and the plan
// never claims the agent did not see the earlier part.
func TestOverTheLimitWithAnEncryptedCompactionIsRefusedWithTheReason(t *testing.T) {
	e := newImportEnv(t)
	e.c.runCtl.harnesses = []any{map[string]any{"id": "codex", "name": "Codex CLI", "provider": "openai", "default_model": "gpt-6-luna", "offer": map[string]any{"standing": "offered"}}}
	e.c.runCtl.keys = []any{map[string]any{"id": "vlt_o", "provider": "openai", "last4": "oooo", "enabled": true}}
	p := filepath.Join(e.home, ".codex", "sessions", "2026", "11", "12", "rollout-2026-11-12T09-00-00-"+codexTID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	text := strings.ReplaceAll(strings.Join(codexRealShape(2), "\n")+"\n", trimSID, codexTID)
	_ = os.WriteFile(p, []byte(text), 0o600)
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=900")
	out, errs, code := e.run("import", "codex", "--latest", "--plan", "--json")
	f := flat(errs + out)
	if code != exitConflict || !strings.Contains(f, "a session can resume up to") || !strings.Contains(f, "holds a compaction") || !strings.Contains(f, "encrypted") || strings.Contains(f, "never saw it again") {
		t.Fatalf("%d\n%s", code, f)
	}
	if e.c.index("POST /api/v2/imports") >= 0 {
		t.Fatal("the service was asked about a conversation that cannot be carried")
	}
}

// A Claude Code compaction needs its summary right after the boundary.
func TestClaudeTrimNeedsTheSummaryAfterTheBoundary(t *testing.T) {
	lines := claudeLines(2, 3, nil)
	var cut []string
	for _, l := range lines {
		if !strings.Contains(l, `"isCompactSummary":true`) {
			cut = append(cut, l)
		}
	}
	full, _, _, _ := cleanBoth(t, "claude-code", writeLines(t, t.TempDir(), trimSID+".jsonl", cut))
	if full.Boundary != -1 || !strings.Contains(full.NoTrimWhy, "summary") {
		t.Fatalf("a boundary with no summary after it (boundary %d, why %q)", full.Boundary, full.NoTrimWhy)
	}
}

// ---- 6. gaps ------------------------------------------------------------------------------

func TestClaudeBoundaryInASidechainIsNotAnAnchor(t *testing.T) {
	lines := claudeLines(2, 3, nil)
	side := fmt.Sprintf(`{"parentUuid":null,"isSidechain":true,"type":"system","subtype":"compact_boundary","uuid":"00000000-0000-4000-8000-0000000000ff","sessionId":%q,"cwd":"/p","content":"a subagent's compaction"}`, trimSID)
	lines = append(lines, side, claudeLines(1, 1, nil)[0])
	src := writeLines(t, t.TempDir(), trimSID+".jsonl", lines)
	_, trimmed, _, tb := cleanBoth(t, "claude-code", src)
	if trimmed == nil || strings.Contains(string(tb), "subagent") || !strings.Contains(string(tb), "SUMMARY-1") || !strings.Contains(string(tb), "LATE-1-2 answer") {
		t.Fatalf("a sidechain boundary moved the trim:\n%s", tb)
	}
}

func TestCodexTrimWithSeveralCompactionsUsesTheLastOne(t *testing.T) {
	secret := fakeValue(t, "github_token")
	base := codexLines(t, secret) // one readable compaction, with its history
	// two earlier readable compactions before it
	var lines []string
	lines = append(lines, base[0])
	for c := 0; c < 2; c++ {
		lines = append(lines,
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"OLDER-%d question"}]}}`, c),
			fmt.Sprintf(`{"type":"compacted","payload":{"message":"OLDER-SUMMARY-%d","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"OLDER-KEPT-%d"}]}]}}`, c, c))
	}
	lines = append(lines, base[1:]...)
	_, trimmed, _, tb := cleanBoth(t, "codex", writeLines(t, t.TempDir(), "rollout.jsonl", lines))
	if trimmed == nil || strings.Contains(string(tb), "OLDER-") || !strings.Contains(string(tb), "SUMMARY of the work") || !strings.Contains(string(tb), "LATE-1 answer") {
		t.Fatalf("several compactions:\n%s", tb)
	}
	if n := strings.Count(string(tb), `"type":"compacted"`); n != 1 {
		t.Fatalf("%d compacted records in the trim", n)
	}
}

// exactly at the limit is accepted, one byte over is not
func TestTheLimitIsExactlyTheLimit(t *testing.T) {
	lines := claudeLines(1, 4, nil) // no compaction: nothing to trim to
	e := newImportEnv(t)
	e.placeLines(lines)
	out, _, _ := e.run("import", "claude-code", "--latest", "--plan", "--json")
	n := int64(jsonInt(decodeJSONOut(t, out)["bytes"]))
	e.env = append(e.env, fmt.Sprintf("KS_IMPORT_MAX_BYTES=%d", n))
	if _, errs, code := e.run("import", "claude-code", "--latest", "--plan", "--json"); code != 0 {
		t.Fatalf("exactly %d bytes at a limit of %d: %d\n%s", n, n, code, errs)
	}
	e2 := newImportEnv(t)
	e2.placeLines(lines)
	e2.env = append(e2.env, fmt.Sprintf("KS_IMPORT_MAX_BYTES=%d", n-1))
	if o2, errs, code := e2.run("import", "claude-code", "--latest", "--plan", "--json"); code != exitConflict || !strings.Contains(o2+errs, "a session can resume up to") {
		t.Fatalf("one byte over: %d\n%s%s", code, o2, errs)
	}
	if importMaxBytes != 120_000_000 {
		t.Fatalf("the limit is %d", importMaxBytes)
	}
}

// at a terminal with --yes there is nobody to ask: over the limit is refused, no question
func TestOverTheLimitWithYesAtATerminalIsRefused(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=1000")
	got, code := e.ptyImport(nil, "import", "claude-code", "--latest", "--yes")
	if code != exitConflict || strings.Contains(got, "What now?") || !strings.Contains(flat(got), "a session can resume up to") || len(e.c.creations) != 0 {
		t.Fatalf("%d\n%s", code, got)
	}
}

// ---- 2. every new field makes the old-service retry ---------------------------------------

func TestOldServiceRetryFiresForAnyOfTheNewFields(t *testing.T) {
	refusal := &hostedErr{Status: 400, Type: "ks_body_unknown_field", Message: `the request body carries a field this route does not take: json: unknown field "trimmed"`}
	for _, fields := range [][]string{{"est_tokens"}, {"trimmed", "untrimmed_bytes"}, {"untrimmed_bytes"}, {"est_compacted"}} {
		decl := map[string]any{"source": map[string]any{"tool": "claude-code"}}
		for _, f := range fields {
			decl["source"].(map[string]any)[f] = 1
		}
		if !retryWithoutNewFields(decl, refusal) {
			t.Fatalf("%v: no retry", fields)
		}
		for _, f := range []string{"est_tokens", "est_compacted", "trimmed", "untrimmed_bytes"} {
			if _, has := decl["source"].(map[string]any)[f]; has {
				t.Fatalf("%v: %s was kept", fields, f)
			}
		}
	}
	plain := map[string]any{"source": map[string]any{"tool": "claude-code"}}
	if retryWithoutNewFields(plain, refusal) {
		t.Fatal("a declaration with none of them is retried")
	}
	if retryWithoutNewFields(map[string]any{"source": map[string]any{"trimmed": true}}, errors.New("a different failure")) {
		t.Fatal("another failure is retried")
	}
}

// ---- 3. a trim that fails says so -----------------------------------------------------------

func TestATrimThatFailsIsAnErrorNotASilentFallback(t *testing.T) {
	dir := t.TempDir()
	src := writeLines(t, dir, trimSID+".jsonl", claudeLines(3, 4, nil))
	info := transcriptInfo{Tool: "claude-code", Path: src, ID: trimSID}
	rw := newPathRewriter(importRoots("/Users/alice/projects/shop", nil, "/Users/alice", importGuestWorkspace))
	full, err := sanitizeTranscript(info, rw, newSummaryBuilder(rw.apply), filepath.Join(dir, "full.jsonl"))
	if err != nil || full.Boundary < 0 {
		t.Fatal(err)
	}
	_ = os.Remove(src) // the file goes away between the two readings
	if _, err := trimTranscript(info, rw, full, filepath.Join(dir, "trim.jsonl")); err == nil || !strings.Contains(err.Error(), "trim") {
		t.Fatalf("the failure of the second reading is lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "trim.jsonl")); err == nil {
		t.Fatal("a half-written trim is left behind")
	}
}

// ---- 4. the plan's counts are the trim's -----------------------------------------------------

func TestPlanCountsAreTheTrimsWhenTrimmed(t *testing.T) {
	e := newImportEnv(t)
	e.placeLines(claudeLines(4, 5, nil)) // 4 segments of 5 pairs: 20 prompts whole, 5 after the last compaction
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=9000")
	out, _, code := e.run("import", "claude-code", "--latest", "--plan")
	f := flat(out)
	if code != 0 || strings.Contains(f, "the full conversation") || !strings.Contains(f, "the conversation from its last compaction on, 5 of your messages") {
		t.Fatalf("the Carries row:\n%s", f)
	}
	// untrimmed keeps the old words
	e2 := newImportEnv(t)
	e2.ready()
	o2, _, _ := e2.run("import", "claude-code", "--latest", "--plan")
	if !strings.Contains(flat(o2), "the full conversation, 2 of your messages") {
		t.Fatalf("untrimmed:\n%s", o2)
	}
}
