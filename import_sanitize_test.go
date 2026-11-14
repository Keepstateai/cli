package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	claudeSID = "7c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	codexTID  = "01a1c2d3-e4f5-7a6b-8c9d-0e1f2a3b4c5d"
	oldRoot   = "/Users/alice/projects/shop"
)

// sessionHome lays a fixture out where the tool keeps it and points the
// client at it; it answers the transcript's path.
func placeSession(t *testing.T, tool, fixture string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	var p string
	if tool == "claude-code" {
		p = filepath.Join(home, ".claude", "projects", "-Users-alice-projects-shop", claudeSID+".jsonl")
	} else {
		p = filepath.Join(home, ".codex", "sessions", "2026", "11", "12", "rollout-2026-11-12T09-00-00-"+codexTID+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(fixtureText(t, fixture)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("a line is not JSON: %v: %q", err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func sanitizeFixture(t *testing.T, tool, fixture string, roots []pathRoot) (*sanitizeResult, []map[string]any, string) {
	t.Helper()
	p := placeSession(t, tool, fixture)
	info, err := scanTranscript(tool, p)
	if err != nil {
		t.Fatal(err)
	}
	if roots == nil {
		roots = importRoots(info.Cwd, info.Cwds, "/Users/alice", importGuestWorkspace)
	}
	out := filepath.Join(t.TempDir(), "t.jsonl")
	res, err := sanitizeTranscript(info, newPathRewriter(roots), nil, out)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	return res, readRecords(t, out), string(raw)
}

func typesOf(recs []map[string]any) map[string]int {
	m := map[string]int{}
	for _, r := range recs {
		m[asStr(r["type"])]++
	}
	return m
}

func TestSanitizeClaudeCodeKeepsOnlyTheConversation(t *testing.T) {
	for _, fx := range []string{"claude-code-2.1.296.jsonl", "claude-code-2.1.312.jsonl"} {
		t.Run(fx, func(t *testing.T) {
			res, recs, raw := sanitizeFixture(t, "claude-code", fx, nil)
			for typ := range typesOf(recs) {
				if typ != "user" && typ != "assistant" && typ != "system" {
					t.Errorf("a record of type %q survived", typ)
				}
			}
			// the chain: one root, each parent is the record before
			var last any
			for i, r := range recs {
				if r["parentUuid"] != last {
					t.Fatalf("record %d: parentUuid %v, want %v", i, r["parentUuid"], last)
				}
				last = r["uuid"]
				if r["cwd"] != importGuestWorkspace {
					t.Errorf("record %d: cwd %v", i, r["cwd"])
				}
				if r["sessionId"] != claudeSID {
					t.Errorf("record %d: sessionId %v", i, r["sessionId"])
				}
				for _, f := range claudeDroppedFields {
					if _, ok := r[f]; ok {
						t.Errorf("record %d keeps %s", i, f)
					}
				}
			}
			for _, bad := range []string{`"thinking"`, `"signature"`, "FOREIGN-SYSTEM-PROMPT-MARKER", "prompt_snapshot", "ownerAccountUuid", "bypassPermissions", "iVBORw0KGgo", "/Users/alice", "alice"} {
				if strings.Contains(raw, bad) {
					t.Errorf("the sanitized transcript still holds %q", bad)
				}
			}
			for _, f := range []string{"FACT-ALPHA-7731", "FACT-CHARLIE-9902"} {
				if !strings.Contains(raw, f) {
					t.Errorf("the conversation lost %s", f)
				}
			}
			if !strings.Contains(raw, importGuestWorkspace+"/config/app.yml") {
				t.Errorf("a path of the old project was not said as a path of the workspace")
			}
			if res.Stripped["attachment"] < 3 || res.Stripped["thinking"] != 1 || res.Stripped["image"] != 1 {
				t.Errorf("stripped: %v", res.Stripped)
			}
			// a tool call and its result are both there
			if !strings.Contains(raw, `"tool_use"`) || !strings.Contains(raw, `"tool_result"`) {
				t.Errorf("the tool call and its result must both stay")
			}
			if res.Redactions["connection_string"] != 1 || res.Redactions["anthropic_key"] != 1 {
				t.Errorf("redactions: %v", res.Redactions)
			}
			// the deploy token sat in an attachment that is dropped: it was never read as a kept string
			if res.Redactions["github_token"] != 0 {
				t.Errorf("a secret in a dropped record was counted: %v", res.Redactions)
			}
		})
	}
}

func TestSanitizeClaudeCodeNewerBuildSpecifics(t *testing.T) {
	_, recs, raw := sanitizeFixture(t, "claude-code", "claude-code-2.1.312.jsonl", nil)
	ty := typesOf(recs)
	if ty["system"] != 1 {
		t.Fatalf("the compact boundary stays: %v", ty)
	}
	for _, r := range recs {
		if asStr(r["type"]) == "system" {
			if cm := asMap(r["compactMetadata"]); cm != nil {
				if cm["preservedSegment"] != nil || cm["preservedMessages"] != nil {
					t.Errorf("links to dropped records stay in the boundary: %v", cm)
				}
			}
		}
		if side, _ := r["isSidechain"].(bool); side {
			t.Errorf("a sidechain record survived")
		}
	}
	if strings.Contains(raw, "subagent note") || strings.Contains(raw, "zz-future-record") || strings.Contains(raw, "api-request") {
		t.Errorf("a record that must not travel did")
	}
}

func TestSanitizeClaudeCodeDropsAToolCallWhoseResultIsMissing(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"cwd":"/p","sessionId":"x","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","cwd":"/p","sessionId":"x","message":{"role":"assistant","content":[{"type":"text","text":"working"},{"type":"tool_use","id":"toolu_orphan","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"a1","cwd":"/p","sessionId":"x","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a2","cwd":"/p","sessionId":"x","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_never_called","content":"x"}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"u2","cwd":"/p","sessionId":"x","message":{"role":"user","content":"next"}}`,
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "11111111-2222-4333-8444-555555555555.jsonl")
	_ = os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	info := transcriptInfo{Tool: "claude-code", Path: src, ID: "11111111-2222-4333-8444-555555555555"}
	out := filepath.Join(dir, "o.jsonl")
	res, err := sanitizeTranscript(info, newPathRewriter(nil), nil, out)
	if err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, out)
	if len(recs) != 3 { // u1, a1 (text only), u3
		t.Fatalf("records: %d %v", len(recs), res.Stripped)
	}
	if recs[2]["parentUuid"] != "a1" {
		t.Fatalf("the chain skips the dropped records: %v", recs[2]["parentUuid"])
	}
	if res.Stripped["tool call without a result"] != 1 || res.Stripped["tool result without a call"] != 1 || res.Stripped["message left empty"] != 2 {
		t.Fatalf("stripped: %v", res.Stripped)
	}
}

func TestSanitizeCodexKeepsOnlyTheConversation(t *testing.T) {
	for _, fx := range []string{"codex-0.162.1.jsonl", "codex-0.167.0-alpha.3.jsonl"} {
		t.Run(fx, func(t *testing.T) {
			res, recs, raw := sanitizeFixture(t, "codex", fx, nil)
			var types []string
			for typ := range typesOf(recs) {
				types = append(types, typ)
			}
			sort.Strings(types)
			for _, typ := range types {
				switch typ {
				case "session_meta", "turn_context", "response_item", "event_msg":
				default:
					t.Errorf("a record of type %q survived", typ)
				}
			}
			meta := asMap(recs[0]["payload"])
			if recs[0]["type"] != "session_meta" || meta["id"] != codexTID || meta["cwd"] != importGuestWorkspace {
				t.Fatalf("the first record: %v", recs[0])
			}
			for _, k := range []string{"base_instructions", "creator_account_id", "creator_user_id", "parent_thread_id", "forked_from_id"} {
				if _, ok := meta[k]; ok {
					t.Errorf("session_meta keeps %s", k)
				}
			}
			for _, r := range recs {
				p := asMap(r["payload"])
				switch r["type"] {
				case "response_item":
					switch asStr(p["type"]) {
					case "reasoning", "compaction", "additional_tools":
						t.Errorf("a %s item survived", p["type"])
					}
					if asStr(p["role"]) == "developer" {
						t.Errorf("a developer message survived")
					}
				case "event_msg":
					if !codexEventsKept[asStr(p["type"])] {
						t.Errorf("an event of type %v survived", p["type"])
					}
				case "turn_context":
					if p["cwd"] != importGuestWorkspace {
						t.Errorf("turn_context cwd %v", p["cwd"])
					}
				}
			}
			for _, bad := range []string{"encrypted_content", "FOREIGN-SYSTEM-PROMPT-MARKER", "environment_context", "/Users/alice", "skills of alice", "world_state"} {
				if strings.Contains(raw, bad) {
					t.Errorf("the sanitized rollout still holds %q", bad)
				}
			}
			for _, f := range []string{"FACT-ALPHA-7731", "FACT-BRAVO-4410", "FACT-CHARLIE-9902"} {
				if !strings.Contains(raw, f) {
					t.Errorf("the conversation lost %s", f)
				}
			}
			if res.Stripped["reasoning"] != 1 || res.Stripped["developer message"] != 1 {
				t.Errorf("stripped: %v", res.Stripped)
			}
			if res.Redactions["connection_string"] != 1 || res.Redactions["openai_key"] != 1 {
				t.Errorf("redactions: %v", res.Redactions)
			}
		})
	}
}

func TestSanitizeIsDeterministicAndHoldsNoSecret(t *testing.T) {
	_, _, a := sanitizeFixture(t, "claude-code", "claude-code-2.1.312.jsonl", nil)
	_, _, b := sanitizeFixture(t, "claude-code", "claude-code-2.1.312.jsonl", nil)
	if a != b {
		t.Fatal("the same session made two different transcripts")
	}
	for _, kind := range []string{"anthropic_key", "connection_string", "github_token", "openai_key"} {
		if strings.Contains(a, fakeValue(t, kind)) {
			t.Errorf("a fake %s is in the output", kind)
		}
	}
}

func TestScanTranscriptReadsIdentityAndCountsOnly(t *testing.T) {
	p := placeSession(t, "claude-code", "claude-code-2.1.312.jsonl")
	info, err := scanTranscript("claude-code", p)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != claudeSID || info.Cwd != oldRoot || info.Version != "2.1.312" || info.Model != "claude-opus-4-8" || info.Turns != 2 || info.Messages != 7 {
		t.Fatalf("%+v", info)
	}
	p = placeSession(t, "codex", "codex-0.167.0-alpha.3.jsonl")
	info, err = scanTranscript("codex", p)
	if err != nil || info.ID != codexTID || info.Cwd != oldRoot || info.Version != "0.167.0-alpha.3" || info.Model != "gpt-6.1-sol" || info.StartedAt != "2026-11-12T09:00:00.000Z" || info.Turns != 2 {
		t.Fatalf("%v %+v", err, info)
	}
}

func TestPathRewriteBoundariesAndLongestRoot(t *testing.T) {
	rw := newPathRewriter([]pathRoot{{"/Users/alice/projects/shop", "/home/specimen/workspace/shop"}, {"/Users/alice/projects/shop/vendor", "/home/specimen/workspace/V"}, {"~/projects/shop", "/home/specimen/workspace/shop"}, {"/Users/alice/scratch", importGuestWorkspace}})
	cases := map[string]string{
		"read /Users/alice/projects/shop/main.go now":      "read /home/specimen/workspace/shop/main.go now",
		"/Users/alice/projects/shop2/x":                    "/Users/alice/projects/shop2/x",
		"/Users/alice/projects/shop-old and shopping":      "/Users/alice/projects/shop-old and shopping",
		"/Users/alice/projects/shop/vendor/lib.go":         "/home/specimen/workspace/V/lib.go",
		"cd ~/projects/shop && ls":                         "cd /home/specimen/workspace/shop && ls",
		"file:///Users/alice/projects/shop/a.go":           "file:///home/specimen/workspace/shop/a.go",
		"/tmp/Users/alice/projects/shop/a.go":              "/tmp/Users/alice/projects/shop/a.go",
		"in /Users/alice/projects/shop.":                   "in /home/specimen/workspace/shop.",
		"/Users/alice/projects/shop/.git/config":           "/home/specimen/workspace/shop/.git/config",
		"\"/Users/alice/scratch/out.txt\" and /Users/bob/": "\"/home/specimen/workspace/out.txt\" and /Users/bob/",
		"no path here":               "no path here",
		"/Users/alice/projects/shop": "/home/specimen/workspace/shop",
	}
	for in, want := range cases {
		if got := rw.apply(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
	if rw.Count == 0 {
		t.Fatal("no rewrite was counted")
	}
}

func TestPathRootsWithAndWithoutFiles(t *testing.T) {
	without := importRoots(oldRoot, []string{oldRoot, oldRoot + "/server", "/Users/alice/other"}, "/Users/alice", importGuestWorkspace)
	got := map[string]string{}
	for _, r := range without {
		got[r.From] = r.To
	}
	if got[oldRoot] != importGuestWorkspace || got["~/projects/shop"] != importGuestWorkspace || got["/Users/alice/other"] != importGuestWorkspace || len(got) != 3 {
		t.Fatalf("without files: %v", got)
	}
	with := importRoots(oldRoot, nil, "/Users/alice", importGuestWorkspace+"/shop")
	if with[0].To != importGuestWorkspace+"/shop" {
		t.Fatalf("with files: %v", with)
	}
}
