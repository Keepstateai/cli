// identity.go: who the client is signed in as, and where (KS-005). The
// control-plane origin is validated before a token is ever sent to it:
// HTTPS, or plain HTTP only on the loopback for development. The stored
// credential names the account it belongs to, so later project bindings
// are keyed by account rather than by whatever token happens to be
// present. Logout removes every source the client reads and reports the
// server-side revocation as its own outcome.
package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// validateControlPlane accepts https anywhere and http only on the
// loopback. Anything else is refused with the reason, before any request.
func validateControlPlane(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a control plane URL (want https://host)", raw)
	}
	host := u.Hostname()
	loopback := host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback:
	case u.Scheme == "http":
		return "", fmt.Errorf("%s is plain HTTP on a remote host; a token sent there could be read on the way. Use https://, or a loopback address for development", raw)
	default:
		return "", fmt.Errorf("%q: scheme %q is not http or https", raw, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q carries credentials in the URL; remove them", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// validateReturnURL: the verification address the device flow prints must
// be on the control plane's own host (or loopback for development) and use
// its scheme, so a compromised or misconfigured endpoint cannot send the
// user to a look-alike page.
func validateReturnURL(ctl, verification string) error {
	c, err := url.Parse(ctl)
	if err != nil {
		return err
	}
	v, err := url.Parse(strings.TrimSpace(verification))
	if err != nil || v.Host == "" {
		return fmt.Errorf("the sign-in page address %q is not a URL", verification)
	}
	if v.Scheme != c.Scheme || !strings.EqualFold(v.Host, c.Host) {
		return fmt.Errorf("the sign-in page %q is not on the control plane %s; refusing to send you there", verification, ctl)
	}
	return nil
}

// legacyTokenPath is the endpoint file the bench and the gates write.
func legacyTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".keepstate", "hosted.json")
}

// identityLine renders the account the stored credential names, without a
// request: "account acct_… on https://ctl.keepstate.ai". A credential that
// predates the account field says so rather than guessing.
func identityLine(cr hostedCreds) string {
	if cr.AccountID == "" {
		return "account unrecorded (signed in before this client recorded it; ks login again to record it) on " + cr.CTL
	}
	return "account " + cr.AccountID + " on " + cr.CTL
}
