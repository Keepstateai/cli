package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestSummarySectionsCapsAndRedaction(t *testing.T) {
	rw := newPathRewriter([]pathRoot{{"/Users/alice/shop", "/home/specimen/workspace/shop"}})
	b := newSummaryBuilder(rw.apply)
	b.add("user", "Build the cart. Config is /Users/alice/shop/config/app.yml and my token is "+fakeValue(t, "github_token"))
	for i := 0; i < 30; i++ {
		b.add("user", fmt.Sprintf("request number %d %s", i, strings.Repeat("w", 900)))
		b.add("assistant", fmt.Sprintf("We decided to keep module %d. Next: handle case %d.", i, i))
	}
	b.add("assistant", "I'll use the queue instead of polling. TODO: add retries\n- [ ] write the README\nSee src/cart/queue.go and docs/design.md and https://example.com/a/b.html.")
	counts := redactCounts{}
	out := b.build("Claude Code", counts)
	for _, want := range []string{"What the person asked for", "Decisions the assistant stated", "Where it stands", "Open tasks", "Files mentioned",
		"/home/specimen/workspace/shop/config/app.yml", "src/cart/queue.go", "docs/design.md", "TODO: add retries", "- [ ] write the README", "instead of polling", "[redacted:github_token]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, firstChars(out, 1800))
		}
	}
	if counts["github_token"] != 1 || strings.Contains(out, fakeValue(t, "github_token")) {
		t.Errorf("redaction: %v", counts)
	}
	files := out[strings.Index(out, "Files mentioned"):]
	if strings.Contains(out, "/Users/alice") || strings.Contains(files, "example.com") {
		t.Errorf("an old path was kept or a url was listed as a file:\n%s", firstChars(files, 600))
	}
	if len([]rune(out)) > summaryMaxChars {
		t.Errorf("%d characters, over the cap", len([]rune(out)))
	}
	// the person's turns: the first, then the most recent; at most 12
	if n := strings.Count(out, "request number") + strings.Count(out, "Build the cart"); n > summaryUserMax {
		t.Errorf("%d turns shown, at most %d", n, summaryUserMax)
	}
	if !strings.Contains(out, "request number 29") || strings.Contains(out, "request number 5 ") || !strings.Contains(out, "Build the cart") {
		t.Errorf("the first and the most recent turns are the ones shown")
	}
	dec := out[strings.Index(out, "Decisions the assistant stated"):strings.Index(out, "Where it stands")]
	if strings.Count(dec, "\n- ") > summaryDecisionMax {
		t.Errorf("more decisions than the cap:\n%s", dec)
	}
}

func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func TestSummaryIsDeterministicAndSaysWhenNothingWasFound(t *testing.T) {
	mk := func() string {
		b := newSummaryBuilder(nil)
		b.add("user", "hello there")
		b.add("assistant", "hi")
		return b.build("Cursor", redactCounts{})
	}
	a := mk()
	if a != mk() {
		t.Fatal("the same conversation made two summaries")
	}
	if strings.Count(a, "none found") != 3 { // decisions, open tasks, files
		t.Fatalf("empty sections say so:\n%s", a)
	}
	if !strings.Contains(a, "no model wrote it") || !strings.Contains(a, "not a transcript") {
		t.Fatalf("the summary says what it is:\n%s", a)
	}
}

func TestSummaryKeepsEverySectionWithinItsCap(t *testing.T) {
	b := newSummaryBuilder(nil)
	for i := 0; i < 12; i++ {
		b.add("user", strings.Repeat("a long request ", 60))
	}
	b.add("assistant", strings.Repeat("the state of things ", 200))
	for i := 0; i < 40; i++ {
		b.add("assistant", fmt.Sprintf("TODO: item %d %s see dir%d/file%d.go", i, strings.Repeat("x", 300), i, i))
		b.add("assistant", fmt.Sprintf("We decided on plan %d %s.", i, strings.Repeat("y", 400)))
	}
	out := b.build("Codex CLI", redactCounts{})
	if r := len([]rune(out)); r > summaryMaxChars {
		t.Fatalf("%d characters, over the cap", r)
	}
	for _, sec := range []string{"What the person asked for", "Decisions the assistant stated", "Where it stands", "Open tasks", "Files mentioned"} {
		if !strings.Contains(out, sec) {
			t.Errorf("the section %q was pushed out", sec)
		}
	}
	if strings.Contains(out, "[the summary was cut]") {
		t.Errorf("the shares should keep it under the cap without a hard cut")
	}
	// the hard cut stays as the last resort
	long := b.build(strings.Repeat("s", 7000), redactCounts{})
	if r := len([]rune(long)); r > summaryMaxChars || !strings.Contains(long, "[the summary was cut]") {
		t.Fatalf("hard cut: %d", r)
	}
}
