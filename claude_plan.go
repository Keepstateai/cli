package main

// claude_plan.go: ks login claude, the Claude plan sign-in. A person signs in
// to Claude once, KeepState keeps the sign-in, and the agents that run Claude
// Code for them use it where an API key would be used.
//
// The sign-in is Claude Code's own long-lived token (claude setup-token). This
// client runs that command on the person's computer, on a pseudo-terminal, so
// the browser step happens in their terminal and the token it prints is
// hidden from the screen (claude_plan_mask.go) and sent once, over HTTPS, to
// the account's keys (POST /api/v2/keys, provider claude_plan). The token is
// never printed, never written to a file, never put in an argument or in the
// environment of a child, never in --json, an error or a log. Where Claude
// Code is not installed here, the person pastes a token made elsewhere at a
// prompt that shows nothing.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	// claudePlanProvider is the provider value of a plan row in the key list.
	claudePlanProvider = "claude_plan"
	// claudePlanKeyArg is --key claude-plan: run the agent on the plan.
	claudePlanKeyArg = "claude-plan"
	// claudePlanCapability is the capability the service publishes when the
	// sign-in is switched on for it.
	claudePlanCapability = "claude_plan_key"
	// claudePlanWarnDays is how long before it ends a sign-in is warned about.
	claudePlanWarnDays = 14
	// claudePlanAnthropic is the one provider a plan serves.
	claudePlanAnthropic = "anthropic"
	// claudePlanSourceWords is how a usage line names calls on the plan.
	claudePlanSourceWords = "your Claude plan, no KeepState charge"
)

// claudePlanFindRE finds a token inside a line a person pasted, which may be
// the export line setup-token suggests.
var claudePlanFindRE = regexp.MustCompile(`sk-ant-oat[0-9]*-[A-Za-z0-9_\-]{16,}`)

// seams for tests: where Claude Code is looked for and how it is run.
var (
	claudeLookPath = exec.LookPath
	claudeCapture  = captureSetupToken
)

// ---- the stored sign-in as the key list shows it -------------------------

// planKeyOf is the account's plan row, nil when it has none.
func planKeyOf(keys []customerKey) *customerKey {
	for i := range keys {
		if keys[i].Provider == claudePlanProvider {
			return &keys[i]
		}
	}
	return nil
}

// planState is the row's standing: the service's word, or, for a service that
// sent only the date, the date's.
func planState(k customerKey, now time.Time) string {
	if k.State != "" {
		return k.State
	}
	if t, ok := planEnds(k); ok {
		switch {
		case !t.After(now):
			return "expired"
		case t.Sub(now) < claudePlanWarnDays*24*time.Hour:
			return "expiring"
		}
	}
	return "ok"
}

func planEnds(k customerKey) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, k.ExpiresAt)
	return t, err == nil
}

func planEndsWords(k customerKey) string {
	if t, ok := planEnds(k); ok {
		return t.UTC().Format("2 Jan 2006")
	}
	return ""
}

// planUsable: a session could be started on this sign-in now.
func planUsable(k customerKey, now time.Time) bool {
	if !k.Enabled {
		return false
	}
	switch planState(k, now) {
	case "ok", "expiring":
		return true
	}
	return false
}

// planStateWords is one plain sentence about a sign-in that needs the
// person's attention, from the service's own words when it sent them.
func planStateWords(k customerKey, now time.Time) string {
	st := planState(k, now)
	if st == "ok" {
		return ""
	}
	if w := sanitize(k.StateWords); w != "" {
		return w
	}
	ends := planEndsWords(k)
	switch st {
	case "expiring":
		if ends != "" {
			return "Your Claude plan sign-in ends on " + ends + ". Run ks login claude to renew it."
		}
		return "Your Claude plan sign-in ends soon. Run ks login claude to renew it."
	case "expired":
		return "Your Claude plan sign-in has ended. Run ks login claude to sign in again."
	case "invalid":
		return "Your Claude plan sign-in no longer works. Run ks login claude to sign in again."
	}
	return ""
}

// planKeyLine is a plan row in a key list: "Claude plan", the last four
// characters, when it ends and what needs attention.
func planKeyLine(k customerKey, now time.Time) string {
	state := "enabled"
	if !k.Enabled {
		state = "disabled"
	}
	if e := planEndsWords(k); e != "" {
		state += ", ends " + e
	}
	switch planState(k, now) {
	case "expiring":
		state += " (ends soon)"
	case "expired":
		state += " (ended)"
	case "invalid":
		state += " (no longer works)"
	}
	return fmt.Sprintf("%-14s %-11s %-18s %-6s %s", k.ID, k.Provider, "Claude plan", "…"+k.Last4, state)
}

