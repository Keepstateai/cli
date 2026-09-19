// ks doctor, client mode: the three questions a broken setup actually has.
// Connectivity to the control plane, token validity, version currency. A check
// that cannot run reports "unavailable", never a fake green (Law 1).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func runDoctor() int {
	fails := 0
	client := &http.Client{Timeout: 10 * time.Second}

	// 1. control plane reachable
	ctl := "https://ctl.keepstate.ai"
	cr, signedIn := hostedToken()
	if signedIn && cr.CTL != "" {
		ctl = cr.CTL
	}
	// 0. identity, from the stored credential, before any request: the
	// account a destructive command would act on is the first thing to know
	if signedIn {
		fmt.Printf("id    %s (credential: %s)\n", identityLine(cr), cr.Source)
	}
	if resp, err := client.Get(ctl + "/healthz"); err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		fmt.Printf("ok    control plane reachable (%s)\n", ctl)
	} else {
		fmt.Printf("FAIL  control plane unreachable (%s)\n", ctl)
		fails++
	}

	// 2. token validity
	if !signedIn {
		fmt.Println("--    not signed in (run: ks login)")
	} else {
		req, _ := http.NewRequest("GET", ctl+"/api/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+cr.Token)
		if resp, err := client.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var who struct {
					AccountID   string `json:"account_id"`
					CohortState string `json:"cohort_state"`
				}
				_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who)
				fmt.Printf("ok    token valid (account %s, cohort %s)\n", who.AccountID, who.CohortState)
			} else {
				fmt.Printf("FAIL  token refused (%s) — run: ks login\n", resp.Status)
				fails++
			}
		} else {
			fmt.Println("--    token check unavailable (control plane unreachable)")
		}
	}

	// 3. version currency (degrades to unavailable offline; never a fake green)
	latest, err := latestReleaseTag()
	switch {
	case err != nil:
		fmt.Println("--    version currency unavailable (cannot reach releases)")
	case version == "dev":
		fmt.Printf("--    running a dev build (latest release: %s)\n", latest)
	case latest == version:
		fmt.Printf("ok    up to date (%s)\n", version)
	default:
		fmt.Printf("note  update available: %s -> %s (run: ks update)\n", version, latest)
	}

	if fails > 0 {
		fmt.Fprintf(os.Stderr, "doctor: %d failure(s)\n", fails)
		return 1
	}
	return 0
}

// runLogout leaves the client signed out whatever it finds: the primary
// file, the legacy bench file, and any local bindings all go, and the
// server-side revocation is attempted and reported on its own. A
// revocation the network lost is said to have been lost, not assumed.
func runLogout() error {
	cr, signedIn := hostedToken()
	if signedIn {
		resp, raw, err := doBounded(cr, "POST", "/api/tokens/revoke", nil, nil)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "server-side revocation: not confirmed (%v); revoke the token from your account page\n", err)
		case resp.StatusCode == http.StatusOK:
			fmt.Println("server-side revocation: done")
		case resp.StatusCode == http.StatusUnauthorized:
			fmt.Println("server-side revocation: the token was already dead")
		case resp.StatusCode == http.StatusNotFound:
			fmt.Println("server-side revocation: this control plane has no self-revocation route; revoke the token from your account page")
		default:
			fmt.Fprintf(os.Stderr, "server-side revocation: not confirmed (%v); revoke the token from your account page\n", hostedError("POST", "/api/tokens/revoke", resp, raw))
		}
	}
	removed := 0
	for _, p := range []string{tokenPath(), legacyTokenPath(), filepath.Join(configDir(), "bindings.json")} {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", p, err)
		}
	}
	if _, still := hostedToken(); still {
		return fmt.Errorf("a credential source is still readable after logout; nothing was left on purpose")
	}
	fmt.Printf("Signed out locally (%d file(s) removed). The operations ledger stays; it holds no secrets.\n", removed)
	return nil
}

func runUninstall() {
	exe, _ := os.Executable()
	fmt.Printf(`To remove ks completely (one line):

    rm -f %s && rm -rf %s

That is the binary and the config (token included). Server-side, revoke the
device token from your account page. Nothing else is installed.
`, exe, configDir())
}
