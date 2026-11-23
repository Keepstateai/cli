package main

import (
	"bytes"
	"strings"
	"testing"
)

// The owner's first real try of the sign-in (the service half is its own pull
// request): Claude's page said "Missing state parameter" because the link the
// service offered was cut. The client never shows or opens a link that is not
// whole, and says the same words the console says.

func TestSigninLinkWholeNeedsWhatClaudesPageNeeds(t *testing.T) {
	cut := signinTestLink[:strings.Index(signinTestLink, "&state=")]
	for name, c := range map[string]struct {
		link string
		ok   bool
	}{
		"whole":            {signinTestLink, true},
		"no state":         {cut, false},
		"empty state":      {cut + "&state=", false},
		"state cut short":  {cut + "&state=dBjf", false},
		"challenge cut":    {signinTestLink[:strings.Index(signinTestLink, "&code_challenge=")+30], false},
		"no client":        {strings.Replace(signinTestLink, "client_id=", "x_id=", 1), false},
		"no redirect":      {strings.Replace(signinTestLink, "redirect_uri=", "x_uri=", 1), false},
		"a state with ctl": {signinTestLink + "\x1b", false},
	} {
		if got := signinLinkWhole(c.link); got != c.ok {
			t.Errorf("%s: whole=%v, want %v", name, got, c.ok)
		}
	}
}

func TestACutLinkIsNeitherShownNorOpened(t *testing.T) {
	opened := false
	old := signinOpenURL
	signinOpenURL = func(string) error { opened = true; return nil }
	defer func() { signinOpenURL = old }()
	var buf bytes.Buffer
	u := newUI(&buf, appTheme{}, 80, false)
	r := &signinRun{ue: u, interactive: false}
	cut := signinTestLink[:strings.Index(signinTestLink, "&state=")]
	code, err := r.takeCode(signinView{URL: cut}, func(func()) {}, true)
	ce, ok := err.(*cliError)
	if code != nil || !ok || ce.Kind != "claude_signin_link" || !strings.HasPrefix(ce.NextAction, "ks ") {
		t.Fatalf("code=%v err=%+v", code != nil, err)
	}
	if opened || strings.Contains(buf.String(), "oauth/authorize") {
		t.Errorf("a cut link was shown or opened:\n%s", buf.String())
	}
	if !strings.Contains(strings.ToLower(ce.Message), "cut short") {
		t.Errorf("the words: %q", ce.Message)
	}
}

func TestAnOlderServersFirstStepIsNotShownAndStartingHasItsWords(t *testing.T) {
	var buf bytes.Buffer
	clk := newFakeClock()
	u := testUI(&buf, 80, false, clk)
	p := &signinProgress{r: &signinRun{ue: u}, st: u.Steps(), printed: map[int]bool{}, active: -1}
	step := func(n, st string) map[string]any { return map[string]any{"name": n, "state": st} }
	older := func(state string, n int, first string) map[string]any {
		return map[string]any{"id": "x", "state": state, "step": n, "elapsed_seconds": 2, "words": "Starting a private sign-in machine and asking Claude for a sign-in link. This takes about a minute.",
			"steps": []any{step("Start a private sign-in machine", first), step("Sign in at Claude", "todo"), step("Paste the code it shows", "todo"), step("Save your sign-in", "todo")}}
	}
	p.show(asView(older("starting", 1, "active")))
	p.show(asView(older("link_ready", 2, "done")))
	p.close()
	got := buf.String()
	if strings.Contains(got, "private") {
		t.Errorf("an older service's words were shown:\n%s", got)
	}
	if strings.Count(got, "Starting sign-in process, generating sign in URL") != 1 {
		t.Errorf("the starting words:\n%s", got)
	}
}
