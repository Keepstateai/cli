package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The publication gate scans every file of this repository for a workstation
// home path and for a private tracker id. This test runs the same two rules
// over the working tree, so a merge that brings one in fails here, in CI, and
// not at the release. The patterns are copied from the gate's rule list; the
// strings below are built in pieces so this file does not match itself.
//
// The gate's home-path rule is "/Users/" then a letter, except the two
// fictional homes alice and bob at a path boundary. RE2 has no lookahead, so
// the exception is checked by hand in homeHits.
var (
	pubHome    = regexp.MustCompile("/" + "Users" + "/([a-z][A-Za-z0-9_]*)?")
	pubHomeOK  = regexp.MustCompile(`^(?:alice|bob)$`)
	pubTracker = regexp.MustCompile(`\b` + "ADR" + `-0\d\d\b|\b` + "BACKLOG" + `-\d+\b`)
)

// homeHits counts the real home paths in data.
func homeHits(data []byte) int {
	n := 0
	for _, m := range pubHome.FindAllSubmatchIndex(data, -1) {
		if m[2] < 0 { // "/Users/" followed by something that is not a lowercase letter
			continue
		}
		// A name that only starts like alice (alicex) is a real home. The name
		// group stops at the first non-word byte, which is the gate's boundary.
		if pubHomeOK.Match(data[m[2]:m[3]]) {
			continue
		}
		n++
	}
	return n
}

func TestPublicationScanRules(t *testing.T) {
	root := "/" + "Users" + "/"
	cases := []struct {
		in   string
		want int
	}{
		{root + "alice/projects", 0},
		{root + "bob", 0},
		{root + "c/x", 1},
		{root + "someone", 1},
		{root + "alicex/p", 1},
		{root + "bobby/p", 1},
		{root + "Shared", 0},
		{"/home/you/work", 0},
	}
	for _, c := range cases {
		if got := homeHits([]byte(c.in)); got != c.want {
			t.Errorf("%q: %d hits, want %d", c.in, got, c.want)
		}
	}
	if !pubTracker.MatchString("see "+"BACKLOG"+"-12") || !pubTracker.MatchString("an "+"ADR"+"-004 reason") {
		t.Error("the tracker rule misses an id")
	}
	if pubTracker.MatchString("ADR-1") {
		t.Error("the tracker rule matched a non-id")
	}
}

func TestRepositoryHasNoWorkstationPathOrTrackerID(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" { // a folder, or a file in a linked worktree
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if n := homeHits(data); n > 0 {
			t.Errorf("%s: %d workstation path(s); build the path at run time or use /home/you", p, n)
		}
		if n := len(pubTracker.FindAll(data, -1)); n > 0 {
			t.Errorf("%s: %d private tracker id(s); build the string in pieces or remove it", p, n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
