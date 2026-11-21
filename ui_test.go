package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The component kit's golden tests: every component rendered at 80, 60 and 40
// columns, in colour (a true-colour dark theme, escapes shown as <ESC>) and
// plain. KS_UPDATE_GOLDEN=1 rewrites them.

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock           { return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }
func testUI(buf *bytes.Buffer, width int, themed bool, clk *fakeClock) *ui {
	t := appTheme{}
	if themed {
		t = appTheme{depth: colorTrue}
	}
	u := newUI(buf, t, width, false)
	u.animate = false
	u.now = clk.now
	return u
}

// kitSample draws every component once: a transfer, a list, a refusal.
func kitSample(u *ui, clk *fakeClock) {
	u.Title(iconDone, toneOK, "Imported into session 3f2a1c")
	u.Facts(
		[2]string{"From", "Claude Code session 2d1eb651 · ~/work/BulkPulse"},
		[2]string{"Size", "137 MB after cleanup (141 MB read)"},
		[2]string{"Messages", "1,204 of yours · 9,881 records"},
		[2]string{"Goes up as", "the conversation itself"},
	)
	u.Blank()
	u.Title("", toneNeutral, "Sessions on this machine", "3 found")
	u.Table([]Col{{Title: "TOOL"}, {Title: "SESSION"}, {Title: "PROJECT", Flex: true}, {Title: "LAST USED", Second: true}, {Title: "SIZE", Right: true, Second: true}}, [][]string{
		{"claude-code", "2d1eb651", "~/work/projects/BulkPulse-main", "open now", "137 MB"},
		{"claude-code", "a91f03c2", "~/keepstate", "2 hours ago", "48 MB"},
		{"codex", "77e1b0aa", "~/ks-ops", "yesterday", "3.1 MB"},
	})
	u.Blank()
	u.Note("The agent's first reply re-reads the whole conversation, so it costs more than a short message.")
	u.Blank()
	u.Warn("This session is open in Claude Code right now", "It was written 4 seconds ago. The import takes it as of now; later turns stay on this machine.")
	u.Blank()
	st := u.Steps()
	st.Start("Reading the session")
	clk.add(800 * time.Millisecond)
	st.Done("Read the session", "")
	st.Start("Cleaning the conversation")
	clk.add(2100 * time.Millisecond)
	st.Done("Cleaned 137 MB", "3 secrets replaced")
	st.Start("Uploading the conversation")
	clk.add(12 * time.Second)
	st.Fail("Upload stopped at 44%", "")
	st.Skip("Hand it to the agent", "not done")
	u.Blank()
	b := u.Progress("Uploading the conversation", 137_000_000)
	for i := 1; i <= 10; i++ {
		clk.add(1700 * time.Millisecond)
		b.Set(int64(i) * 13_700_000)
	}
	b.Done("Uploaded the conversation")
	u.Blank()
	u.Success("Imported into session 3f2a1c", [][2]string{{"Agent", "main (claude-code)"}, {"Secrets", "3 replaced before upload"}}, "ks agent open main --session 3f2a1c")
	u.Blank()
	u.NextWhy("ks agent open main --session 3f2a1c", "Opens the agent's window. Its first reply re-reads the whole conversation.")
	u.Blank()
	u.Next("ks import list --cursor", "ks session list")
	u.Blank()
	u.Next("ks session show 2d1eb651 --json --quiet --plain --no-input --wait-timeout 120s --yes --no-color")
}

// errorSamples are the K10 variants.
func errorSamples(u *ui) {
	renderError(u, &cliError{Code: exitConflict, Kind: "import_too_large", WorkStarted: workNo,
		Message:    "this session is 2.4 GB after cleanup; the limit is 2 GB",
		NextAction: "ks session show 2d1eb651"})
	u.Blank()
	renderError(u, &cliError{Code: exitUsage, Kind: "usage", WorkStarted: workNo,
		Title: "Unknown option --buget", Why: []string{"Did you mean --budget-tokens?"}, NextAction: "ks run --help"})
	u.Blank()
	renderError(u, &cliError{Code: exitTemporary, Kind: "unreachable", WorkStarted: workUnknown, OperationID: "op_7Hc2",
		Message: "KeepState stopped answering after the upload. It may have applied it"})
	u.Blank()
	renderError(u, &cliError{Code: exitFailed, Kind: "failed", WorkStarted: workYes,
		Title: "Can't finish the import", Why: []string{"Session 3f2a1c and the upload are in place, but KeepState didn't apply it."},
		NextAction: "ks session show 3f2a1c"})
}

