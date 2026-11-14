// ks profile and ks email: your own profile and email choices from the
// terminal, through the routes the console's account menu uses. A change
// names only what was given, an empty one is refused before any request, a
// refusal is shown in the service's words, and neither pair ever sends an
// organization's context.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type prefsCtl struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	refuse   string // "name", "title", "empty" or ""
	noPic    bool
}

func (c *prefsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	refusal := func(code int, typ, msg, next string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"schema_version":2,"request_id":"r","error":{"code":%q,"type":%q,"message":%q,"retryable":false,"work_started":"no","next_action":%q}}`, typ, typ, msg, next)
	}
	if r.URL.Path != "/api/v2/profile" && r.URL.Path != "/api/v2/email-preferences" {
		w.WriteHeader(404)
		return
	}
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	c.bodies = append(c.bodies, string(b))
	profile := map[string]any{"display_name": "Ada Example", "title": "Analyst", "picture_url": "https://example.test/ada.png", "show_picture": true, "initials": "AE", "email": "ada@example.test"}
	if c.noPic {
		profile["picture_url"] = ""
	}
	switch {
	case r.URL.Path == "/api/v2/profile" && r.Method == "GET":
		env(200, profile)
	case r.URL.Path == "/api/v2/profile" && r.Method == "PATCH":
		switch c.refuse {
		case "name":
			refusal(422, "ks_profile_name_invalid", "a name is 1 to 80 characters with no control characters", "ks profile show")
			return
		case "title":
			refusal(422, "ks_profile_title_invalid", "a title is up to 80 characters", "")
			return
		case "empty":
			refusal(400, "ks_patch_empty", "name at least one thing to change", "")
			return
		}
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if v, ok := body["display_name"]; ok {
			profile["display_name"] = v
		}
		if v, ok := body["title"]; ok {
			profile["title"] = v
		}
		if v, ok := body["show_picture"]; ok {
			profile["show_picture"] = v
			if v == false {
				profile["picture_url"] = ""
			}
		}
		env(200, profile)
	case r.URL.Path == "/api/v2/email-preferences" && r.Method == "GET":
		env(200, map[string]any{"onboarding": true, "changelog": false, "onboarding_max": 3})
	case r.URL.Path == "/api/v2/email-preferences" && r.Method == "PATCH":
		if c.refuse == "empty" {
			refusal(400, "ks_patch_empty", "name at least one choice to change", "ks email show")
			return
		}
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		on, ch := true, false
		if v, ok := body["onboarding"].(bool); ok {
			on = v
		}
		if v, ok := body["changelog"].(bool); ok {
			ch = v
		}
		env(200, map[string]any{"onboarding": on, "changelog": ch, "onboarding_max": 3})
	default:
		w.WriteHeader(405)
	}
}

func prefsHarness(t *testing.T) (*prefsCtl, *httptest.Server, func(args ...string) (string, string, int), string) {
	t.Helper()
	c := &prefsCtl{}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	return c, srv, func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }, cfg
}

func (c *prefsCtl) last() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.requests) - 1
	if n < 0 {
		return "", ""
	}
	return c.requests[n], c.bodies[n]
}

func (c *prefsCtl) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func TestProfileFromTheTerminal(t *testing.T) {
	c, _, run, _ := prefsHarness(t)

	out, _, code := run("profile", "show")
	if code != 0 || !strings.Contains(out, "Ada Example") || !strings.Contains(out, "Analyst") || !strings.Contains(out, "ada@example.test") || !strings.Contains(out, "your sign-in picture shows") {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if req, _ := c.last(); req != "GET /api/v2/profile" {
		t.Errorf("show asked %s", req)
	}

	// only what was named is sent
	out, _, code = run("profile", "set", "--name", "Ada Q. Example")
	if code != 0 || !strings.Contains(out, "Your profile is saved.") || !strings.Contains(out, "Ada Q. Example") {
		t.Fatalf("set --name: %d\n%s", code, out)
	}
	if req, body := c.last(); req != "PATCH /api/v2/profile" || body != `{"display_name":"Ada Q. Example"}` {
		t.Errorf("set --name sent %s %s", req, body)
	}
	// an empty title clears it, and is still sent
	out, _, code = run("profile", "set", "--title", "")
	if code != 0 || !strings.Contains(out, "(none)") {
		t.Fatalf("set --title \"\": %d\n%s", code, out)
	}
	if _, body := c.last(); body != `{"title":""}` {
		t.Errorf("an empty title is sent as an empty title: %s", body)
	}
	out, _, code = run("profile", "set", "--picture", "off", "--title", "Lead")
	if code != 0 || !strings.Contains(out, "off (your initials show instead)") {
		t.Fatalf("set --picture off: %d\n%s", code, out)
	}
	if _, body := c.last(); body != `{"show_picture":false,"title":"Lead"}` {
		t.Errorf("set sent %s", body)
	}
	if out, _, code := run("profile", "set", "--picture", "ON"); code != 0 || !strings.Contains(out, "Picture:") {
		t.Fatalf("set --picture ON: %d\n%s", code, out)
	}
	if _, body := c.last(); body != `{"show_picture":true}` {
		t.Errorf("set sent %s", body)
	}
	// on with no picture yet says so
	c.mu.Lock()
	c.noPic = true
	c.mu.Unlock()
	if out, _, _ := run("profile", "show"); !strings.Contains(out, "you have no picture yet") {
		t.Errorf("no picture:\n%s", out)
	}
	c.mu.Lock()
	c.noPic = false
	c.mu.Unlock()

	// --json carries the service's answer in the usual envelope
	out, _, code = run("profile", "show", "--json")
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Data          struct {
			DisplayName string `json:"display_name"`
			ShowPicture bool   `json:"show_picture"`
			Email       string `json:"email"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.SchemaVersion != 2 || doc.Data.DisplayName != "Ada Example" || !doc.Data.ShowPicture || doc.Data.Email != "ada@example.test" {
		t.Errorf("show --json: %d\n%s", code, out)
	}
	if out, _, code := run("profile", "set", "--name", "New Name", "--json"); code != 0 || !strings.Contains(out, `"display_name":"New Name"`) {
		t.Errorf("set --json: %d\n%s", code, out)
	}
}

