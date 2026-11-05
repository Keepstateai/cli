// The KeepState plugin for herdr: the manifest against its golden file and
// herdr's id rules, install, uninstall and status against a fake herdr that
// records every call, the agents pane opening an agent in a new tab, and the
// receipts pane following a job to its receipt. No real herdr is run.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeHerdrCLI writes a herdr that records its arguments (one call per
// line, arguments after |) and answers as herdr's CLI reference says:
// --version prints a version, tab create prints the new tab's JSON. When
// the file "fail" exists beside it, it prints its contents and exits 1.
func fakeHerdrCLI(t *testing.T) (dir, bin, log string) {
	t.Helper()
	dir = t.TempDir()
	bin, log = filepath.Join(dir, "herdr"), filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
line=""
for a in "$@"; do line="$line|$a"; done
printf '%s\n' "$line" >> ` + log + `
if [ -f ` + dir + `/fail ]; then cat ` + dir + `/fail; exit 1; fi
case "$1" in
--version) echo "herdr 0.9.5" ;;
tab) printf '{"id":"cli","result":{"tab":{"tab_id":"w1:t9"},\n "root_pane":{"pane_id":"w1:p9"}}}\n' ;;
plugin) case "$2" in
  unlink|uninstall) if [ -f ` + dir + `/$2.out ]; then cat ` + dir + `/$2.out; fi ;;
  list) if [ -f ` + dir + `/list.out ]; then cat ` + dir + `/list.out; else echo '{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}'; fi ;;
  esac ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, bin, log
}

// fakeOpener is a browser opener (open on macOS, xdg-open elsewhere) that
// records the address it was given.
func fakeOpener(t *testing.T, dir string) string {
	t.Helper()
	log := filepath.Join(dir, "opened.log")
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+log+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func noHerdrEnv() []string { return []string{"HERDR_ENV=", "HERDR_PANE_ID=", "HERDR_BIN_PATH="} }

func TestHerdrManifestIsTheGoldenFileAndFollowsHerdrsIDRules(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "herdr", "herdr-plugin.toml.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got := herdrManifest("v0.1.33"); got != string(golden) {
		t.Errorf("manifest differs from the golden file:\n%s", got)
	}
	if !strings.Contains(herdrManifest("dev"), `version = "0.0.0-dev"`) || !strings.Contains(herdrManifest("0.2.0-rc.1"), `version = "0.2.0-rc.1"`) {
		t.Error("versions")
	}
	m := string(golden)
	// the plugin id: letters, digits, dot, colon, underscore, hyphen
	if !regexp.MustCompile(`(?m)^id = "[A-Za-z0-9.:_-]+"$`).MatchString(m) {
		t.Error("plugin id")
	}
	// local ids (panes and actions) never carry a dot
	local := regexp.MustCompile(`(?m)^\[\[(panes|actions)\]\]\nid = "([^"]*)"`).FindAllStringSubmatch(m, -1)
	if len(local) != 3 {
		t.Fatalf("local ids %v", local)
	}
	for _, l := range local {
		if !regexp.MustCompile(`^[A-Za-z0-9:_-]+$`).MatchString(l[2]) {
			t.Errorf("local id %q", l[2])
		}
	}
	// every command runs ks itself, as an argv array
	for _, c := range regexp.MustCompile(`(?m)^command = (.*)$`).FindAllStringSubmatch(m, -1) {
		var argv []string
		if err := json.Unmarshal([]byte(c[1]), &argv); err != nil || argv[0] != "ks" || argv[1] != "herdr" {
			t.Errorf("command %s", c[1])
		}
	}
	for _, need := range []string{`min_herdr_version = "0.9.2"`, `placement = "overlay"`, `placement = "split"`, `platforms = ["linux", "macos"]`, `contexts = ["workspace"]`} {
		if !strings.Contains(m, need) {
			t.Errorf("manifest lacks %s", need)
		}
	}
}

func herdrClientFixture(t *testing.T) (bin, cfg string) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return buildAndAuth(t, srv)
}

