package main

import (
	"bytes"
	"fmt"
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
	renderClaudePlanSuccess(u, claudePlanKey("ok", ""), claudePlanGoldenNow)
	u.Blank()
	renderClaudePlanSuccess(u, claudePlanKey("expiring", "Your Claude plan sign-in ends on 3 Dec 2026. Run ks login claude to renew it."), claudePlanGoldenNow)
	u.Blank()
	renderClaudePlanPasteIntro(u, "Claude Code is not installed on this computer.")
}

func claudePlanErrors(u *ui) {
	for i, ce := range []*cliError{
		claudePlanPersonalErr(), claudePlanTokenInvalidErr(), claudePlanUnavailableErr(), claudePlanUnconfirmedErr(), claudePlanNoneErr(),
		claudePlanRunnerErr("this agent calls OpenAI, not a Claude model"), claudePlanNotFinishedErr(), claudePlanUnreadErr(),
		claudePlanTimedOutErr(), claudePlanStoppedErr(), claudePlanUnusableErr(claudePlanKey("invalid", "")), claudePlanUnusableErr(claudePlanKey("expired", "")),
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
