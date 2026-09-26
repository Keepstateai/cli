// models_catalog.go: the Cruise model catalog (KS-075).
//
//	ks cruise models            GET /api/v2/models: each model's exact id,
//	                            family, provider route and rung; what records
//	                            show about it under the Cruise runner
//	                            (exercised / not_verified, as stated); its
//	                            price standing; your own key route per
//	                            provider; the catalog version and when it was
//	                            served. Live provider availability is NOT
//	                            checked by the catalog and it says so.
//	ks cruise models --cached   the last catalog this client read, labelled
//	                            CACHED with its version and served_at -- never
//	                            presented as live
//
// ks cruise approve and ks cruise run check the ladder against the live
// catalog before anything is uploaded: a model the catalog does not list is
// refused, and nothing is substituted for it.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type catalogModelRow struct {
	ID                  string `json:"id"`
	Family              string `json:"family"`
	ProviderRoute       string `json:"provider_route"`
	Rung                int    `json:"rung"`
	RunnerCompatibility struct {
		Standing string   `json:"standing"`
		Runner   string   `json:"runner"`
		Protocol string   `json:"protocol"`
		Evidence []string `json:"evidence"`
		Note     string   `json:"note"`
	} `json:"runner_compatibility"`
	Availability struct {
		Standing       string `json:"standing"`
		CatalogVersion string `json:"catalog_version"`
		Live           string `json:"live"`
	} `json:"availability"`
	Price struct {
		Standing    string `json:"standing"`
		RateVersion string `json:"rate_version"`
		Note        string `json:"note"`
	} `json:"price"`
	KeyRoute struct {
		Provider string `json:"provider"`
		Standing string `json:"standing"`
		KeyID    string `json:"key_id,omitempty"`
	} `json:"key_route"`
}

type modelCatalogDoc struct {
	CatalogVersion string            `json:"catalog_version"`
	Ratified       string            `json:"ratified"`
	ServedAt       string            `json:"served_at"`
	DefaultLadder  []rung            `json:"default_ladder"`
	Models         []catalogModelRow `json:"models"`
	Note           string            `json:"note"`
	// set only on this client's cached copy
	CachedAt     string `json:"cached_at,omitempty"`
	ControlPlane string `json:"control_plane,omitempty"`
}

var errNoCatalog = errors.New("this control plane serves no model catalog (GET /api/v2/models)")

func catalogCachePath() string { return filepath.Join(configDir(), "models-catalog.json") }