func TestHerdrInstallShowsAsksThenLinksOnly(t *testing.T) {
	bin, cfg := herdrClientFixture(t)
	dir, _, log := fakeHerdrCLI(t)
	env := append(append(fastEnv(cfg), noHerdrEnv()...), "PATH="+dir+":"+os.Getenv("PATH"))
	pluginDir := filepath.Join(cfg, "keepstate", "herdr-plugin")
	manifest := filepath.Join(pluginDir, "herdr-plugin.toml")

	// without a yes nothing is written or linked: --no-input, and no terminal
	for _, args := range [][]string{{"herdr", "install", "--no-input"}, {"herdr", "install"}} {
		stdout, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, args...)
		if code != exitUsage || !strings.Contains(errs, "Next: ks herdr install --yes") || !strings.Contains(stdout, `id = "keepstate"`) {
			t.Errorf("%v: exit %d\n%s%s", args, code, stdout, errs)
		}
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatal("a refused install wrote the manifest")
	}
	// a refused install only read herdr's plugin list
	for _, c := range herdrCalls(t, log) {
		if c != herdrListCall {
			t.Fatalf("a refused install ran herdr: %v", c)
		}
	}
	_ = os.Remove(log)

	stdout, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "herdr", "install", "--yes")
	if code != 0 {
		t.Fatalf("install: exit %d\n%s%s", code, stdout, errs)
	}
	if calls := herdrCalls(t, log); len(calls) != 2 || calls[0] != herdrListCall || calls[1] != "|plugin|link|"+pluginDir {
		t.Errorf("herdr calls %v", calls)
	}
	b, err := os.ReadFile(manifest)
	if err != nil || string(b) != herdrManifest("dev") {
		t.Errorf("written manifest (%v):\n%s", err, b)
	}
	if !strings.Contains(stdout, "This writes "+manifest) || !strings.Contains(stdout, "plugin link "+pluginDir) || !strings.Contains(stdout, "herdr plugin pane open --plugin keepstate --entrypoint agents") {
		t.Errorf("install output:\n%s", stdout)
	}
	if fi, err := os.Stat(manifest); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("manifest mode %v", fi.Mode())
	}

	// status: the herdr found, its version, and the manifest is this client's
	out := runOKEnv(t, bin, cfg, env, "herdr", "status")
	if !strings.Contains(out, "(herdr 0.9.5); the plugin needs 0.9.2 or later") || !strings.Contains(out, "(this client's)") {
		t.Errorf("status:\n%s", out)
	}

	// herdr refuses the link: its words, and the next step
	if err := os.WriteFile(filepath.Join(dir, "fail"), []byte("error: plugin requires herdr 9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code = auditExec(t, bin, cfg, t.TempDir(), env, "herdr", "install", "--yes")
	if code != exitFailed || !strings.Contains(errs, "herdr did not link it: error: plugin requires herdr 9.9.9") || !strings.Contains(errs, "Next: ks herdr status") {
		t.Errorf("refused link: exit %d\n%s", code, errs)
	}
	_ = os.Remove(filepath.Join(dir, "fail"))

	// uninstall: unlink, and the files install wrote are gone
	_ = os.Remove(log)
	out = runOKEnv(t, bin, cfg, env, "herdr", "uninstall")
	if calls := herdrCalls(t, log); len(calls) != 2 || calls[0] != herdrListCall || calls[1] != "|plugin|unlink|keepstate" {
		t.Errorf("uninstall calls %v", calls)
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) || !strings.Contains(out, "Unlinked") {
		t.Errorf("plugin folder left (%v):\n%s", err, out)
	}

	// herdr says what it removed: a plugin that was not linked is not
	// reported as unlinked (herdr 0.9.5's own answers, exit 0 both times)
	for _, tc := range []struct {
		said, words string
		unlinked    bool
	}{
		{`{"id":"cli:plugin","result":{"plugin_id":"keepstate","removed":false,"type":"plugin_unlinked"}}`, "The KeepState plugin was not linked in herdr, so nothing was unlinked.", false},
		{`{"id":"cli:plugin","result":{"plugin_id":"keepstate","removed":true,"type":"plugin_unlinked"}}`, "Unlinked the KeepState plugin from herdr.", true},
	} {
		if err := os.WriteFile(filepath.Join(dir, "unlink.out"), []byte(tc.said+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out = runOKEnv(t, bin, cfg, env, "herdr", "uninstall")
		if !strings.Contains(out, tc.words) || (!tc.unlinked && strings.Contains(out, "Unlinked")) {
			t.Errorf("herdr said %s:\n%s", tc.said, out)
		}
		js := runOKEnv(t, bin, cfg, env, "herdr", "uninstall", "--json")
		if !strings.Contains(js, fmt.Sprintf(`"unlinked":%v`, tc.unlinked)) && !strings.Contains(js, fmt.Sprintf(`"unlinked": %v`, tc.unlinked)) {
			t.Errorf("--json after %s:\n%s", tc.said, js)
		}
	}
}

func runOKEnv(t *testing.T, bin, cfg string, env []string, args ...string) string {
	t.Helper()
	stdout, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d\n%s%s", args, code, stdout, errs)
	}
	return stdout + errs
}

func TestHerdrInstallWithoutHerdrLinksNothing(t *testing.T) {
	bin, cfg := herdrClientFixture(t)
	env := append(append(fastEnv(cfg), noHerdrEnv()...), "PATH="+t.TempDir())
	_, errs, code := auditExec(t, bin, cfg, t.TempDir(), env, "herdr", "install", "--yes")
	if code != exitFailed || !strings.Contains(errs, "herdr is not installed on this machine; nothing was linked") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(cfg, "keepstate", "herdr-plugin")); !os.IsNotExist(err) {
		t.Error("files written without herdr")
	}
	out := runOKEnv(t, bin, cfg, env, "herdr", "status")
	if !strings.Contains(out, "herdr          not installed on this machine") || !strings.Contains(out, "plugin         not installed: ks herdr install") {
		t.Errorf("status:\n%s", out)
	}
}

func TestOpeningAnAgentInAHerdrTabIsTabCreateThenPaneRun(t *testing.T) {
	dir, bin, log := fakeHerdrCLI(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	pane, err := openAgentInHerdrTab(bin, "main", "3f2a1c")
	if err != nil || pane != "w1:p9" {
		t.Fatalf("pane %q, %v", pane, err)
	}
	// the tab starts in the home folder, never in the plugin's folder (the
	// pane's own, which uninstall removes and herdr then cannot restore)
	want := []string{"|tab|create|--label|main|--cwd|" + home + "|--focus", "|pane|run|w1:p9|ks agent open main --session 3f2a1c"}
	if calls := herdrCalls(t, log); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls\n%v\nwant\n%v", calls, want)
	}
	// a name that is not safe to type is never typed
	_ = os.Remove(log)
	if _, err := openAgentInHerdrTab(bin, "x; rm -rf ~", "3f2a1c"); err == nil || len(herdrCalls(t, log)) != 0 {
		t.Errorf("an unsafe name reached herdr: %v %v", err, herdrCalls(t, log))
	}
	// a tab whose pane herdr does not name: nothing is typed anywhere
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"|$1|$2\" >> "+log+"\necho '{\"result\":{}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(log)
	if _, err := openAgentInHerdrTab(bin, "main", "3f2a1c"); err == nil || len(herdrCalls(t, log)) != 1 {
		t.Errorf("no pane id: %v %v", err, herdrCalls(t, log))
	}
	_ = dir
}

func TestReceiptsNoticeAJobFinishingOnceAndNeverAtTheFirstRead(t *testing.T) {
	f := newReceiptsFollow()
	read := func(states ...string) []string {
		var jobs []paneJob
		for i := 0; i+1 < len(states); i += 2 {
			jobs = append(jobs, paneJob{ID: states[i], State: states[i+1]})
		}
		return f.finished(jobs)
	}
	if got := read("job_old", "accepted", "job_r", "running"); len(got) != 0 {
		t.Errorf("first read: %v", got)
	}
	if got := read("job_old", "accepted", "job_r", "verifying"); len(got) != 0 {
		t.Errorf("still going: %v", got)
	}
	if got := read("job_old", "accepted", "job_r", "accepted", "job_q", "failed"); strings.Join(got, ",") != "job_r,job_q" {
		t.Errorf("finished: %v", got)
	}
	if got := read("job_old", "accepted", "job_r", "accepted", "job_q", "failed"); len(got) != 0 {
		t.Errorf("again: %v", got)
	}
}

// drivePane runs a pane in a pseudo-terminal with the plan's keys and
// answers the screen's bytes. With KS_SNAPSHOT_OUT set the steps' snapshots
// are kept there for review.
func drivePane(t *testing.T, bin string, env map[string]string, steps [][]string, rows, cols int, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 drives the pseudo-terminal")
	}
	driver, _ := filepath.Abs("testdata/ptydrive.py")
	outdir := os.Getenv("KS_SNAPSHOT_OUT")
	if outdir == "" {
		outdir = t.TempDir()
	}
	env["TERM"], env["LANG"], env["KS_THEME"], env["COLORTERM"] = "xterm-256color", "en_US.UTF-8", "dark", "truecolor"
	spec := map[string]any{"argv": append([]string{bin}, args...), "env": env, "timeout": 15, "rows": rows, "cols": cols, "steps": steps}
	f := filepath.Join(t.TempDir(), "spec.json")
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureBarrier()
	out, err := exec.Command("python3", driver, f, outdir).CombinedOutput()
	fixtureBarrier()
	if err != nil {
		steps := ""
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "@@ STEP") {
				steps += l + "\n"
			}
		}
		t.Fatalf("pane %v: %v\n%s", args, err, steps)
	}
	return string(out)
}

