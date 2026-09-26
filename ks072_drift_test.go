// KS-072 drift guard: the vendored golden is exactly the file recorded in
// testdata/ks072/SOURCE.json (server commit and sha256), and -- wherever a
// checkout of the service repository is given in KS_SERVER_CHECKOUT, as the
// ks072-golden CI workflow gives it -- the service's current golden has the
// same sha256. A server change to the golden therefore fails this client's
// CI until the golden is re-vendored and the port re-checked.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestKS072VendoredGoldenMatchesItsSourceAndTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		Files        map[string]struct {
			From   string `json:"from"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	b, err := os.ReadFile("testdata/ks072/SOURCE.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &src); err != nil || src.ServerCommit == "" {
		t.Fatalf("SOURCE.json: %v", err)
	}
	for name, f := range src.Files {
		if got := fileSHA(t, filepath.Join("testdata/ks072", name)); got != f.SHA256 {
			t.Errorf("testdata/ks072/%s is %s, SOURCE.json records %s: re-vendor both from the service together", name, got, f.SHA256)
		}
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service's current golden runs in the ks072-golden CI workflow")
	}
	golden := src.Files["discovery.json"]
	if got := fileSHA(t, filepath.Join(server, golden.From)); got != golden.SHA256 {
		t.Fatalf("DRIFT: the service's %s is now %s, but this client vendored %s from commit %s; re-vendor it (and the layouts), re-run the KS-072 cross-check and update SOURCE.json",
			golden.From, got, golden.SHA256, src.ServerCommit)
	}
}
