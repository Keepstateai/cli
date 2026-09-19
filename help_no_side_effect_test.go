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
	"strings"
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

// manifestVerbs returns every verb and alias as the argument list that
// invokes it: a subcommand row ("cruise init") is two arguments.
func manifestVerbs(t *testing.T) [][]string {
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
	var names [][]string
	for _, c := range m.Commands {
		names = append(names, strings.Fields(c.Verb))
		for _, a := range c.Aliases {
			names = append(names, strings.Fields(a))
		}
	}
	if len(names) == 0 {
		t.Fatal("commands.json lists no verbs; the guard would assert nothing")
	}
	return names
}

// runQuiet runs the binary with the given arguments against the signed-in
// config and reports whether it exited in time, its error and its output.
func runQuiet(t *testing.T, bin, cfg string, env []string, args ...string) (timedOut bool, err error, out []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), helpDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+cfg), env...)
	out, err = cmd.CombinedOutput()
	return ctx.Err() != nil, err, out
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
			name := strings.Join(v, " ")
			t.Run(name+" "+f, func(t *testing.T) {
				before := len(rec.seen())
				timedOut, err, out := runQuiet(t, bin, cfg, nil, append(append([]string{}, v...), f)...)
				if timedOut {
					t.Errorf("`ks %s %s` did not exit within %s; help must print and stop, never block",
						name, f, helpDeadline)
				} else if err != nil {
					t.Errorf("`ks %s %s` exited with error %v; help must succeed\n%s", name, f, err, out)
				}
				if got := rec.seen(); len(got) != before {
					t.Errorf("`ks %s %s` made %d request(s) to the control plane: %v\nhelp must never act",
						name, f, len(got)-before, got[before:])
				}
				if len(out) == 0 {
					t.Errorf("`ks %s %s` printed nothing; help must print usage", name, f)
				}
			})
		}
	}
	t.Logf("%d verbs and aliases x %d help forms = %d invocations, %d control-plane requests",
		len(verbs), len(forms), len(verbs)*len(forms), len(rec.seen()))
}

// TestCruiseHelpFormsHaveNoSideEffect covers the help paths that are not a
// manifest verb followed by a help word: a bare `ks cruise` (which prints
// usage and exits 2, like a bare `ks`), `ks cruise --help` and `-h`, and
// `ks help cruise`. Gate CR-7 counts exactly these.
func TestCruiseHelpFormsHaveNoSideEffect(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	forms := [][]string{{"cruise"}, {"cruise", "--help"}, {"cruise", "-h"}, {"help", "cruise"}, {"cruise", "nonsense", "--help"}}
	for _, args := range forms {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			before := len(rec.seen())
			timedOut, _, out := runQuiet(t, bin, cfg, nil, args...)
			if timedOut {
				t.Errorf("`ks %s` did not exit within %s", name, helpDeadline)
			}
			if got := rec.seen(); len(got) != before {
				t.Errorf("`ks %s` made %d request(s): %v", name, len(got)-before, got[before:])
			}
			if !strings.Contains(string(out), "ks cruise init") {
				t.Errorf("`ks %s` did not print the cruise usage:\n%s", name, out)
			}
		})
	}
}

// The cruise controls: the recorder sees a cruise verb that acts, and the
// CR-7 sabotage hook makes `ks cruise run --help` act, so a green run of
// the tests above is a measurement.
func TestCruiseSideEffectIsDetectable(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)

	_, _, out := runQuiet(t, bin, cfg, nil, "cruise", "status", "job_x")
	if got := rec.seen(); len(got) == 0 {
		t.Fatalf("`ks cruise status job_x` made no request; the cruise guard would be vacuous\n%s", out)
	} else if got[0] != "GET /api/jobs/job_x" {
		t.Errorf("`ks cruise status job_x` asked for %v, want GET /api/jobs/job_x", got)
	}

	before := len(rec.seen())
	_, _, out = runQuiet(t, bin, cfg, []string{"KS_CLI_SABOTAGE_HELP=1"}, "cruise", "run", "--help")
	if got := rec.seen(); len(got) != before+1 {
		t.Errorf("the sabotage hook made %d request(s), want exactly 1 (gate CR-7 --sabotage proves the counter bites)\n%s", len(got)-before, out)
	}
	if !strings.Contains(string(out), "ks cruise run") {
		t.Errorf("the sabotage hook must still print the command's help:\n%s", out)
	}
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
