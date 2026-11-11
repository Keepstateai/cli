// A customer can download the signed receipt of an accepted Cruise job:
//
//	ks cruise receipt JOB [--out FILE]
//
// The file is written exactly as the service serves it. Its bytes are checked
// against the digest the job's status names (the X-KS-Sha256 header and the
// bytes themselves) before anything is written, and nothing on the path
// parses the receipt and writes it out again: the Ed25519 signature covers
// what the receipt means in its canonical form, and a re-written copy changes
// numbers and escapes.
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

// sealedLooking is a receipt file as the judge writes it: indented, a trailing
// newline, a number written 1.0 and an escaped non-ASCII letter. A program
// that parsed and re-wrote it would change every one of those.
const sealedLooking = "{\n  \"candidate_digest\": \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\n  \"elapsed_s\": 1.0,\n  \"hmac_sha256\": \"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\",\n  \"key_id\": \"kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk\",\n  \"note\": \"caf\\u00e9\",\n  \"result\": \"accepted\",\n  \"sig\": \"c2ln\",\n  \"sig_alg\": \"ed25519\"\n}\n"

type signedReceiptCtl struct {
	mu        sync.Mutex
	requests  []string
	receipt   []byte
	header    string // override for X-KS-Sha256; "" = the true digest, "-" = none
	statusSHA string // override for status.result.receipt.sha256
	alg       string // "ed25519" or "" (HMAC only)
	none      bool   // the status names no receipt (an older result)
	notDone   bool
}

func (c *signedReceiptCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, r.Method+" "+r.URL.Path)
	rc, header, statusSHA, alg, none, notDone := c.receipt, c.header, c.statusSHA, c.alg, c.none, c.notDone
	c.mu.Unlock()
	sum := sha256.Sum256(rc)
	truth := hex.EncodeToString(sum[:])
	if statusSHA == "" {
		statusSHA = truth
	}
	switch r.URL.Path {
	case "/api/jobs/job_r/status":
		w.Header().Set("Content-Type", "application/json")
		res := map[string]any{"artifact_sha256": strings.Repeat("a", 64), "badge": "verified", "limitations": []string{}}
		if !none {
			res["receipt"] = map[string]any{"sha256": statusSHA, "sig_alg": alg, "key_id": strings.Repeat("e", 64),
				"publicly_verifiable": alg == "ed25519", "download": "GET /api/jobs/job_r/receipt"}
		}
		st := map[string]any{"job_id": "job_r", "state": "accepted", "state_label": "Accepted: the checks passed", "result": res, "follow": map[string]any{}}
		if notDone {
			st["state"], st["state_label"], st["result"] = "review", "Waiting for your decision", nil
		}
		_ = json.NewEncoder(w).Encode(st)
	case "/api/jobs/job_r/receipt":
		switch header {
		case "":
			w.Header().Set("X-KS-Sha256", truth)
		case "-":
		default:
			w.Header().Set("X-KS-Sha256", header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rc)
	default:
		w.WriteHeader(404)
	}
}

func (c *signedReceiptCtl) served() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func TestCruiseReceiptWritesTheExactBytes(t *testing.T) {
	c := &signedReceiptCtl{receipt: []byte(sealedLooking), alg: "ed25519"}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	out := filepath.Join(dir, "r.json")

	stdout, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r", "--out", out)
	if code != 0 || strings.TrimSpace(stdout) != out {
		t.Fatalf("receipt: exit %d, stdout %q\n%s", code, stdout, errs)
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != sealedLooking {
		t.Fatalf("the written receipt is not the served bytes (%v):\n%q", err, got)
	}
	if !strings.Contains(errs, "publicly verifiable") || strings.Contains(errs, "service-verifiable only") {
		t.Errorf("the words for an Ed25519 receipt:\n%s", errs)
	}
	// the default name is in the working directory and an existing file is
	// never written over
	if _, _, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r"); code != 0 {
		t.Fatalf("default name: exit %d", code)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "job_r.receipt.json")); err != nil || string(b) != sealedLooking {
		t.Fatalf("default file: %v", err)
	}
	_, errs, code = auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r", "--out", out)
	if code != exitConflict || !strings.Contains(errs, "never written over") {
		t.Fatalf("an existing file: exit %d\n%s", code, errs)
	}
	// SABOTAGE (in the test): a copy that was parsed and written out again has
	// other bytes, which is what the digest check below refuses.
	var v map[string]any
	_ = json.Unmarshal([]byte(sealedLooking), &v)
	again, _ := json.Marshal(v)
	if string(again) == sealedLooking {
		t.Fatal("the fixture would not catch a re-serialising client")
	}
}

