package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const trimSID = "7c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

// claudeLines builds a Claude Code transcript from segments. Every segment but the
// last ends in a compaction (a compact_boundary record and its summary); a segment is
// pairs of messages whose text names it ("EARLY-0-1" is segment 0, pair 1).
func claudeLines(segs int, pairs int, extra func(seg int) []string) []string {
	var out []string
	parent := "null"
	n := 0
	rec := func(typ, text string) string {
		n++
		uid := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		var role, content string
		if typ == "user" {
			role, content = "user", fmt.Sprintf("%q", text)
		} else {
			role, content = "assistant", fmt.Sprintf(`[{"type":"text","text":%q}]`, text)
		}
		l := fmt.Sprintf(`{"parentUuid":%s,"isSidechain":false,"type":%q,"uuid":%q,"timestamp":"2026-11-12T09:00:%02d.000Z","cwd":"/Users/alice/projects/shop","sessionId":%q,"version":"2.1.296","message":{"role":%q,"content":%s}}`,
			parent, typ, uid, n%60, trimSID, role, content)
		parent = fmt.Sprintf("%q", uid)
		return l
	}
	for s := 0; s < segs; s++ {
		if s > 0 { // the compaction that opens this segment
			n++
			bid := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
			out = append(out, fmt.Sprintf(`{"parentUuid":null,"logicalParentUuid":%s,"isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","uuid":%q,"timestamp":"2026-11-12T09:01:%02d.000Z","cwd":"/Users/alice/projects/shop","sessionId":%q,"version":"2.1.296","compactMetadata":{"trigger":"manual","preTokens":1234,"preservedSegment":{"headUuid":"x"}}}`, parent, bid, n%60, trimSID))
			parent = fmt.Sprintf("%q", bid)
			n++
			sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
			out = append(out, fmt.Sprintf(`{"parentUuid":%s,"isSidechain":false,"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"uuid":%q,"timestamp":"2026-11-12T09:01:%02d.000Z","cwd":"/Users/alice/projects/shop","sessionId":%q,"version":"2.1.296","message":{"role":"user","content":"SUMMARY-%d of everything before"}}`, parent, sid, n%60, trimSID, s))
			parent = fmt.Sprintf("%q", sid)
		}
		tag := "EARLY"
		if s == segs-1 {
			tag = "LATE"
		}
		for i := 0; i < pairs; i++ {
			out = append(out, rec("user", fmt.Sprintf("%s-%d-%d question %s", tag, s, i, strings.Repeat("x", 200))))
			out = append(out, rec("assistant", fmt.Sprintf("%s-%d-%d answer %s", tag, s, i, strings.Repeat("y", 200))))
		}
		if extra != nil {
			out = append(out, extra(s)...)
		}
	}
	return out
}

