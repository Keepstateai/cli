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

// TestProvenanceOfStatedFacts: the manifest is what the public docs page
// renders, so a number or a promise in it is a public claim. Two fields
// carry claims rather than descriptions, and neither may appear without
// saying where it came from. Founder ruling 2026-09-09 (DEC-01 findings 2
// and 3): document the kill guard as the safety property it is, and state
// the per-tier session budgets, with a guard on each.
func TestProvenanceOfStatedFacts(t *testing.T) {
	raw, err := os.ReadFile("commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Commands []struct {
			Verb           string `json:"verb"`
			Safety         string `json:"safety"`
			SafetyMeasured string `json:"safetyMeasured"`
			BudgetDefaults *struct {
				Unit           string `json:"unit"`
				Free           int    `json:"free"`
				Paid           int    `json:"paid"`
				FreeProvenance string `json:"freeProvenance"`
				PaidProvenance string `json:"paidProvenance"`
				Override       string `json:"override"`
			} `json:"budgetDefaults"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	sawSafety, sawBudget := false, false
	for _, c := range m.Commands {
		if c.Safety != "" {
			sawSafety = true
			if c.SafetyMeasured == "" {
				t.Errorf("%s states a safety property with no safetyMeasured: a promise the docs page "+
					"repeats must say when it was last confirmed", c.Verb)
			}
		}
		if c.SafetyMeasured != "" && c.Safety == "" {
			t.Errorf("%s records a measurement of a safety property it does not state", c.Verb)
		}
		if b := c.BudgetDefaults; b != nil {
			sawBudget = true
			if b.Free <= 0 || b.Paid <= 0 {
				t.Errorf("%s budgetDefaults must state both tiers as positive numbers, got free=%d paid=%d",
					c.Verb, b.Free, b.Paid)
			}
			if b.Unit == "" || b.Override == "" {
				t.Errorf("%s budgetDefaults must name its unit and how to override it", c.Verb)
			}
			if b.FreeProvenance == "" || b.PaidProvenance == "" {
				t.Errorf("%s budgetDefaults states numbers without provenance for each tier; the product "+
					"constitution carried a single wrong number for exactly this reason", c.Verb)
			}
		}
	}
	// The two rulings are only satisfied if the manifest actually carries
	// them. An empty manifest must not pass this test quietly.
	if !sawSafety {
		t.Error("no verb states a safety property; the kill guard is a ruled entry (DEC-01 finding 2)")
	}
	if !sawBudget {
		t.Error("no verb states budgetDefaults; the per-tier budgets are a ruled entry (DEC-01 finding 3)")
	}
}
