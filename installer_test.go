// installer_test: KS-006's QA-006-1 against the real install.sh and a
// loopback release mirror. HTML where a binary should be, a truncated
// binary and a wrong checksum each fail with nothing installed; the true
// bytes install.
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

func TestInstallerRefusesBadDownloads(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	asset := "ks-" + runtime.GOOS + "-" + runtime.GOARCH
	good := []byte("#!/bin/sh\necho ks-fake\n")
	sum := sha256.Sum256(good)
	cases := []struct {
		name     string
		body     []byte
		sums     string
		wantFail string
	}{
		{"html page", []byte("<!DOCTYPE html><title>oops</title>"), hex.EncodeToString(sum[:]) + "  " + asset + "\n", "checksum mismatch"},
		{"truncated", good[:10], hex.EncodeToString(sum[:]) + "  " + asset + "\n", "checksum mismatch"},
		{"wrong checksum", good, strings.Repeat("0", 64) + "  " + asset + "\n", "checksum mismatch"},
		{"no entry", good, strings.Repeat("0", 64) + "  ks-other\n", "no SHA256SUMS entry"},
		{"true bytes", good, hex.EncodeToString(sum[:]) + "  " + asset + "\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
					fmt.Fprint(w, c.sums)
				case strings.HasSuffix(r.URL.Path, "/"+asset):
					w.Write(c.body)
				default:
					w.WriteHeader(404)
				}
			}))
			defer mirror.Close()
			dest := t.TempDir()
			cmd := exec.Command("sh", "install.sh")
			cmd.Env = append(os.Environ(), "KS_INSTALL_BASE="+mirror.URL, "KS_INSTALL_DIR="+dest, "PATH="+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			installed := false
			if _, serr := os.Stat(filepath.Join(dest, "ks")); serr == nil {
				installed = true
			}
			if c.wantFail == "" {
				if err != nil || !installed {
					t.Fatalf("true bytes did not install: %v\n%s", err, out)
				}
				return
			}
			if err == nil || installed || !strings.Contains(string(out), c.wantFail) {
				t.Fatalf("%s: err=%v installed=%v\n%s", c.name, err, installed, out)
			}
			if !strings.Contains(string(out), "Nothing was installed") && !strings.Contains(string(out), "not installing") {
				t.Errorf("%s: the refusal does not say nothing was installed:\n%s", c.name, out)
			}
		})
	}
}