func TestTheAgentsPaneOpensAnAgentInANewTabTheConsoleAndSavedPoints(t *testing.T) {
	c := newSnapCtl()
	syncFixture(t, &c.agentCtl.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir, hbin, log := fakeHerdrCLI(t)
	opened := fakeOpener(t, dir)
	home := t.TempDir()
	env := map[string]string{"XDG_CONFIG_HOME": cfg, "KS_HTTP_TIMEOUT_MS": "4000", "HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p1", "HERDR_BIN_PATH": hbin,
		"PATH": dir + ":" + os.Getenv("PATH"), "HOME": home}
	screen := drivePane(t, bin, env, [][]string{
		{"Needs you", "", "wait:0.6", "snap:herdr-01-agents"},
		{"", "\r", "wait:0.8"},
		{"Opened main in a new tab", "", "snap:herdr-02-opened"},
		{"", "c", "wait:0.6"},
		{"Opened the console", "", "new"},
		{"", "s", "wait:0.8"},
		{"Saved points", "", "new", "snap:herdr-03-saved"},
		{"", "\x1b", "wait:0.5"},
		{"", "q", "wait:0.5"},
	}, 34, 110, "herdr", "pane", "agents")
	for _, want := range []string{"open in a new tab", "Needs you: permission", "ck_9b01", "not usable now"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the pane never showed %q", want)
		}
	}
	calls := herdrCalls(t, log)
	want := []string{"|tab|create|--label|main|--cwd|" + home + "|--focus", "|pane|run|w1:p9|ks agent open main --session 3f2a1c"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("herdr calls\n%v\nwant\n%v", calls, want)
	}
	if b, _ := os.ReadFile(opened); strings.TrimSpace(string(b)) != srv.URL+"/console/agents#agent/"+agentSessionRecord+"/agt_main0001" {
		t.Errorf("opened %q", b)
	}
}

