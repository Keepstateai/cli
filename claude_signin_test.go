package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The sign-in machine against a scripted service, driven through the binary
// with the code on standard input (the path with no terminal).

const machineCode = "ZXCV-4321-qwer"

func machineArgs(extra ...string) []string {
	return append([]string{"login", "claude", "--machine", "--no-input"}, extra...)
}

func TestMachineSignInFromStartToTheCard(t *testing.T) {
	c := newPlanCtl()
	c.signin = goodSignin()
	bin, _, home, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, machineCode+"\n", machineArgs()...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	starts, codes, cancelled := c.signinState()
	if starts != 1 || len(codes) != 1 || codes[0] != machineCode || cancelled {
		t.Fatalf("starts %d, codes %v, cancelled %v", starts, len(codes), cancelled)
	}
	if n := len(c.posts("/api/v2/keys")); n != 0 {
		t.Errorf("%d token requests went out; the machine keeps the token on the service", n)
	}
	for _, want := range []string{
		"Starting sign-in process, generating sign in URL",
		"Sign in at Claude", signinTestLink, "Signed in to your Claude plan", "…9999", "22 Nov 2027",
	} {
		if !strings.Contains(out+errs, want) {
			t.Errorf("the run never said %q:\n%s\n%s", want, out, errs)
		}
	}
	for _, banned := range []string{"private sign-in machine", "private machine", "No Claude Code needed", "Start a private"} {
		if strings.Contains(out+errs, banned) {
			t.Errorf("the run still says %q:\n%s\n%s", banned, out, errs)
		}
	}
	// the link is on a line of its own, whole
	if !strings.Contains("\n"+errs, "\n"+signinTestLink+"\n") {
		t.Errorf("the link is not alone on its line:\n%s", errs)
	}
	if strings.Contains(out+errs, machineCode) {
		t.Errorf("the code was echoed:\n%s%s", out, errs)
	}
	if hits := treeHoldsText(t, home, machineCode); len(hits) > 0 {
		t.Errorf("the code was written to a file: %v", hits)
	}
	if strings.Contains(strings.ReplaceAll(out+errs, signinTestLink, ""), "%") {
		t.Errorf("a percentage was shown; nothing here knows how far along the work is:\n%s%s", out, errs)
	}
}

func treeHoldsText(t *testing.T, dir, text string) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Size() < 8<<20 {
			if b, e := os.ReadFile(p); e == nil && bytes.Contains(b, []byte(text)) {
				hits = append(hits, p)
			}
		}
		return nil
	})
	return hits
}

func TestMachineSignInUnderAnOrganizationAsksForThatContext(t *testing.T) {
	c := newPlanCtl()
	c.signin = goodSignin()
	bin, _, _, env := planEnv(t, c)
	out, errs, code := runPlan(t, bin, env, machineCode+"\n", machineArgs("--org", "org_acme")...)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if q := c.signinQueries(); len(q) != 1 || q[0] != "context=org_acme" {
		t.Errorf("the sign-in was started with %q", q)
	}
}

func TestMachineSignInEveryFailureIsInPlainWordsWithOneNext(t *testing.T) {
	refused := sview("failed", 0, map[string]any{"words": "Claude did not accept that code. Start again to get a new link, and paste the new code.",
		"error": map[string]any{"code": "ks_signin_code_refused", "words": "Claude did not accept that code. Start again to get a new link, and paste the new code.", "next": "ks login claude"}})
	for _, tc := range []struct {
		name  string
		setup func(s *signinScript)
		code  int
		says  string
	}{
		{"the code was refused", func(s *signinScript) {
			s.views = []map[string]any{sview("starting", 0, nil), sview("link_ready", 1, map[string]any{"url": signinTestLink}), sview("checking", 3, nil), refused}
		}, exitFailed, "did not accept that code"},
		{"it expired", func(s *signinScript) {
			s.views = []map[string]any{sview("starting", 0, nil), sview("expired", 0, nil)}
		}, exitFailed, "took too long"},
		{"it was cancelled elsewhere", func(s *signinScript) {
			s.views = []map[string]any{sview("starting", 0, nil), sview("cancelled", 0, nil)}
		}, exitFailed, "was cancelled"},
		{"the machine could not be made", func(s *signinScript) {
			s.views = []map[string]any{sview("starting", 0, nil), sview("failed", 0, map[string]any{"error": map[string]any{"code": "ks_signin_machine", "words": "The sign-in machine did not start.", "next": "ks login claude"}})}
		}, exitFailed, "machine did not start"},
		{"too many attempts", func(s *signinScript) {
			s.startFail = &planFail{429, "ks_signin_rate", "Too many sign-ins were started in a short time."}
		}, exitTemporary, "too many sign-ins"},
		{"not allowed in this organization", func(s *signinScript) { s.startFail = &planFail{403, "ks_org_forbidden", "no"} }, exitAuth, "does not allow it"},
		{"no machine available", func(s *signinScript) { s.startFail = &planFail{503, "ks_fleet_unavailable", "no fleet"} }, exitTemporary, "could not start a sign-in machine"},
		{"switched off", func(s *signinScript) { s.startFail = &planFail{404, "ks_claude_plan_off", "off"} }, exitFailed, "not available"},
		{"not the shape of a code", func(s *signinScript) {
			s.views = []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink})}
			s.codeFail = &planFail{422, "ks_signin_code_shape", "that is not a code"}
		}, exitUsage, "not the code claude shows"},
		{"a link that is not Claude's", func(s *signinScript) {
			s.views = []map[string]any{sview("link_ready", 1, map[string]any{"url": "https://evil.example/login"})}
		}, exitFailed, "not an https link to claude"},
	} {
		c := newPlanCtl()
		c.signin = &signinScript{}
		tc.setup(c.signin)
		bin, _, _, env := planEnv(t, c)
		out, errs, code := runPlan(t, bin, env, machineCode+"\n", machineArgs()...)
		if code != tc.code || !strings.Contains(strings.ToLower(errs), tc.says) || strings.Count(errs, "Next:") != 1 {
			t.Errorf("%s: exit %d, want %d; says %q?\n%s\n%s", tc.name, code, tc.code, tc.says, out, errs)
		}
		if strings.Contains(out+errs, machineCode) || strings.Contains(out+errs, "evil.example") {
			t.Errorf("%s: what was typed or an untrusted link was printed:\n%s%s", tc.name, out, errs)
		}
		if n := len(c.posts("/api/v2/keys")); n != 0 {
			t.Errorf("%s: %d token requests", tc.name, n)
		}
	}
}

