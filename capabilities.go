// capabilities.go: negotiation with the control plane's capability
// registry (KS-010). The client never assumes a feature exists because it
// knows the verb: a command that needs a capability checks the LIVE
// registry before it runs, and a control plane that lacks it, or reports a
// state this client does not understand, disables the command with the
// reason. Read-only results are cached with their fetch time for doctor to
// show; execution never trusts the cache.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type capabilityRow struct {
	ID            string         `json:"id"`
	Availability  string         `json:"availability"`
	Summary       string         `json:"summary"`
	Surface       string         `json:"surface"`
	Prerequisites []string       `json:"prerequisites,omitempty"`
	Limits        map[string]any `json:"limits,omitempty"`
	Evidence      []string       `json:"evidence,omitempty"`
	Note          string         `json:"note,omitempty"`
}

type capabilitySet struct {
	RegistryVersion string          `json:"registry_version"`
	Build           string          `json:"build"`
	FetchedAt       string          `json:"fetched_at"`
	PriceBook       string          `json:"price_book"`
	Capabilities    []capabilityRow `json:"capabilities"`
	Limits          map[string]any  `json:"limits"`
	ControlPlane    string          `json:"control_plane"`
	CachedAt        string          `json:"cached_at"`
}

var errNoCapabilities = errors.New("this control plane publishes no capability registry (no GET /api/capabilities)")

func capabilitiesCachePath() string { return filepath.Join(configDir(), "capabilities.json") }

// fetchCapabilities reads the live registry, bounded, and refreshes the
// read-only cache. A plain 404 is the older control plane.
func fetchCapabilities(cr hostedCreds) (*capabilitySet, error) {
	resp, raw, err := doBounded(cr, "GET", "/api/capabilities", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoCapabilities
	}
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("GET", "/api/capabilities", resp, raw)
	}
	var env struct {
		Data capabilitySet `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.RegistryVersion == "" {
		return nil, fmt.Errorf("the capability registry could not be read (unexpected shape)")
	}
	set := env.Data
	set.ControlPlane = cr.CTL
	set.CachedAt = time.Now().UTC().Format(time.RFC3339)
	if b, err := json.MarshalIndent(set, "", " "); err == nil {
		_ = os.MkdirAll(configDir(), 0o700)
		_ = os.WriteFile(capabilitiesCachePath(), b, 0o600)
	}
	return &set, nil
}

func cachedCapabilities() *capabilitySet {
	b, err := os.ReadFile(capabilitiesCachePath())
	if err != nil {
		return nil
	}
	var set capabilitySet
	if json.Unmarshal(b, &set) != nil || set.RegistryVersion == "" {
		return nil
	}
	return &set
}

func (s *capabilitySet) find(id string) *capabilityRow {
	for i := range s.Capabilities {
		if s.Capabilities[i].ID == id {
			return &s.Capabilities[i]
		}
	}
	return nil
}

// requireCapability is the gate a command with Needs passes before it
// runs: the live registry, not the cache. Absent, unavailable, degraded or
// a state this client does not know each disable the command with the
// reason; nothing is attempted on a guess.
func requireCapability(cr hostedCreds, c *Command) {
	if c.Needs == "" {
		return
	}
	set, err := fetchCapabilities(cr)
	if err != nil {
		if errors.Is(err, errNoCapabilities) {
			fail(&cliError{Code: exitFailed, Kind: "capability_unknown", Message: fmt.Sprintf("ks %s needs %s, and this control plane publishes no capability registry to confirm it; the command is disabled here", c.Name(), c.Needs)})
		}
		fail(&cliError{Code: exitTemporary, Kind: "capability_unreadable", Message: fmt.Sprintf("ks %s needs %s, and the capability registry could not be read: %v", c.Name(), c.Needs, sanitize(err.Error()))})
	}
	row := set.find(c.Needs)
	switch {
	case row == nil:
		fail(&cliError{Code: exitFailed, Kind: "capability_unsupported", Message: fmt.Sprintf("ks %s needs %s, which this control plane (registry %s, build %s) does not list; the command is disabled here", c.Name(), c.Needs, set.RegistryVersion, set.Build)})
	case row.Availability == "available":
		return
	case row.Availability == "unavailable" || row.Availability == "degraded":
		fail(&cliError{Code: exitFailed, Kind: "capability_" + row.Availability, Message: fmt.Sprintf("ks %s needs %s, which the control plane reports %s: %s", c.Name(), c.Needs, row.Availability, row.Note)})
	default:
		fail(&cliError{Code: exitFailed, Kind: "capability_state_unsupported", Message: fmt.Sprintf("ks %s needs %s, which the control plane reports in a state this client does not understand (%q); update the client rather than guessing", c.Name(), c.Needs, row.Availability)})
	}
}

// capabilitySummary is doctor's line: counts by availability, and every
// command of this client that the control plane cannot serve.
func capabilitySummary(set *capabilitySet) (line string, disabled []string) {
	counts := map[string]int{}
	for _, c := range set.Capabilities {
		switch c.Availability {
		case "available", "unavailable", "degraded":
			counts[c.Availability]++
		default:
			counts["unsupported-by-this-client"]++
		}
	}
	line = fmt.Sprintf("%d available, %d unavailable, %d degraded, %d in states this client does not know (registry %s, build %s, price book %s)",
		counts["available"], counts["unavailable"], counts["degraded"], counts["unsupported-by-this-client"], set.RegistryVersion, set.Build, set.PriceBook)
	for _, c := range reg { // the run-time alias: the registry's own initializer must not be named here
		if c.Needs == "" {
			continue
		}
		row := set.find(c.Needs)
		switch {
		case row == nil:
			disabled = append(disabled, fmt.Sprintf("ks %s: disabled, %s is not listed by this control plane", c.Name(), c.Needs))
		case row.Availability != "available":
			disabled = append(disabled, fmt.Sprintf("ks %s: disabled, %s is %s (%s)", c.Name(), c.Needs, row.Availability, row.Note))
		}
	}
	return line, disabled
}
