// The organization logo verbs against a fake control plane: what each sends
// (method, path, query and the file's own bytes), what each prints, the
// refusals made before anything is sent, and the service's words printed as
// they come.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type orgLogoCtl struct {
	mu   sync.Mutex
	sent []string
	body []byte
	has  bool
}

func (c *orgLogoCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.RequestURI())
	c.body = b
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refuse := func(code int, kind, words string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "error": map[string]any{"code": kind, "type": kind, "message": words, "retryable": false, "work_started": "no", "next_action": ""}})
	}
	switch {
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_a/logo":
		if bytes.HasPrefix(b, []byte("<svg")) {
			refuse(415, "ks_logo_type", "a logo is a PNG, JPEG or WebP picture; nothing was changed")
			return
		}
		tile := r.URL.Query().Get("tile")
		if tile == "" {
			tile = "light"
		}
		c.has = true
		env(200, map[string]any{"url": "/console/organization/logo/org_a/0123456789ab.png", "sha256": strings.Repeat("ab", 32), "tile": tile, "width": 512, "height": 256})
	case r.Method == "PUT" && r.URL.Path == "/api/v2/orgs/org_member/logo":
		refuse(403, "ks_forbidden", "forbidden: your role in this organization (member) does not allow org.update")
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/logo":
		had := c.has
		c.has = false
		env(200, map[string]any{"removed": had})
	default:
		refuse(404, "ks_not_found", "no such resource")
	}
}

func (c *orgLogoCtl) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return ""
	}
	return c.sent[len(c.sent)-1]
}

func (c *orgLogoCtl) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

func TestOrganizationLogoVerbs(t *testing.T) {
	c := &orgLogoCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }

	file := filepath.Join(dir, "logo.png")
	pic := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 300)...)
	if err := os.WriteFile(file, pic, 0o600); err != nil {
		t.Fatal(err)
	}
	// set: the file's own bytes, to the logo route, with no tile unless one is given
	out, errs, code := run("organization", "logo", "set", file, "--organization", "org_a")
	if code != 0 || c.last() != "PUT /api/v2/orgs/org_a/logo" || !bytes.Equal(c.body, pic) || !strings.Contains(out, "org_a logo set: 512 x 256 px, on a light tile") || !strings.Contains(out, "original file is not kept") {
		t.Fatalf("logo set: %d %q\n%s%s", code, c.last(), out, errs)
	}
	out, errs, code = run("organization", "logo", "set", file, "--organization", "org_a", "--tile", "dark")
	if code != 0 || c.last() != "PUT /api/v2/orgs/org_a/logo?tile=dark" || !strings.Contains(out, "on a dark tile") {
		t.Fatalf("logo set --tile: %d %q\n%s%s", code, c.last(), out, errs)
	}
	out, _, code = run("organization", "logo", "set", file, "--organization", "org_a", "--json")
	var sj struct {
		Data struct {
			Organization string `json:"organization"`
			Logo         struct {
				Tile   string `json:"tile"`
				Width  int    `json:"width"`
				SHA256 string `json:"sha256"`
			} `json:"logo"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &sj) != nil || sj.Data.Organization != "org_a" || sj.Data.Logo.Width != 512 || len(sj.Data.Logo.SHA256) != 64 {
		t.Errorf("logo set --json: %d %s", code, out)
	}

	// refused before anything is sent: no file, a missing file, a folder, over 1 MB, a tile that is not one, no organization
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(big, bytes.Repeat([]byte{1}, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"organization", "logo", "set", "--organization", "org_a"},
		{"organization", "logo", "set", filepath.Join(dir, "none.png"), "--organization", "org_a"},
		{"organization", "logo", "set", dir, "--organization", "org_a"},
		{"organization", "logo", "set", big, "--organization", "org_a"},
		{"organization", "logo", "set", file, "--organization", "org_a", "--tile", "pink"},
		{"organization", "logo", "set", file, "--organization", "acct_x"},
		{"organization", "logo", "set", file},
		{"organization", "logo", "remove", "--organization", "acct_x"},
	} {
		n := c.count()
		if _, errs, code := run(bad...); code != exitUsage || c.count() != n || !strings.Contains(strings.ToLower(errs), "nothing was changed") {
			t.Errorf("%v: exit %d, %d requests, %q", bad, code, c.count()-n, errs)
		}
	}

	// the service's own words, as they come
	if _, errs, code := run("organization", "logo", "set", writeTemp(t, dir, "x.svg", "<svg xmlns='x'/>"), "--organization", "org_a"); code == 0 || !strings.Contains(errs, "a logo is a PNG, JPEG or WebP picture") {
		t.Errorf("a refused type: %d %s", code, errs)
	}
	if _, errs, code := run("organization", "logo", "set", file, "--organization", "org_member"); code == 0 || !strings.Contains(errs, "your role in this organization (member)") {
		t.Errorf("a member: %d %s", code, errs)
	}

	// remove: once removes, again says there was none
	out, errs, code = run("organization", "logo", "remove", "--organization", "org_a")
	if code != 0 || c.last() != "DELETE /api/v2/orgs/org_a/logo" || !strings.Contains(out, "org_a logo removed") {
		t.Fatalf("logo remove: %d %q\n%s%s", code, c.last(), out, errs)
	}
	if out, _, code := run("organization", "logo", "remove", "--organization", "org_a"); code != 0 || !strings.Contains(out, "had no logo") {
		t.Errorf("logo remove again: %d %s", code, out)
	}
}

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