func writeLines(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// cleanBoth cleans a file whole and trimmed from its last compaction.
func cleanBoth(t *testing.T, tool, path string) (full, trimmed *sanitizeResult, fb, tb []byte) {
	t.Helper()
	dir := t.TempDir()
	info := transcriptInfo{Tool: tool, Path: path, ID: trimSID}
	rw := newPathRewriter(importRoots("/Users/alice/projects/shop", nil, "/Users/alice", importGuestWorkspace))
	var err error
	full, err = sanitizeTranscript(info, rw, newSummaryBuilder(rw.apply), filepath.Join(dir, "full.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fb, _ = os.ReadFile(full.OutPath)
	if full.Boundary >= 0 {
		trimmed, err = sanitizeTranscriptTrim(info, rw, nil, filepath.Join(dir, "trim.jsonl"), trimOpts{On: true, Start: full.Boundary})
		if err != nil {
			t.Fatal(err)
		}
		tb, _ = os.ReadFile(trimmed.OutPath)
	}
	return
}

func jsonLine(t *testing.T, b []byte, i int) map[string]any {
	t.Helper()
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var m map[string]any
	if i >= len(ls) || json.Unmarshal([]byte(ls[i]), &m) != nil {
		t.Fatalf("record %d of the output is not there or not JSON", i)
	}
	return m
}

func TestTrimClaudeKeepsTheLastBoundaryAndWhatFollows(t *testing.T) {
	for _, segs := range []int{2, 4} { // one boundary, and several
		dir := t.TempDir()
		lines := claudeLines(segs, 6, nil)
		src := writeLines(t, dir, trimSID+".jsonl", lines)
		full, trimmed, fb, tb := cleanBoth(t, "claude-code", src)
		if trimmed == nil {
			t.Fatalf("%d segments: no boundary found", segs)
		}
		if int64(len(tb)) >= int64(len(fb)) || trimmed.Bytes != int64(len(tb)) {
			t.Fatalf("%d segments: trimmed %d bytes, whole %d", segs, len(tb), len(fb))
		}
		// byte-checked: the trim is exactly the cleaning of the tail of the source from its last boundary
		var tail []string
		last := -1
		for i, l := range lines {
			if strings.Contains(l, `"compact_boundary"`) {
				last = i
			}
		}
		tail = lines[last:]
		tp := writeLines(t, dir, "tail.jsonl", tail)
		rw := newPathRewriter(importRoots("/Users/alice/projects/shop", nil, "/Users/alice", importGuestWorkspace))
		want, err := sanitizeTranscript(transcriptInfo{Tool: "claude-code", Path: tp, ID: trimSID}, rw, nil, filepath.Join(dir, "want.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		wb, _ := os.ReadFile(want.OutPath)
		if !bytes.Equal(tb, wb) || trimmed.SHA256 != want.SHA256 {
			t.Fatalf("%d segments: the trim is not the cleaning of the tail", segs)
		}
		// the chain is valid for the harness: the boundary is the root, its summary follows it
		b0, b1 := jsonLine(t, tb, 0), jsonLine(t, tb, 1)
		if b0["subtype"] != "compact_boundary" || b0["parentUuid"] != nil {
			t.Fatalf("the first record: %v", b0)
		}
		if _, has := b0["logicalParentUuid"]; has {
			t.Fatal("the boundary still points at a record that is not there (logicalParentUuid)")
		}
		if b1["isCompactSummary"] != true || b1["parentUuid"] != b0["uuid"] {
			t.Fatalf("the summary does not follow the boundary: %v", b1)
		}
		prev := b1["uuid"]
		for i := 2; i < strings.Count(string(tb), "\n"); i++ {
			r := jsonLine(t, tb, i)
			if r["parentUuid"] != prev {
				t.Fatalf("record %d breaks the chain", i)
			}
			prev = r["uuid"]
		}
		// nothing from before the boundary, everything from after it
		if strings.Contains(string(tb), "EARLY-") {
			t.Fatal("text from before the last compaction travelled")
		}
		if !strings.Contains(string(tb), fmt.Sprintf("SUMMARY-%d", segs-1)) || !strings.Contains(string(tb), fmt.Sprintf("LATE-%d-5 answer", segs-1)) {
			t.Fatal("the summary or the last messages are missing")
		}
		if strings.Contains(string(tb), "preservedSegment") || strings.Contains(string(tb), "/Users/alice") {
			t.Fatal("something that must not travel is in the trim")
		}
		_ = full
	}
}

func TestTrimWithNoBoundaryIsNoTrim(t *testing.T) {
	src := writeLines(t, t.TempDir(), trimSID+".jsonl", claudeLines(1, 6, nil))
	full, trimmed, _, _ := cleanBoth(t, "claude-code", src)
	if full.Boundary != -1 || trimmed != nil {
		t.Fatalf("boundary %d: there is nothing to trim to", full.Boundary)
	}
}

// A tool call before the boundary whose result comes after it (or the reverse) has no
// partner in the trim: it is taken out, as an unpaired one is anywhere.
func TestTrimDropsAToolResultWhoseCallIsBeforeTheBoundary(t *testing.T) {
	lines := claudeLines(2, 2, func(seg int) []string {
		if seg != 0 {
			return nil
		}
		return []string{fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"assistant","uuid":"00000000-0000-4000-8000-0000000000a1","sessionId":%q,"cwd":"/p","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ORPHAN","name":"Bash","input":{"command":"ls"}}]}}`, trimSID)}
	})
	// the result comes after the boundary, in the last segment
	lines = append(lines, fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","uuid":"00000000-0000-4000-8000-0000000000a2","sessionId":%q,"cwd":"/p","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ORPHAN","content":"files"},{"type":"text","text":"LATE-after the result"}]}}`, trimSID))
	src := writeLines(t, t.TempDir(), trimSID+".jsonl", lines)
	_, trimmed, _, tb := cleanBoth(t, "claude-code", src)
	if trimmed == nil || strings.Contains(string(tb), "toolu_ORPHAN") || !strings.Contains(string(tb), "LATE-after the result") {
		t.Fatalf("the orphan result is in the trim, or the text beside it is lost:\n%s", tb)
	}
}

// ---- Codex ------------------------------------------------------------------------------

func codexLines(t *testing.T, secret string) []string {
	msg := func(role, kind, text string) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":%q,"text":%q}]}}`, role, kind, text)
	}
	return []string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":"2026-11-12T09:00:00.000Z","cwd":"/Users/alice/projects/shop","cli_version":"0.162.1","base_instructions":{"text":"THE OLD MACHINES PROMPT"}}}`, trimSID),
		`{"type":"turn_context","payload":{"cwd":"/Users/alice/projects/shop","model":"gpt-6-luna"}}`,
		msg("user", "input_text", "EARLY-1 question "+strings.Repeat("x", 300)),
		msg("assistant", "output_text", "EARLY-1 answer "+strings.Repeat("y", 300)),
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"EARLY-ENCRYPTED"}}`,
		msg("user", "input_text", "EARLY-2 question"),
		fmt.Sprintf(`{"type":"compacted","payload":{"message":"SUMMARY of the work at /Users/alice/projects/shop with %s","replacement_history":[`+
			`{"type":"message","role":"developer","content":[{"type":"input_text","text":"DEVELOPER-PROMPT must not travel"}]},`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd /Users/alice</environment_context>"}]},`+
			`{"type":"reasoning","encrypted_content":"HISTORY-ENCRYPTED"},`+
			`{"type":"compaction","encrypted_content":"HISTORY-COMPACTION"},`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"KEPT-USER remember FACT-ALPHA %s"}]},`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"KEPT-ASSISTANT ok"}]}],"extra_unknown_field":"DROPME"}}`, secret, secret),
		msg("user", "input_text", "LATE-1 question FACT-BRAVO"),
		msg("assistant", "output_text", "LATE-1 answer"),
	}
}

