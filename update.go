// ks update: self-update with checksum verification (ADR-021). The new binary
// is downloaded from the public release, its SHA256 checked against the
// release's SHA256SUMS, and ONLY a verified binary replaces the current one —
// atomically, with the old binary restored on any failure. A mismatch is
// refused loudly. KS_UPDATE_BASE overrides the download base (the gate's
// tampered-mirror sabotage uses it); the verification path is identical.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releaseRepo = "keepstateai/cli"

func assetName() string {
	return "ks-" + runtime.GOOS + "-" + runtime.GOARCH
}

func updateBase() string {
	if b := os.Getenv("KS_UPDATE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://github.com/" + releaseRepo + "/releases/latest/download"
}

func latestReleaseTag() (string, error) {
	if b := os.Getenv("KS_UPDATE_BASE"); b != "" {
		// a mirror serves its tag in a plain file
		body, err := fetch(strings.TrimRight(b, "/") + "/TAG")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(body)), nil
	}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("releases api %s", resp.Status)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}

func fetch(url string) ([]byte, error) {
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// verifySHA256 finds this asset's line in SHA256SUMS and compares. Returns the
// expected sum for the message; an absent line is a refusal, not a pass.
func verifySHA256(sums []byte, asset string, bin []byte) error {
	got := sha256.Sum256(bin)
	gotHex := hex.EncodeToString(got[:])
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			if f[0] == gotHex {
				return nil
			}
			return fmt.Errorf("CHECKSUM MISMATCH for %s: expected %s, got %s — refusing this binary", asset, f[0], gotHex)
		}
	}
	return fmt.Errorf("no SHA256SUMS entry for %s — refusing an unverifiable binary", asset)
}

func runUpdate() error {
	latest, err := latestReleaseTag()
	if err != nil {
		return fmt.Errorf("cannot determine the latest release: %w", err)
	}
	if latest == version {
		fmt.Println("already up to date:", version)
		return nil
	}
	base := updateBase()
	asset := assetName()
	fmt.Printf("updating %s -> %s\n", version, latest)
	sums, err := fetch(base + "/SHA256SUMS")
	if err != nil {
		return fmt.Errorf("cannot fetch SHA256SUMS: %w", err)
	}
	bin, err := fetch(base + "/" + asset)
	if err != nil {
		return fmt.Errorf("cannot fetch %s: %w", asset, err)
	}
	if err := verifySHA256(sums, asset, bin); err != nil {
		return err // the refusal: nothing is replaced
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Println("updated to", latest, "(checksum verified)")
	return nil
}

func tokenPath() string { return filepath.Join(configDir(), "token.json") }
func configDir() string { return filepath.Join(configHome(), "keepstate") }
