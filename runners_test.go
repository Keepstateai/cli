// The runner list: the vendored copy is the service's ctl/runners.json byte
// for byte (wherever KS_SERVER_CHECKOUT names a checkout of the service, as
// the drift jobs do), and this client classifies every path of the shared
// vectors exactly as the verifier does. A runner is chosen only while the
// list offers it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunnersVendoredListMatchesTheServer(t *testing.T) {
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service's list runs in the drift jobs")
	}
	got, err := os.ReadFile(filepath.Join(server, "ctl", "runners.json"))
	if err != nil {
		t.Fatal(err)
	}
	vec, err := os.ReadFile(filepath.Join(server, "docs", "upgrade", "vectors", "runners-classify-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KS_RUNNERS_WRITE") == "1" {
		head, _ := os.ReadFile("runners_list.go")
		i := strings.Index(string(head), "const runnersJSON = `")
		if i < 0 {
			t.Fatal("runners_list.go has no runnersJSON constant")
		}
		if err := os.WriteFile("runners_list.go", []byte(string(head[:i])+"const runnersJSON = `"+string(got)+"`\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/runners/runners-classify-v1.json", vendoredVectors(t, vec), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("rewritten from the service; run the tests again")
	}
	if string(got) != runnersJSON {
		t.Fatalf("DRIFT: the service's ctl/runners.json differs from the vendored runners_list.go; re-vendor with KS_RUNNERS_WRITE=1")
	}
	mine, _ := os.ReadFile("testdata/runners/runners-classify-v1.json")
	if string(mine) != string(vendoredVectors(t, vec)) {
		t.Fatalf("DRIFT: the service's runner classification vectors differ from testdata/runners; re-vendor with KS_RUNNERS_WRITE=1")
	}
}

// vendoredVectors is the service's vectors file as this client keeps it: the
// same digest and cases, without the service's source note.
func vendoredVectors(t *testing.T, server []byte) []byte {
	t.Helper()
	var v struct {
		RunnersSHA256 string            `json:"runners_sha256"`
		Cases         []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(server, &v); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("{\n \"about\": \"the service's runner classification vectors (path -> ecosystem roles), vendored without their source note\",\n")
	b.WriteString(" \"runners_sha256\": \"" + v.RunnersSHA256 + "\",\n \"cases\": [")
	for i, c := range v.Cases {
		var x any
		_ = json.Unmarshal(c, &x)
		one, _ := json.Marshal(x)
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n  " + string(one))
	}
	b.WriteString("\n ]\n}\n")
	return []byte(b.String())
}

func TestRunnersClassifyAsTheVerifier(t *testing.T) {
	b, err := os.ReadFile("testdata/runners/runners-classify-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		RunnersSHA256 string `json:"runners_sha256"`
		Cases         []struct {
			Path  string     `json:"path"`
			Roles [][]string `json:"roles"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(runnersJSON))
	if hex.EncodeToString(sum[:]) != v.RunnersSHA256 {
		t.Fatal("the vectors were written from another runner list; re-vendor both together")
	}
	if len(v.Cases) < 1000 {
		t.Fatalf("only %d vectors", len(v.Cases))
	}
	for _, c := range v.Cases {
		got := [][]string{}
		for _, r := range ks072Classify(c.Path) {
			got = append(got, []string{r.eco, r.role})
		}
		want := c.Roles
		if want == nil {
			want = [][]string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, the verifier %v", c.Path, got, want)
		}
	}
}

func TestRunnersRustAndRubyAreChosen(t *testing.T) {
	disc := func(paths ...string) discovery {
		files := map[string]string{}
		for _, p := range paths {
			files[p] = strings.Repeat("0", 64)
		}
		return ks072Discover(files, nil)
	}
	if s, _, err := ks072Select(disc("Cargo.toml", "Cargo.lock", "src/lib.rs", "tests/calc.rs"), "", ""); err != nil || s.Runner != "cargo" || ks072Command(s.Runner) != "cargo test --test '*'" {
		t.Fatalf("a Rust crate: %+v %v", s, err)
	}
	if _, code, err := ks072Select(disc("Cargo.toml", "tests/calc.rs"), "", ""); code != "ks_check_not_found" || !strings.Contains(err.Error(), "needs the project's Cargo.lock") {
		t.Fatalf("a Rust crate without its lockfile: %s %v", code, err)
	}
	if s, _, err := ks072Select(disc("lib/calc.rb", "test/test_calc.rb"), "", ""); err != nil || s.Runner != "minitest" {
		t.Fatalf("a Ruby project: %+v %v", s, err)
	}
	if s, _, err := ks072Select(disc("tests/test_a.py", "Cargo.toml", "Cargo.lock", "crates/x/tests/it.rs"), "", ""); err != nil || s.Runner != "pytest" {
		t.Fatalf("a legacy check beside a later language is chosen as before: %+v %v", s, err)
	}
	if _, code, err := ks072Select(disc("tests/test_a.py"), "rspec", ""); code != "ks_check_invalid" || !strings.Contains(err.Error(), "pytest, go, node, cargo or minitest") {
		t.Fatalf("an unknown runner: %s %v", code, err)
	}
}
