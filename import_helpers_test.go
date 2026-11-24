package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic Codex rollouts only: a parent session, two forks a helper thread
// spawned (each begins with its own header, then a copy of the parent's), a
// guardian thread and an empty session. Nothing here is a real session.
const (
	fxParent   = "aaaaaaaa-0000-7000-8000-000000000001"
	fxForkA    = "bbbbbbbb-0000-7000-8000-000000000002"
	fxForkB    = "cccccccc-0000-7000-8000-000000000003"
	fxGuardian = "dddddddd-0000-7000-8000-000000000004"
	fxEmpty    = "eeeeeeee-0000-7000-8000-000000000005"
)

func fxLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fxMeta(t *testing.T, id, cwd, ts string, extra map[string]any) string {
	p := map[string]any{"id": id, "cwd": cwd, "timestamp": ts, "cli_version": "0.99.0", "source": "vscode"}
	for k, v := range extra {
		p[k] = v
	}
	return fxLine(t, map[string]any{"type": "session_meta", "timestamp": ts, "payload": p})
}

func fxMsg(t *testing.T, role string) string {
	typ := "input_text"
	if role == "assistant" {
		typ = "output_text"
	}
	return fxLine(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": role,
		"content": []map[string]any{{"type": typ, "text": "synthetic " + role + " text"}}}})
}

// fxWrite writes one rollout under home/sessions/YYYY/MM/DD and sets its time.
func fxWrite(t *testing.T, home, id string, when time.Time, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", when.Format("2006"), when.Format("01"), when.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-"+when.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func fxSpawn(parent string) map[string]any {
	return map[string]any{
		"source":           map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1, "agent_path": "x", "agent_nickname": "n"}}},
		"forked_from_id":   parent,
		"parent_thread_id": parent,
	}
}

// fxCodexHome builds the synthetic machine and points CODEX_HOME at it.
func fxCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "no-claude"))
	fxPopulate(t, home, true)
	return home
}

// fxPopulate writes the synthetic rollouts; without the parent they are a
// machine where the parent's own file is missing.
func fxPopulate(t *testing.T, home string, withParent bool) {
	t.Helper()
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	const cwd = "/work/demo"
	ts := base.Format(time.RFC3339)
	if withParent {
		fxWrite(t, home, fxParent, base,
			fxMeta(t, fxParent, cwd, ts, nil), fxMsg(t, "user"), fxMsg(t, "assistant"), fxMsg(t, "user"), fxMsg(t, "assistant"))
	}
	for i, id := range []string{fxForkA, fxForkB} {
		when := base.Add(time.Duration(i+1) * time.Hour)
		fxWrite(t, home, id, when,
			fxMeta(t, id, "/work/helper", when.Format(time.RFC3339), fxSpawn(fxParent)),
			fxMeta(t, fxParent, cwd, ts, nil), // the parent's header, copied
			fxMsg(t, "user"), fxMsg(t, "assistant"), fxMsg(t, "user"))
	}
	gw := base.Add(3 * time.Hour)
	fxWrite(t, home, fxGuardian, gw,
		fxMeta(t, fxGuardian, "/work/guard", gw.Format(time.RFC3339), map[string]any{
			"source": map[string]any{"subagent": map[string]any{"other": "guardian"}}, "parent_thread_id": fxParent}))
	ew := base.Add(-48 * time.Hour)
	fxWrite(t, home, fxEmpty, ew, fxMeta(t, fxEmpty, "/work/empty", ew.Format(time.RFC3339), nil))
}

func TestForkedRolloutsDoNotRepeatTheParent(t *testing.T) {
	fxCodexHome(t)
	seen := map[string]int{}
	for _, s := range localTranscripts("codex") {
		seen[s.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("session %s is listed %d times", id, n)
		}
	}
	for _, id := range []string{fxForkA, fxForkB, fxGuardian} {
		if seen[id] > 0 {
			t.Errorf("helper thread %s is listed as a session", id)
		}
	}
	if seen[fxParent] != 1 {
		t.Errorf("the parent is listed %d times, want 1", seen[fxParent])
	}
}

