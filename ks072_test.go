// KS-072 cross-check: for the server's golden layouts, this client's
// discovery, pinned set and digest equal what judge/checks.py records.
//
// testdata/ks072/layouts.json is judge/tests/test_checks.py LAYOUTS and
// testdata/ks072/discovery.json is judge/tests/golden/ks072_discovery.json,
// both copied unchanged from the service repository (testdata/ks072/SOURCE.json records the commit and sha256; the KS-072
// merge). Regenerate both together, never one alone.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestKS072DiscoveryMatchesTheServersGolden(t *testing.T) {
	var layouts map[string]map[string]string
	var golden map[string]struct {
		Discovery json.RawMessage `json:"discovery"`
		Selection json.RawMessage `json:"selection"`
	}
	for p, v := range map[string]any{"testdata/ks072/layouts.json": &layouts, "testdata/ks072/discovery.json": &golden} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	if len(layouts) == 0 || len(layouts) != len(golden) {
		t.Fatalf("layouts %d, golden %d", len(layouts), len(golden))
	}
	names := make([]string, 0, len(layouts))
	for n := range layouts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		files := map[string]string{}
		for rel, body := range layouts[name] {
			s := sha256.Sum256([]byte(body))
			files[rel] = hex.EncodeToString(s[:])
		}
		d := ks072Discover(files, nil)
		got, _ := json.Marshal(d)
		if !sameJSON(t, got, golden[name].Discovery) {
			t.Errorf("%s: discovery differs\n got  %s\n want %s", name, got, golden[name].Discovery)
		}
		sel, code, err := ks072Select(d, "", "")
		var gotSel any
		if err != nil {
			gotSel = map[string]any{"refused": code}
		} else {
			rows := ks072InputsOf(d, sel.Ecosystem)
			gotSel = map[string]any{"pinned_inputs": rows, "pinned_inputs_digest": ks072InputsDigest(rows), "runner": sel.Runner, "source": sel.Source}
		}
		gs, _ := json.Marshal(gotSel)
		if !sameJSON(t, gs, golden[name].Selection) {
			t.Errorf("%s: selection differs\n got  %s\n want %s", name, gs, golden[name].Selection)
		}
	}
	// ambiguity names the choices; an explicit check resolves it
	files := map[string]string{}
	for rel, body := range layouts["mixed"] {
		s := sha256.Sum256([]byte(body))
		files[rel] = hex.EncodeToString(s[:])
	}
	d := ks072Discover(files, nil)
	if _, _, err := ks072Select(d, "", ""); err == nil || !strings.Contains(err.Error(), "--check pytest|node") {
		t.Errorf("ambiguous: %v", err)
	}
	if s, _, err := ks072Select(d, "node", ""); err != nil || s.Source != "explicit" || s.Ecosystem != "node" {
		t.Errorf("explicit: %v %v", s, err)
	}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// ks cruise init over the golden layouts: nested tests are pinned with
// every input they read, at their relative paths, digested with the
// repository formula; an ambiguous repository writes nothing until the
// check is named; the pinned-inputs digest equals the server's.
func TestCruiseInitPinsNestedTestsLikeTheVerifier(t *testing.T) {
	var layouts map[string]map[string]string
	b, _ := os.ReadFile("testdata/ks072/layouts.json")
	if err := json.Unmarshal(b, &layouts); err != nil {
		t.Fatal(err)
	}
	var golden map[string]struct {
		Selection struct {
			PinnedInputs []pinnedInput `json:"pinned_inputs"`
			Digest       string        `json:"pinned_inputs_digest"`
		} `json:"selection"`
	}
	gb, _ := os.ReadFile("testdata/ks072/discovery.json")
	if err := json.Unmarshal(gb, &golden); err != nil {
		t.Fatal(err)
	}
	_, bin, cfg := startFake(t)
	for _, name := range []string{"py-nested", "go-nested", "node-nested"} {
		repo := t.TempDir()
		writeTree(t, repo, layouts[name])
		out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--json")
		if code != 0 {
			t.Fatalf("%s: init exit %d\n%s%s", name, code, out, errs)
		}
		var doc struct {
			Data struct {
				Digest string `json:"pinned_inputs_digest"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &doc)
		if doc.Data.Digest != golden[name].Selection.Digest {
			t.Errorf("%s: pinned inputs digest %s, the server's %s", name, doc.Data.Digest, golden[name].Selection.Digest)
		}
		m, err := readDraftAt(repo)
		if err != nil {
			t.Fatal(err)
		}
		pp := m["verifier"].(map[string]any)["prohibited_paths"].([]any)
		got := map[string]bool{}
		for _, x := range pp {
			got[x.(string)] = true
		}
		for _, r := range golden[name].Selection.PinnedInputs {
			if !got[r.Path] {
				t.Errorf("%s: %s is not pinned", name, r.Path)
			}
		}
	}
	// mixed: ambiguous, nothing written; --check resolves it and is recorded
	repo := t.TempDir()
	writeTree(t, repo, layouts["mixed"])
	_, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init")
	if code != 2 || !strings.Contains(errs, "--check pytest|node") {
		t.Fatalf("mixed: %d\n%s", code, errs)
	}
	if _, err := os.Stat(repo + "/.keepstate"); err == nil {
		t.Fatal("an ambiguous init wrote a draft")
	}
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init", "--check", "node"); code != 0 {
		t.Fatalf("mixed --check node: %d\n%s", code, errs)
	}
	m, _ := readDraftAt(repo)
	if c, _ := m["verifier"].(map[string]any)["check"].(map[string]any); c["runner"] != "node" {
		t.Fatalf("the explicit check is not recorded: %v", m["verifier"])
	}
}

func readDraftAt(repo string) (map[string]any, error) {
	b, err := os.ReadFile(repo + "/" + cruiseDraft)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}
