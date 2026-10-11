package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAppSnapshots is a design tool, not a check: with KS_SNAPSHOT_DRIVER (a
// pseudo-terminal driver that writes raw screens) and KS_SNAPSHOT_OUT set, it
// runs the app against the fake service through a person's session and keeps
// the raw terminal bytes at each named step, for rendering and review. It is
// skipped everywhere else.
func TestAppSnapshots(t *testing.T) {
	driver, outDir := os.Getenv("KS_SNAPSHOT_DRIVER"), os.Getenv("KS_SNAPSHOT_OUT")
	if driver == "" || outDir == "" {
		t.Skip("a design tool: set KS_SNAPSHOT_DRIVER and KS_SNAPSHOT_OUT")
	}
	c := newAgentCtl()
	c.addApproval("apr_push1", "git push origin main", 1, "hash-push-1")
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := map[string]string{"XDG_CONFIG_HOME": cfg, "KS_HTTP_TIMEOUT_MS": "2000", "TERM": "xterm-256color", "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
	spec := map[string]any{"argv": []string{bin}, "env": env, "timeout": 15, "rows": 34, "cols": 110, "steps": [][]string{
		{"Your agents:", "", "snap:1-home"},
		{"", "\r"},
		{"[y] allow once", "", "snap:2-window-approval"},
		{"", "fix the login test\r"},
		{"task tsk_new1", "y"},
		{"approved", "/help\r"},
		{"/take", "", "snap:3-help"},
		{"", "/agent list\r"},
		{"$ ks agent list --session", "", "snap:4-slash"},
		{"", "/agent --help\r"},
		{"ks agent list", "", "snap:5-group-help"},
		{"", "ks agent open\r"},
		{"is a ks command", "", "snap:6-ks-typed"},
		{"", "/agent st"},
		{"agent status", "", "snap:7-suggest"},
		{"", "\x03"},
		{"", "\x03"},
		{"", "\x03"},
		{"left", "q"},
	}}
	f := filepath.Join(t.TempDir(), "spec.json")
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("python3", driver, f, outDir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
