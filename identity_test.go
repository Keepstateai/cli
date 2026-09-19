// identity_test: KS-005's guards. A signed-out client stays signed out
// whatever credential files exist; an unsafe control plane, a look-alike
// sign-in page and an unreadable credential each fail clearly; and the
// stored credential names its account.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlPlaneURLValidation(t *testing.T) {
	good := map[string]string{"https://ctl.keepstate.ai": "https://ctl.keepstate.ai", "https://ctl.keepstate.ai/": "https://ctl.keepstate.ai",
		"http://127.0.0.1:8471": "http://127.0.0.1:8471", "http://localhost:8471": "http://localhost:8471", "http://[::1]:8471": "http://[::1]:8471"}
	for in, want := range good {
		if got, err := validateControlPlane(in); err != nil || got != want {
			t.Errorf("validateControlPlane(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://ctl.keepstate.ai", "http://10.0.0.5:8471", "ftp://x", "ctl.keepstate.ai", "", "https://user:pw@ctl.keepstate.ai"} {
		if _, err := validateControlPlane(bad); err == nil {
			t.Errorf("validateControlPlane(%q) accepted", bad)
		}
	}
	if err := validateReturnURL("https://ctl.keepstate.ai", "https://ctl.keepstate.ai/device"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"https://ctl.keepstate.ai.evil.example/device", "http://ctl.keepstate.ai/device", "https://keepstate.ai/device", "not a url"} {
		if err := validateReturnURL("https://ctl.keepstate.ai", bad); err == nil {
			t.Errorf("validateReturnURL accepted %q", bad)
		}
	}
}

// QA-005-1 and VER-005-1: a legacy credential exists after the primary is
// deleted; logout still leaves the client signed out, on disk and in
// process output.
func TestLogoutRemovesEveryCredentialSource(t *testing.T) {
	rec := &auditRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	home := t.TempDir()
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + cfg}
	// a legacy bench file beside the primary
	if err := os.MkdirAll(filepath.Join(home, ".keepstate"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]string{"token": "legacy-token", "ctl": srv.URL})
	if err := os.WriteFile(filepath.Join(home, ".keepstate", "hosted.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg, "keepstate", "token.json")); err != nil {
		t.Fatal(err)
	}
	// still signed in through the legacy source
	out, _, code := auditExec(t, bin, cfg, t.TempDir(), env, "doctor")
	if code != 0 || !strings.Contains(out, "hosted.json") {
		t.Fatalf("doctor before logout:\n%s", out)
	}
	out, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "logout")
	if code != 0 || !strings.Contains(out, "Signed out locally") {
		t.Fatalf("logout: exit %d\n%s%s", code, out, errs)
	}
	for _, p := range []string{filepath.Join(cfg, "keepstate", "token.json"), filepath.Join(home, ".keepstate", "hosted.json")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after logout", p)
		}
	}
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), env, "doctor")
	if !strings.Contains(out, "not signed in") || strings.Contains(out, "legacy-token") {
		t.Errorf("doctor after logout:\n%s", out)
	}
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), env, "run")
	if code != exitAuth || !strings.Contains(errs, "Not signed in") {
		t.Errorf("run after logout: exit %d (want %d, sign-in) %s", code, exitAuth, errs)
	}
	// the revocation was attempted against the recorder before the files went
	seen := false
	for _, h := range rec.drain() {
		if h.Method == "POST" && h.Path == "/api/tokens/revoke" {
			seen = true
		}
	}
	if !seen {
		t.Error("logout did not attempt the server-side revocation")
	}
}

// QA-005-3: a remote HTTP endpoint, a look-alike return URL and an
// unreadable credential file each fail clearly, before any token is sent.
func TestUnsafeEndpointsAndUnreadableCredentialFailClearly(t *testing.T) {
	rec := &auditRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	// a stored credential pointing at plain HTTP on a remote host
	tok, _ := json.Marshal(map[string]string{"token": "t", "ctl": "http://10.1.2.3:8471"})
	p := filepath.Join(cfg, "keepstate", "token.json")
	if err := os.WriteFile(p, tok, 0o600); err != nil {
		t.Fatal(err)
	}
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "meter", "s1")
	if code != 2 || !strings.Contains(errs, "plain HTTP on a remote host") {
		t.Errorf("remote http: exit %d %s", code, errs)
	}
	if len(rec.drain()) != 0 {
		t.Error("a request was made with an unsafe control plane")
	}
	// login --ctl over plain http to a remote host is refused before any request
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "login", "--ctl", "http://ctl.example.org")
	if code == 0 || !strings.Contains(errs, "plain HTTP") || len(rec.drain()) != 0 {
		t.Errorf("login over http: exit %d %s", code, errs)
	}
	// a device flow whose sign-in page is on another host is refused
	lookalike := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/device/code" {
			w.Write([]byte(`{"device_code":"d","user_code":"ABCD","verification_uri":"http://127.0.0.1:1/evil","expires_in":5,"interval":1}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer lookalike.Close()
	os.Remove(p)
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "login", "--ctl", lookalike.URL)
	if code == 0 || !strings.Contains(errs, "not on the control plane") {
		t.Errorf("look-alike return URL: exit %d %s", code, errs)
	}
	// an unreadable credential file names itself instead of falling through
	if err := os.WriteFile(p, tok, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	if os.Getuid() != 0 {
		_, errs, code = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "meter", "s1")
		if code != 2 || !strings.Contains(errs, "cannot be read") {
			t.Errorf("unreadable credential: exit %d %s", code, errs)
		}
	}
}

// The stored credential names its account, and doctor prints it before any
// request; a credential from before the field says so.
func TestIdentityIsRecordedAndShown(t *testing.T) {
	rec := &auditRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	out, _, _ := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "doctor")
	if !strings.Contains(out, "account unrecorded") {
		t.Errorf("an old credential should say the account is unrecorded:\n%s", out)
	}
	tok, _ := json.Marshal(map[string]string{"token": "t", "ctl": srv.URL, "account_id": "acct_example"})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "token.json"), tok, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, _ = auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir()}, "doctor")
	if !strings.Contains(out, "id    account acct_example on "+srv.URL) {
		t.Errorf("doctor does not lead with the identity:\n%s", out)
	}
}
