// manifest_test: commands.json is the machine-readable statement of what
// this binary dispatches, and it must never drift from the registry in
// command.go/main.go, in either direction: a manifest verb the registry
// does not have, a registry verb the manifest does not list, a usage line
// or a flag that differs. Downstream documentation (keepstate.ai/docs/cli)
// is generated from commands.json, so a lie here becomes a public lie.
//
// The manifest's usage lines and flag tables are GENERATED from the
// registry (KS_MANIFEST_DUMP writes the registry as JSON for the sync
// script) and this test is the guard that they were regenerated.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type manifestRow struct {
	Verb    string   `json:"verb"`
	Aliases []string `json:"aliases"`
	Usage   string   `json:"usage"`
	Summary string   `json:"summary"`
	Status  string   `json:"status"`
	Surface string   `json:"surface"`
	Flags   []struct {
		Flag    string `json:"flag"`
		Summary string `json:"summary"`
	} `json:"flags"`
}

func loadManifest(t *testing.T) []manifestRow {
	t.Helper()
	raw, err := os.ReadFile("commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Commands []manifestRow `json:"commands"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Commands
}

// registryRow is the registry's own description of a command, the shape
// the manifest sync script consumes.
type registryRow struct {
	Verb    string   `json:"verb"`
	Aliases []string `json:"aliases"`
	Usage   string   `json:"usage"`
	Summary string   `json:"summary"`
	Surface string   `json:"surface"`
	Flags   []struct {
		Flag    string `json:"flag"`
		Summary string `json:"summary"`
	} `json:"flags"`
}

func registryRows() []registryRow {
	var out []registryRow
	for _, c := range registry {
		if c.Group {
			continue
		}
		r := registryRow{Verb: c.Name(), Usage: c.Usage(), Summary: c.Summary, Surface: c.Surface, Aliases: []string{}}
		for _, a := range c.Aliases {
			r.Aliases = append(r.Aliases, strings.Join(a, " "))
		}
		for _, f := range c.Flags {
			name := f.render()
			if len(f.Aliases) > 0 {
				name += " (also --" + strings.Join(f.Aliases, ", --") + ")"
			}
			r.Flags = append(r.Flags, struct {
				Flag    string `json:"flag"`
				Summary string `json:"summary"`
			}{name, f.Summary})
		}
		out = append(out, r)
	}
	return out
}

// TestRegistryDump writes the registry as JSON when KS_MANIFEST_DUMP names
// a file; scripts/sync-manifest.py merges it into commands.json. It is a
// generator, not a check, and does nothing otherwise.
func TestRegistryDump(t *testing.T) {
	p := os.Getenv("KS_MANIFEST_DUMP")
	if p == "" {
		t.Skip("set KS_MANIFEST_DUMP=path to export the registry")
	}
	b, _ := json.MarshalIndent(registryRows(), "", " ")
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestMatchesRegistry(t *testing.T) {
	rows := loadManifest(t)
	byVerb := map[string]manifestRow{}
	manifested := map[string]bool{}
	for _, r := range rows {
		byVerb[r.Verb] = r
		manifested[r.Verb] = true
		for _, a := range r.Aliases {
			manifested[a] = true
		}
		if r.Status != "available" && r.Status != "planned" {
			t.Errorf("verb %q has status %q outside the enum", r.Verb, r.Status)
		}
		if r.Surface != "client" && r.Surface != "hosted" {
			t.Errorf("verb %q has surface %q outside the enum", r.Verb, r.Surface)
		}
	}
	known := map[string]bool{}
	for _, reg := range registryRows() {
		known[reg.Verb] = true
		for _, a := range reg.Aliases {
			known[a] = true
		}
		row, ok := byVerb[reg.Verb]
		if !ok {
			t.Errorf("registry dispatches %q but commands.json has no row for it", reg.Verb)
			continue
		}
		if row.Usage != reg.Usage {
			t.Errorf("%s: manifest usage %q, registry renders %q (regenerate: KS_MANIFEST_DUMP=r.json go test -run TestRegistryDump && python3 scripts/sync-manifest.py r.json)", reg.Verb, row.Usage, reg.Usage)
		}
		if row.Summary != reg.Summary {
			t.Errorf("%s: manifest summary %q, registry says %q", reg.Verb, row.Summary, reg.Summary)
		}
		if row.Surface != reg.Surface {
			t.Errorf("%s: manifest surface %q, registry says %q", reg.Verb, row.Surface, reg.Surface)
		}
		if strings.Join(row.Aliases, ",") != strings.Join(reg.Aliases, ",") {
			t.Errorf("%s: manifest aliases %v, registry has %v", reg.Verb, row.Aliases, reg.Aliases)
		}
		want := map[string]string{}
		for _, f := range reg.Flags {
			want[f.Flag] = f.Summary
		}
		got := map[string]string{}
		for _, f := range row.Flags {
			got[f.Flag] = f.Summary
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: manifest flag %q is %q, registry says %q", reg.Verb, k, got[k], v)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%s: manifest describes flag %q the registry does not have", reg.Verb, k)
			}
		}
	}
	for v := range manifested {
		if !known[v] {
			t.Errorf("manifest verb or alias %q has no registry command", v)
		}
	}
}

