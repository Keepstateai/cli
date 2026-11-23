package main

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

// The Claude plan sign-in's screens at 80, 60 and 40 columns, in colour and
// plain, as goldens beside the kit's: the card that says it is stored, the
// notes before the sign-in, its steps, and every refusal. KS_UPDATE_GOLDEN=1
// rewrites them.

var claudePlanGoldenNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func claudePlanKey(state, words string) customerKey {
	return customerKey{ID: "vlt_plan0001", Provider: claudePlanProvider, Kind: claudePlanProvider, Last4: "9999", Enabled: true,
		ExpiresAt: "2027-11-22T10:00:00Z", State: state, StateWords: words}
}

func claudePlanScreens(u *ui, clk *fakeClock) {
	renderClaudePlanIntro(u)
	st := u.Steps()
	st.Done("Opened Claude Code's sign-in", "")
	st.Done("Read the token", "it was hidden from the screen")
	st.Start("Storing your sign-in")
	clk.add(900 * time.Millisecond)
	st.Done("Stored your sign-in", "")
	u.Blank()
	renderClaudePlanSuccess(u, claudePlanKey("ok", ""), claudePlanGoldenNow, "")
	u.Blank()
	renderClaudePlanSuccess(u, claudePlanKey("expiring", "Your Claude plan sign-in ends on 3 Dec 2026. Run ks login claude to renew it."), claudePlanGoldenNow, "")
	u.Blank()
	renderClaudePlanPasteIntro(u, "Claude Code is not installed on this computer.")
	u.Blank()
	renderClaudePlanUnread(u, "cut")
}

func claudePlanErrors(u *ui) {
	for i, ce := range []*cliError{
		claudePlanPersonalErr(), claudePlanTokenInvalidErr(), claudePlanUnavailableErr(), claudePlanUnconfirmedErr(), claudePlanNoEchoErr(), claudePlanNoneErr(),
		claudePlanRunnerErr("this agent calls OpenAI, not a Claude model"), claudePlanNotFinishedErr(), claudePlanUnreadErr(),
		claudePlanTimedOutErr(), claudePlanStoppedErr(), claudePlanLineTooLongErr(), claudePlanUnusableErr(claudePlanKey("invalid", "")), claudePlanUnusableErr(claudePlanKey("expired", "")),
	} {
		if i > 0 {
			u.Blank()
		}
		renderError(u, ce)
	}
}

func TestClaudePlanGoldens(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		for _, themed := range []bool{true, false} {
			mode := "plain"
			if themed {
				mode = "color"
			}
			var buf bytes.Buffer
			clk := newFakeClock()
			u := testUI(&buf, width, themed, clk)
			claudePlanScreens(u, clk)
			checkGolden(t, fmt.Sprintf("claude-plan-screens-%d-%s", width, mode), stripEsc(buf.String()))

			buf.Reset()
			claudePlanErrors(u)
			checkGolden(t, fmt.Sprintf("claude-plan-errors-%d-%s", width, mode), stripEsc(buf.String()))
		}
	}
}

func TestClaudePlanScreensFitTheWidthAndCarryNoEscapeInPlain(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		var buf bytes.Buffer
		clk := newFakeClock()
		u := testUI(&buf, width, false, clk)
		claudePlanScreens(u, clk)
		claudePlanErrors(u)
		for _, l := range bytes.Split(buf.Bytes(), []byte("\n")) {
			if bytes.Contains(l, []byte("\x1b")) {
				t.Errorf("width %d: plain output holds an escape: %q", width, l)
			}
			if displayWidth(string(l)) > width && !bytes.Contains(l, []byte("ks ")) {
				t.Errorf("width %d: %q is %d columns", width, l, displayWidth(string(l)))
			}
		}
	}
}

// ---- the sign-in machine and the organization verbs ---------------------------------

func claudeSigninScreens(u *ui, clk *fakeClock) {
	r := &signinRun{ue: u}
	p := &signinProgress{r: r, st: u.Steps(), printed: map[int]bool{}, active: -1}
	u.Title("", toneNeutral, "Sign in to your Claude plan")
	u.Blank()
	m := sview("starting", 0, nil)
	p.show(asView(m))
	clk.add(75 * time.Second)
	m["elapsed_seconds"] = 75
	p.show(asView(m))
	p.show(asView(sview("link_ready", 1, map[string]any{"url": signinTestLink})))
	p.close()
	renderSigninLink(u, signinTestLink)
	p.show(asView(sview("checking", 3, nil)))
	clk.add(4 * time.Second)
	p.show(asView(sview("done", 4, map[string]any{"key": map[string]any{"id": "vlt_plan0001", "last4": "9999", "expires_at": "2027-11-22T10:00:00Z"}})))
	p.close()
	u.Blank()
	renderClaudePlanSuccess(u, claudePlanKey("ok", ""), claudePlanGoldenNow, "org_acme")
	u.Blank()
	u.Warn("Claude did not accept that code", "The code only works once and for a short time. Start again to get a new link.")
	u.Blank()
	rows := []orgPlanRow{
		{MemberID: "mem_you", Last4: "9999", State: "ok", ExpiresAt: "2027-11-22T10:00:00Z", You: true},
		{MemberID: "mem_two", Last4: "1234", State: "expiring", ExpiresAt: "2026-12-03T10:00:00Z"},
		{MemberID: "mem_three", Last4: "5678", State: "invalid", ExpiresAt: "2027-01-05T10:00:00Z"},
	}
	rows[0].Who = &struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Email string `json:"email"`
	}{"Ada Lovelace", "", "ada@example.com"}
	rows[1].Who = &struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Email string `json:"email"`
	}{"Grace Hopper", "", "grace@example.com"}
	renderOrgPlanKeys(u, "org_acme", rows, claudePlanGoldenNow)
	u.Blank()
	renderOrgPlanKeys(u, "org_acme", nil, claudePlanGoldenNow)
	u.Blank()
	renderOrgPlanRemoved(u, "Grace Hopper's")
}

func claudeSigninErrors(u *ui) {
	for i, ce := range []*cliError{
		signinRateErr("Too many sign-ins were started in a short time."), signinForbiddenErr(),
		signinFailedErr("ks_signin_code_refused", "Claude did not accept that code. Start again to get a new link, and paste the new code.", "ks login claude"),
		signinFailedErr("ks_signin_machine", "The sign-in machine did not start.", "ks login claude"),
		signinGoneErr("expired"), signinGoneErr("cancelled"), signinLostErr(), signinStartErr(transportErr{err: io.EOF}).(*cliError), signinTooSlowErr(), signinCodeEmptyErr(), signinNoEchoErr(),
		claudePlanErr(&hostedErr{Status: 409, Type: "ks_claude_plan_org", Message: "no plan"}).(*cliError),
	} {
		if i > 0 {
			u.Blank()
		}
		renderError(u, ce)
	}
}

func TestClaudeSigninGoldens(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		for _, themed := range []bool{true, false} {
			mode := "plain"
			if themed {
				mode = "color"
			}
			var buf bytes.Buffer
			clk := newFakeClock()
			u := testUI(&buf, width, themed, clk)
			claudeSigninScreens(u, clk)
			checkGolden(t, fmt.Sprintf("claude-signin-screens-%d-%s", width, mode), stripEsc(buf.String()))
			buf.Reset()
			claudeSigninErrors(u)
			checkGolden(t, fmt.Sprintf("claude-signin-errors-%d-%s", width, mode), stripEsc(buf.String()))
		}
	}
}
