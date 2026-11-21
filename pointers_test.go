package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// docs/POINTERS.md names flows by file and symbol. Every file must exist and
// every symbol after it must occur in it as a whole word, so a rename or a
// move that leaves the map behind fails CI.
func TestPointerAnchorsExist(t *testing.T) {
	raw, err := os.ReadFile("docs/POINTERS.md")
	if err != nil {
		t.Fatal(err)
	}
	tick := regexp.MustCompile("`([^`]+)`")
	isPath := regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(go|json|md|yml|py|sh)$`)
	checked := 0
	for i, line := range strings.Split(string(raw), "\n") {
		file := ""
		var body []byte
		for _, m := range tick.FindAllStringSubmatch(line, -1) {
			tok := m[1]
			if isPath.MatchString(tok) {
				file = tok
				if body, err = os.ReadFile(file); err != nil {
					t.Errorf("POINTERS.md:%d: %s does not exist", i+1, file)
					file = ""
				}
				continue
			}
			if file == "" || strings.ContainsAny(tok, " =/:") || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "go ") {
				continue
			}
			checked++
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(tok) + `\b`).Match(body) {
				t.Errorf("POINTERS.md:%d: %s has no %s", i+1, file, tok)
			}
		}
	}
	if checked < 50 {
		t.Errorf("only %d anchors checked", checked)
	}
	if regexp.MustCompile(`BACKLOG-|ADR-\d`).Match(raw) {
		t.Error("POINTERS.md carries a tracker id")
	}
}
