// KS-074: approve binds the selection, the check's inputs and one key per
// ladder provider into the approved bytes; run sends NOTHING when any of
// them changed, and otherwise sends exactly the approved bytes.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheApprovalBindsWhatWillRunAndRunSendsNothingOnAChange(t *testing.T) {
	f, bin, cfg := startFake(t)
	repo := demoRepo(t)
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "init"); code != 0 {
		t.Fatalf("init: %s", errs)
	}
	out, errs, code := ksIn(t, bin, cfg, repo, "cruise", "approve")
	if code != 0 || !strings.Contains(out, "bound: upload selection") || !strings.Contains(out, "anthropic=vlt_anthropic1") {
		t.Fatalf("approve: %d\n%s%s", code, out, errs)
	}
	m, _ := readDraftAt(repo)
	ws := m["workspace"].(map[string]any)
	ver := m["verifier"].(map[string]any)
	if m["binding_version"] != float64(1) || !isHex64(ws["selection_digest"].(string)) || !isHex64(ver["inputs_digest"].(string)) ||
		ver["check"].(map[string]any)["runner"] != "pytest" {
		t.Fatalf("binding: %v %v %v", m["binding_version"], ws, ver)
	}
	routes := m["routes"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["key_created_at"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("routes: %v", routes)
	}
	jobs := func() int {
		n := 0
		for _, h := range f.seen() {
			if h == "POST /api/jobs" {
				n++
			}
		}
		return n
	}
	// the key was rotated after approval: nothing sent
	f.mu.Lock()
	f.keyAt = "2026-09-26T00:00:00Z"
	f.mu.Unlock()
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run"); code == 0 || !strings.Contains(errs, "was replaced after approval") || !strings.Contains(errs, "nothing was sent") || jobs() != 0 {
		t.Fatalf("rotated: %d\n%s", code, errs)
	}
	// disabled: nothing sent
	f.mu.Lock()
	f.keyAt, f.keyOff = "", true
	f.mu.Unlock()
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run"); code == 0 || !strings.Contains(errs, "is disabled") || jobs() != 0 {
		t.Fatalf("disabled: %d\n%s", code, errs)
	}
	// unchanged: the bytes sent are the approved bytes
	f.mu.Lock()
	f.keyOff = false
	f.mu.Unlock()
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run"); code != 0 || jobs() != 1 {
		t.Fatalf("run: %d\n%s", code, errs)
	}
	var posted struct {
		Manifest    json.RawMessage `json:"manifest"`
		ManifestSHA string          `json:"manifest_sha"`
	}
	f.mu.Lock()
	_ = json.Unmarshal(f.posted, &posted)
	f.mu.Unlock()
	lkRaw, _ := os.ReadFile(filepath.Join(repo, cruiseLock))
	var lk lockFile
	_ = json.Unmarshal(lkRaw, &lk)
	sum := sha256.Sum256(posted.Manifest)
	if hex.EncodeToString(sum[:]) != lk.SHA256 || posted.ManifestSHA != lk.SHA256 {
		t.Fatalf("the bytes sent are not the approved ones: %x vs %s", sum, lk.SHA256)
	}
	// a changed check input (a new conftest): nothing sent
	writeTree(t, repo, map[string]string{"conftest.py": "import os\n"})
	if _, errs, code := ksIn(t, bin, cfg, repo, "cruise", "run"); code == 0 || jobs() != 1 {
		t.Fatalf("changed input: %d\n%s", code, errs)
	}
}

func TestApproveNeverGuessesBetweenKeys(t *testing.T) {
	keys := []customerKey{{ID: "k1", Provider: "anthropic", Enabled: true, CreatedAt: "a"}, {ID: "k2", Provider: "anthropic", Enabled: true, CreatedAt: "b"}, {ID: "k3", Provider: "anthropic", Enabled: false}}
	if _, err := chooseRoutes(keys, []string{"anthropic"}, nil); err == nil || !strings.Contains(err.Error(), "--key anthropic=") {
		t.Fatalf("two enabled keys: %v", err)
	}
	r, err := chooseRoutes(keys, []string{"anthropic"}, map[string]string{"anthropic": "k2"})
	if err != nil || r[0].(map[string]any)["key_id"] != "k2" || r[0].(map[string]any)["key_created_at"] != "b" {
		t.Fatalf("named: %v %v", r, err)
	}
	if _, err := chooseRoutes(keys, []string{"anthropic"}, map[string]string{"anthropic": "k3"}); err == nil {
		t.Fatal("a disabled key was bound")
	}
	if _, err := chooseRoutes(keys, []string{"openrouter"}, nil); err == nil || !strings.Contains(err.Error(), "no enabled openrouter key") {
		t.Fatalf("missing provider: %v", err)
	}
}