// claudePlanListed: the service lists the sign-in as available to this
// client, by the registry the last verb refreshed. It asks nothing.
func claudePlanListed(cr hostedCreds) bool {
	set := cachedCapabilities()
	if set == nil || set.ControlPlane != cr.CTL {
		return false
	}
	row := set.find(claudePlanCapability)
	return row != nil && row.Availability == "available" && row.SinceProtocol <= clientProtocol
}

// planOffered says whether to mention the sign-in to a person: the service
// lists it, or the account already has one.
func planOffered(cr hostedCreds, keys []customerKey) bool {
	return planKeyOf(keys) != nil || claudePlanListed(cr)
}

// ---- the designed refusals -------------------------------------------------

func claudePlanPersonalErr() *cliError {
	return &cliError{Code: exitUsage, Kind: "claude_plan_personal", WorkStarted: workNo,
		Title:      "A Claude plan belongs to one person",
		Why:        []string{"It cannot be stored for an organization. Your own account can."},
		Message:    "a Claude plan belongs to one person and cannot be stored for an organization; your own account can",
		NextAction: "ks context use personal"}
}

func claudePlanTokenInvalidErr() *cliError {
	return &cliError{Code: exitUsage, Kind: "claude_plan_token_invalid", WorkStarted: workNo,
		Title:      "That is not a Claude plan token",
		Why:        []string{"A token made by claude setup-token starts with sk-ant-oat."},
		Message:    "that is not a Claude plan token; one made by claude setup-token starts with sk-ant-oat. Nothing was stored",
		NextAction: "ks login claude"}
}

func claudePlanUnavailableErr() *cliError {
	return &cliError{Code: exitFailed, Kind: "claude_plan_unavailable", WorkStarted: workNo,
		Title:      "Claude plan sign-in is not available yet",
		Why:        []string{"This KeepState service has not turned it on.", "An API key works as before."},
		Message:    "Claude plan sign-in is not available on this service yet; an API key works as before",
		NextAction: "ks key add --provider anthropic"}
}

func claudePlanNoneErr() *cliError {
	return &cliError{Code: exitConflict, Kind: "claude_plan_none", WorkStarted: workNo,
		Title:      "You are not signed in to a Claude plan",
		Why:        []string{"There is no usable Claude plan sign-in on your account."},
		Message:    "you have no usable Claude plan sign-in on your account. Nothing was created",
		NextAction: "ks login claude"}
}

func claudePlanRunnerErr(what string) *cliError {
	return &cliError{Code: exitUsage, Kind: "claude_plan_runner", WorkStarted: workNo,
		Title:      "A Claude plan runs Claude Code only",
		Why:        []string{capFirst(strings.TrimSuffix(what, ".")) + "."},
		Message:    "a Claude plan runs Claude Code with a Claude model only; " + what + ". Nothing was created",
		NextAction: "ks run --agent --key own"}
}

func claudePlanNotFinishedErr() *cliError {
	return &cliError{Code: exitFailed, Kind: "claude_plan_not_finished", WorkStarted: workNo,
		Title:      "The Claude Code sign-in did not finish",
		Why:        []string{"Claude Code ended before it made a token."},
		Message:    "the Claude Code sign-in did not finish: Claude Code ended before it made a token. Nothing was stored",
		NextAction: "ks login claude"}
}

func claudePlanUnreadErr() *cliError {
	return &cliError{Code: exitFailed, Kind: "claude_plan_unread", WorkStarted: workNo,
		Title:      "Claude Code made a token this client could not read",
		Why:        []string{"It was hidden from the screen and not kept.", "Make a new one."},
		Message:    "Claude Code made a token this client could not read; it was hidden and not kept. Nothing was stored",
		NextAction: "ks login claude"}
}

func claudePlanStoppedErr() *cliError {
	return &cliError{Code: exitInterrupt, Kind: "claude_plan_stopped", WorkStarted: workNo,
		Title:      "The Claude Code sign-in was stopped",
		Why:        []string{"You pressed Ctrl-C."},
		Message:    "the Claude Code sign-in was stopped. Nothing was stored",
		NextAction: "ks login claude"}
}