// jobsCtl serves a job list and each job's status: job_r moves through
// states (one per status read), job_old finished long ago.
type jobsCtl struct {
	mu     sync.Mutex
	states []string
	reads  int
}

func (c *jobsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	cur := c.states[min(c.reads, len(c.states)-1)]
	switch r.URL.Path {
	case "/api/jobs":
		_ = enc.Encode([]map[string]any{
			{"id": "job_old", "state": "accepted", "goal": "fix the rounding", "created_at": "2026-11-02T10:00:00Z"},
			{"id": "job_r", "state": cur, "goal": "make the inventory tests pass", "created_at": "2026-11-03T10:00:00Z"},
		})
	case "/api/jobs/job_r/status":
		c.reads++
		_ = enc.Encode(receiptStatus(cur, nil, ""))
	case "/api/jobs/job_old/status":
		st := receiptStatus("accepted", nil, "")
		st["job_id"] = "job_old"
		_ = enc.Encode(st)
	default:
		w.WriteHeader(404)
		_ = enc.Encode(map[string]any{"error": map[string]any{"type": "not_found", "message": "no such route"}})
	}
}

func TestTheReceiptsPaneFollowsAJobToItsReceiptAndNotifiesOnce(t *testing.T) {
	c := &jobsCtl{states: []string{"running", "verifying", "verifying", "accepted"}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	_, hbin, log := fakeHerdrCLI(t)
	env := map[string]string{"XDG_CONFIG_HOME": cfg, "KS_HTTP_TIMEOUT_MS": "4000", "KS_WATCH_SECOND_MS": "150", "KS_STATUS_OSC": "on",
		"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p2", "HERDR_BIN_PATH": hbin}
	screen := drivePane(t, bin, env, [][]string{
		{"KeepState receipts", "", "wait:0.2", "snap:herdr-04-receipts"},
		{"Receipt: Accepted", "", "wait:0.3", "snap:herdr-05-receipt"},
		{"none yet; KeepState opens pull requests once GitHub is connected", "", "wait:0.2"},
		{"", "\x1b", "wait:0.4"},
		{"", "\x1b[B\r", "wait:0.6"},
		{"ks cruise apply job_old", "", "new", "snap:herdr-06-old-receipt"},
		{"", "q", "wait:0.6"},
	}, 34, 110, "herdr", "pane", "receipts")
	for _, want := range []string{"Download the files: ks cruise result job_r --download", "Get the change now: ks cruise apply job_r", "Working on it"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the pane never showed %q", want)
		}
	}
	var notes []string
	for _, l := range herdrCalls(t, log) {
		if strings.HasPrefix(l, "|notification|show|") {
			notes = append(notes, l)
		}
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "|notification|show|Cruise job job_r finished|--body|Accepted: verified.|") || !strings.HasSuffix(notes[0], "|--sound|done") {
		t.Errorf("notifications %v", notes)
	}
	// terminal status per job id, and cleared when the pane closes
	got := refDecode(t, screen)
	var forR []string
	for _, d := range got {
		if d.ID == "job_r" {
			forR = append(forR, d.State)
		}
	}
	ok := len(forR) >= 2 && forR[len(forR)-1] == "done"
	for _, s := range forR[:max(0, len(forR)-1)] {
		ok = ok && s == "working"
	}
	if !ok || got[len(got)-1].State != "clear" || got[len(got)-1].ID != "" {
		t.Errorf("status records %+v", got)
	}
}

