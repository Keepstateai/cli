// ks cruise verify: the receipt check runs offline against the published key
// list, and must agree byte for byte with the service's own verifier. The
// signed bytes are what Python's json.dumps(sort_keys=True, separators=(",",
// ":")) writes for the receipt without hmac_sha256 and sig, escapes and
// number spelling included, so these tests hold this client's canonical form
// to the shared vectors (testdata/receipt-verify, vendored from the service
// with its source recorded in SOURCE.json), and a drift guard compares the
// vendored copy with the service's when KS_SERVER_CHECKOUT is given (the
// service's client-drift job gives it).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type receiptVectors struct {
	Name     string                     `json:"name"`
	Version  int                        `json:"version"`
	KeyDocs  map[string]json.RawMessage `json:"key_docs"`
	Cases    []receiptVectorCase        `json:"cases"`
	Canon    []receiptCanonCase         `json:"canonical_cases"`
	FilePath string                     `json:"-"`
}

type receiptVectorCase struct {
	Name        string   `json:"name"`
	Receipt     string   `json:"receipt"`
	SignedBytes string   `json:"signed_bytes"`
	KeyDoc      string   `json:"key_doc"`
	Flags       []string `json:"flags"`
	Exit        int      `json:"exit"`
	Words       string   `json:"words"`
}

type receiptCanonCase struct {
	Name, Input, Canonical string
}

func loadReceiptVectors(t *testing.T) receiptVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/receipt-verify/receipt-verify-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v receiptVectors
	if err := json.Unmarshal(b, &v); err != nil || v.Name != "ks-receipt-verify" || v.Version != 1 || len(v.Cases) < 35 {
		t.Fatalf("vectors: %v %q %d %d", err, v.Name, v.Version, len(v.Cases))
	}
	return v
}