func TestTrimCodexKeepsSessionMetaAndTheCompactionsHistory(t *testing.T) {
	secret := fakeValue(t, "github_token")
	src := writeLines(t, t.TempDir(), "rollout.jsonl", codexLines(t, secret))
	full, trimmed, fb, tb := cleanBoth(t, "codex", src)
	if strings.Contains(string(fb), "SUMMARY of the work") {
		t.Fatal("the whole-file cleaning must still drop the compacted record (the golden output)")
	}
	if trimmed == nil || full.Boundary != 6 {
		t.Fatalf("boundary %d", full.Boundary)
	}
	got := string(tb)
	if jsonLine(t, tb, 0)["type"] != "session_meta" || jsonLine(t, tb, 1)["type"] != "compacted" {
		t.Fatalf("a trimmed rollout starts with its session_meta, then the compaction:\n%s", got)
	}
	for _, want := range []string{"SUMMARY of the work", "KEPT-USER remember FACT-ALPHA", "KEPT-ASSISTANT ok", "LATE-1 question FACT-BRAVO", "LATE-1 answer", "/home/specimen/workspace"} {
		if !strings.Contains(got, want) {
			t.Errorf("the trim lacks %q", want)
		}
	}
	for _, never := range []string{"EARLY-", "THE OLD MACHINES PROMPT", "DEVELOPER-PROMPT", "environment_context", "HISTORY-ENCRYPTED", "HISTORY-COMPACTION", "DROPME", "/Users/alice", secret} {
		if strings.Contains(got, never) {
			t.Errorf("the trim carries %q", never)
		}
	}
	if trimmed.Redactions["github_token"] != 2 {
		t.Errorf("redactions in the trim: %v (the secret was in the summary and in the history)", trimmed.Redactions)
	}
	allFakesAbsent(t, "the trimmed rollout", tb)
	// every record is JSON and the compaction has the shape the harness reads
	p := jsonLine(t, tb, 1)["payload"].(map[string]any)
	if _, ok := p["message"].(string); !ok || len(p["replacement_history"].([]any)) != 2 {
		t.Fatalf("the compaction's payload: %v", p)
	}
}

// ---- the command ------------------------------------------------------------------------