// fetchCatalog reads the LIVE catalog and refreshes the labelled cache.
func fetchCatalog(cr hostedCreds) (*modelCatalogDoc, error) {
	resp, raw, err := doBounded(cr, "GET", "/api/v2/models", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoCatalog
	}
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("GET", "/api/v2/models", resp, raw)
	}
	var env struct {
		Data modelCatalogDoc `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.CatalogVersion == "" {
		return nil, fmt.Errorf("the model catalog could not be read (unexpected shape)")
	}
	c := env.Data
	cached := c
	cached.CachedAt, cached.ControlPlane = time.Now().UTC().Format(time.RFC3339), cr.CTL
	if b, err := json.MarshalIndent(cached, "", " "); err == nil {
		_ = os.MkdirAll(configDir(), 0o700)
		_ = os.WriteFile(catalogCachePath(), b, 0o600)
	}
	return &c, nil
}

func cachedCatalog() (*modelCatalogDoc, error) {
	b, err := os.ReadFile(catalogCachePath())
	if err != nil {
		return nil, fmt.Errorf("no cached catalog: run ks cruise models while signed in")
	}
	var c modelCatalogDoc
	if err := json.Unmarshal(b, &c); err != nil || c.CatalogVersion == "" {
		return nil, fmt.Errorf("the cached catalog is unreadable")
	}
	return &c, nil
}

// ladderAgainstCatalog refuses every rung the catalog does not list, by
// exact id within its family; nothing is ever substituted.
func ladderAgainstCatalog(c *modelCatalogDoc, ladder []any) []string {
	listed := map[string]bool{}
	for _, m := range c.Models {
		listed[m.Family+"\x00"+m.ID] = true
	}
	var out []string
	for i, x := range ladder {
		r, _ := x.(map[string]any)
		fam, _ := r["family"].(string)
		model, _ := r["model"].(string)
		if !listed[fam+"\x00"+model] {
			out = append(out, fmt.Sprintf("rung %d: model %q (family %s) is not listed in catalog %s served at %s", i+1, model, fam, c.CatalogVersion, figure(c.ServedAt)))
		}
	}
	return out
}

// checkLadderLive validates a draft's ladder against the live catalog. A
// control plane without a catalog is said; the job intake still validates.
func checkLadderLive(cr hostedCreds, m map[string]any) error {
	c, err := fetchCatalog(cr)
	if errors.Is(err, errNoCatalog) {
		progress("this control plane serves no model catalog; the job intake validates the ladder")
		return nil
	}
	if err != nil {
		return fmt.Errorf("the model catalog could not be read to check the ladder (%v); nothing was sent", errText(err))
	}
	ladder, _ := m["ladder"].([]any)
	if bad := ladderAgainstCatalog(c, ladder); len(bad) > 0 {
		return &cliError{Code: exitConflict, Kind: "ks_manifest_invalid",
			Message:    "the ladder names models the catalog does not list, so nothing was sent and nothing is substituted: " + strings.Join(bad, "; "),
			NextAction: "ks cruise models, then ks cruise init --ladder family:model,..."}
	}
	return nil
}

func cruiseModels(inv *Invocation) {
	var c *modelCatalogDoc
	var err error
	live := !inv.Bool("cached")
	if live {
		cr := mustCreds()
		c, err = fetchCatalog(cr)
		if errors.Is(err, errNoCatalog) {
			legacyModels(cr)
			return
		}
	} else {
		c, err = cachedCatalog()
	}
	if err != nil {
		die(err)
	}
	emit(map[string]any{"catalog": c, "live": live}, func() {
		if live {
			fmt.Printf("model catalog %s, served at %s (ratified %s)\n", c.CatalogVersion, figure(c.ServedAt), figure(c.Ratified))
		} else {
			fmt.Printf("CACHED model catalog %s, served at %s, cached %s from %s -- not a live answer\n", c.CatalogVersion, figure(c.ServedAt), figure(c.CachedAt), figure(c.ControlPlane))
		}
		fmt.Println("live provider availability is NOT checked by the catalog: it is known only when a call is made")
		models := append([]catalogModelRow(nil), c.Models...)
		sort.SliceStable(models, func(i, j int) bool {
			if models[i].Family != models[j].Family {
				return models[i].Family < models[j].Family
			}
			return models[i].Rung < models[j].Rung
		})
		fmt.Printf("  %-34s %-10s %-11s %-4s %-13s %-12s %s\n", "MODEL", "FAMILY", "ROUTE", "RUNG", "UNDER RUNNER", "PRICE", "YOUR KEY")
		for _, m := range models {
			key := figure(m.KeyRoute.Standing)
			if m.KeyRoute.KeyID != "" {
				key += " (" + m.KeyRoute.KeyID + ")"
			}
			fmt.Printf("  %-34s %-10s %-11s %-4d %-13s %-12s %s\n", m.ID, m.Family, m.ProviderRoute, m.Rung, figure(m.RunnerCompatibility.Standing), figure(m.Price.Standing), key)
		}
		if len(c.DefaultLadder) > 0 {
			fmt.Printf("default ladder: %s\n", ladderWords(c.DefaultLadder))
		}
		fmt.Println("under runner: exercised = a record shows the Cruise runner ran it; not_verified = nothing does (no interchangeability is claimed)")
		if c.Note != "" {
			fmt.Println(sanitize(c.Note))
		}
	})
}

// legacyModels is the older control plane's table.
func legacyModels(c hostedCreds) {
	var t modelTable
	if err := hostedCall(c, "GET", "/api/models", nil, &t); err != nil {
		die(err)
	}
	if out.json {
		emit(t, nil)
		return
	}
	fmt.Printf("model table %s (from %s; this control plane serves no catalog)\n", t.Version, c.CTL)
	for _, f := range familyNames(t) {
		fmt.Printf("  %s (provider %s): %s\n", f, t.Families[f].Provider, strings.Join(t.Families[f].Rungs, ", "))
	}
	if len(t.DefaultLadder) > 0 {
		fmt.Printf("default ladder: %s\n", ladderWords(t.DefaultLadder))
	}
}
