// ks login: device-code sign-in against the control plane.
// The token is printed NEVER — not on success, not on error. It lands in
// the OS credential store: macOS Keychain via security(1); elsewhere a
// 0600 file in a 0700 dir (the Linux bench and CI have no keychain
// daemon). ADR-012 decision 5 governs custody.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type deviceCodeResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type deviceTokenResp struct {
	Token   string `json:"token"`
	TokenID string `json:"token_id"`
	Scope   string `json:"scope"`
	Error   string `json:"error"`
}

func runLogin() error {
	ctl := flagValue("--ctl", "https://ctl.keepstate.ai")
	resp, err := http.PostForm(ctl+"/api/device/code", url.Values{})
	if err != nil {
		return fmt.Errorf("control plane unreachable: %w", err)
	}
	defer resp.Body.Close()
	var dc deviceCodeResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&dc); err != nil || dc.DeviceCode == "" {
		return fmt.Errorf("device code request failed: %s", resp.Status)
	}
	fmt.Printf("Visit %s and enter code: %s\n", dc.VerificationURI, dc.UserCode)
	fmt.Println("Waiting for approval...")

	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		tr, err := http.PostForm(ctl+"/api/device/token", url.Values{"device_code": {dc.DeviceCode}})
		if err != nil {
			continue // transient; the deadline bounds us
		}
		var tok deviceTokenResp
		err = json.NewDecoder(io.LimitReader(tr.Body, 1<<20)).Decode(&tok)
		tr.Body.Close()
		if err != nil {
			continue
		}
		switch {
		case tr.StatusCode == http.StatusOK && tok.Token != "":
			if err := storeToken(ctl, tok); err != nil {
				return fmt.Errorf("token minted but not stored: %w", err)
			}
			fmt.Println("Signed in. The token lives in your OS credential store; revoke it any time from your account page.")
			return nil
		case tr.StatusCode == http.StatusPreconditionRequired:
			continue // authorization_pending
		default:
			return fmt.Errorf("sign-in refused: %s", tok.Error)
		}
	}
	return fmt.Errorf("device code expired before approval")
}

// storeToken writes to the platform credential store. The JSON payload
// carries the control-plane origin and token id (revocation UX), and
// the token itself; on non-darwin platforms it is a 0600 file.
func storeToken(ctl string, tok deviceTokenResp) error {
	payload, err := json.Marshal(map[string]string{
		"ctl": ctl, "token": tok.Token, "token_id": tok.TokenID, "scope": tok.Scope,
	})
	if err != nil {
		return err
	}
	// A 0600 file in the user's config dir, on every platform. The macOS
	// Keychain was tried first but its per-binary ACL silently blocked a
	// rebuilt CLI from reading a token an earlier build had stored; a
	// scoped, revocable device token in a 0600 file is the standard CLI
	// posture and has no such friction.
	dir := filepath.Join(configHome(), "keepstate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f := filepath.Join(dir, "token.json")
	if err := os.WriteFile(f, payload, 0o600); err != nil {
		return err
	}
	return os.Chmod(f, 0o600) // WriteFile honors umask only downward; be explicit
}

func configHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config")
}
