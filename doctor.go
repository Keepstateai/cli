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

func runLogout() error {
	p := tokenPath()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Signed out. The server-side token can also be revoked from your account page.")
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
