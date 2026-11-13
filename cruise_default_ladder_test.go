package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The default ladder this client drafts with (ks cruise init, and a run with
// no --ladder) is the service's: Claude Haiku 5.5, then Claude Sonnet 5.5.
func TestCruiseDefaultLadderIsHaikuFiveFiveThenSonnetFiveFive(t *testing.T) {
	want := []rung{{Family: "anthropic", Model: "claude-haiku-5-5"}, {Family: "anthropic", Model: "claude-sonnet-5-5"}}
	if !reflect.DeepEqual(embeddedModels.DefaultLadder, want) {
		t.Fatalf("the default ladder is %v, want %v", embeddedModels.DefaultLadder, want)
	}
	for _, r := range embeddedModels.DefaultLadder {
		if !embeddedModels.knows(r) {
			t.Errorf("the default rung %v is not in the table this client carries", r)
		}
	}
	// a bare model id of the 5.5 family is read as the table knows it
	l, err := parseLadder([]string{"claude-haiku-5-5", "claude-opus-5-5"})
	if err != nil || len(l) != 2 || l[0].Family != "anthropic" || l[1].Model != "claude-opus-5-5" {
		t.Fatalf("parseLadder of the 5.5 ids: %v, %v", l, err)
	}
}

// The embedded table mirrors the service's ctl/models.json, which the drift
// jobs hold it to when a server checkout is named.
func TestCruiseEmbeddedModelsMatchTheServer(t *testing.T) {
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service's table runs in the drift jobs")
	}
	raw, err := os.ReadFile(filepath.Join(server, "ctl", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var srv struct {
		Families      map[string]modelFamily `json:"families"`
		DefaultLadder []rung                 `json:"default_ladder"`
	}
	if err := json.Unmarshal(raw, &srv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(srv.DefaultLadder, embeddedModels.DefaultLadder) {
		t.Errorf("DRIFT: the service's default ladder is %v, this client's is %v (embeddedModels in cruise.go)", srv.DefaultLadder, embeddedModels.DefaultLadder)
	}
	for fam, f := range embeddedModels.Families {
		if got := srv.Families[fam]; !reflect.DeepEqual(got.Rungs, f.Rungs) {
			t.Errorf("DRIFT: family %s has the rungs %v in the service's table and %v in this client's", fam, got.Rungs, f.Rungs)
		}
	}
}
