// review_r02_test: the permanent guards for review finding R02
// (2026-09-20). ks logout --json emits exactly one document on stdout,
// whatever the server-side revocation did, and every branch of that
// outcome is a field, never a printed line.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func revokeServer(t *testing.T, status int, plain bool) (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tokens/revoke" {
			atomic.AddInt32(&hits, 1)
			if plain {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(status)
				w.Write([]byte("404 page not found"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(`{"revoked":true}`))
			return
		}
		w.WriteHeader(404)
	}))
	return srv, &hits
}

func TestLogoutJSONIsOneDocumentOnEveryBranch(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		plain      bool
		closed     bool
		signedOut  bool
		revocation string
		hits       int32
	}{
		{"confirmed", 200, false, false, false, "confirmed", 1},
		{"already invalid", 401, false, false, false, "already_invalid", 1},
		{"route missing", 404, true, false, false, "unsupported", 1},
		{"network loss", 0, false, true, false, "unconfirmed", 0},
		{"not signed in", 200, false, false, true, "not_attempted", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, hits := revokeServer(t, c.status, c.plain)
			bin, cfg := buildAndAuth(t, srv)
			home := t.TempDir()
			env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + cfg, "KS_HTTP_TIMEOUT_MS=400"}
			if c.signedOut {
				os.Remove(filepath.Join(cfg, "keepstate", "token.json"))
			}
			if c.closed {
				srv.Close()
			} else {
				defer srv.Close()
			}
			stdout, stderr, code := auditExec(t, bin, cfg, t.TempDir(), env, "logout", "--json")
			if code != 0 {
				t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
			}
			env2 := parseEnvelope(t, stdout)
			data := env2["data"].(map[string]any)
			if data["signed_out"] != true || data["revocation"] != c.revocation {
				t.Errorf("data: %v", data)
			}
			if got := atomic.LoadInt32(hits); got != c.hits {
				t.Errorf("revocation requests %d, want %d", got, c.hits)
			}
			if _, still := os.Stat(filepath.Join(cfg, "keepstate", "token.json")); still == nil {
				t.Error("token.json still exists")
			}
		})
	}
}

// Failed local removal: the document is still one document, signed_out is
// false, the failure is named, and the exit code is non-zero.
func TestLogoutReportsAFailedRemovalHonestly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove anything")
	}
	srv, _ := revokeServer(t, 200, false)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := filepath.Join(cfg, "keepstate")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	stdout, _, code := auditExec(t, bin, cfg, t.TempDir(), []string{"HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + cfg}, "logout", "--json")
	if code == 0 {
		t.Fatalf("a failed removal exited 0:\n%s", stdout)
	}
	env := parseEnvelope(t, stdout)
	e, ok := env["error"].(map[string]any)
	if !ok || !strings.Contains(e["message"].(string), "still readable") && !strings.Contains(e["message"].(string), "could not be removed") {
		t.Errorf("error: %v", env)
	}
	var probe map[string]any
	_ = json.Unmarshal([]byte(stdout), &probe)
	if probe["error"] == nil {
		t.Error("no error document")
	}
}