func claudePlanTimedOutErr() *cliError {
	return &cliError{Code: exitTemporary, Kind: "claude_plan_timed_out", WorkStarted: workNo,
		Title:      "The Claude Code sign-in took too long",
		Why:        []string{"It was stopped after 20 minutes."},
		Message:    "the Claude Code sign-in took too long and was stopped. Nothing was stored",
		NextAction: "ks login claude"}
}

// claudePlanErr words a refusal of the sign-in the service made by name; any
// other refusal passes through as it was.
func claudePlanErr(err error) error {
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_claude_plan_off":
			return claudePlanUnavailableErr()
		case "ks_claude_plan_personal":
			return claudePlanPersonalErr()
		case "ks_claude_plan_token_invalid":
			return claudePlanTokenInvalidErr()
		case "ks_claude_plan_none":
			return claudePlanNoneErr()
		case "ks_claude_plan_runner":
			ce := claudePlanRunnerErr(strings.TrimSuffix(sanitize(he.Message), "."))
			return ce
		}
	}
	return err
}

// ---- ks login claude ---------------------------------------------------------

// hostedLoginClaude signs in once and stores the sign-in.
func hostedLoginClaude(cr hostedCreds, inv *Invocation) {
	uo, _ := cmdUI()
	k, err := signInClaudePlan(cr)
	if err != nil {
		fail(err)
	}
	now := time.Now()
	emit(map[string]any{"ok": true, "key_id": k.ID, "last4": k.Last4, "expires_at": k.ExpiresAt, "state": planState(k, now)}, func() {
		renderClaudePlanSuccess(uo, k, now)
	})
}

// signInClaudePlan reads a token from the person and stores it as their
// Claude plan sign-in. It returns errors rather than exiting, so ks run can
// offer it in the middle of starting an agent.
func signInClaudePlan(cr hostedCreds) (customerKey, error) {
	if keyContext(cr) != "" {
		return customerKey{}, claudePlanPersonalErr()
	}
	_, ue := cmdUI()
	quiet := out.quiet || out.json || appChild()
	tok, err := readClaudePlanToken(ue, quiet)
	if err != nil {
		return customerKey{}, err
	}
	// the request body is built as bytes so it can be wiped; the token is
	// only token characters, so it needs no escaping
	body := claudePlanBody(tok)
	wipe(tok)

	var st *stepList
	if !quiet {
		st = ue.Steps()
		st.Start("Storing your sign-in")
	}
	var env struct {
		Data customerKey `json:"data"`
	}
	err = hostedCall(cr, "POST", "/api/v2/keys", body, &env)
	wipe(body)
	if err != nil {
		if st != nil {
			st.Fail("Could not store your sign-in", "")
		}
		return customerKey{}, claudePlanErr(err)
	}
	if st != nil {
		st.Done("Stored your sign-in", "")
	}
	return env.Data, nil
}

// claudePlanBody is the request that stores a token: the one place the token
// is written into a request.
func claudePlanBody(tok []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"provider":"` + claudePlanProvider + `","secret":"`)
	b.Write(tok)
	b.WriteString(`"}`)
	return b.Bytes()
}

// renderClaudePlanSuccess is the card that says the sign-in is stored.
func renderClaudePlanSuccess(u *ui, k customerKey, now time.Time) {
	facts := [][2]string{
		{"Token", "ends …" + sanitize(k.Last4) + "; it is never shown again"},
	}
	if e := planEndsWords(k); e != "" {
		facts = append(facts, [2]string{"Ends", e})
	}
	facts = append(facts, [2]string{"Used by", "your Claude Code agents, instead of an API key; no KeepState charge"})
	u.Success("Signed in to your Claude plan", facts, "ks run --agent --key claude-plan")
	if w := planStateWords(k, now); w != "" {
		u.Blank()
		u.Warn("Check this sign-in", w)
	}
}

