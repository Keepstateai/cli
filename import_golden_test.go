package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden outputs: what the sanitizer makes of each synthetic transcript,
// with its secret markers expanded first, so a golden holds only
// [redacted:kind] markers. The service's bench resumes these very files in a
// guest with the pinned runners, so a change to the sanitizer that changes a
// golden is a change the runners must be shown again.
//
//	KS_UPDATE_GOLDEN=1 go test -run TestImportGolden .
var goldenFixtures = []struct{ tool, fixture string }{
	{"claude-code", "claude-code-2.1.296.jsonl"},
	{"claude-code", "claude-code-2.1.312.jsonl"},
	{"codex", "codex-0.162.1.jsonl"},
	{"codex", "codex-0.167.0-alpha.3.jsonl"},
}

func TestImportGolden(t *testing.T) {
	for _, g := range goldenFixtures {
		_, _, got := sanitizeFixture(t, g.tool, g.fixture, nil)
		name := filepath.Join("testdata", "import", "golden", strings.TrimSuffix(g.fixture, ".jsonl")+".sanitized.jsonl")
		if os.Getenv("KS_UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v (KS_UPDATE_GOLDEN=1 writes it)", name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from what the sanitizer makes now (KS_UPDATE_GOLDEN=1 rewrites it)", name)
		}
		// a golden holds no credential and none of the old machine
		for _, kind := range []string{"anthropic_key", "openai_key", "github_token", "connection_string"} {
			if strings.Contains(string(want), fakeValue(t, kind)) {
				t.Errorf("%s holds a fake %s", name, kind)
			}
		}
		if strings.Contains(string(want), "/Users/alice") || strings.Contains(string(want), "@@FAKE") {
			t.Errorf("%s holds the old machine's paths or an unexpanded marker", name)
		}
	}
}
