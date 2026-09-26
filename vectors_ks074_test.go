// KS-074 VER-074-1, the client's half: this client's canonical form passes
// the same versioned vectors as ctl and the judge
// (docs/upgrade/vectors/manifest-canonical-v1.json, vendored with its
// source recorded in testdata/vectors/SOURCE.json), and a drift guard
// compares the vendored copy with the service's when KS_SERVER_CHECKOUT is
// given (the ks072-golden workflow gives it).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func strictLoad(s string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// the number rule is enforced by the canonical writer itself
	_, err := canonicalJSON(v)
	return v, err
}

func TestKS074ClientPassesTheSharedCanonicalVectors(t *testing.T) {
	var v struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Cases   []struct {
			Name, Input, Canonical, SHA256 string
		} `json:"cases"`
		Refused []struct {
			Name, Input string
		} `json:"refused"`
	}
	b, err := os.ReadFile("testdata/vectors/manifest-canonical-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Name != "ks-manifest-canonical" || v.Version != 1 || len(v.Cases) == 0 {
		t.Fatalf("vectors: %v %s %d", err, v.Name, v.Version)
	}
	for _, c := range v.Cases {
		obj, err := strictLoad(c.Input)
		if err != nil {
			t.Errorf("%s: refused: %v", c.Name, err)
			continue
		}
		got, err := canonicalJSON(obj)
		if err != nil || string(got) != c.Canonical {
			t.Errorf("%s: canonical\n got  %s\n want %s (%v)", c.Name, got, c.Canonical, err)
		}
		s := sha256.Sum256(got)
		if hex.EncodeToString(s[:]) != c.SHA256 {
			t.Errorf("%s: sha256 %x, want %s", c.Name, s, c.SHA256)
		}
	}
	for _, c := range v.Refused {
		if _, err := strictLoad(c.Input); err == nil {
			t.Errorf("%s: %s was accepted", c.Name, c.Input)
		}
	}
}

func TestKS074VendoredVectorsMatchTheirSourceAndTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From   string `json:"from"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/vectors/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" {
		t.Fatalf("SOURCE.json: %v", err)
	}
	for name, f := range src.Files {
		if got := fileSHA(t, filepath.Join("testdata/vectors", name)); got != f.SHA256 {
			t.Errorf("testdata/vectors/%s is %s, SOURCE.json records %s", name, got, f.SHA256)
		}
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in CI")
	}
	for name, f := range src.Files {
		if got := fileSHA(t, filepath.Join(server, f.From)); got != f.SHA256 {
			t.Fatalf("DRIFT: the service's %s is now %s, but this client vendored %s (%s) from commit %s; re-vendor and re-run the vectors",
				f.From, got, f.SHA256, name, src.ServerCommit)
		}
	}
}
