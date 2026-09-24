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

// QA-087-3 and QA-006-3, executed rather than implemented-and-unmeasured.
//
// QA-087-3 failed when it was first run: the architecture branch said only
// `unsupported architecture: <arch>` and named nothing that IS supported,
// while the OS branch has always carried a truthful support statement. The
// asymmetry is fixed; this holds both statements to what release.yml
// actually builds, so a new platform target cannot silently make the
// installer's promise false.
func TestInstallerRefusesUnsupportedPlatformsTruthfully(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("release.yml is the source of truth for what is supported: %v", err)
	}
	for _, target := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		if !strings.Contains(string(wf), target) {
			t.Fatalf("release.yml no longer builds ks-%s: the installer's support statement must be updated with it", target)
		}
	}
	if strings.Contains(string(wf), "ks-windows-") {
		t.Errorf("release.yml builds a Windows asset, so `Windows is on the roadmap` is no longer truthful")
	}

	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "uname"), []byte(
		"#!/bin/sh\ncase \"$1\" in\n  -m) echo \"${FAKE_ARCH:-arm64}\" ;;\n  -s) echo \"${FAKE_OS:-Linux}\" ;;\n  *) echo \"${FAKE_OS:-Linux}\" ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, arch, os_, want string }{
		{"unsupported architecture", "riscv128", "Linux", "unsupported architecture: riscv128 (arm64 and amd64 today)"},
		{"unsupported OS", "arm64", "Windows_NT", "macOS and Linux today; Windows is on the roadmap"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dest := t.TempDir()
			cmd := exec.Command("sh", "install.sh")
			// KS_VERSION is pinned so the refusal is reached without any
			// network call at all: it precedes the asset fetch.
			cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_ARCH="+c.arch, "FAKE_OS="+c.os_, "KS_VERSION=v0.1.8", "KS_INSTALL_DIR="+dest)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("an unsupported platform was not refused:\n%s", out)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("the refusal does not name what is supported.\nwant substring: %s\ngot:\n%s", c.want, out)
			}
			if strings.Contains(string(out), "Downloading") {
				t.Errorf("the refusal must precede the download:\n%s", out)
			}
			if _, serr := os.Stat(filepath.Join(dest, "ks")); serr == nil {
				t.Errorf("something was installed despite the refusal")
			}
		})
	}
}

// QA-006-3's other half: an install directory whose path contains spaces.
func TestInstallerHandlesSpacesInTheInstallPath(t *testing.T) {
	asset := "ks-" + runtime.GOOS + "-" + runtime.GOARCH
	body := []byte("#!/bin/sh\necho synthetic\n")
	sum := sha256.Sum256(body)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%x  %s\n", sum, asset)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(body)
		default:
			w.WriteHeader(404)
		}
	}))
	defer mirror.Close()
	dest := filepath.Join(t.TempDir(), "a dir with spaces", "bin")
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(), "KS_INSTALL_BASE="+mirror.URL, "KS_INSTALL_DIR="+dest, "KS_VERSION=v0.1.8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a path with spaces broke the installer: %v\n%s", err, out)
	}
	if _, serr := os.Stat(filepath.Join(dest, "ks")); serr != nil {
		t.Fatalf("nothing was installed at a path containing spaces: %v\n%s", serr, out)
	}
}