// barSample: the running states, which a terminal redraws.
func barSamples(u *ui, clk *fakeClock) {
	b := u.Progress("Uploading the conversation", 137_000_000)
	clk.add(3 * time.Second)
	b.Set(61_200_000)
	for _, l := range b.barLines() {
		fmt.Fprintln(u.w, l)
	}
	fmt.Fprintln(u.w, u.spinnerLine("Starting the machine", 3, 12*time.Second))
	fmt.Fprintln(u.w, u.spinnerLine("Starting the machine", 3, time.Second))
}

func stripEsc(s string) string { return strings.ReplaceAll(s, "\x1b", "<ESC>") }

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", "golden", "ui", name+".txt")
	if os.Getenv("KS_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: no golden file (KS_UPDATE_GOLDEN=1 writes it): %v", name, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from its golden output\n--- want ---\n%s--- got ---\n%s", name, want, got)
	}
}

func TestKitGoldens(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		for _, themed := range []bool{true, false} {
			mode := "plain"
			if themed {
				mode = "color"
			}
			var buf bytes.Buffer
			clk := newFakeClock()
			u := testUI(&buf, width, themed, clk)
			kitSample(u, clk)
			checkGolden(t, fmt.Sprintf("kit-%d-%s", width, mode), stripEsc(buf.String()))

			buf.Reset()
			errorSamples(u)
			checkGolden(t, fmt.Sprintf("errors-%d-%s", width, mode), stripEsc(buf.String()))

			buf.Reset()
			barSamples(u, newFakeClock())
			clk2 := newFakeClock()
			u.now = clk2.now
			checkGolden(t, fmt.Sprintf("motion-%d-%s", width, mode), stripEsc(buf.String()))
		}
	}
}

// The uncoloured goldens are what --plain and NO_COLOR give, and they carry
// no escape at all.
func TestKitPlainHasNoEscapes(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		var buf bytes.Buffer
		clk := newFakeClock()
		u := testUI(&buf, width, false, clk)
		kitSample(u, clk)
		errorSamples(u)
		if strings.Contains(buf.String(), "\x1b") {
			t.Errorf("width %d: plain output holds an escape", width)
		}
	}
}

// Nothing the kit draws is wider than the terminal, at any of the widths,
// except a command in a Next line (never cut).
func TestKitFitsTheWidth(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		for _, themed := range []bool{true, false} {
			var buf bytes.Buffer
			clk := newFakeClock()
			u := testUI(&buf, width, themed, clk)
			kitSample(u, clk)
			errorSamples(u)
			for _, l := range strings.Split(buf.String(), "\n") {
				if displayWidth(l) > width && !strings.Contains(l, "ks ") {
					t.Errorf("width %d: %q is %d columns", width, l, displayWidth(l))
				}
			}
		}
	}
}

func TestCutMiddleKeepsBothEnds(t *testing.T) {
	got := cutMiddle("~/work/projects/BulkPulse-main", 20)
	if displayWidth(got) > 20 || !strings.HasPrefix(got, "~/wo") || !strings.HasSuffix(got, "-main") || !strings.Contains(got, "…") {
		t.Errorf("cutMiddle = %q", got)
	}
	if cutMiddle("short", 20) != "short" {
		t.Error("a short value was cut")
	}
}