func TestThePanesJobCardsSayWhatHappenedAndTheOneNextStep(t *testing.T) {
	jobCard := func(state string, pr any, verdict string) card {
		raw, _ := json.Marshal(receiptStatus(state, pr, verdict))
		var st jobLiveStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		return paneJobCard(st)
	}
	text := func(c card) string { return strings.Join(c.render(110), "\n") }
	if c := jobCard("failed", nil, "the checks failed on every rung"); c.head != "Failed: the checks failed on every rung" || !strings.Contains(text(c), "Read why: ks cruise logs job_r") {
		t.Errorf("failed:\n%s", text(c))
	}
	if c := jobCard("review", nil, ""); !strings.Contains(text(c), "Review it: ks cruise review job_r") || c.tone != toneWarn {
		t.Errorf("review:\n%s", text(c))
	}
	if c := jobCard("cancelled", nil, ""); !strings.Contains(c.head, "Cancelled") || strings.Contains(text(c), "Pull request") {
		t.Errorf("cancelled:\n%s", text(c))
	}
	c := jobCard("accepted", map[string]any{"url": "https://github.com/acme/shop/pull/12", "number": 12, "state": "open"}, "")
	if s := text(c); !strings.Contains(s, "#12: https://github.com/acme/shop/pull/12 (open)") || strings.Contains(s, "none yet") || strings.Contains(s, "cruise apply") {
		t.Errorf("served pull request:\n%s", s)
	}
	c = jobCard("accepted", map[string]any{"url": "javascript:alert(1)", "number": 3, "state": "open"}, "")
	if s := text(c); strings.Contains(s, "javascript") {
		t.Errorf("unsafe link shown:\n%s", s)
	}
}