func TestProfileRefusalsSendNothing(t *testing.T) {
	c, _, run, _ := prefsHarness(t)
	n := c.count()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"profile", "set"}, "give --name, --title or --picture"},
		{[]string{"profile", "set", "--name", "   "}, "--name cannot be empty"},
		{[]string{"profile", "set", "--name", strings.Repeat("a", 81)}, "at most 80 characters"},
		{[]string{"profile", "set", "--title", strings.Repeat("é", 81)}, "at most 80 characters"},
		{[]string{"profile", "set", "--picture", "maybe"}, `--picture is on or off, not "maybe"`},
		{[]string{"profile", "set", "--name"}, ""},
		{[]string{"profile", "show", "extra"}, ""},
	} {
		_, errs, code := run(tc.args...)
		if code != exitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: %d\n%s", tc.args, code, errs)
		}
		if tc.want != "" && !strings.Contains(errs, "nothing was changed") {
			t.Errorf("%v does not say nothing was changed:\n%s", tc.args, errs)
		}
	}
	// a name of exactly 80 characters is the service's to judge, not refused here
	if _, _, code := run("profile", "set", "--name", strings.Repeat("é", 80)); code != 0 {
		t.Errorf("80 characters refused: %d", code)
	}
	if got := c.count() - n; got != 1 {
		t.Errorf("refused commands sent %d requests (only the 80-character name should have)", got)
	}
}

func TestProfileServiceRefusalsAreShownInItsWords(t *testing.T) {
	c, _, run, _ := prefsHarness(t)
	c.mu.Lock()
	c.refuse = "name"
	c.mu.Unlock()
	_, errs, code := run("profile", "set", "--name", "x\x07y")
	if code != exitUsage || !strings.Contains(errs, "a name is 1 to 80 characters with no control characters") || !strings.Contains(errs, "Nothing was changed") || !strings.Contains(errs, "Next: ks profile show") || strings.Contains(errs, "goroutine") {
		t.Errorf("name refusal: %d\n%s", code, errs)
	}
	c.mu.Lock()
	c.refuse = "title"
	c.mu.Unlock()
	if _, errs, code := run("profile", "set", "--title", "t"); code != exitUsage || !strings.Contains(errs, "a title is up to 80 characters") {
		t.Errorf("title refusal: %d\n%s", code, errs)
	}
	// in --json the error is the usual shape, with the service's code
	c.mu.Lock()
	c.refuse = "name"
	c.mu.Unlock()
	out, _, code := run("profile", "set", "--name", "x", "--json")
	if code != exitUsage || !strings.Contains(out, `"code":"ks_profile_name_invalid"`) || !strings.Contains(out, "a name is 1 to 80 characters") {
		t.Errorf("name refusal --json: %d\n%s", code, out)
	}
}