func TestMachineSignInCancelsTheMachineWhenWhatIsTypedIsNotUsable(t *testing.T) {
	for name, in := range map[string]string{"no code": "\n", "a code with a space only": "   \n"} {
		c := newPlanCtl()
		c.signin = &signinScript{views: []map[string]any{sview("link_ready", 1, map[string]any{"url": signinTestLink})}}
		bin, _, _, env := planEnv(t, c)
		_, errs, code := runPlan(t, bin, env, in, machineArgs()...)
		if code != exitUsage || !strings.Contains(errs, "no code was read") || !c.signinWas() {
			t.Errorf("%s: exit %d cancelled=%v\n%s", name, code, c.signinWas(), errs)
		}
	}
}

func TestMachineProgressIsHonestAboutALongWait(t *testing.T) {
	var buf bytes.Buffer
	clk := newFakeClock()
	u := testUI(&buf, 80, false, clk)
	r := &signinRun{ue: u}
	p := &signinProgress{r: r, st: u.Steps(), printed: map[int]bool{}, active: -1}
	m := sview("starting", 0, nil)
	p.show(asView(m))
	m["elapsed_seconds"] = 75
	p.show(asView(m))
	p.show(asView(m))
	got := buf.String()
	if strings.Count(got, "taking longer than usual") != 1 {
		t.Errorf("the long wait is said %d times:\n%s", strings.Count(got, "taking longer than usual"), got)
	}
	if strings.Contains(got, "%") {
		t.Errorf("a percentage was shown:\n%s", got)
	}
}

func TestCodeIsRefusedWhenTheTerminalCannotHideIt(t *testing.T) {
	old := signinNoEcho
	signinNoEcho = func() (func(), bool) { return func() {}, false }
	defer func() { signinNoEcho = old }()
	first := true
	u := newUI(io.Discard, appTheme{}, 80, false)
	u.in = bufio.NewReader(readerFunc(func(p []byte) (int, error) {
		if first {
			first = false
			return copy(p, "n\n"), nil // the answer to the browser question
		}
		t.Error("the code was read although the terminal could not hide it")
		return 0, io.EOF
	}))
	r := &signinRun{ue: u, interactive: true}
	code, err := r.takeCode(signinView{URL: signinTestLink}, func(func()) {}, true)
	ce, ok := err.(*cliError)
	if code != nil || !ok || ce.Kind != "claude_signin_no_echo" || !strings.HasPrefix(ce.NextAction, "ks ") {
		t.Fatalf("code=%v err=%+v", code != nil, err)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestSigninLinkOnlyAcceptsAnHttpsLinkToClaude(t *testing.T) {
	for link, ok := range map[string]bool{
		signinTestLink:                      true,
		"https://claude.ai/login?x=1":       true,
		"http://claude.com/x":               false,
		"https://claude.com.evil.example/x": false,
		"https://evil.example/claude.com":   false,
		"https://user:pw@claude.com/x":      false,
		"https://claude.com/x y":            false,
		"https://claude.com/x\x1b]0;t\x07":  false,
		"":                                  false,
		"javascript:alert(1)":               false,
		"https://notclaude.com/x":           false,
	} {
		if _, got := signinLink(link); got != ok {
			t.Errorf("%q: accepted=%v, want %v", link, got, ok)
		}
	}
}

func TestMachineIsTheDefaultOnlyWithNoLocalClaudeAndATerminal(t *testing.T) {
	// no terminal: a token on standard input is what it always was
	c := newPlanCtl()
	c.signin = goodSignin()
	bin, _, _, env := planEnv(t, c)
	_, errs, code := runPlan(t, bin, env, fixtureClaudeToken()+"\n", "login", "claude")
	if code != 0 || len(c.posts("/api/v2/keys")) != 1 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	if starts, _, _ := c.signinState(); starts != 0 {
		t.Error("a machine was started for a token that was piped in")
	}
	_ = time.Second
}

func asView(m map[string]any) signinView {
	raw, _ := json.Marshal(m)
	var v signinView
	_ = json.Unmarshal(raw, &v)
	return v
}
