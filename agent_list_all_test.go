// ks agent list --all: every agent across your sessions in the console
// rail's words, what needs you first; --watch writes terminal status per
// agent and for all of them; --notify sends one notification per change
// into needing you or failing, never for the first read.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

type listCtl struct {
	*agentCtl
	mu    sync.Mutex
	lists [][]map[string]any // served in order, one per read; the last repeats
	reads int
	none  bool // an older control plane: no such route
}

func (c *listCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v2/agent-status" {
		c.agentCtl.ServeHTTP(w, r)
		return
	}
	c.mu.Lock()
	i := c.reads
	if i >= len(c.lists) {
		i = len(c.lists) - 1
	}
	c.reads++
	items, none := c.lists[i], c.none
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if none {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"schema_version":2,"error":{"type":"not_found","message":"no such route"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "data": map[string]any{"items": items, "counts": map[string]any{}, "truncated": false}})
}

func srec(id, sess, name, sessName, state, kind, msg string) map[string]any {
	r := map[string]any{"agent_id": id, "session_id": sess, "name": name, "session_name": sessName, "state": state, "reason": "r",
		"msg": msg, "needs_you": state == "blocked" || state == "error", "source": "keepstate"}
	if kind != "" {
		r["kind"] = kind
	}
	if state == "blocked" {
		r["action"] = map[string]any{"label": "Approve or deny", "verb": "ks agent approve apr_1 --session " + sess, "console": "#agent/" + sess + "/" + id}
	}
	return r
}

// three agents across two sessions, served in an order the list must fix
var threeAgents = []map[string]any{
	srec("agt_b", "ses_two", "build", "web", "working", "", "Running a tool: shell."),
	srec("agt_r", "ses_one", "review", "api", "idle", "", "Ready."),
	srec("agt_c", "ses_one", "checkout", "api", "blocked", "permission", "shell: npm test"),
}

func listFixture(t *testing.T, lists ...[]map[string]any) (*listCtl, string, string) {
	t.Helper()
	c := &listCtl{agentCtl: newAgentCtl(), lists: lists}
	syncFixture(t, &c.agentCtl.mu)
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	bin, cfg := buildAndAuth(t, srv)
	return c, bin, cfg
}