func (e *importEnv) placeLines(lines []string) {
	e.t.Helper()
	p := filepath.Join(e.home, ".claude", "projects", "-p", claudeSID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	text := strings.ReplaceAll(strings.Join(lines, "\n")+"\n", trimSID, claudeSID)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
}

func TestPlanSaysWhenTheConversationWasTrimmed(t *testing.T) {
	e := newImportEnv(t)
	e.placeLines(claudeLines(4, 5, nil))
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=9000")
	out, errs, code := e.run("import", "claude-code", "--latest", "--plan", "--json")
	doc := decodeJSONOut(t, out)
	if code != 0 || doc["trimmed"] != true || jsonInt(doc["bytes_before_trim"]) <= jsonInt(doc["bytes"]) {
		t.Fatalf("--plan --json: %d\n%s\n%s", code, out, errs)
	}
	if jsonInt(doc["bytes"]) > 9000 {
		t.Fatalf("still over the limit: %v", doc["bytes"])
	}
	src := asMap(e.c.plans[0]["source"])
	if src["trimmed"] != true || jsonInt(src["untrimmed_bytes"]) != jsonInt(doc["bytes_before_trim"]) {
		t.Fatalf("the declaration does not bind the trim: %v", src)
	}
	text := flat(strings.Join(asStrings(doc["lines"]), " "))
	if !strings.Contains(text, "Trimmed to the last compaction:") || !strings.Contains(text, "The earlier part stays on this machine; the agent never saw it again anyway.") {
		t.Fatalf("the plan does not say it:\n%s", text)
	}
	// the human plan too
	hout, _, _ := e.run("import", "claude-code", "--latest", "--plan")
	if !strings.Contains(flat(hout), "Trimmed to the last compaction:") {
		t.Fatalf("the human plan:\n%s", hout)
	}
	// an untrimmed conversation says nothing of it
	e2 := newImportEnv(t)
	e2.placeLines(claudeLines(2, 2, nil))
	o2, _, _ := e2.run("import", "claude-code", "--latest", "--plan", "--json")
	if d2 := decodeJSONOut(t, o2); d2["trimmed"] != nil {
		t.Fatalf("an untrimmed plan carries trimmed: %v", d2["trimmed"])
	}
}

func asStrings(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, asStr(x))
	}
	return out
}

// Still over after the trim, or nothing to trim to: refused in plain words with the
// reason a session can resume only so much; a summary works.
func TestStillOverAfterTheTrimIsRefusedInPlainWords(t *testing.T) {
	e := newImportEnv(t)
	e.placeLines(claudeLines(3, 12, nil))
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=2000")
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != exitConflict || !strings.Contains(errs, "a session can resume up to 2 KB") || !strings.Contains(errs, "trimmed to its last compaction") || !strings.Contains(errs, "Nothing was sent") || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("still over after the trim: %d\n%s", code, errs)
	}
	e2 := newImportEnv(t)
	e2.placeLines(claudeLines(1, 40, nil)) // no compaction at all
	e2.env = append(e2.env, "KS_IMPORT_MAX_BYTES=2000")
	_, errs, code = e2.run("import", "claude-code", "--latest", "--yes")
	if code != exitConflict || !strings.Contains(errs, "holds no compaction to trim to") {
		t.Fatalf("no compaction: %d\n%s", code, errs)
	}
	nx := nextLines(errs)
	if len(nx) != 1 || !strings.Contains(nx[0], "--brief") {
		t.Fatalf("next: %v", nx)
	}
}

// The whole import of a trimmed conversation: what is uploaded is the trim, the
// declaration binds it, and the service's apply sees the same declaration.
func TestATrimmedConversationIsWhatIsUploaded(t *testing.T) {
	e := newImportEnv(t)
	e.placeLines(claudeLines(4, 5, nil))
	e.env = append(e.env, "KS_IMPORT_MAX_BYTES=9000")
	out, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported into session") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	u := e.c.upload("conversation")
	if u == nil || strings.Contains(string(u.Got), "EARLY-") || !strings.Contains(string(u.Got), "LATE-3-4 answer") || jsonLine(t, u.Got, 0)["subtype"] != "compact_boundary" {
		t.Fatalf("what was uploaded is not the trim")
	}
	if int64(len(u.Got)) != int64(jsonInt(asMap(e.c.applies[0]["declaration"])["source"].(map[string]any)["bytes"])) {
		t.Fatal("the declared size is not the uploaded size")
	}
	// an older service that knows neither field is asked again without them
	e2 := newImportEnv(t)
	e2.placeLines(claudeLines(4, 5, nil))
	e2.env = append(e2.env, "KS_IMPORT_MAX_BYTES=9000")
	e2.c.rejectEst = true
	_, errs, code = e2.run("import", "claude-code", "--latest", "--yes")
	if code != 0 {
		t.Fatalf("against an older service: %d\n%s", code, errs)
	}
}
