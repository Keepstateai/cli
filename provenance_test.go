// KS-006: build provenance is verified by gh or it is not verified: a fake
// gh on PATH that passes, fails, is missing or answers garbage; and ks update
// replaces nothing when gh rejects the new binary (the old binary still runs).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGh writes a gh that behaves per mode and records its arguments.
func fakeGh(t *testing.T, mode string) (dir, argsFile string) {
	t.Helper()
	dir = t.TempDir()
	argsFile = filepath.Join(dir, "args")
	body := map[string]string{
		"pass":    `echo '[{"verificationResult":{"statement":{"predicateType":"https://slsa.dev/provenance/v1"}}}]'; exit 0`,
		"fail":    `echo 'Error: verifying with issuer "sigstore.dev": no matching attestations' >&2; exit 1`,
		"garbled": `echo 'all good, trust me'; exit 0`,
		"empty":   `echo '[]'; exit 0`,
	}[mode]
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, argsFile
}

func TestVersionVerifyNeverPassesByDefault(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ks")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	run := func(path string) (string, int) {
		cmd := exec.Command(bin, "version", "--verify")
		cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	dir, args := fakeGh(t, "pass")
	out, code := run(dir)
	if code != 0 || !strings.Contains(out, "provenance: verified") {
		t.Fatalf("pass: %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(args); !strings.Contains(string(a), "attestation verify") || !strings.Contains(string(a), "--repo Keepstateai/cli") {
		t.Fatalf("gh was called as %q", a)
	}
	for _, c := range []struct {
		mode string
		code int
		want string
	}{
		{"fail", exitIntegrity, "exited 1"},
		{"garbled", exitFailed, "not a list of verification results"},
		{"empty", exitFailed, "not a list of verification results"},
		{"missing", exitFailed, "gh command line tool is not installed"},
	} {
		path := t.TempDir() // no gh at all
		if c.mode != "missing" {
			path, _ = fakeGh(t, c.mode)
		}
		out, code := run(path)
		if code != c.code || !strings.Contains(out, "provenance: NOT verified") || !strings.Contains(out, c.want) {
			t.Errorf("%s: %d\n%s", c.mode, code, out)
		}
	}
}

func TestUpdateReplacesNothingWhenGhRejectsTheNewBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ks")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before, _ := os.ReadFile(bin)
	newBin := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(newBin)
	asset := "ks-" + runtime.GOOS + "-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/TAG":
			fmt.Fprint(w, "v9.9.9")
		case "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		case "/" + asset:
			w.Write(newBin)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	run := func(ghMode string, args ...string) (string, int) {
		path := t.TempDir()
		if ghMode != "missing" {
			path, _ = fakeGh(t, ghMode)
		}
		cmd := exec.Command(bin, append([]string{"update"}, args...)...)
		cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "KS_UPDATE_BASE=" + srv.URL}
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	// gh rejects: nothing replaced, the old binary is intact
	out, code := run("fail")
	if code != exitIntegrity || !strings.Contains(out, "nothing was replaced") {
		t.Fatalf("rejected: %d\n%s", code, out)
	}
	if now, _ := os.ReadFile(bin); string(now) != string(before) {
		t.Fatal("a rejected update replaced the binary")
	}
	if _, err := os.Stat(bin + ".new"); err == nil {
		t.Fatal("a rejected update left its temporary file")
	}
	// no gh and --require-provenance: nothing replaced either
	out, code = run("missing", "--require-provenance")
	if code != exitIntegrity || !strings.Contains(out, "NOT verified") {
		t.Fatalf("required: %d\n%s", code, out)
	}
	if now, _ := os.ReadFile(bin); string(now) != string(before) {
		t.Fatal("an unverified update replaced the binary under --require-provenance")
	}
	// no gh, not required: installed with the provenance plainly NOT verified
	out, code = run("missing")
	if code != 0 || !strings.Contains(out, "provenance NOT verified") {
		t.Fatalf("unrequired: %d\n%s", code, out)
	}
	if now, _ := os.ReadFile(bin); string(now) != string(newBin) {
		t.Fatal("the checksum-verified binary was not installed")
	}
}
