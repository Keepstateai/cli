package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The text rules for everything a person reads (docs/app-design.md and the
// terminal design spec): no em dashes, no tracker ids, no capitals for
// emphasis, one "Next:" label. They run over the string literals of the
// source and over every human golden, so a new violation fails CI.

var enDash = regexp.MustCompile(`\S – `)

var (
	trackerID  = regexp.MustCompile(`\b(BACKLOG-\d|ADR-\d|DISC-\d|KS-\d{3})`)
	capsWord   = regexp.MustCompile(`\b[A-Z]{3,}\b`)
	nextLabel  = regexp.MustCompile(`(?i)\bnext:`)
	hasLower   = regexp.MustCompile(`[a-z]`)
	retiredLbl = []string{"follow it:", "to do it:", "run it:", "import one with:", "(or: "}
)

// acronyms and proper names that are written in capitals.
var capsAllowed = map[string]bool{
	"JSON": true, "HTTP": true, "HTTPS": true, "URL": true, "API": true, "SSH": true, "TTY": true, "MCP": true,
	"CLI": true, "DNS": true, "TLS": true, "SHA": true, "PID": true, "UTC": true, "UUID": true, "CSV": true,
	"USD": true, "EOF": true, "GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true,
	"BYOK": true, "CPU": true, "GPU": true, "RAM": true, "SQL": true, "HTML": true, "YAML": true, "TOML": true,
	"PDF": true, "VAT": true, "PNG": true, "NUL": true, "TERM": true, "COLUMNS": true, "YYYY": true, "AKIA": true, "BEGIN": true,
	"PDF2": true, "TUI": true, "ASCII": true, "UTF": true, "RFC": true, "OSC": true, "ANSI": true, "SGR": true,
}

// emphasis words found in the audit: never allowed, even alone.
var shouting = map[string]bool{
	"NOT": true, "DONE": true, "NOTHING": true, "FROZEN": true, "HELD": true, "UNKNOWN": true, "STALE": true,
	"RECOVERY": true, "READ": true, "UNREAD": true, "WITHOUT": true, "PAUSED": true, "NEVER": true, "ONLY": true,
	"WARNING": true, "ERROR": true, "FAILED": true, "OFFERED": true,
}

// textViolations are the rule breaks in one piece of output text.
func textViolations(s string) []string {
	var v []string
	if strings.Contains(s, "—") {
		v = append(v, "an em dash")
	}
	if enDash.MatchString(s) {
		v = append(v, "an en dash used as a dash")
	}
	if m := trackerID.FindString(s); m != "" {
		v = append(v, "a tracker id ("+m+")")
	}
	allCaps := !hasLower.MatchString(s)
	// in usage text, a metavariable is written in capitals (--folder NAME)
	usage := strings.Contains(s, "--") || strings.Contains(s, "ks ")
	for _, w := range capsWord.FindAllString(s, -1) {
		switch {
		case shouting[w]:
			v = append(v, "a capital word ("+w+")")
		case capsAllowed[w]:
		case usage:
		case allCaps:
			// a table header line is the one place for capitals
		default:
			v = append(v, "a capital word ("+w+")")
		}
	}
	for _, m := range nextLabel.FindAllString(s, -1) {
		if m != "Next:" {
			v = append(v, "a Next label that is not 'Next:' ("+m+")")
		}
	}
	for _, r := range retiredLbl {
		if strings.Contains(s, r) {
			v = append(v, "the retired label "+strconv.Quote(r))
		}
	}
	return v
}

// lintExempt: files the source lint does not read, and why.
var lintExempt = map[string]string{
	"runners_list.go": "vendored byte for byte from the service",
	"skill.go":        "the manual agents read, kept byte for byte",
	"completion.go":   "shell scripts: their variables are capitals",
}

func lintSkips(name string) bool {
	_, ok := lintExempt[name]
	return ok
}

const lintBaselineFile = "testdata/text-lint-baseline.txt"

// TestSourceStringsObeyTheTextRules is a ratchet. The strings that broke the
// rules before the lint existed are listed in the baseline, to be reworded as
// each verb moves onto the kit (their tests assert the old words, and some
// reach --json). A new violation fails; so does a baseline entry that no
// longer occurs, so the list only shrinks. KS_UPDATE_GOLDEN=1 rewrites it.
func TestSourceStringsObeyTheTextRules(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || lintSkips(f) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			for _, v := range textViolations(s) {
				found[fmt.Sprintf("%s: %s: %q", f, v, lintKey(s))] = true
			}
			return true
		})
	}
	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if os.Getenv("KS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(lintBaselineFile, []byte(strings.Join(keys, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(lintBaselineFile)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			base[l] = true
		}
	}
	for _, k := range keys {
		if !base[k] {
			t.Errorf("new text-rule violation (reword it): %s", k)
		}
	}
	for k := range base {
		if !found[k] {
			t.Errorf("fixed, remove from %s: %s", lintBaselineFile, k)
		}
	}
}

// lintKey is the part of a literal that identifies it in the baseline.
func lintKey(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 70 {
		s = s[:70]
	}
	return s
}

func TestTextRuleExamples(t *testing.T) {
	bad := []string{"a — b", "see BACKLOG-12", "NOT DONE: x", "next: ks login", "to do it: x", "It is HELD", "an ADR-004 reason", "KS-031 says"}
	for _, s := range bad {
		if len(textViolations(s)) == 0 {
			t.Errorf("%q passed the lint", s)
		}
	}
	good := []string{"Next: ks login", "TOOL  SESSION  PROJECT", "Reads JSON over HTTP", "set NO_COLOR or KS_COLOR=0", "–"}
	for _, s := range good {
		if v := textViolations(s); len(v) != 0 {
			t.Errorf("%q failed the lint: %v", s, v)
		}
	}
}
