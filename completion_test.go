// completion_test: KS-008's guards. The scripts come from the registry and
// name every verb, subcommand and option; generating them makes no
// request; each script parses in its shell where that shell exists; and
// the offline commands answer within the C12 latency target.
package main

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCompletionScriptsCoverTheRegistry(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script, err := completionScript(shell)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range registry {
			if c.Group {
				if !strings.Contains(script, c.Path[0]) {
					t.Errorf("%s: group %s missing", shell, c.Path[0])
				}
				continue
			}
			for _, w := range c.Path {
				if !strings.Contains(script, w) {
					t.Errorf("%s: word %q of %s missing", shell, w, c.Name())
				}
			}
			for _, f := range c.Flags {
				name := "--" + f.Name
				if shell == "fish" {
					name = "-l " + f.Name
				}
				if !strings.Contains(script, name) {
					t.Errorf("%s: option %s of %s missing", shell, name, c.Name())
				}
			}
		}
		for _, g := range globalFlags {
			if !strings.Contains(script, g.Name) {
				t.Errorf("%s: global option --%s missing", shell, g.Name)
			}
		}
		if !strings.Contains(script, "never edits shell files") {
			t.Errorf("%s: the script does not say installation is manual", shell)
		}
	}
	if _, err := completionScript("powershell"); err == nil {
		t.Error("an unknown shell must be refused")
	}
	// the reference table lists every command once
	ref := referenceTable(registry)
	for _, c := range registry {
		if !c.Group && strings.Count(ref, "`"+c.Usage()+"`") != 1 {
			t.Errorf("reference lacks %s", c.Name())
		}
	}
}

// Each script is accepted by its shell's parser where that shell is installed.
//
// This test used to PASS while never exercising fish: an absent shell was a
// t.Logf and a `continue` inside one flat test, so a third of the matrix
// could be vacuous and the single green said nothing about which third ran.
// VER-087-1 asks for exactly the thing that hid: the combinations actually
// exercised, named.
//
// Now every shell is its own subtest, so an absent one is reported by the
// test runner as `--- SKIP: .../fish` under its own name and can never be
// mistaken for a parse that happened. The exercised set is logged in one
// machine-readable line, a run that exercises NOTHING fails rather than
// passing vacuously, and KS_REQUIRE_ALL_SHELLS=1 (for release CI, where the
// matrix is supposed to be complete) turns an absent shell into a failure.
func TestCompletionScriptsParseInTheirShells(t *testing.T) {
	checks := map[string][]string{"bash": {"bash", "-n"}, "zsh": {"zsh", "-n"}, "fish": {"fish", "--no-execute"}}
	shells := make([]string, 0, len(checks))
	for shell := range checks {
		shells = append(shells, shell)
	}
	sort.Strings(shells)

	strict := os.Getenv("KS_REQUIRE_ALL_SHELLS") == "1"
	var exercised, absent []string
	for _, shell := range shells {
		check := checks[shell]
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(check[0])
			if err != nil {
				if strict {
					t.Fatalf("%s is not installed and KS_REQUIRE_ALL_SHELLS=1: the matrix is incomplete", shell)
				}
				absent = append(absent, shell)
				t.Skipf("%s is not installed: this shell's script was NOT parsed by anything", shell)
			}
			script, _ := completionScript(shell)
			cmd := exec.Command(check[0], check[1:]...)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s rejected the generated script: %v\n%s", shell, err, out)
				return
			}
			exercised = append(exercised, shell+"="+path)
		})
	}
	// The matrix this run actually exercised, recorded rather than implied.
	t.Logf("shell parse matrix: platform=%s/%s exercised=[%s] absent=[%s]",
		runtime.GOOS, runtime.GOARCH, strings.Join(exercised, " "), strings.Join(absent, " "))
	if len(exercised) == 0 {
		t.Fatalf("no shell was exercised: this test proved nothing about any generated script")
	}
}

// QA-008-1 and VER-008-2 (smoke form): completion, help and version answer
// offline, make no request, and stay inside the 150 ms target at p95 over
// a sample; the thousand-invocation gate is C12's on the stated machine.
func TestOfflineCommandsAreFastAndSilent(t *testing.T) {
	rec, bin, cfg := startAuditRecorder(t)
	cases := [][]string{{"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"version"}, {"run", "--help"}, {"help"}, {"reference"}}
	var durations []time.Duration
	for i := 0; i < 8; i++ {
		for _, args := range cases {
			start := time.Now()
			out, _, code := auditExec(t, bin, cfg, t.TempDir(), nil, args...)
			durations = append(durations, time.Since(start))
			if code != 0 || len(out) == 0 {
				t.Errorf("`ks %s`: exit %d, %d bytes", strings.Join(args, " "), code, len(out))
			}
		}
	}
	if got := rec.drain(); len(got) != 0 {
		t.Errorf("offline commands made requests: %v", got)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[len(durations)*95/100]
	if p95 > 150*time.Millisecond {
		t.Errorf("p95 %s over the 150 ms target (%d samples)", p95, len(durations))
	}
	t.Logf("p95 %s over %d offline invocations, zero requests", p95, len(durations))
	_ = httptest.NewRecorder
}