func TestCruiseReceiptHMACOnlySaysSo(t *testing.T) {
	c := &signedReceiptCtl{receipt: []byte(strings.Replace(sealedLooking, "ed25519", "", 1)), alg: ""}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r", "--out", filepath.Join(dir, "r.json"))
	if code != 0 || !strings.Contains(errs, "service-verifiable only") || strings.Contains(errs, "publicly verifiable") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
}

func TestCruiseReceiptRefusesBytesThatAreNotTheRecordedOnes(t *testing.T) {
	for name, c := range map[string]*signedReceiptCtl{
		"header names other bytes":   {receipt: []byte(sealedLooking), alg: "ed25519", header: strings.Repeat("0", 64)},
		"no header":                  {receipt: []byte(sealedLooking), alg: "ed25519", header: "-"},
		"status names other bytes":   {receipt: []byte(sealedLooking), alg: "ed25519", statusSHA: strings.Repeat("1", 64)},
		"served bytes re-serialised": {receipt: reserialisedJSON(t, sealedLooking), alg: "ed25519", statusSHA: sha256Of(sealedLooking), header: sha256Of(sealedLooking)},
	} {
		syncFixture(t, &c.mu)
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		dir := t.TempDir()
		out := filepath.Join(dir, "r.json")
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r", "--out", out)
		srv.Close()
		if code != exitIntegrity || !strings.Contains(errs, "nothing was written") {
			t.Errorf("%s: exit %d\n%s", name, code, errs)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%s: a file was written", name)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".ks-receipt-*")); len(left) != 0 {
			t.Errorf("%s: temp files left behind: %v", name, left)
		}
	}
}

func TestCruiseReceiptOlderResultAndUnfinishedJobSayPlainly(t *testing.T) {
	for name, c := range map[string]*signedReceiptCtl{
		"older result": {receipt: []byte(sealedLooking), none: true},
		"not accepted": {receipt: []byte(sealedLooking), notDone: true},
	} {
		syncFixture(t, &c.mu)
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		dir := t.TempDir()
		out := filepath.Join(dir, "r.json")
		_, errs, code := auditExec(t, bin, cfg, dir, fastEnv(cfg), "cruise", "receipt", "job_r", "--out", out)
		srv.Close()
		if code != exitFailed || !strings.Contains(errs, "no signed receipt file") || !strings.Contains(errs, "nothing was written") {
			t.Errorf("%s: exit %d\n%s", name, code, errs)
		}
		for _, r := range c.served() {
			if strings.HasSuffix(r, "/receipt") {
				t.Errorf("%s: asked for a receipt the status did not name", name)
			}
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%s: a file was written", name)
		}
	}
}

func sha256Of(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func reserialisedJSON(t *testing.T, s string) []byte {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return b
}

// ks cruise result points at the receipt file, and says who can check it, only
// when the service kept one.
func TestCruiseResultNamesTheReceiptFile(t *testing.T) {
	for name, c := range map[string]*signedReceiptCtl{
		"public": {receipt: []byte(sealedLooking), alg: "ed25519"},
		"hmac":   {receipt: []byte(sealedLooking), alg: ""},
		"none":   {receipt: []byte(sealedLooking), none: true},
	} {
		syncFixture(t, &c.mu)
		srv := httptest.NewServer(c)
		bin, cfg := buildAndAuth(t, srv)
		out, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "cruise", "result", "job_r")
		srv.Close()
		all := out + errs
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", name, code, all)
		}
		has := strings.Contains(all, "ks cruise receipt job_r")
		switch name {
		case "public":
			if !has || !strings.Contains(all, "publicly verifiable") {
				t.Errorf("%s:\n%s", name, all)
			}
		case "hmac":
			if !has || !strings.Contains(all, "service-verifiable only") {
				t.Errorf("%s:\n%s", name, all)
			}
		default:
			if has {
				t.Errorf("%s: names a receipt file the service did not keep:\n%s", name, all)
			}
		}
	}
}