func TestKitBytesAndDurations(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 999: "999 B", 3_100_000: "3.1 MB", 61_200_000: "61.2 MB", 137_000_000: "137 MB", 2_400_000_000: "2.4 GB"} {
		if got := kitBytes(in); got != want {
			t.Errorf("kitBytes(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{800 * time.Millisecond: "0.8s", 14 * time.Second: "14s", 65 * time.Second: "1m05s"} {
		if got := kitDuration(in); got != want {
			t.Errorf("kitDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

// Under 40 columns a transfer has no bar, only its numbers; time left needs
// two seconds of data.
func TestBarNarrowAndTimeLeft(t *testing.T) {
	var buf bytes.Buffer
	clk := newFakeClock()
	u := testUI(&buf, 36, false, clk)
	b := u.Progress("Uploading", 137_000_000)
	clk.add(time.Second)
	b.Set(10_000_000)
	if _, _, ok := b.rate(); ok {
		t.Error("time left shown after one second")
	}
	clk.add(2 * time.Second)
	b.Set(30_000_000)
	if _, left, ok := b.rate(); !ok || left <= 0 {
		t.Errorf("no time left after three seconds (%v, %v)", left, ok)
	}
	ls := b.barLines()
	if len(ls) != 1 || strings.Contains(ls[0], "━") || !strings.Contains(ls[0], "21%") {
		t.Errorf("narrow bar = %q", ls)
	}
}

func TestStepsAndBarPrintNoRedrawsWithoutAMotionTerminal(t *testing.T) {
	var buf bytes.Buffer
	clk := newFakeClock()
	u := testUI(&buf, 80, true, clk)
	kitSample(u, clk)
	if strings.ContainsAny(buf.String(), "\r") || strings.Contains(buf.String(), "[2K") {
		t.Error("a stream with no motion got a redraw")
	}
	if !strings.Contains(buf.String(), "uploading the conversation: 30% (41.1 MB of 137 MB)") {
		t.Errorf("no line at 30%%:\n%s", buf.String())
	}
}

// The confirm prompt: --yes answers, no terminal refuses with exit 2, no or an
// empty answer to [y/N] declines with the command's own line.
func TestConfirm(t *testing.T) {
	ask := func(input string, def bool, tty bool, yes, noInput bool) (bool, error, string) {
		saved := out
		defer func() { out = saved }()
		out.yes, out.noInput = yes, noInput
		var buf bytes.Buffer
		u := testUI(&buf, 80, false, newFakeClock())
		u.in, u.inTTY = bufio.NewReader(strings.NewReader(input)), tty
		ok, err := u.Confirm("Import this session?", def, "--yes", "Nothing was created.")
		return ok, err, buf.String()
	}
	if ok, err, _ := ask("", false, false, true, false); !ok || err != nil {
		t.Error("--yes did not answer")
	}
	ok, err, _ := ask("y\n", false, false, false, false)
	ce, _ := err.(*cliError)
	if ok || ce == nil || ce.Code != exitUsage || ce.Kind != "confirmation_unavailable" ||
		ce.Message != "confirmation needed and this terminal can't answer: add --yes. Nothing was created." {
		t.Errorf("no terminal: ok=%v err=%v", ok, err)
	}
	if ok, err, _ := ask("y\n", false, true, false, true); ok || err == nil {
		t.Error("--no-input asked anyway")
	}
	if ok, _, prompt := ask("y\n", false, true, false, false); !ok || !strings.HasPrefix(prompt, "? Import this session? [y/N] ") {
		t.Errorf("yes: ok=%v prompt=%q", ok, prompt)
	}
	if ok, _, _ := ask("\n", true, true, false, false); !ok {
		t.Error("empty answer to [Y/n] was not yes")
	}
	ok, err, shown := ask("\n", false, true, false, false)
	ce, _ = err.(*cliError)
	if ok || ce == nil || ce.Kind != "confirmation_declined" || ce.Code != exitUsage || !strings.Contains(shown, "Nothing was created.") {
		t.Errorf("declined: ok=%v err=%v shown=%q", ok, err, shown)
	}
}

func TestDetectTheme(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	saved := out
	defer func() { out = saved }()
	base := []string{"TERM", "xterm-256color"}
	cases := []struct {
		name string
		env  []string
		tty  bool
		set  func()
		want bool
	}{
		{"terminal", base, true, nil, true},
		{"pipe", base, false, nil, false},
		{"NO_COLOR", append([]string{"NO_COLOR", "1"}, base...), true, nil, false},
		{"KS_COLOR=0", append([]string{"KS_COLOR", "0"}, base...), true, nil, false},
		{"dumb", []string{"TERM", "dumb"}, true, nil, false},
		{"CI", append([]string{"CI", "true"}, base...), true, nil, false},
		{"CI with FORCE_COLOR", append([]string{"CI", "true", "FORCE_COLOR", "1"}, base...), true, nil, true},
		{"FORCE_COLOR in a pipe", append([]string{"FORCE_COLOR", "1"}, base...), false, nil, true},
		{"NO_COLOR beats FORCE_COLOR", append([]string{"NO_COLOR", "1", "FORCE_COLOR", "1"}, base...), false, nil, false},
		{"--no-color", base, true, func() { out.noColor = true }, false},
		{"--plain", base, true, func() { out.plain = true }, false},
		{"--json", base, true, func() { out.json = true }, false},
	}
	for _, c := range cases {
		out = outputSettings{}
		if c.set != nil {
			c.set()
		}
		if got := detectTheme(env(c.env...), c.tty, true).on(); got != c.want {
			t.Errorf("%s: colour %v, want %v", c.name, got, c.want)
		}
	}
	// the app keeps its own rule: a terminal only
	out = outputSettings{}
	if detectTheme(env(append([]string{"FORCE_COLOR", "1"}, base...)...), false, false).on() {
		t.Error("the app coloured a pipe")
	}
}

func TestSplitMessage(t *testing.T) {
	title, why := splitMessage("this session is 2.4 GB after cleanup; the limit is 2 GB; nothing was sent")
	if title != "This session is 2.4 GB after cleanup" || len(why) != 2 || why[0] != "The limit is 2 GB." || why[1] != "Nothing was sent." {
		t.Errorf("%q %q", title, why)
	}
	if title, why := splitMessage("short"); title != "Short" || why != nil {
		t.Errorf("%q %q", title, why)
	}
}

// ---- the text lints --------------------------------------------------------------

var nextCmdLine = regexp.MustCompile(`^\s*(?:Next:|or:)\s+(ks .+)$`)

// splitArgs splits a command line as a shell would, for double quotes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	has := false
	for _, r := range s {
		switch {
		case r == '"':
			in = !in
			has = true
		case r == ' ' && !in:
			if has || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if has || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// nextParses: a suggested command must at least parse. It would have caught
// a Next that failed with a usage error when run as given.
func nextParses(cmdline string) error {
	args := splitArgs(cmdline)[1:] // drop "ks"
	c, rest, _ := lookup(registry, args)
	if c == nil {
		return fmt.Errorf("no such command")
	}
	if _, uerr := c.parse(rest); uerr != nil {
		return fmt.Errorf("%s", uerr.Message)
	}
	return nil
}

var escSeq = regexp.MustCompile(`<ESC>\[[0-9;]*m`)

// humanGoldens: the goldens a person reads (the kit's, the error blocks and
// the existing human snapshots).
func humanGoldens(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pat := range []string{"testdata/golden/ui/*.txt", "testdata/golden/*-human.txt"} {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no human goldens")
	}
	return files
}

func TestHumanGoldensObeyTheTextRules(t *testing.T) {
	for _, f := range humanGoldens(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			plain := escSeq.ReplaceAllString(line, "")
			where := fmt.Sprintf("%s:%d", f, i+1)
			for _, v := range textViolations(plain) {
				t.Errorf("%s: %s: %q", where, v, line)
			}
			if m := nextCmdLine.FindStringSubmatch(plain); m != nil {
				if err := nextParses(m[1]); err != nil {
					t.Errorf("%s: a Next command does not parse (%v): %q", where, err, line)
				}
			}
			// a golden line is at most 80 columns, except a command (never cut)
			if strings.Contains(f, "golden/ui/") && displayWidth(plain) > 80 && !strings.Contains(plain, "ks ") && !strings.HasPrefix(plain, "usage:") {
				t.Errorf("%s: %d columns: %q", where, displayWidth(plain), line)
			}
		}
	}
}

// ---- stream contract -------------------------------------------------------------

// fail() output in plain mode is the contract the app and scripts read: it
// must be byte for byte what it was, whatever colour is asked for on a pipe.
func TestPlainErrorBytesAndJSONBytesIgnoreColour(t *testing.T) {
	srv := goldenServer()
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	run := func(env []string, args ...string) (string, string, int) {
		return auditExec(t, bin, cfg, t.TempDir(), append([]string{"HOME=" + t.TempDir()}, env...), args...)
	}
	for _, args := range [][]string{{"meter", "nope"}, {"meter", "denied"}} {
		var wantOut, wantErr string
		var wantCode int
		for i, env := range [][]string{nil, {"NO_COLOR=1"}, {"FORCE_COLOR=1"}, {"CLICOLOR_FORCE=1", "TERM=xterm-256color"}, {"KS_COLOR=0"}} {
			o, e, c := run(env, args...)
			if i == 0 {
				wantOut, wantErr, wantCode = o, e, c
				if !strings.Contains(e, "error: ") || !strings.Contains(e, "Remote work started: no.") {
					t.Fatalf("%v: the contract lines are gone:\n%s", args, e)
				}
				continue
			}
			if o != wantOut || e != wantErr || c != wantCode {
				t.Errorf("%v with %v differs from the plain run:\n%q\n%q", args, env, e, wantErr)
			}
		}
		// --no-color is accepted and changes nothing on a pipe
		o, e, _ := run(nil, append(args, "--no-color")...)
		if o != wantOut || e != wantErr {
			t.Errorf("%v --no-color changed the bytes", args)
		}
		// --json bytes do not depend on colour or on the stream
		jo, je, _ := run(nil, append(args, "--json")...)
		for _, env := range [][]string{{"NO_COLOR=1"}, {"FORCE_COLOR=1"}, {"CI=true"}} {
			o2, e2, _ := run(env, append(args, "--json")...)
			norm := func(s string) string {
				return strings.ReplaceAll(s, `"request_id":"`+requestIDIn(s)+`"`, `"request_id":"req_…"`)
			}
			if norm(o2) != norm(jo) || e2 != je {
				t.Errorf("%v --json with %v changed", args, env)
			}
		}
	}
}