// readClaudePlanToken gets a token from the person: from Claude Code's own
// setup-token on this computer where it is installed, otherwise pasted at a
// prompt that shows nothing. The caller wipes the returned bytes.
func readClaudePlanToken(ue *ui, quiet bool) ([]byte, error) {
	interactive := stdinIsTerminal() && !out.noInput
	if !interactive {
		// the piped path every script drives: one line of standard input
		return readPastedToken(ue, false)
	}
	bin, lerr := claudeLookPath("claude")
	if lerr != nil {
		return pasteWithReason(ue, "Claude Code is not installed on this computer.")
	}
	display := io.Writer(os.Stdout)
	if out.json || appChild() {
		display = os.Stderr // standard output stays the answer
	}
	if !quiet {
		renderClaudePlanIntro(ue)
	}
	rows := 50
	res, err := claudeCapture(bin, os.Stdin, os.Stdin, display, termColumns(), rows, claudePlanLimit)
	if errors.Is(err, errNoPTY) {
		return pasteWithReason(ue, "This terminal could not run Claude Code's sign-in for you.")
	}
	if !quiet {
		fmt.Fprintln(display)
	}
	switch {
	case errors.Is(err, errSetupTimedOut):
		return nil, claudePlanTimedOutErr()
	case errors.Is(err, errSetupInterrupted):
		return nil, claudePlanStoppedErr()
	case err != nil:
		return nil, claudePlanNotFinishedErr()
	case res.Token != nil:
		if !quiet {
			st := ue.Steps()
			st.Done("Read the token", "it was hidden from the screen")
		}
		return res.Token, nil
	case res.Hidden:
		return nil, claudePlanUnreadErr()
	}
	return nil, claudePlanNotFinishedErr()
}

// pasteWithReason says why the person is asked to paste, and asks.
func pasteWithReason(ue *ui, why string) ([]byte, error) {
	if !out.quiet && !out.json {
		renderClaudePlanPasteIntro(ue, why)
	}
	return readPastedToken(ue, true)
}

// renderClaudePlanIntro is what the person reads before Claude Code's own
// sign-in takes the screen.
func renderClaudePlanIntro(u *ui) {
	u.Title("", toneNeutral, "Sign in to your Claude plan")
	u.Note("Claude Code's own sign-in runs below. Approve it in your browser and, if it asks, paste the code it shows. The token it makes is hidden here and kept by KeepState; nobody sees it, you included.")
	u.Blank()
}

// renderClaudePlanPasteIntro is what the person reads when they are asked to
// paste a token instead.
func renderClaudePlanPasteIntro(u *ui, why string) {
	u.Title("", toneNeutral, "Sign in to your Claude plan")
	u.Note(why + " On a computer that has Claude Code, run claude setup-token, sign in there, and paste the token it prints here. Nothing is shown as you paste.")
	u.Blank()
}

// readPastedToken reads one line that holds a token, with echo off at a
// terminal. The line is never kept past the call and never echoed back in an
// error.
func readPastedToken(ue *ui, tty bool) ([]byte, error) {
	r := ue.in
	if r == nil {
		r = bufio.NewReader(os.Stdin)
	}
	restore := func() {}
	if tty {
		var off bool
		restore, off = noEcho()
		defer restore()
		if !off {
			fmt.Fprintln(os.Stderr, "this terminal will show the token as you paste it; it is not kept")
		} else {
			fmt.Fprint(os.Stderr, "Token: ")
		}
	}
	line, err := readBoundedLine(r, 8192)
	if tty {
		restore()
		fmt.Fprintln(os.Stderr)
	}
	if err != nil && len(line) == 0 {
		return nil, &cliError{Code: exitUsage, Kind: "usage", WorkStarted: workNo,
			Message: "no token was read from standard input. Nothing was stored", NextAction: "ks login claude"}
	}
	tok := claudePlanFindRE.Find(line)
	var got []byte
	if tok != nil {
		got = append([]byte(nil), tok...)
	}
	wipe(line)
	if got == nil {
		return nil, claudePlanTokenInvalidErr()
	}
	return got, nil
}

// readBoundedLine reads up to limit bytes of one line, without the line end.
func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for len(line) < limit {
		b, err := r.ReadByte()
		if err != nil {
			return line, err
		}
		if b == '\n' || b == '\r' {
			if len(line) == 0 {
				continue // blank lines before the token are skipped
			}
			return line, nil
		}
		line = append(line, b)
	}
	return line, nil
}

// ---- running an agent on the plan --------------------------------------------

// chooseClaudePlan is --key claude-plan: the agent's Claude Code calls go on
// the account's Claude plan sign-in. Refused before anything is created when
// the agent does not call Anthropic (the Codex runner, an open model) or when
// there is no usable sign-in.
func chooseClaudePlan(cr hostedCreds, provider string) (runKeyChoice, error) {
	c := runKeyChoice{plan: true}
	if provider != claudePlanAnthropic {
		return c, claudePlanRunnerErr("this agent calls " + providerName(provider) + ", not a Claude model")
	}
	keys, err := fetchKeys(cr)
	if err != nil {
		return c, err
	}
	k := planKeyOf(keys)
	switch {
	case k == nil:
		return c, claudePlanNoneErr()
	case !planUsable(*k, time.Now()):
		return c, claudePlanUnusableErr(*k)
	}
	return planRunChoice(cr, *k, provider), nil
}