func TestEmailChoicesFromTheTerminal(t *testing.T) {
	c, _, run, _ := prefsHarness(t)

	out, _, code := run("email", "show")
	if code != 0 || !strings.Contains(out, "Onboarding emails: on") || !strings.Contains(out, "up to 3 in the first 14 days") || !strings.Contains(out, "Changelog email:   off") ||
		!strings.Contains(out, "Mail about your account always arrives") || !strings.Contains(out, "ks email set") {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if req, _ := c.last(); req != "GET /api/v2/email-preferences" {
		t.Errorf("show asked %s", req)
	}
	out, _, code = run("email", "set", "--changelog", "on")
	if code != 0 || !strings.Contains(out, "Your email choices are saved.") || !strings.Contains(out, "Changelog email:   on") {
		t.Fatalf("set --changelog: %d\n%s", code, out)
	}
	if req, body := c.last(); req != "PATCH /api/v2/email-preferences" || body != `{"changelog":true}` {
		t.Errorf("set --changelog sent %s %s", req, body)
	}
	out, _, code = run("email", "set", "--onboarding", "off", "--changelog", "off")
	if code != 0 || !strings.Contains(out, "Onboarding emails: off") {
		t.Fatalf("set both: %d\n%s", code, out)
	}
	if _, body := c.last(); body != `{"changelog":false,"onboarding":false}` {
		t.Errorf("set both sent %s", body)
	}
	out, _, code = run("email", "show", "--json")
	var doc struct {
		Data struct {
			Onboarding    bool `json:"onboarding"`
			Changelog     bool `json:"changelog"`
			OnboardingMax int  `json:"onboarding_max"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || !doc.Data.Onboarding || doc.Data.Changelog || doc.Data.OnboardingMax != 3 {
		t.Errorf("show --json: %d\n%s", code, out)
	}
	if out, _, code := run("email", "set", "--onboarding", "on", "--json"); code != 0 || !strings.Contains(out, `"onboarding":true`) {
		t.Errorf("set --json: %d\n%s", code, out)
	}
}

func TestEmailRefusalsSendNothing(t *testing.T) {
	c, _, run, _ := prefsHarness(t)
	n := c.count()
	for _, tc := range [][]string{
		{"email", "set"},
		{"email", "set", "--onboarding", "yes"},
		{"email", "set", "--changelog", ""},
		{"email", "set", "--onboarding", "on", "--changelog", "1"},
		{"email", "set", "--changelog"},
	} {
		if _, errs, code := run(tc...); code != exitUsage || strings.TrimSpace(errs) == "" {
			t.Errorf("%v: %d\n%s", tc, code, errs)
		}
	}
	_, errs, _ := run("email", "set")
	if !strings.Contains(errs, "give --onboarding or --changelog") || !strings.Contains(errs, "nothing was changed") || !strings.Contains(errs, "Next: ks email show") {
		t.Errorf("empty set:\n%s", errs)
	}
	if c.count() != n {
		t.Errorf("a refused command sent a request: %v", c.requests[n:])
	}
	// the service's own refusal of an empty change is shown too
	c.mu.Lock()
	c.refuse = "empty"
	c.mu.Unlock()
	if _, errs, code := run("email", "set", "--changelog", "on"); code != exitUsage || !strings.Contains(errs, "name at least one choice to change") || !strings.Contains(errs, "Next: ks email show") {
		t.Errorf("service empty refusal: %d\n%s", code, errs)
	}
}

// These are routes of your own account: an organization's context is never
// sent with them, and they work the same whichever context is chosen.
func TestProfileAndEmailNeverSendAContext(t *testing.T) {
	c, srv, run, cfg := prefsHarness(t)
	ctx, _ := json.Marshal(map[string]string{srv.URL: "org_a"})
	if err := os.WriteFile(filepath.Join(cfg, "keepstate", "context.json"), ctx, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := run("context", "show"); !strings.Contains(out, "org_a") {
		t.Fatalf("the context was not set:\n%s", out)
	}
	for _, args := range [][]string{
		{"profile", "show"}, {"profile", "set", "--title", "x"},
		{"email", "show"}, {"email", "set", "--onboarding", "off"},
	} {
		if _, errs, code := run(args...); code != 0 {
			t.Fatalf("%v in an organization's context: %d\n%s", args, code, errs)
		}
	}
	for _, r := range c.requests {
		if strings.Contains(r, "context=") || strings.Contains(r, "?") {
			t.Errorf("asked %s: these routes take no context", r)
		}
	}
}

func TestProfileAndEmailHelpMakesNoRequest(t *testing.T) {
	c, _, run, _ := prefsHarness(t)
	for _, args := range [][]string{
		{"profile", "--help"}, {"profile", "show", "--help"}, {"profile", "set", "--help"},
		{"email", "--help"}, {"email", "show", "--help"}, {"email", "set", "--help"},
		{"profile", "set", "--name", "x", "--help"}, {"email", "set", "--changelog", "on", "--help"},
	} {
		out, errs, code := run(args...)
		if code != 0 || !strings.Contains(out, "ks ") {
			t.Errorf("%v: %d\n%s%s", args, code, out, errs)
		}
	}
	if c.count() != 0 {
		t.Errorf("help asked the service: %v", c.requests)
	}
	// every ks command the help prints is a real verb
	out, _, _ := run("email", "set", "--help")
	for _, want := range []string{"ks email set --onboarding off", "--onboarding on|off", "--changelog on|off"} {
		if !strings.Contains(out, want) {
			t.Errorf("email set help lacks %q:\n%s", want, out)
		}
	}
}