func TestAgentListAllShowsWhatNeedsYouFirstInTheRailsWords(t *testing.T) {
	_, bin, cfg := listFixture(t, threeAgents)
	out := runOK(t, bin, cfg, "agent", "list", "--all")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "1 needs you \u00b7 1 working \u00b7 1 idle" {
		t.Errorf("first line %q", lines[0])
	}
	var rows []string
	for _, l := range lines {
		for _, w := range []string{"Needs you: permission", "Working ", "Idle "} {
			if strings.HasPrefix(l, w) {
				rows = append(rows, l)
			}
		}
	}
	if len(rows) != 3 || !strings.Contains(rows[0], "checkout") || !strings.Contains(rows[1], "build") || !strings.Contains(rows[2], "review") {
		t.Fatalf("rows out of order:\n%s", out)
	}
	for _, want := range []string{"api ", "web ", "shell: npm test", "    next: Approve or deny: ks agent approve apr_1 --session ses_one", "open one: ks agent open checkout --session ses_one"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape bytes in a one-shot list: %q", out)
	}
	js := runOK(t, bin, cfg, "agent", "list", "--all", "--json")
	var doc struct {
		Data struct {
			Items  []statusRecord    `json:"items"`
			Counts agentStatusCounts `json:"counts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil || len(doc.Data.Items) != 3 || doc.Data.Items[0].Name != "checkout" || doc.Data.Counts.NeedsYou != 1 || doc.Data.Counts.Working != 1 {
		t.Errorf("json %v: %s", err, js)
	}
}

func TestStatusWordsAreTheRails(t *testing.T) {
	for _, tc := range []struct{ state, kind, want string }{
		{"working", "", "Working"}, {"blocked", "permission", "Needs you: permission"}, {"blocked", "question", "Needs you: a question"},
		{"blocked", "auth", "Needs you: sign-in or key"}, {"blocked", "", "Needs you"}, {"done", "", "Done"}, {"error", "", "Error"},
		{"idle", "", "Idle"}, {"bogus", "", "Status unavailable"},
	} {
		if got := statusWord(statusRecord{State: tc.state, Kind: tc.kind}); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.state, tc.kind, got, tc.want)
		}
	}
	for _, tc := range []struct {
		c    agentStatusCounts
		want string
	}{
		{agentStatusCounts{}, "Nothing needs you"},
		{agentStatusCounts{NeedsYou: 2, Working: 1, Error: 1}, "2 need you \u00b7 1 working \u00b7 1 failed"},
	} {
		if got := needYouLine(tc.c); got != tc.want {
			t.Errorf("%+v: %q", tc.c, got)
		}
	}
}

func TestAgentListAllRefusesWhatDoesNotGoTogether(t *testing.T) {
	_, bin, cfg := listFixture(t, threeAgents)
	for _, args := range [][]string{{"agent", "list", "--all", "--session", "ses_one"}, {"agent", "list", "--watch", "--session", "ses_one"}, {"agent", "list", "--notify", "--session", "ses_one"}} {
		_, errs, code := auditExec(t, bin, cfg, t.TempDir(), fastEnv(cfg), args...)
		if code != exitUsage || !strings.Contains(errs, "Next: ks agent list --all") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
	}
	c2, bin2, cfg2 := listFixture(t, threeAgents)
	c2.none = true
	_, errs, code := auditExec(t, bin2, cfg2, t.TempDir(), fastEnv(cfg2), "agent", "list", "--all")
	if code != exitFailed || !strings.Contains(errs, "does not serve the status of every agent") {
		t.Errorf("older control plane: exit %d\n%s", code, errs)
	}
}

// watchAll runs ks agent list --all --watch until the fake served n reads,
// then Ctrl-C, and answers stdout.
func watchAll(t *testing.T, c *listCtl, bin, cfg string, n int, extra ...string) string {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"agent", "list", "--all", "--watch"}, extra...)...)
	cmd.Env = append(os.Environ(), append(fastEnv(cfg), "KS_STATUS_OSC=on", "KS_WATCH_SECOND_MS=40")...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the watch to read the list", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.reads > n
	})
	_ = cmd.Process.Signal(os.Interrupt)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("watch exited %v\n%s", err, se.String())
	}
	return so.String()
}

func TestAgentListAllWatchWritesEachAgentAndTheWhole(t *testing.T) {
	later := []map[string]any{
		srec("agt_b", "ses_two", "build", "web", "done", "", "Finished instruction tsk_9."),
		srec("agt_c", "ses_one", "checkout", "api", "blocked", "permission", "shell: npm test"),
	}
	c, bin, cfg := listFixture(t, threeAgents, threeAgents, later)
	out := watchAll(t, c, bin, cfg, 3)
	got := refDecode(t, out)
	byID := map[string][]decoded{}
	for _, d := range got {
		byID[d.ID] = append(byID[d.ID], d)
	}
	if b := byID["agt_b"]; len(b) != 2 || b[0].State != "working" || b[1].State != "done" || b[0].Title != "build \u00b7 web" {
		t.Errorf("build's records %+v", b)
	}
	if ch := byID["agt_c"]; len(ch) != 1 || ch[0].State != "blocked" || ch[0].Kind != "permission" {
		t.Errorf("checkout's records %+v (an unchanged record is written once)", ch)
	}
	if r := byID["agt_r"]; len(r) != 2 || r[0].State != "idle" || r[1].State != "clear" {
		t.Errorf("review, which left the list, %+v", r)
	}
	root := byID[""]
	if len(root) < 2 || root[0].State != "blocked" || root[0].Kind != "permission" || root[0].Msg != "1 needs you \u00b7 1 working \u00b7 1 idle" ||
		root[len(root)-1].State != "clear" {
		t.Errorf("the whole %+v", root)
	}
	if strings.Contains(out, "\x1b]9;") {
		t.Errorf("a notification without --notify: %q", out)
	}
}

func TestAgentListAllNotifyFiresOncePerChangeIntoNeedingYou(t *testing.T) {
	calm := []map[string]any{
		srec("agt_b", "ses_two", "build", "web", "working", "", "Working."),
		srec("agt_c", "ses_one", "checkout", "api", "blocked", "permission", "shell: npm test"), // already waiting at the first read
	}
	stuck := []map[string]any{
		srec("agt_b", "ses_two", "build", "web", "blocked", "question", "Held: the last instruction failed."),
		srec("agt_c", "ses_one", "checkout", "api", "blocked", "permission", "shell: npm test"),
	}
	failed := []map[string]any{
		srec("agt_b", "ses_two", "build", "web", "error", "", "The runner failed."),
		srec("agt_c", "ses_one", "checkout", "api", "blocked", "permission", "shell: npm test"),
		srec("agt_n", "ses_two", "new", "web", "done", "", "Finished."),
	}
	hbin, log, _ := fakeHerdr(t)
	c, bin, cfg := listFixture(t, calm, stuck, stuck, failed, failed)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "p_7")
	t.Setenv("HERDR_BIN_PATH", hbin)
	out := watchAll(t, c, bin, cfg, 5, "--notify")
	var notes []string
	for _, part := range strings.Split(out, "\x1b]9;")[1:] {
		text, rest, ok := strings.Cut(part, "\x1b\\")
		if !ok || !strings.HasPrefix(rest, "\a") {
			t.Fatalf("notification not terminated with ST and a bell: %q", part)
		}
		notes = append(notes, text)
	}
	want := []string{"KeepState: build needs you: a question. Held: the last instruction failed.", "KeepState: build failed. The runner failed."}
	if fmt.Sprint(notes) != fmt.Sprint(want) {
		t.Errorf("notifications %q, want %q", notes, want)
	}
	waitUntil(t, "herdr's notifications", func() bool {
		n := 0
		for _, l := range herdrCalls(t, log) {
			if strings.HasPrefix(l, "|notification|show|") {
				n++
			}
		}
		return n == 2
	})
	for _, l := range herdrCalls(t, log) {
		if strings.HasPrefix(l, "|notification|show|") && (!strings.Contains(l, "|--sound|request") || !strings.HasPrefix(l, "|notification|show|KeepState: build|--body|KeepState: build ")) {
			t.Errorf("herdr notification %s", l)
		}
	}
}