// planRunChoice is the choice of the plan for a run: a KeepState default for
// the provider is set aside with an empty managed list, as an own key over
// that default would be.
func planRunChoice(cr hostedCreds, k customerKey, provider string) runKeyChoice {
	c := runKeyChoice{plan: true, planKey: k}
	if _, listed := managedListed(cr); listed {
		if doc, err := fetchModelKeys(cr); err == nil {
			c.answered = true
			if doc.defaultFor(provider) == "managed" {
				none := []string{}
				c.send = &none
			}
		}
	}
	return c
}

// claudePlanUnusableErr is a sign-in that is there and cannot be used.
func claudePlanUnusableErr(k customerKey) *cliError {
	why := planStateWords(k, time.Now())
	if why == "" {
		why = "Your Claude plan sign-in is turned off."
	}
	return &cliError{Code: exitConflict, Kind: "claude_plan_unusable", WorkStarted: workNo,
		Title:      "Your Claude plan sign-in cannot be used",
		Why:        []string{why},
		Message:    "your Claude plan sign-in cannot be used: " + strings.TrimSuffix(why, ".") + ". Nothing was created",
		NextAction: "ks login claude"}
}

// planInsteadOfNoKey: a run with no key chosen and none of your own for the
// provider goes on the Claude plan sign-in when there is a usable one, and
// is offered one at a terminal when the service has it switched on. An API
// key of your own keeps being what it was.
func planInsteadOfNoKey(cr hostedCreds, provider string) (customerKey, bool, error) {
	if provider != claudePlanAnthropic {
		return customerKey{}, false, nil
	}
	keys, err := fetchKeys(cr)
	if err != nil {
		return customerKey{}, false, nil // runKey reports what it cannot read
	}
	for _, k := range keys {
		if k.Provider == provider && k.Enabled {
			return customerKey{}, false, nil
		}
	}
	if p := planKeyOf(keys); p != nil {
		if planUsable(*p, time.Now()) {
			return *p, true, nil
		}
		return customerKey{}, false, claudePlanUnusableErr(*p)
	}
	if claudePlanListed(cr) {
		k, ok, oerr := offerPlanSignIn(cr)
		if oerr != nil {
			return customerKey{}, false, oerr
		}
		if ok {
			return k, true, nil
		}
	}
	return customerKey{}, false, nil
}

// offerPlanSignIn asks, at a terminal, whether to sign in to the plan now,
// and does. --yes does not answer it: it opens a browser.
func offerPlanSignIn(cr hostedCreds) (customerKey, bool, error) {
	_, ue := cmdUI()
	if out.yes || out.noInput || out.json || out.quiet || appChild() || startObserver != nil || ue.in == nil || !ue.inTTY {
		return customerKey{}, false, nil
	}
	ue.printf("%s %s %s ", ue.t.info("?"), ue.t.bold("You have no key for Claude Code. Sign in to your Claude plan now?"), ue.t.dim("[y/N]"))
	line, _ := ue.in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return customerKey{}, false, nil
	}
	k, err := signInClaudePlan(cr)
	if err != nil {
		return customerKey{}, false, err
	}
	progress("signed in to your Claude plan (…%s)", k.Last4)
	return k, true, nil
}

// withoutKeyBlocker takes the preflight's "no key" blocker out of the list
// for a run on the plan: a Claude plan sign-in is not an API key, and the
// session's creation checks it.
func withoutKeyBlocker(blockers []any, checks any) []any {
	drop := map[string]bool{}
	if cs, ok := checks.([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok && m["check"] == "key" && m["status"] == "block" {
				drop[fmt.Sprint(m["detail"])] = true
			}
		}
	}
	var kept []any
	for _, b := range blockers {
		if !drop[fmt.Sprint(b)] {
			kept = append(kept, b)
		}
	}
	return kept
}

// planWorthTrying: a run that named no key, in your own context, on Anthropic,
// not on KeepState's key and not already on the plan, may go on the plan.
func planWorthTrying(org, keyArg string, c runKeyChoice, provider string) bool {
	return org == "" && keyArg == "" && !c.managed && !c.plan && provider == claudePlanAnthropic
}

// hasKeyBlocker: the preflight blocks on the account's key for the provider.
func hasKeyBlocker(checks any) bool {
	cs, _ := checks.([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok && m["check"] == "key" && m["status"] == "block" {
			return true
		}
	}
	return false
}
