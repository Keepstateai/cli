// KS-010 gating: each verb is judged by its own capability row; only a
// registry that predates that row (an older protocol) falls back to
// agent.workspace; an unavailable row disables the verb with its reason.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type gateCtl struct {
	mu    sync.Mutex
	caps  string
	calls []string
}

func (c *gateCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.URL.Path)
	switch r.URL.Path {
	case "/api/capabilities":
		fmt.Fprintf(w, `{"schema_version":2,"data":{"registry_version":"t","build":"b","fetched_at":"x","price_book":"v1.3","capabilities":[%s],"limits":{}}}`, c.caps)
	case "/api/v2/results/res_1":
		fmt.Fprint(w, `{"schema_version":2,"data":{"id":"res_1","name":"r.txt","bytes":1,"sha256":"`+strings.Repeat("a", 64)+`"}}`)
	default:
		w.WriteHeader(404)
	}
}

func (c *gateCtl) reached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.calls {
		if strings.HasPrefix(x, "/api/v2/results") {
			return true
		}
	}
	return false
}

func TestEachVerbIsGatedByItsOwnRow(t *testing.T) {
	c := &gateCtl{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	run := func(caps string) (string, int) {
		c.mu.Lock()
		c.caps, c.calls = caps, nil
		c.mu.Unlock()
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), "result", "show", "res_1")
		return errs, code
	}
	ws := func(a string) string {
		return fmt.Sprintf(`{"id":"agent.workspace","availability":%q,"summary":"s","surface":"api","unavailable_reason":"agent sessions are not available in this release"}`, a)
	}
	// its own row available: runs, whatever agent.workspace says
	if errs, code := run(ws("unavailable") + `,{"id":"tasks.results","availability":"available","summary":"s","surface":"api","since_protocol":1}`); code != 0 || !c.reached() {
		t.Fatalf("own row available: %d\n%s", code, errs)
	}
	// its own row unavailable: refused with that row's reason, even though agent.workspace is available
	errs, code := run(ws("available") + `,{"id":"tasks.results","availability":"unavailable","summary":"s","surface":"api","unavailable_reason":"results are not served by this build"}`)
	if code != exitFailed || !strings.Contains(errs, "tasks.results") || !strings.Contains(errs, "results are not served by this build") || c.reached() {
		t.Fatalf("own row unavailable: %d\n%s", code, errs)
	}
	// a registry that predates the row: judged by agent.workspace
	errs, code = run(ws("unavailable"))
	if code != exitFailed || !strings.Contains(errs, "agent.workspace") || !strings.Contains(errs, "agent sessions are not available") || c.reached() {
		t.Fatalf("fallback unavailable: %d\n%s", code, errs)
	}
	if errs, code := run(ws("available")); code != 0 || !c.reached() {
		t.Fatalf("fallback available: %d\n%s", code, errs)
	}
}
