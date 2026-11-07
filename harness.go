// harness.go: the runner an agent works inside. Claude Code by default; the
// service's catalog (GET /api/v2/models, harnesses) names the others, the
// provider each calls, and whether it is offered: a harness is offered once
// KeepState's certification run on it has passed. A session runs one
// harness; it changes only while the session is paused (ks session harness).
package main

import (
	"errors"
	"fmt"
	"net/http"
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
	Models []harnessModel `json:"models"`
}

// harnessModel is one model a harness runs, as the catalog serves it.
type harnessModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Offer says whether a session may be created with it or changed to it
	// on this harness (offered), or not, with the reason (withheld); absent
	// from an older catalog
	Offer struct {
		Standing string `json:"standing"`
		Note     string `json:"note,omitempty"`
	} `json:"offer"`
	// Efforts: the effort levels the harness and model take, in order;
	// empty when the model takes none, absent (nil) from an older
	// catalog. DefaultEffort: the level used when none is chosen.
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	// Provider is the provider whose key the model calls
	// (openrouter for an open model, else the harness's), Route the words a
	// person reads beside an open model ("open model via OpenRouter"),
	// Price its list price per million tokens, ManagedOffer its price on
	// KeepState's key while a managed book is in force, and KeyRoute this
	// account's key for its provider when that is not the harness's. All
	// absent from an older catalog.
	Provider      string               `json:"provider,omitempty"`
	Route         string               `json:"route,omitempty"`
	ContextTokens int64                `json:"context_tokens,omitempty"`
	Price         *harnessModelPrice   `json:"price,omitempty"`
	ManagedOffer  *harnessManagedOffer `json:"managed_offer,omitempty"`
	KeyRoute      *harnessKeyRoute     `json:"key_route,omitempty"`
}

type harnessModelPrice struct {
	Standing    string        `json:"standing"` // priced | unavailable
	RateVersion string        `json:"rate_version,omitempty"`
	List        *managedRates `json:"list_microusd_per_mtok,omitempty"`
	Note        string        `json:"note,omitempty"`
}

type harnessManagedOffer struct {
	Standing string       `json:"standing"` // offered | withheld
	Charged  managedRates `json:"charged_microusd_per_mtok"`
}

type harnessKeyRoute struct {
	Provider    string `json:"provider"`
	Standing    string `json:"standing"` // enabled | missing | unavailable
	KeyID       string `json:"key_id,omitempty"`
	Managed     string `json:"managed,omitempty"`
	ManagedNote string `json:"managed_note,omitempty"`
}

// openModel says whether a catalog model is an open model: one that calls a
// provider of its own (OpenRouter) rather than its harness's.
func (m harnessModel) openModel(h catalogHarness) bool {
	return m.Route != "" || (m.Provider != "" && h.Provider != "" && m.Provider != h.Provider)
}

// keyProvider is the provider whose key the model calls on harness h.
func (m harnessModel) keyProvider(h catalogHarness) string {
	return firstNonEmptyStr(m.Provider, h.Provider)
}

// label is the model as a person reads it: its id and, when the catalog
// names it, its name.
func (m harnessModel) label() string {
	if m.Name != "" && m.Name != m.ID {
		return sanitize(m.ID) + " (" + sanitize(m.Name) + ")"
	}
	return sanitize(m.ID)
}

// perMtokDollars is a price in micro-USD per million tokens, as dollars:
// two decimals, more only where the price has them ($0.0699).
func perMtokDollars(micros int64) string {
	s := fmt.Sprintf("%.6f", float64(micros)/1e6)
	s = strings.TrimRight(s, "0")
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	return "$" + s
}

// inOutWords is a rate's input and output prices, in words.
func inOutWords(r managedRates) string {
	return perMtokDollars(r.Input) + " in / " + perMtokDollars(r.Output) + " out per million tokens"
}

// openModelLine is what a person reads about an open model: its route, its
// offer, its list price on their own key and, where KeepState's key offers
// it, its price there.
func (m harnessModel) openModelLine(h catalogHarness) string {
	route := firstNonEmptyStr(sanitize(m.Route), "open model via "+providerName(m.keyProvider(h)))
	line := route + ": " + m.label() + ", " + figure(sanitize(m.Offer.Standing))
	switch {
	case m.Price != nil && m.Price.Standing == "priced" && m.Price.List != nil:
		line += "; " + inOutWords(*m.Price.List) + " on your " + providerName(m.keyProvider(h)) + " key"
	case m.Price != nil && m.Price.Note != "":
		line += "; price: " + sanitize(m.Price.Note)
	}
	if m.ManagedOffer != nil && m.ManagedOffer.Standing == "offered" {
		line += "; on KeepState's key: " + inOutWords(m.ManagedOffer.Charged)
	}
	return line
}

// harnessByID finds a harness in the catalog's list.
func harnessByID(hs []catalogHarness, id string) (catalogHarness, bool) {
	for _, h := range hs {
		if h.ID == id {
			return h, true
		}
	}
	return catalogHarness{}, false
}

// chooseModel reads the model a session is to run on harness h from the
// catalog, before anything is sent: one the catalog withholds there is
// refused with the catalog's own note. A model the catalog does not list
// for the harness (or a catalog that lists no models, or no catalog) is left
// to the service, which decides; ok is then false. h.Models is read when
// the harness came from the catalog; otherwise the catalog is read now.
func chooseModel(cr hostedCreds, h catalogHarness, model, nothing string) (harnessModel, bool, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return harnessModel{}, false, nil
	}
	if h.Models == nil {
		hs, err := fetchHarnesses(cr)
		if err != nil {
			var he *hostedErr
			if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
				return harnessModel{}, false, nil // no catalog: the service decides
			}
			return harnessModel{}, false, err
		}
		got, ok := harnessByID(hs, firstNonEmptyStr(h.ID, defaultHarness))
		if !ok {
			return harnessModel{}, false, nil
		}
		if h.Provider == "" {
			h.Provider = got.Provider
		}
		h.Models = got.Models
		if h.Name == "" {
			h.Name = got.Name
		}
	}
	for _, m := range h.Models {
		if m.ID != model {
			continue
		}
		if m.Offer.Standing != "" && m.Offer.Standing != "offered" {
			why := firstNonEmptyStr(sanitize(m.Offer.Note), "the catalog does not offer it")
			return m, true, &cliError{Code: exitConflict, Kind: "model_unavailable",
				Message:    fmt.Sprintf("%s is not offered on %s: %s. %s", m.label(), harnessLabel(h), why, nothing),
				NextAction: "ks cruise models (the models each harness offers)"}
		}
		if m.Provider == "" {
			m.Provider = h.Provider
		}
		return m, true, nil
	}
	return harnessModel{}, false, nil
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
