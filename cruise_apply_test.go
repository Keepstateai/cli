// KS-078 QA-078-3 client: an accepted Cruise result applied file by file.
// The changeset's bytes are checked against status.result.changeset.sha256
// (the X-KS-Sha256 header and the bytes themselves) before they are read;
// then the KS-057 apply runs unchanged, so a changed local base is refused
// whole and the local work is kept.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type applyCtl struct {
	mu          sync.Mutex
	requests    []string
	changeset   []byte
	header      string // override for X-KS-Sha256; "" means the true digest, "-" means none
	statusSHA   string // override for status.result.changeset.sha256
	noCs        bool
	notAccepted bool
}

func (c *applyCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	cs, header, statusSHA, noCs, notAcc := c.changeset, c.header, c.statusSHA, c.noCs, c.notAccepted
	c.mu.Unlock()
	sum := sha256.Sum256(cs)
	truth := hex.EncodeToString(sum[:])
	if statusSHA == "" {
		statusSHA = truth
	}
	switch r.URL.Path {
	case "/api/jobs/job_c/status":
		w.Header().Set("Content-Type", "application/json")
		res := map[string]any{"artifact_sha256": strings.Repeat("a", 64), "badge": "verified", "limitations": []string{}}
		if !noCs {
			res["changeset"] = map[string]any{"sha256": statusSHA, "changes": 3, "download": "GET /api/jobs/job_c/changeset"}
		}
		st := map[string]any{"job_id": "job_c", "state": "accepted", "state_label": "Accepted: the checks passed", "result": res, "follow": map[string]any{}}
		if notAcc {
			st["state"], st["state_label"], st["result"] = "review", "Waiting for your decision", nil
		}
		_ = json.NewEncoder(w).Encode(st)
	case "/api/jobs/job_c/changeset":
		switch header {
		case "":
			w.Header().Set("X-KS-Sha256", truth)
		case "-":
		default:
			w.Header().Set("X-KS-Sha256", header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cs)
	default:
		w.WriteHeader(404)
	}
}

func (c *applyCtl) served() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func TestCruiseApplyChecksTheChangesetThenAppliesOntoItsBase(t *testing.T) {
	root, raw := changesetFixture(t)
	c := &applyCtl{changeset: raw}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])[:12]

	// without a terminal: confirmation required, nothing changed
	_, errs, code := auditExec(t, bin, cfg, root, fastEnv(cfg), "cruise", "apply", "job_c", "--dir", root)
	if code != exitUsage || !strings.Contains(errs, "ks cruise apply job_c --confirm "+want) {
		t.Fatalf("unconfirmed: exit %d\n%s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "old a\n" {
		t.Fatal("an unconfirmed apply changed a file")
	}

	// QA-078-3: the local base changed since the job's base: refused whole,
	// the local work kept
	writeFileT(t, filepath.Join(root, "a.txt"), "my own work\n")
	_, errs, code = auditExec(t, bin, cfg, root, fastEnv(cfg), "cruise", "apply", "job_c", "--dir", root, "--confirm", want)
	if code != exitConflict || !strings.Contains(errs, "not the base the changes were made against") || !strings.Contains(errs, "a.txt") {
		t.Fatalf("changed base: exit %d\n%s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "my own work\n" {
		t.Fatal("the conflicting local work was not kept")
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal("a refused apply deleted a file")
	}

	// the base restored: applied, confirmed by the digest
	writeFileT(t, filepath.Join(root, "a.txt"), "old a\n")
	_ = os.Chmod(filepath.Join(root, "a.txt"), 0o644)
	out, errs, code := auditExec(t, bin, cfg, root, fastEnv(cfg), "cruise", "apply", "job_c", "--dir", root, "--confirm", want)
	if code != 0 || !strings.Contains(out, "applied 3 change(s) from job_c/changeset") {
		t.Fatalf("apply: exit %d\n%s%s", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "new a\n" {
		t.Errorf("a.txt: %q", b)
	}
	for _, r := range c.served() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("apply sent %q", r)
		}
	}
}

func TestCruiseApplyRefusesAChangesetItCannotTrust(t *testing.T) {
	root, raw := changesetFixture(t)
	for name, c := range map[string]*applyCtl{
		"header differs": {changeset: raw, header: strings.Repeat("9", 64)},
		"no header":      {changeset: raw, header: "-"},
		"bytes differ":   {changeset: append(append([]byte(nil), raw...), ' '), statusSHA: func() string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }(), header: func() string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }()},
	} {
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		_, errs, code := auditExec(t, bin, cfg, root, fastEnv(cfg), "cruise", "apply", "job_c", "--dir", root, "--confirm", "x")
		srv.Close()
		if code != exitIntegrity || !strings.Contains(errs, "it is not read, and nothing was changed") {
			t.Errorf("%s: exit %d\n%s", name, code, errs)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "old a\n" {
		t.Fatal("an untrusted changeset changed a file")
	}
	// no changeset recorded, and a job not accepted: said, nothing fetched
	for name, c := range map[string]*applyCtl{"no changeset": {changeset: raw, noCs: true}, "not accepted": {changeset: raw, notAccepted: true}} {
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		_, errs, code := auditExec(t, bin, cfg, root, fastEnv(cfg), "cruise", "apply", "job_c", "--dir", root)
		srv.Close()
		if code == 0 || (!strings.Contains(errs, "without a per-file changeset") && !strings.Contains(errs, "only an accepted job")) {
			t.Errorf("%s: exit %d\n%s", name, code, errs)
		}
		for _, r := range c.served() {
			if strings.HasSuffix(r, "/changeset") {
				t.Errorf("%s: the changeset was fetched", name)
			}
		}
	}
}