// The id, the folder and the time are the first header's, never a later one.
func TestScanTakesTheFirstSessionMeta(t *testing.T) {
	home := fxCodexHome(t)
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+fxForkA+".jsonl"))
	if len(paths) != 1 {
		t.Fatalf("fixture: %v", paths)
	}
	info, err := scanTranscript("codex", paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != fxForkA || !info.Helper || info.ParentID != fxParent || info.Cwd != "/work/helper" {
		t.Errorf("a fork's header is its own: id %s helper %v parent %s folder %s", info.ID, info.Helper, info.ParentID, info.Cwd)
	}
	guard, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+fxGuardian+".jsonl"))
	gi, _ := scanTranscript("codex", guard[0])
	if !gi.Helper || gi.ParentID != fxParent {
		t.Errorf("a guardian thread is a helper of its parent: %+v", gi)
	}
	top, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+fxParent+".jsonl"))
	pi, _ := scanTranscript("codex", top[0])
	if pi.Helper || pi.ID != fxParent || pi.StartedAt != "2026-11-20T10:00:00Z" {
		t.Errorf("the parent is a session of its own: %+v", pi)
	}
}

func TestSessionsCountTheirHelperThreads(t *testing.T) {
	home := fxCodexHome(t)
	all := localTranscripts("codex")
	if len(all) != 2 {
		t.Fatalf("want the parent and the empty session, got %d", len(all))
	}
	var parent transcriptInfo
	for _, s := range all {
		if s.ID == fxParent {
			parent = s
		}
	}
	if parent.Helpers != 3 {
		t.Errorf("the parent folds %d helper threads, want 3 (two forks and a guardian)", parent.Helpers)
	}
	if !strings.Contains(parent.Path, home) || !strings.HasSuffix(parent.Path, fxParent+".jsonl") || parent.Messages != 4 {
		t.Errorf("the parent's own file is the one read: %s (%d messages)", parent.Path, parent.Messages)
	}
}

