// manifest_test: commands.json is the machine-readable statement of what
// this binary dispatches, and it must never drift from the source. Both
// directions fail: a manifest verb with no case arm, and a case arm with
// no manifest row. Downstream documentation (keepstate.ai/docs/cli) is
// generated from commands.json, so a lie here becomes a public lie.
package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

type manifest struct {
	Commands []struct {
		Verb    string   `json:"verb"`
		Aliases []string `json:"aliases"`
		Status  string   `json:"status"`
		Surface string   `json:"surface"`
	} `json:"commands"`
}

// sourceVerbs extracts the case-arm strings of the two dispatch switches.
func sourceVerbs(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]bool{}
	re := regexp.MustCompile(`(?m)^\s*case ((?:"[a-z-]+"(?:, )?)+):`)
	q := regexp.MustCompile(`"([a-z-]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		for _, v := range q.FindAllStringSubmatch(m[1], -1) {
			if v[1][0] == '-' { // flag spellings (-v, --help) are meta, not verbs
				continue
			}
			verbs[v[1]] = true
		}
	}
	return verbs
}

func TestManifestMatchesDispatch(t *testing.T) {
	raw, err := os.ReadFile("commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	dispatch := sourceVerbs(t, "main.go")
	for v := range sourceVerbs(t, "hosted.go") {
		dispatch[v] = true
	}
	// meta arms that are not customer verbs
	for _, meta := range []string{"help"} {
		delete(dispatch, meta)
	}

	manifested := map[string]bool{}
	for _, c := range m.Commands {
		manifested[c.Verb] = true
		for _, a := range c.Aliases {
			manifested[a] = true
		}
		if !dispatch[c.Verb] {
			t.Errorf("manifest verb %q has no case arm in the source", c.Verb)
		}
		for _, a := range c.Aliases {
			if !dispatch[a] {
				t.Errorf("manifest alias %q (of %q) has no case arm in the source", a, c.Verb)
			}
		}
		if c.Status != "available" && c.Status != "planned" {
			t.Errorf("verb %q has status %q outside the enum", c.Verb, c.Status)
		}
	}
	for v := range dispatch {
		if !manifested[v] {
			t.Errorf("source dispatches %q but commands.json has no row for it", v)
		}
	}
}