// TestFlagsAreDescribed: a row that lists flags lists each with its
// meaning, and every flag it lists appears in the row's usage line, so
// the docs page never renders a flag the binary does not take.
func TestFlagsAreDescribed(t *testing.T) {
	for _, c := range loadManifest(t) {
		for _, f := range c.Flags {
			name := strings.Fields(f.Flag)
			if len(name) == 0 || f.Summary == "" {
				t.Errorf("%s: a flag row needs a flag and a summary", c.Verb)
				continue
			}
			if !strings.Contains(c.Usage, name[0]) {
				t.Errorf("%s: flag %s is described but absent from the usage line %q", c.Verb, name[0], c.Usage)
			}
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
			// the override must be a flag the registry actually takes
			if !strings.Contains(b.Override, "--budget-tokens") {
				t.Errorf("%s budgetDefaults.override %q does not name the canonical flag --budget-tokens", c.Verb, b.Override)
			}
		}
	}
	if !sawSafety {
		t.Error("no verb states a safety property; the kill guard is a ruled entry (DEC-01 finding 2)")
	}
	if !sawBudget {
		t.Error("no verb states budgetDefaults; the per-tier budgets are a ruled entry (DEC-01 finding 3)")
	}
}

// TestManifestDeclaresTheGlobalFlags: commands.json lists the flags every
// command accepts, exactly as output.go's globalFlags defines them.
//
// Written 2026-09-24. v0.1.9's manifest listed each verb's own flags and
// none of the global ones, so a downstream reader could not tell that
// `ks meter <session> --json` is valid: the website's docs lint refused a
// correct instruction because nothing it could read said --json exists.
// A flag the binary accepts and the manifest omits is a statement the
// manifest fails to make; this holds the two together in both directions.
func TestManifestDeclaresTheGlobalFlags(t *testing.T) {
	raw, err := os.ReadFile("commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		GlobalFlags []struct {
			Flag    string `json:"flag"`
			Summary string `json:"summary"`
		} `json:"globalFlags"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.GlobalFlags) == 0 {
		t.Fatal("commands.json declares no globalFlags; every command accepts output.go's globalFlags and the manifest must say so")
	}
	want := map[string]string{}
	for _, f := range globalFlags {
		want[f.render()] = f.Summary
	}
	got := map[string]string{}
	for _, f := range m.GlobalFlags {
		got[f.Flag] = f.Summary
	}
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("the binary accepts global flag %q and commands.json does not declare it", k)
		} else if g != v {
			t.Errorf("global flag %q: manifest says %q, output.go says %q", k, g, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("commands.json declares global flag %q the binary does not accept", k)
		}
	}
}
