package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
const testState = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gbdMbOQSY"

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
		"whole":                {signinTestLink, true},
		"no state":             {cut, false},
		"empty state":          {cut + "&state=", false},
		"state cut short":      {cut + "&state=dBjf", false},
		"challenge cut":        {signinTestLink[:strings.Index(signinTestLink, "&code_challenge=")+30], false},
		"no client":            {strings.Replace(signinTestLink, "client_id=", "x_id=", 1), false},
		"no redirect":          {strings.Replace(signinTestLink, "redirect_uri=", "x_uri=", 1), false},
		"a state with ctl":     {signinTestLink + "\x1b", false},
		"cut inside the state": {signinTestLink[:len(signinTestLink)-5], false},
		"a state of 42":        {strings.Replace(signinTestLink, testState, testState[:42], 1), false},
		"a challenge of 42":    {strings.Replace(signinTestLink, testChallenge, testChallenge[:42], 1), false},
		"a challenge of 44":    {strings.Replace(signinTestLink, testChallenge, testChallenge+"A", 1), false},
		"a plain method":       {strings.Replace(signinTestLink, "code_challenge_method=S256", "code_challenge_method=plain", 1), false},
		"a foreign callback":   {strings.Replace(signinTestLink, "platform.claude.com%2Foauth", "evil.example%2Foauth", 1), false},
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

// Only Claude's three hosts, https, no userinfo: no other subdomain.
func TestSigninLinkTakesOnlyTheThreeExactHosts(t *testing.T) {
	for link, ok := range map[string]bool{
		"https://claude.ai/oauth/authorize?x=1":           true,
		"https://claude.com/cai/oauth/authorize?x=1":      true,
		"https://platform.claude.com/oauth/authorize?x=1": true,
		"https://evil.claude.com/oauth/authorize?x=1":     false,
		"https://a.claude.ai/oauth/authorize?x=1":         false,
		"https://claude.ai.evil.example/oauth/authorize":  false,
		"http://claude.ai/oauth/authorize?x=1":            false,
		"https://user@claude.ai/oauth/authorize?x=1":      false,
	} {
		if _, got := signinLink(link); got != ok {
			t.Errorf("%q: accepted=%v, want %v", link, got, ok)
		}
	}
}

// A terminal waits a long time for the person to sign in at Claude and paste
// the code; the service ends a sign-in nobody looks at, so the client looks now and then.
func TestTheClientLooksNowAndThenWhileItWaitsForTheCode(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/claude-signins/") {
			gets.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":2,"data":{"id":"csi_1","state":"link_ready"}}`))
	}))
	defer srv.Close()
	old := signinKeepaliveEvery
	signinKeepaliveEvery = 20 * time.Millisecond
	defer func() { signinKeepaliveEvery = old }()
	pr, pw := io.Pipe()
	go func() { time.Sleep(300 * time.Millisecond); _, _ = pw.Write([]byte("AbCdEf123456#xYz\n")); pw.Close() }()
	var buf bytes.Buffer
	u := newUI(&buf, appTheme{}, 80, false)
	u.in = bufio.NewReader(pr)
	r := &signinRun{cr: hostedCreds{CTL: srv.URL, Token: "t"}, ue: u, interactive: false}
	code, err := r.takeCode(signinView{ID: "csi_1", URL: signinTestLink}, func(func()) {}, true)
	if err != nil || string(code) != "AbCdEf123456#xYz" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if n := gets.Load(); n < 3 {
		t.Errorf("the client looked %d times in 300 ms of waiting", n)
	}
}