func keyListOf(t *testing.T, raw json.RawMessage) []any {
	t.Helper()
	keys, err := parseKeyList(raw)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestReceiptVerifySignedBytesAreThePythonCanonicalForm(t *testing.T) {
	v := loadReceiptVectors(t)
	for _, c := range v.Cases {
		rec, err := parsePyJSON([]byte(c.Receipt))
		if err != nil {
			t.Errorf("%s: refused: %v", c.Name, err)
			continue
		}
		obj, _ := rec.(map[string]any)
		got, err := receiptSignedBytes(obj)
		if err != nil || string(got) != c.SignedBytes {
			t.Errorf("%s: signed bytes\n got  %s\n want %s (%v)", c.Name, got, c.SignedBytes, err)
		}
	}
	for _, c := range v.Canon {
		val, err := parsePyJSON([]byte(c.Input))
		if err != nil {
			t.Errorf("%s: refused: %v", c.Name, err)
			continue
		}
		got, err := pyCanonical(val)
		if err != nil || string(got) != c.Canonical {
			t.Errorf("%s: canonical\n got  %s\n want %s (%v)", c.Name, got, c.Canonical, err)
		}
	}
}

func TestReceiptVerifyAnswersLikeTheServiceVerifier(t *testing.T) {
	v := loadReceiptVectors(t)
	seen := map[int]int{}
	for _, c := range v.Cases {
		trust := false
		for _, f := range c.Flags {
			trust = trust || f == "--trust-before-revocation"
		}
		doc, ok := v.KeyDocs[c.KeyDoc]
		if !ok {
			t.Fatalf("%s: no key doc %q", c.Name, c.KeyDoc)
		}
		rec, err := parsePyJSON([]byte(c.Receipt))
		if err != nil {
			t.Errorf("%s: refused: %v", c.Name, err)
			continue
		}
		code, words := checkReceipt(rec.(map[string]any), keyListOf(t, doc), trust)
		seen[code]++
		if code != c.Exit || words != c.Words {
			t.Errorf("%s: got exit %d %q\n want exit %d %q", c.Name, code, words, c.Exit, c.Words)
		}
	}
	if seen[0] == 0 || seen[1] == 0 || seen[3] == 0 {
		t.Errorf("the vectors must exercise exit 0, 1 and 3: %v", seen)
	}
}

func TestReceiptVerifyRefusesWhatPythonCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"bom":            "\xef\xbb\xbf{}",
		"trailing comma": `{"a":1,}`,
		"single quotes":  `{'a':1}`,
		"leading zero":   `{"a":01}`,
		"raw control":    "{\"a\":\"x\ty\"}",
		"bad escape":     `{"a":"\q"}`,
		"short unicode":  `{"a":"\u12"}`,
		"trailing text":  `{} x`,
		"empty":          ``,
		"bare plus":      `{"a":+1}`,
	} {
		if _, err := parsePyJSON([]byte(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
	if _, err := parsePyJSON([]byte("{\"a\":\"\xff\"}")); err == nil {
		t.Errorf("invalid UTF-8 accepted")
	}
}

// ---- the verb itself, run as the binary, signed out ----

func receiptVerbCase(t *testing.T, name string) (receiptVectorCase, receiptVectors) {
	t.Helper()
	v := loadReceiptVectors(t)
	for _, c := range v.Cases {
		if c.Name == name {
			return c, v
		}
	}
	t.Fatalf("no vector %q", name)
	return receiptVectorCase{}, v
}

func TestReceiptVerifyVerbOutcomesAndExitCodes(t *testing.T) {
	_, bin, _ := startFake(t) // a built binary; the verb needs no sign-in
	cfg := t.TempDir()        // signed out: no token at all
	dir := t.TempDir()
	v := loadReceiptVectors(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/.well-known/keepstate-receipt-keys.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(v.KeyDocs["main"])
	}))
	defer srv.Close()
	keysFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keysFile, v.KeyDocs["main"], 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(name string) string {
		c, _ := receiptVerbCase(t, name)
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(c.Receipt), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(args ...string) (string, string, int) { return ksIn(t, bin, cfg, dir, args...) }

	// verified, offline from a file: exit 0, the words on stdout, no request
	hits.Store(0)
	out, errs, code := run("cruise", "verify", write("emoji-surrogate-pair-raw"), "--keys-file", keysFile)
	if code != 0 || !strings.Contains(out, "verified: Ed25519 under key ") || errs != "" {
		t.Errorf("verified: exit %d\nstdout %q\nstderr %q", code, out, errs)
	}
	if hits.Load() != 0 {
		t.Errorf("--keys-file made %d request(s)", hits.Load())
	}
	// from a URL: the same answer, one request, the receipt never sent
	out, _, code = run("cruise", "verify", write("html-chars"), "--keys-url", srv.URL+"/.well-known/keepstate-receipt-keys.json")
	if code != 0 || !strings.Contains(out, "verified") || hits.Load() != 1 {
		t.Errorf("--keys-url: exit %d %q hits %d", code, out, hits.Load())
	}
	// altered: exit 1, words on stderr, stdout empty
	out, errs, code = run("cruise", "verify", write("tampered-value"), "--keys-file", keysFile)
	if code != 1 || out != "" || !strings.Contains(errs, "the receipt was altered or is signed under another key") {
		t.Errorf("tampered: exit %d stdout %q stderr %q", code, out, errs)
	}
	// HMAC-only: exit 3, "service-verifiable only"
	_, errs, code = run("cruise", "verify", write("hmac-only-no-signature"), "--keys-file", keysFile)
	if code != 3 || !strings.Contains(errs, "service-verifiable only") {
		t.Errorf("hmac-only: exit %d %q", code, errs)
	}
	// a key outside its window, revoked, and the opt-in
	_, errs, code = run("cruise", "verify", write("outside-window-before-valid-from"), "--keys-file", keysFile)
	if code != 1 || !strings.Contains(errs, "older than its key's valid_from") {
		t.Errorf("window: exit %d %q", code, errs)
	}
	_, errs, code = run("cruise", "verify", write("revoked-key-refused-by-default"), "--keys-file", keysFile)
	if code != 1 || !strings.Contains(errs, "revoked at 2023-11-15T00:00:00Z") {
		t.Errorf("revoked: exit %d %q", code, errs)
	}
	out, _, code = run("cruise", "verify", write("revoked-key-trusted-before-revocation"), "--keys-file", keysFile, "--trust-before-revocation")
	if code != 0 || !strings.Contains(out, "verified") {
		t.Errorf("trust before revocation: exit %d %q", code, out)
	}
	// unlisted key
	_, errs, code = run("cruise", "verify", write("unlisted-key"), "--keys-file", keysFile)
	if code != 1 || !strings.Contains(errs, "is not in the key list") {
		t.Errorf("unlisted: exit %d %q", code, errs)
	}
	// --json: one envelope on stdout, the verdict in data
	out, _, code = run("cruise", "verify", write("ascii-basic"), "--keys-file", keysFile, "--json")
	var env struct {
		Data struct {
			Verified bool   `json:"verified"`
			Exit     int    `json:"exit"`
			Message  string `json:"message"`
			KeyID    string `json:"key_id"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &env) != nil || !env.Data.Verified || env.Data.Exit != 0 || len(env.Data.KeyID) != 64 {
		t.Errorf("--json: exit %d %q", code, out)
	}
	// usage and unreadable inputs: exit 2, nothing claimed
	for name, args := range map[string][]string{
		"no receipt":      {"cruise", "verify"},
		"two key sources": {"cruise", "verify", write("ascii-basic"), "--keys-file", keysFile, "--keys-url", srv.URL},
		"missing file":    {"cruise", "verify", filepath.Join(dir, "nope.json"), "--keys-file", keysFile},
		"missing keys":    {"cruise", "verify", write("ascii-basic"), "--keys-file", filepath.Join(dir, "nope.json")},
		"not an object":   {"cruise", "verify", writeRaw(t, dir, "list.json", "[1]"), "--keys-file", keysFile},
		"keys not json":   {"cruise", "verify", write("ascii-basic"), "--keys-file", writeRaw(t, dir, "bad.json", "{")},
		"url unreachable": {"cruise", "verify", write("ascii-basic"), "--keys-url", "http://127.0.0.1:1/keys.json"},
	} {
		out, errs, code := run(args...)
		if code != 2 || out != "" || strings.Contains(out+errs, "verified:") {
			t.Errorf("%s: exit %d stdout %q stderr %q", name, code, out, errs)
		}
	}
	// a public key given directly
	c, _ := receiptVerbCase(t, "ascii-basic")
	var doc struct {
		Keys []struct {
			KeyID     string `json:"key_id"`
			PublicKey string `json:"public_key"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(v.KeyDocs["main"], &doc)
	out, _, code = run("cruise", "verify", write("ascii-basic"), "--public-key", doc.Keys[0].PublicKey)
	if code != 0 || !strings.Contains(out, "verified") {
		t.Errorf("--public-key: exit %d %q (%s)", code, out, c.Name)
	}
}

func writeRaw(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReceiptVerifyHelpMakesNoRequestAndNamesTheDefaultKeys(t *testing.T) {
	f, bin, cfg := startFake(t)
	f.mu.Lock()
	f.hits = nil
	f.mu.Unlock()
	out, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "verify", "--help")
	if code != 0 || !strings.Contains(out, "https://ctl.keepstate.ai/.well-known/keepstate-receipt-keys.json") ||
		!strings.Contains(out, "--keys-file") || !strings.Contains(out, "service-verifiable only") {
		t.Errorf("help: exit %d\n%s", code, out)
	}
	f.mu.Lock()
	n := len(f.hits)
	f.mu.Unlock()
	if n != 0 {
		t.Errorf("help made %d request(s)", n)
	}
	for _, bad := range []string{"@", "\xe2\x80\x94", "github.com"} {
		if strings.Contains(out, bad) {
			t.Errorf("help contains %q", bad)
		}
	}
}

// ---- the vendored copy ----

func TestReceiptVerifyVendoredVectorsMatchTheirSourceAndTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		From         string `json:"from"`
		SHA256       string `json:"sha256"`
		Vendored     string `json:"vendored_sha256"`
	}
	b, err := os.ReadFile("testdata/receipt-verify/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" || src.From == "" {
		t.Fatalf("SOURCE.json: %v", err)
	}
	if got := fileSHA(t, "testdata/receipt-verify/receipt-verify-v1.json"); got != src.Vendored {
		t.Errorf("testdata/receipt-verify/receipt-verify-v1.json is %s, SOURCE.json records %s", got, src.Vendored)
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in CI")
	}
	raw, err := os.ReadFile(filepath.Join(server, src.From))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(publicScrub(raw))
	if got := hex.EncodeToString(sum[:]); got != src.Vendored {
		t.Fatalf("DRIFT: the service's %s, scrubbed for the public repository, is now %s, but this client vendored %s from commit %s; re-vendor it (cp the file, update SOURCE.json) and re-run the vectors",
			src.From, got, src.Vendored, src.ServerCommit)
	}
	if !bytes.Equal(publicScrub(raw), mustRead(t, "testdata/receipt-verify/receipt-verify-v1.json")) {
		t.Fatalf("DRIFT: the vendored vectors differ from the service's")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