func fxListJSON(t *testing.T, e *importEnv, args ...string) (rows []map[string]any, hidden int, plain string) {
	t.Helper()
	out, errs, code := e.run(append([]string{"import", "list", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("list: %d\n%s", code, errs)
	}
	var doc struct {
		Data struct {
			Sessions    []map[string]any `json:"sessions"`
			HiddenEmpty int              `json:"hidden_empty"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pl, _, _ := e.run(append([]string{"import", "list"}, args...)...)
	return doc.Data.Sessions, doc.Data.HiddenEmpty, pl
}

func TestImportListFoldsHelpersAndHidesEmpty(t *testing.T) {
	e := newImportEnv(t)
	fxPopulate(t, filepath.Join(e.home, ".codex"), true)
	rows, hidden, plain := fxListJSON(t, e)
	if len(rows) != 1 || rows[0]["id"] != fxParent || rows[0]["helper_threads"] != float64(3) || hidden != 1 {
		t.Fatalf("want one row for the parent with 3 helper threads and 1 hidden, got %v (hidden %d)", rows, hidden)
	}
	if n := strings.Count(plain, fxParent); n != 1 {
		t.Errorf("the parent's id is on %d lines:\n%s", n, plain)
	}
	for _, id := range []string{fxForkA, fxForkB, fxGuardian, fxEmpty} {
		if strings.Contains(plain, id) {
			t.Errorf("%s is on the list:\n%s", id, plain)
		}
	}
	for _, want := range []string{"3 helper threads", "1 empty session hidden; ks import list --all"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the list lacks %q:\n%s", want, plain)
		}
	}
	rows, hidden, plain = fxListJSON(t, e, "--all")
	if len(rows) != 2 || hidden != 0 || !strings.Contains(plain, fxEmpty) || strings.Contains(plain, "hidden") {
		t.Errorf("--all shows the empty session and says nothing is hidden: %d rows\n%s", len(rows), plain)
	}
}

func TestHelpersOfAMissingParentShareOneRow(t *testing.T) {
	e := newImportEnv(t)
	fxPopulate(t, filepath.Join(e.home, ".codex"), false)
	rows, _, plain := fxListJSON(t, e)
	if len(rows) != 1 || rows[0]["id"] != fxParent || rows[0]["helper_threads"] != float64(3) {
		t.Fatalf("want one row for the missing parent with its helper threads, got %v", rows)
	}
	if !strings.Contains(plain, "its own file is not on this machine") || strings.Contains(plain, fxForkA) || strings.Contains(plain, fxGuardian) {
		t.Errorf("the row says plainly that the file is missing and shows no helper as a row:\n%s", plain)
	}
}

func TestLatestAndSessionNeverPickAHelper(t *testing.T) {
	home := fxCodexHome(t)
	inv := &Invocation{set: map[string]bool{"latest": true}, strs: map[string]string{"project": "/work/demo"}}
	if got := chooseTranscript("codex", inv); got.ID != fxParent || filepath.Base(got.Path) != "rollout-2026-11-20T10-00-00-"+fxParent+".jsonl" {
		t.Errorf("--latest chose %s (%s), want the parent's own file", got.ID, got.Path)
	}
	inv = &Invocation{set: map[string]bool{"latest": true}, strs: map[string]string{"project": "/nowhere"}}
	if got := chooseTranscript("codex", inv); got.ID != fxParent {
		t.Errorf("--latest with no matching folder chose %s: a helper thread or an empty session was newer", got.ID)
	}
	inv = &Invocation{set: map[string]bool{"session": true}, strs: map[string]string{"session": fxParent[:8]}}
	if got := chooseTranscript("codex", inv); got.ID != fxParent || !strings.Contains(got.Path, home) || !strings.HasSuffix(got.Path, "-"+fxParent+".jsonl") {
		t.Errorf("--session with the parent's id chose %s (%s), want the parent's own file", got.ID, got.Path)
	}
	// a helper's own id is refused in words, and nothing is sent
	e := newImportEnv(t)
	fxPopulate(t, filepath.Join(e.home, ".codex"), true)
	_, errs, code := e.run("import", "codex", "--session", fxForkA, "--plan")
	if code != 2 || !strings.Contains(errs, "helper thread") || !strings.Contains(errs, "ks import codex --session "+fxParent) {
		t.Errorf("a helper's id: %d\n%s", code, errs)
	}
	if n := len(e.c.seenLog()); n != 0 {
		t.Errorf("the refusal asked the service (%d calls)", n)
	}
}

func TestGuidedPickerListsOnlyTopLevelSessionsWithMessages(t *testing.T) {
	fxCodexHome(t)
	rows := guidedRows("codex")
	if len(rows) != 1 || rows[0].ID != fxParent {
		t.Fatalf("the picker lists %d rows, want the parent alone: %+v", len(rows), rows)
	}
}

// A stacked row whose folder wraps never ends a line on the separator.
func TestStackedRowsNeverEndOnASeparator(t *testing.T) {
	long := "/home/you/work/projects/a-folder-with-a-rather-long-name/and/a/second/one/below-it"
	rows := []importListEntry{
		{Tool: "codex", ID: "01a1c2d3-e4f5-7a6b-8c9d-0e1f2a3b4c5d", Project: long, Date: "2026-11-22T09:00:00Z", Bytes: 3_100_000, Messages: 88, Helpers: 4},
		{Tool: "codex", ID: fxParent, Project: long + "/more/folders/still", Date: "2026-11-21T09:00:00Z", Bytes: 31_000, Messages: 8, Note: "its own file is not on this machine", Helpers: 2},
	}
	for _, width := range []int{120, 80, 60} {
		out := renderScreen(importListScreen(rows, "", 2), width, false, false)
		for _, l := range strings.Split(out, "\n") {
			if t2 := strings.TrimRight(l, " "); strings.HasSuffix(t2, "\u00b7") {
				t.Errorf("at %d columns a line ends on a separator: %q", width, l)
			}
		}
	}
}

// Every session with messages is listed, in a pipe and on a terminal: the list
// never collapses rows behind a "more" count.
func TestEveryNonEmptySessionIsListed(t *testing.T) {
	e := newImportEnv(t)
	home := filepath.Join(e.home, ".codex")
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 14; i++ {
		id := fmt.Sprintf("%08x-0000-7000-8000-0000000000%02d", 0x10000000+i, i)
		ids = append(ids, id)
		when := base.Add(time.Duration(i) * time.Hour)
		fxWrite(t, home, id, when, fxMeta(t, id, "/work/demo", when.Format(time.RFC3339), nil), fxMsg(t, "user"), fxMsg(t, "assistant"))
	}
	check := func(label, out string) {
		for _, id := range ids {
			if !strings.Contains(out, id) {
				t.Errorf("%s: session %s is not listed:\n%s", label, id, out)
			}
		}
		if strings.Contains(out, " more") || strings.Contains(out, "...") {
			t.Errorf("%s: rows were collapsed:\n%s", label, out)
		}
	}
	pipe, _, _ := e.run("import", "list")
	check("pipe", pipe)
	for _, w := range []string{"100", "80", "60"} {
		out, _, _ := auditExec(t, e.bin, e.cfg, e.dir, append(append([]string{}, e.env...), "KS_FORCE_COLUMNS="+w), "import", "list")
		check("terminal at "+w, out)
	}
}
