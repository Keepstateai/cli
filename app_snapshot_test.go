package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestAppSnapshots is a design tool, not a check: with KS_SNAPSHOT_OUT set,
// it runs the app against the snapshot service (snapCtl) through a person's
// journeys, in a pseudo-terminal driven by testdata/ptydrive.py, and keeps
// the raw terminal bytes at each named step for rendering and review. Every
// screen the app has is reached: home with rows that need you, a paused
// session and a plain session; the plain-session card; the new-agent card
// with a name it refuses, the key choice and the start; the paused and the
// out-of-credit cards; resuming; the recovery card; the agent's window with
// a permission request, its details, /help, an error, a ks command typed as
// an instruction, a / command and its suggestions; Esc with nothing to
// interrupt; the confirm cards for stopping an agent and deleting a session
// (the plan's summary, its whole plan, kept, deleted); a / command that is
// not one, with the nearest suggested.
//
// Each journey runs at 110 x 34, again at 80 x 30 (OUT/narrow) and, for a
// few, on a light terminal (OUT/light). KS_SNAPSHOT_ONLY limits the run to
// journeys whose name contains it. KS_SNAPSHOT_VARIANT=before replays the
// same journeys with the keys the previous release's screens take, so the
// two can be compared side by side. It is skipped everywhere else.
func TestAppSnapshots(t *testing.T) {
	outDir := os.Getenv("KS_SNAPSHOT_OUT")
	if outDir == "" {
		t.Skip("a design tool: set KS_SNAPSHOT_OUT")
	}
	driver := os.Getenv("KS_SNAPSHOT_DRIVER")
	if driver == "" {
		driver, _ = filepath.Abs("testdata/ptydrive.py")
	}
	before := os.Getenv("KS_SNAPSHOT_VARIANT") == "before"

	// one server for every journey; each journey gets a fresh account
	var mu sync.Mutex
	var cur *snapCtl
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := cur
		mu.Unlock()
		c.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cur = newSnapCtl()
	bin, cfg0 := buildAndAuth(t, srv)
	token, err := os.ReadFile(filepath.Join(cfg0, "keepstate", "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	// the row each journey opens, counted from the first row selected
	row := map[string]int{"plain": 5, "dev1": 4, "ops": 1, "fixer": 2}
	if before {
		row = map[string]int{"plain": 5, "dev1": 3, "ops": 4, "fixer": 1}
	}
	down := func(name string) string { return strings.Repeat("\x1b[B", row[name]) }
	snap := func(name, expect string, wait float64) []string {
		return []string{expect, "", "new", "any", "soft", "wait:" + strconv.FormatFloat(wait, 'f', 1, 64), "snap:" + name}
	}
	send := func(keys string) []string { return []string{"", keys, "wait:0.4"} }
	back := "\x1b"
	stopCmd := "/Stop\r"
	if before {
		back = ""
		stopCmd = "/stop\r"
	}

	journeys := []struct {
		name  string
		light bool
		steps [][]string
	}{
		{"home", true, [][]string{
			snap("01-home", "Your agents", 1.0),
			send("q"),
		}},
		{"plain", false, [][]string{
			{"Your agents", ""},
			send(down("plain")),
			send("\r"),
			snap("02-plain-session", "attach 697ca7", 0.8),
			send(back), send("q"),
		}},
		{"parked", true, [][]string{
			{"Your agents", ""},
			send(down("dev1")),
			send("\r"),
			snap("03-parked", "resume", 1.0),
			send(back), send("q"),
		}},
		{"funds", false, [][]string{
			{"Your agents", ""},
			send(down("ops")),
			send("\r"),
			snap("04-funds-paused", "credit", 1.0),
			send(back), send("q"),
		}},
		{"recovery", false, [][]string{
			{"Your agents", ""},
			send(down("fixer")),
			send("\r"),
			snap("05-recovery", "stopped||recovery", 1.0),
			send(back), send("q"),
		}},
		{"resume", false, [][]string{
			{"Your agents", ""},
			send(down("dev1")),
			send("\r"),
			{"resume", "r", "any", "wait:0.8"},
			snap("06-resuming", "esuming", 0.3),
			snap("07-resumed-needs-attention", "migration", 2.5),
			send("\x03"), send("\x03"), send("q"),
		}},
		{"new", false, newAgentJourney(before, snap, send)},
		{"manage", false, [][]string{
			{"Your agents", ""},
			send("\r"),
			{"allow", "", "any", "wait:1.5"},
			send("\x1b"),
			snap("21-esc-nothing", "nterrupt", 0.4),
			send(stopCmd),
			snap("22-stop-confirm", "Stop||stop", 0.8),
			send("y"),
			snap("23-stopped", "top", 2.5),
			send("/stpo\r"),
			snap("24-unknown-slash", "not a ks command", 0.6),
			send("/session delete\r"),
			snap("25-delete-confirm", "elete", 1.5),
			send("d"),
			snap("26-delete-whole-plan", "plan", 0.8),
			send("n"),
			snap("27-delete-kept", "ept", 0.8),
			send("/kill\r"),
			{"elete||plan", "y", "any", "new", "soft", "wait:1.5"},
			snap("28-deleted", "eleted", 3.0),
			send("\x03"), send("\x03"), send("q"),
		}},
		{"window", true, [][]string{
			{"Your agents", ""},
			send("\r"),
			snap("12-window-permission", "allow", 1.5),
			send("d"),
			snap("13-permission-details", "git push", 0.8),
			send("y"),
			send("fix the login test\r"),
			snap("14-conversation", "Queued||tsk_new1", 1.2),
			send("/help\r"),
			snap("15-help", "/take", 0.6),
			send("/switch nosuch\r"),
			snap("16-error", "nosuch", 0.6),
			send("ks agent list\r"),
			snap("17-ks-typed", "ks command", 0.6),
			send("\x15/agent st"),
			snap("18-suggest", "agent status", 0.5),
			send("\x15/agent list\r"),
			snap("19-slash", "agent list", 3.0),
			send("\x03"), send("\x03"),
			snap("20-left", "Your agents", 1.0),
			send("q"),
		}},
	}
	only := os.Getenv("KS_SNAPSHOT_ONLY")
	type size struct {
		dir        string
		cols, rows int
		light      bool
	}
	for _, j := range journeys {
		if only != "" && !strings.Contains(j.name, only) {
			continue
		}
		sizes := []size{{outDir, 110, 34, false}, {filepath.Join(outDir, "narrow"), 80, 30, false}}
		if j.light {
			sizes = append(sizes, size{filepath.Join(outDir, "light"), 110, 34, true})
		}
		for _, sz := range sizes {
			mu.Lock()
			cur = newSnapCtl()
			mu.Unlock()
			cfg := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg, "keepstate"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg, "keepstate", "token.json"), token, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(sz.dir, 0o755); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"XDG_CONFIG_HOME": cfg, "KS_HTTP_TIMEOUT_MS": "4000", "TERM": "xterm-256color",
				"COLORTERM": "truecolor", "LANG": "en_US.UTF-8", "KS_THEME": "dark"}
			if sz.light {
				env["KS_THEME"] = "light"
			}
			spec := map[string]any{"argv": []string{bin}, "env": env, "timeout": 15, "rows": sz.rows, "cols": sz.cols, "steps": j.steps}
			f := filepath.Join(t.TempDir(), "spec.json")
			b, _ := json.Marshal(spec)
			if err := os.WriteFile(f, b, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("python3", driver, f, sz.dir).CombinedOutput()
			if err != nil {
				t.Errorf("journey %s at %dx%d: %v\n%s", j.name, sz.cols, sz.rows, err, tail(string(out), 3000))
			}
			if s := string(out); strings.Contains(s, "@@ STEP") {
				for _, l := range strings.Split(s, "\n") {
					if strings.HasPrefix(l, "@@ STEP") {
						t.Logf("journey %s at %dx%d: %s", j.name, sz.cols, sz.rows, l)
					}
				}
			}
			_ = os.Rename(filepath.Join(sz.dir, "final.raw"), filepath.Join(sz.dir, fmt.Sprintf("final-%s.raw", j.name)))
		}
	}
}

// newAgentJourney starts an agent: the card, a name the service would
// refuse, the key choice, the start and the agent's window.
func newAgentJourney(before bool, snap func(string, string, float64) []string, send func(string) []string) [][]string {
	steps := [][]string{
		{"Your agents", ""},
		send("n"),
		snap("08-new-agent", "Name||name", 0.6),
		send("my agent!\r"),
		snap("09-new-agent-invalid", "not a name||letter", 0.6),
	}
	if before {
		steps = append(steps, send("n"), send("helper\r"))
	} else {
		steps = append(steps, send("\x15helper\r"))
	}
	steps = append(steps, snap("10-new-agent-key", "key", 1.0))
	if before {
		steps = append(steps, []string{"", "key_team\r", "wait:0.6"})
	} else {
		steps = append(steps, []string{"", "\r", "wait:0.6"})
	}
	steps = append(steps,
		snap("11a-new-agent-starting", "", 0.1),
		snap("11b-new-agent-started", "helper", 2.5),
		send("\x03"), send("\x03"), send("q"))
	return steps
}
