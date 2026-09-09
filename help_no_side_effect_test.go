// help_no_side_effect_test: the permanent guard for the defect class that
// has now appeared twice. `ks demo --help` (v0.1.1) and `ks run --help`
// (measured 2026-09-09, DEC-01 gate) both DID THE WORK instead of printing
// help, because a verb that takes no required argument reads its flags and
// falls straight through into the API call. `ks run --help` started, and
// billed, a real session.
//
// Founder ruling 2026-09-09: fix under the patch law and generalize the
// guard, "asserted across the whole manifest, so this class cannot appear
// a third time". So this test does not test `run`. It runs EVERY verb and
// EVERY alias in commands.json, in each help form the binary accepts,
// against a control plane that records what it is asked to do, and fails
// if any of them asks for anything at all.
//
// The last test in this file is the control: it proves the recorder can
// see a side effect, so a green run above means "no request was made",
// not "the test could not have noticed".
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Every invocation is deadlined. Without the guard, `ks login --help` does
// not merely act, it BLOCKS: the device flow waits on a browser that is
// never coming. A guard whose failure mode is a ten-minute hang reports
// nothing useful, so a verb that does not exit is itself a failure here.
const helpDeadline = 20 * time.Second

type recorder struct {
	mu   sync.Mutex
	hits []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits = append(r.hits, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	// A plausible session body, so a verb that got this far would proceed
	// happily rather than erroring out for an unrelated reason.
	fmt.Fprint(w, `{"id":"sess-guard","image":"test","state":"running"}`)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

// buildAndAuth builds the binary under test and hands back a config home
// that is ALREADY SIGNED IN against srv, because a signed-out client stops
// at "Not signed in" before any network call and would make this test pass
// for the wrong reason.
func buildAndAuth(t *testing.T, srv *httptest.Server) (bin, cfg string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "ks-under-test")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	cfg = t.TempDir()
	dir := filepath.Join(cfg, "keepstate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok, _ := json.Marshal(map[string]string{"token": "guard-test-token", "ctl": srv.URL})
	if err := os.WriteFile(filepath.Join(dir, "token.json"), tok, 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, cfg
}

func manifestVerbs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Commands []struct {
			Verb    string   `json:"verb"`
			Aliases []string `json:"aliases"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range m.Commands {
		names = append(names, c.Verb)
		names = append(names, c.Aliases...)
	}
	if len(names) == 0 {
		t.Fatal("commands.json lists no verbs; the guard would assert nothing")
	}
	return names
}

func TestHelpHasNoSideEffect(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	verbs := manifestVerbs(t)
	forms := []string{"--help", "-h", "help"}
	for _, v := range verbs {
		for _, f := range forms {
			t.Run(v+" "+f, func(t *testing.T) {
				before := len(rec.seen())
				ctx, cancel := context.WithTimeout(context.Background(), helpDeadline)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, v, f)
				cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Errorf("`ks %s %s` did not exit within %s; help must print and stop, never block",
						v, f, helpDeadline)
				} else if err != nil {
					t.Errorf("`ks %s %s` exited with error %v; help must succeed\n%s", v, f, err, out)
				}
				if got := rec.seen(); len(got) != before {
					t.Errorf("`ks %s %s` made %d request(s) to the control plane: %v\nhelp must never act",
						v, f, len(got)-before, got[before:])
				}
				if len(out) == 0 {
					t.Errorf("`ks %s %s` printed nothing; help must print usage", v, f)
				}
			})
		}
	}
	t.Logf("%d verbs and aliases x %d help forms = %d invocations, %d control-plane requests",
		len(verbs), len(forms), len(verbs)*len(forms), len(rec.seen()))
}

// The control. Without it, a broken recorder or a binary that cannot reach
// the server at all would make the test above green while proving nothing.
func TestSideEffectIsDetectable(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	cmd := exec.Command(bin, "run")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
	out, _ := cmd.CombinedOutput()
	got := rec.seen()
	if len(got) == 0 {
		t.Fatalf("`ks run` with no help flag made no request, so this test cannot detect a side effect and the guard above is vacuous\n%s", out)
	}
	t.Logf("control: `ks run` asks for %v, which is exactly what --help must not do", got)
}
