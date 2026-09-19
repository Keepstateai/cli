// ks doctor, client mode: the three questions a broken setup actually has.
// Connectivity to the control plane, token validity, version currency. A check
// that cannot run reports "unavailable", never a fake green (Law 1).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func runDoctor() int {
	fails := 0
	client := ordinaryClient()
	var checks []map[string]any
	note := func(status, name, detail string) {
		checks = append(checks, map[string]any{"check": name, "status": status, "detail": detail})
		if !out.json {
			fmt.Printf("%-5s %s\n", status, detail)
		}
	}
	defer func() {
		if out.json {
			emit(map[string]any{"checks": checks, "failures": fails}, nil)
		}
	}()

	// 1. control plane reachable
	ctl := "https://ctl.keepstate.ai"
	cr, signedIn := hostedToken()
	if signedIn && cr.CTL != "" {
		ctl = cr.CTL
	}
	// 0. identity, from the stored credential, before any request: the
	// account a destructive command would act on is the first thing to know
	if signedIn {
		note("id", "identity", identityLine(cr)+" (credential: "+cr.Source+")")
	}
	if resp, err := client.Get(ctl + "/healthz"); err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		note("ok", "control-plane", "control plane reachable ("+ctl+")")
	} else {
		note("FAIL", "control-plane", "control plane unreachable ("+ctl+")")
		fails++
	}

	// 2. token validity
	if !signedIn {
		note("--", "token", "not signed in (run: ks login)")
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
				note("ok", "token", "token valid (account "+who.AccountID+", cohort "+who.CohortState+")")
			} else {
				note("FAIL", "token", "token refused ("+resp.Status+"); run: ks login")
				fails++
			}
		} else {
			note("--", "token", "token check unavailable (control plane unreachable)")
		}
	}

	// 2b. what the control plane says it can do, and which of this client's
	// commands it therefore disables; a registry that cannot be read shows
	// its cached copy with the fetch time, never as current
	if signedIn {
		if set, err := fetchCapabilities(cr); err == nil {
			line, disabled := capabilitySummary(set)
			note("ok", "capabilities", "capabilities: "+line)
			for _, d := range disabled {
				note("note", "capabilities", d)
			}
		} else if errors.Is(err, errNoCapabilities) {
			note("--", "capabilities", "capability registry: not published by this control plane; commands that need one are disabled")
		} else if cached := cachedCapabilities(); cached != nil {
			line, _ := capabilitySummary(cached)
			note("--", "capabilities", "capability registry unreachable; cached copy fetched "+cached.CachedAt+": "+line)
		} else {
			note("--", "capabilities", "capability registry unreachable and no cached copy")
		}
	}

	// 3. version currency (degrades to unavailable offline; never a fake green)
	latest, err := latestReleaseTag()
	switch {
	case err != nil:
		note("--", "version", "version currency unavailable (cannot reach releases)")
	case version == "dev":
		note("--", "version", "running a dev build (latest release: "+latest+")")
	case latest == version:
		note("ok", "version", "up to date ("+version+")")
	default:
		note("note", "version", "update available: "+version+" -> "+latest+" (run: ks update)")
	}

	if fails > 0 {
		progress("doctor: %d failure(s)", fails)
		return 1
	}
	return 0
}

// runLogout leaves the client signed out whatever it finds: the primary
// file, the legacy bench file, and any local bindings all go, and the
// server-side revocation is attempted and reported on its own. A
// revocation the network lost is said to have been lost, not assumed.
func runLogout() error {
	// one structured result: the server-side outcome in its own field, the
	// local removals and their failures in theirs; --json emits exactly this
	// document and nothing else on stdout (review R02, 2026-09-20)
	revocation, detail := "not_attempted", "no stored credential to revoke"
	cr, signedIn := hostedToken()
	if signedIn {
		resp, raw, err := doBounded(cr, "POST", "/api/tokens/revoke", nil, nil)
		switch {
		case err != nil:
			revocation, detail = "unconfirmed", "the control plane did not answer: "+sanitize(err.Error())+"; revoke the token from your account page"
		case resp.StatusCode == http.StatusOK:
			revocation, detail = "confirmed", "the control plane revoked the token"
		case resp.StatusCode == http.StatusUnauthorized:
			revocation, detail = "already_invalid", "the token was already dead on the control plane"
		case resp.StatusCode == http.StatusNotFound:
			revocation, detail = "unsupported", "this control plane has no self-revocation route; revoke the token from your account page"
		default:
			revocation, detail = "failed", sanitize(hostedError("POST", "/api/tokens/revoke", resp, raw).Error())+"; revoke the token from your account page"
		}
	}
	// every source goes, whatever the network did; a source that will not go
	// is reported, and the others still go
	removed := []string{}
	failures := []map[string]string{}
	for _, p := range []string{tokenPath(), legacyTokenPath(), filepath.Join(configDir(), "bindings.json")} {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if !os.IsNotExist(err) {
			failures = append(failures, map[string]string{"path": p, "error": sanitize(err.Error())})
		}
	}
	stillReadable := ""
	if again, still := hostedTokenQuiet(); still {
		stillReadable = again.Source
	}
	signedOut := len(failures) == 0 && stillReadable == ""
	result := map[string]any{
		"signed_out": signedOut, "files_removed": len(removed), "removed": removed, "removal_failures": failures,
		"still_readable": stillReadable, "revocation": revocation, "revocation_detail": detail,
	}
	if !signedOut {
		msg := "a credential source could not be removed"
		if stillReadable != "" {
			msg = "a credential source is still readable after logout: " + stillReadable
		}
		fail(&cliError{Code: exitFailed, Kind: "logout_incomplete", Message: msg + " (server-side revocation: " + revocation + ")", WorkStarted: workNo,
			NextAction: "remove it by hand, then ks doctor", OperationID: ""})
	}
	emit(result, func() {
		fmt.Printf("server-side revocation: %s (%s)\n", revocation, detail)
		fmt.Printf("Signed out locally (%d file(s) removed). The operations ledger stays; it holds no secrets.\n", len(removed))
	})
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
