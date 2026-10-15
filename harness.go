// harness.go: the runner an agent works inside. Claude Code by default; the
// service's catalog (GET /api/v2/models, harnesses) names the others, the
// provider each calls, and whether it is offered: a harness is offered once
// KeepState's certification run on it has passed. A session runs one
// harness; it changes only while the session is paused (ks session harness).
package main

import (
	"fmt"
	"strings"
)

const defaultHarness = "claude-code"

// catalogHarness is one harness as the catalog serves it.
type catalogHarness struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Pin          string `json:"pin"`
	DefaultModel string `json:"default_model"`
	Offer        struct {
		Standing string `json:"standing"`
		Note     string `json:"note"`
	} `json:"offer"`
	Models []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"models"`
}

// fetchHarnesses reads the catalog's harnesses; a service that predates
// them answers none.
func fetchHarnesses(cr hostedCreds) ([]catalogHarness, error) {
	var env struct {
		Data struct {
			Harnesses []catalogHarness `json:"harnesses"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/models", nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Harnesses, nil
}

// chooseHarness reads the named harness from the catalog before anything is
// created: an unknown one, or one the catalog withholds, is refused with the
// catalog's own reason. Claude Code needs no read.
func chooseHarness(cr hostedCreds, id string) (catalogHarness, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == defaultHarness {
		return catalogHarness{ID: defaultHarness, Name: "Claude Code", Provider: runnerProvider}, nil
	}
	hs, err := fetchHarnesses(cr)
	if err != nil {
		return catalogHarness{}, err
	}
	var ids []string
	for _, h := range hs {
		ids = append(ids, h.ID)
		if h.ID != id {
			continue
		}
		if h.Offer.Standing != "offered" {
			return h, &cliError{Code: exitConflict, Kind: "harness_unavailable",
				Message:    fmt.Sprintf("%s is not offered: %s. Nothing was created", harnessLabel(h), sanitize(h.Offer.Note)),
				NextAction: "ks cruise models (the harnesses and the models each runs)"}
		}
		return h, nil
	}
	return catalogHarness{}, &cliError{Code: exitUsage, Kind: "usage",
		Message:    fmt.Sprintf("--harness %s is not a harness this service runs (it runs: %s). Nothing was created", sanitize(id), strings.Join(ids, ", ")),
		NextAction: "ks cruise models"}
}

func harnessLabel(h catalogHarness) string {
	if h.Name != "" {
		return sanitize(h.Name)
	}
	return sanitize(h.ID)
}
