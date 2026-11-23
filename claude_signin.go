package main

// claude_signin.go: the sign-in machine, for a person whose computer has no
// Claude Code. KeepState starts a short-lived machine that runs Claude Code's
// own sign-in, this client shows the link it makes, takes the code Claude
// shows afterwards at a prompt that shows nothing, and the service saves the
// result. The token is made and kept on the service side and never comes back
// here: nothing in this file ever holds one.
//
// Waiting is shown honestly: the four steps the service reports, a spinner
// with the seconds that have passed (a terminal) or one line per step (a
// pipe), the service's own words for what it is doing, and a plain
// "taking longer than usual" after a minute. No percentages: nothing here
// knows how far along the work is. Ctrl-C at any point cancels the sign-in
// on the service, so its machine is destroyed, before this process exits.

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type signinStepRow struct {
	Name  string `json:"name"`
	State string `json:"state"` // done | active | todo
}

type signinView struct {
	ID      string          `json:"id"`
	State   string          `json:"state"` // starting | link_ready | checking | done | failed | cancelled | expired
	Step    int             `json:"step"`
	Steps   []signinStepRow `json:"steps"`
	Words   string          `json:"words"`
	URL     string          `json:"url"`
	Seconds int64           `json:"elapsed_seconds"`
	Expires string          `json:"expires_at"`
	Error   *struct {
		Code  string `json:"code"`
		Words string `json:"words"`
		Next  string `json:"next"`
	} `json:"error"`
	Key *struct {
		ID      string `json:"id"`
		Last4   string `json:"last4"`
		Expires string `json:"expires_at"`
	} `json:"key"`
}

// seams for tests
var (
	signinPollEvery = 1500 * time.Millisecond
	signinBound     = 25 * time.Minute
	signinOpenURL   = openLink
	signinNoEcho    = sttyNoEcho
	signinSlow      = 60 * time.Second
)

// errMachineUnavailable says the service could not start a sign-in machine
// now; the person is offered the paste instead.
var errMachineUnavailable = errors.New("the sign-in machine is not available")

// errCodeShape says the service did not accept what was typed as a code.
var errCodeShape = errors.New("not shaped like a code")

// signinEvery is the pause between looks at the sign-in: about a second and a
// half, or KS_SIGNIN_POLL_MS for a run that must not wait (the tests).
func signinEvery() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("KS_SIGNIN_POLL_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return signinPollEvery
}

func signinPath(org, rest string) string { return keyPath(org, "/api/v2/claude-signins"+rest) }

func startSignin(cr hostedCreds, org string) (signinView, error) {
	var env struct {
		Data signinView `json:"data"`
	}
	if err := hostedCall(cr, "POST", signinPath(org, ""), map[string]any{}, &env); err != nil {
		return signinView{}, err
	}
	return env.Data, nil
}

func getSignin(cr hostedCreds, id string) (signinView, error) {
	var env struct {
		Data signinView `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/claude-signins/"+url.PathEscape(id), nil, &env); err != nil {
		return signinView{}, err
	}
	return env.Data, nil
}

func cancelSignin(cr hostedCreds, id string) error {
	return hostedCall(cr, "DELETE", "/api/v2/claude-signins/"+url.PathEscape(id), nil, nil)
}

// sendSigninCode gives the service the code Claude showed. The body is built
// as bytes so it can be wiped; the code is only characters a code has.
func sendSigninCode(cr hostedCreds, id string, code []byte) error {
	var b []byte
	b = append(b, `{"code":"`...)
	b = append(b, code...)
	b = append(b, `"}`...)
	err := hostedCall(cr, "POST", "/api/v2/claude-signins/"+url.PathEscape(id)+"/code", b, nil)
	wipe(b)
	return err
}

// ---- the designed refusals of the machine ------------------------------------

func signinErr(kind, title string, code int, why []string, msg, next string) *cliError {
	return &cliError{Code: code, Kind: kind, WorkStarted: workNo, Title: title, Why: why, Message: msg, NextAction: next}
}

func signinRateErr(words string) *cliError {
	w := firstNonEmptyStr(sanitize(words), "Too many sign-ins were started in a short time.")
	return signinErr("claude_signin_rate", "Too many sign-in attempts", exitTemporary, []string{w, "Wait a little, then try again."},
		strings.TrimSuffix(w, ".")+". Wait a little, then try again", "ks login claude")
}

func signinForbiddenErr() *cliError {
	return signinErr("claude_signin_forbidden", "You cannot sign in for this organization", exitAuth,
		[]string{"Your role or seat there does not allow it."}, "you cannot sign in to a Claude plan for this organization: your role or seat there does not allow it", "ks organization list")
}

func signinFailedErr(code, words, next string) *cliError {
	w := firstNonEmptyStr(sanitize(words), "The sign-in did not finish.")
	if !strings.HasPrefix(next, "ks ") {
		next = "ks login claude"
	}
	return signinErr("claude_signin_"+firstNonEmptyStr(strings.TrimPrefix(code, "ks_signin_"), "failed"), "The Claude plan sign-in did not finish", exitFailed,
		[]string{w, "No sign-in was saved and the sign-in machine is gone."}, strings.TrimSuffix(w, ".")+". No sign-in was saved and the sign-in machine is gone", sanitize(next))
}

func signinGoneErr(state string) *cliError {
	word := map[string]string{"expired": "took too long and ended", "cancelled": "was cancelled"}[state]
	if word == "" {
		word = "ended"
	}
	return signinErr("claude_signin_"+state, "The Claude plan sign-in "+word, exitFailed,
		[]string{"The sign-in " + word + ".", "No sign-in was saved and the sign-in machine is gone."}, "the sign-in "+word+". No sign-in was saved and the sign-in machine is gone", "ks login claude")
}

func signinLostErr() *cliError {
	return signinErr("claude_signin_lost", "Lost contact with KeepState", exitTemporary,
		[]string{"The sign-in may still be running. It ends on its own, and nothing is saved unless it finishes."}, "lost contact with KeepState; the sign-in may still be running, and it ends on its own. Nothing was saved", "ks login claude")
}

func signinTooSlowErr() *cliError {
	return signinErr("claude_signin_timed_out", "The sign-in took too long", exitTemporary,
		[]string{"It was cancelled. Nothing was saved."}, "the sign-in took too long and was cancelled. Nothing was saved", "ks login claude")
}

func signinCodeEmptyErr() *cliError {
	return signinErr("claude_signin_no_code", "No code was read", exitUsage,
		[]string{"Claude shows a code after you sign in at the link."}, "no code was read; Claude shows a code after you sign in at the link. The sign-in was cancelled", "ks login claude")
}

func signinNoEchoErr() *cliError {
	return signinErr("claude_signin_no_echo", "This terminal cannot hide the code", exitUsage,
		[]string{"Nothing was read, so the code was never shown. The sign-in was cancelled."},
		"this terminal cannot hide the code, so nothing was read and the code was never shown; give the code on standard input instead. The sign-in was cancelled", "ks login claude --machine --no-input")
}

// signinStartErr words a refusal to start; the machine being unavailable is
// the one the caller turns into the paste.
func signinStartErr(err error) error {
	var te transportErr
	if errors.As(err, &te) {
		return signinErr("claude_signin_unsure", "Could not tell whether the sign-in started", exitTemporary,
			[]string{"KeepState did not answer. A sign-in machine may be starting; it ends on its own within a few minutes, and nothing is saved unless you finish the sign-in."},
			"KeepState did not answer, so it is not known whether a sign-in machine started; if one did it ends on its own within a few minutes and nothing is saved", "ks login claude")
	}
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_fleet_unavailable":
			return errMachineUnavailable
		case "ks_signin_rate":
			return signinRateErr(he.Message)
		case "ks_org_forbidden":
			return signinForbiddenErr()
		}
	}
	return claudePlanErr(err)
}

// ---- the flow ----------------------------------------------------------------

type signinRun struct {
	cr          hostedCreds
	org         string
	ue          *ui
	quiet       bool
	interactive bool

	mu          sync.Mutex
	id          string // the sign-in the cancel handler would end
	done        bool
	starting    bool          // the start has been asked and has not answered
	startCh     chan startEnd // how the start ended, for the cancel handler
	interrupted bool          // the person pressed Ctrl-C: the handler owns the exit
}

// startEnd is how asking for a sign-in ended: its id when it answered, and
// whether it never answered at all (a machine may be starting that is not known).
type startEnd struct {
	id      string
	unknown bool
}

func (r *signinRun) beginStart() {
	r.mu.Lock()
	r.starting, r.startCh = true, make(chan startEnd, 1)
	r.mu.Unlock()
}

func (r *signinRun) endStart(id string, unknown bool) {
	r.mu.Lock()
	r.starting = false
	if id != "" {
		r.id = id
	}
	ch := r.startCh
	r.mu.Unlock()
	select {
	case ch <- startEnd{id, unknown}:
	default:
	}
}

// holdIfInterrupted parks the flow once Ctrl-C was pressed: the handler is
// settling the sign-in and then exits, and nothing else may print or exit first.
func (r *signinRun) holdIfInterrupted() {
	r.mu.Lock()
	i := r.interrupted
	r.mu.Unlock()
	if i {
		select {}
	}
}

func (r *signinRun) setID(id string) { r.mu.Lock(); r.id = id; r.mu.Unlock() }
func (r *signinRun) finish()         { r.mu.Lock(); r.done = true; r.mu.Unlock() }

// guard ends the sign-in on the service when the process is interrupted, and
// puts the terminal back, before it exits.
func (r *signinRun) guard(restoreEcho func() func()) (stop func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	quit := make(chan struct{})
	go func() {
		select {
		case <-quit:
			return
		case s := <-sigs:
			if restoreEcho != nil {
				if f := restoreEcho(); f != nil {
					f()
				}
			}
			r.mu.Lock()
			r.interrupted = true
			id, starting, ch := r.id, r.starting, r.startCh
			r.mu.Unlock()
			fmt.Fprintln(os.Stderr)
			if id == "" && starting {
				// the start has not answered: if it does, that machine is ended
				// and said; if it never does, say it may still be starting
				select {
				case e := <-ch:
					id = e.id
					if id == "" && e.unknown {
						fmt.Fprintln(os.Stderr, mayStillBeStarting)
					} else if id == "" {
						fmt.Fprintln(os.Stderr, "Cancelled. Nothing was saved.")
					}
				case <-time.After(8 * time.Second):
					fmt.Fprintln(os.Stderr, mayStillBeStarting)
				}
			} else if id == "" {
				fmt.Fprintln(os.Stderr, "Cancelled. Nothing was saved.")
			}
			if id != "" {
				fmt.Fprintln(os.Stderr, interruptWords(r.cr, id))
			}
			runExitHooks()
			signal.Stop(sigs)
			code := 130
			if s == syscall.SIGTERM {
				code = 143
			}
			os.Exit(code)
		}
	}()
	return func() { signal.Stop(sigs); close(quit) }
}

const mayStillBeStarting = "Cancelled here. A sign-in machine may still be starting; it ends on its own within a few minutes, and nothing is saved unless you finish the sign-in."

// interruptWords says what became of a sign-in that the person stopped, from
// what the service says and not from what was hoped. It ends the sign-in if it
// is still going, and looks again, because it may have finished in between.
func interruptWords(cr hostedCreds, id string) string {
	settled := func(v signinView) (string, bool) {
		switch v.State {
		case "done":
			last4 := ""
			if v.Key != nil && v.Key.Last4 != "" {
				last4 = " (…" + sanitize(v.Key.Last4) + ")"
			}
			return "The sign-in had already finished: your Claude plan was saved" + last4 + ". The machine is gone. Remove it with ks key delete if you did not mean to keep it.", true
		case "cancelled":
			return "Cancelled. The sign-in machine is gone and nothing was saved.", true
		case "expired", "failed":
			return "The sign-in had already ended (" + v.State + "). The machine is gone and nothing was saved.", true
		}
		return "", false
	}
	v, err := getSignin(cr, id)
	if err == nil {
		if w, ok := settled(v); ok {
			return w
		}
	}
	_ = cancelSignin(cr, id)
	if v2, err2 := getSignin(cr, id); err2 == nil {
		if w, ok := settled(v2); ok {
			return w
		}
	}
	return "Stopped here, but what happened to the sign-in could not be confirmed. Run ks key list to see whether your Claude plan was saved. If its machine is still running it ends on its own within a few minutes."
}

// machineSignIn runs the whole sign-in on the service and returns the stored
// sign-in. It returns errMachineUnavailable when no machine could be started.
func machineSignIn(cr hostedCreds, org string) (customerKey, error) {
	_, ue := cmdUI()
	r := &signinRun{cr: cr, org: org, ue: ue, quiet: out.quiet || out.json || appChild(),
		interactive: stdinIsTerminal() && !out.noInput}
	var restore func()
	var rmu sync.Mutex
	stop := r.guard(func() func() { rmu.Lock(); defer rmu.Unlock(); return restore })
	defer stop()
	setRestore := func(f func()) { rmu.Lock(); restore = f; rmu.Unlock() }

	restarts := 0
	for {
		k, again, err := r.attempt(setRestore)
		if err != nil || !again {
			return k, err
		}
		restarts++
		if restarts > 2 {
			return customerKey{}, signinFailedErr("ks_signin_code_refused", "Claude did not accept the code.", "ks login claude")
		}
	}
}

// attempt is one sign-in from start to its end. again says the code was
// refused and the person chose to start over.
func (r *signinRun) attempt(setRestore func(func())) (customerKey, bool, error) {
	ue := r.ue
	if !r.quiet {
		ue.Title("", toneNeutral, "Sign in to your Claude plan")
		ue.Blank()
	}
	r.beginStart()
	v, err := startSignin(r.cr, r.org)
	var te transportErr
	r.endStart(v.ID, errors.As(err, &te))
	r.holdIfInterrupted()
	if err != nil {
		return customerKey{}, false, signinStartErr(err)
	}
	p := &signinProgress{r: r, st: nil, printed: map[int]bool{}, active: -1}
	if !r.quiet {
		p.st = ue.Steps()
	}
	defer p.close()

	started := time.Now()
	bad := 0
	codeGiven := false
	for {
		p.show(v)
		switch v.State {
		case "done":
			r.finish()
			if v.Key == nil {
				return customerKey{}, false, signinLostErr()
			}
			return customerKey{ID: v.Key.ID, Provider: claudePlanProvider, Kind: claudePlanProvider, Last4: v.Key.Last4, Enabled: true, ExpiresAt: v.Key.Expires, State: "ok"}, false, nil
		case "failed":
			r.finish()
			code, words, next := "", v.Words, ""
			if v.Error != nil {
				code, words, next = v.Error.Code, firstNonEmptyStr(v.Error.Words, v.Words), v.Error.Next
			}
			if code == "ks_signin_code_refused" && r.interactive {
				p.close()
				ue.Warn("Claude did not accept that code", "The code only works once and for a short time. Start again to get a new link.")
				if r.askYes("Start again with a new link?", false) {
					return customerKey{}, true, nil
				}
			}
			return customerKey{}, false, signinFailedErr(code, words, next)
		case "expired", "cancelled":
			r.finish()
			return customerKey{}, false, signinGoneErr(v.State)
		case "link_ready":
			if codeGiven {
				break // the answer is being checked; keep following it
			}
			p.close()
			for tries := 0; ; tries++ {
				code, cerr := r.takeCode(v, setRestore, tries == 0)
				if cerr != nil {
					_ = cancelSignin(r.cr, v.ID)
					r.finish()
					return customerKey{}, false, cerr
				}
				serr := r.giveCode(v.ID, code)
				wipe(code)
				if errors.Is(serr, errCodeShape) && r.interactive && tries < 2 {
					ue.Warn("That is not the code Claude shows", "It is a short line of letters and numbers that Claude shows after you sign in at the link. Paste it again.")
					continue
				}
				if errors.Is(serr, errCodeShape) {
					_ = cancelSignin(r.cr, v.ID)
					r.finish()
					return customerKey{}, false, signinErr("claude_signin_code_shape", "That is not the code Claude shows", exitUsage,
						[]string{"The sign-in was cancelled and nothing was saved."}, "that is not the code Claude shows; the sign-in was cancelled and nothing was saved", "ks login claude")
				}
				if serr != nil {
					return customerKey{}, false, serr
				}
				codeGiven = true
				break
			}
		}
		if time.Since(started) > signinBound {
			_ = cancelSignin(r.cr, v.ID)
			r.finish()
			return customerKey{}, false, signinTooSlowErr()
		}
		time.Sleep(signinEvery())
		nv, gerr := getSignin(r.cr, v.ID)
		r.holdIfInterrupted()
		if gerr != nil {
			bad++
			if bad >= 5 {
				return customerKey{}, false, signinLostErr()
			}
			continue
		}
		bad = 0
		v = nv
	}
}

// giveCode sends the code; a code that is not shaped like one is asked for
// again at a terminal, up to three times.
func (r *signinRun) giveCode(id string, code []byte) error {
	err := sendSigninCode(r.cr, id, code)
	var he *hostedErr
	if err == nil {
		return nil
	}
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_signin_code_shape":
			return errCodeShape
		case "ks_signin_not_waiting":
			if v, gerr := getSignin(r.cr, id); gerr == nil && (v.State == "expired" || v.State == "cancelled" || v.State == "failed") {
				r.finish()
				if v.State == "failed" && v.Error != nil {
					return signinFailedErr(v.Error.Code, v.Error.Words, v.Error.Next)
				}
				return signinGoneErr(v.State)
			}
			return nil // it moved on by itself: keep following it
		}
	}
	return claudePlanErr(err)
}

// takeCode shows the link, offers to open it and reads the code with nothing
// shown. A terminal that cannot hide the code is refused; with no terminal
// the code is one line of standard input.
func (r *signinRun) takeCode(v signinView, setRestore func(func()), showLink bool) ([]byte, error) {
	ue := r.ue
	link, ok := signinLink(v.URL)
	if ok && !signinLinkWhole(link) {
		return nil, signinErr("claude_signin_link", "The sign-in link was cut short", exitFailed,
			[]string{"Claude's sign-in page would refuse it, so it was not shown or opened.", "The sign-in was cancelled and nothing was saved."}, "the sign-in link was cut short, so it was not shown or opened. The sign-in was cancelled and nothing was saved", "ks login claude")
	}
	if !ok {
		return nil, signinErr("claude_signin_link", "The sign-in link could not be trusted", exitFailed,
			[]string{"The link KeepState sent is not an https link to Claude."}, "the sign-in link is not an https link to Claude, so it was not shown or opened. The sign-in was cancelled", "ks login claude")
	}
	if showLink {
		renderSigninLink(ue, link)
	}
	if r.interactive {
		if showLink && r.askYes("Open it in your browser?", true) {
			if err := signinOpenURL(link); err != nil {
				ue.Note("Could not open a browser here. Open the link above yourself.")
			}
		}
		restore, off := signinNoEcho()
		if !off {
			return nil, signinNoEchoErr()
		}
		setRestore(restore)
		defer func() { restore(); setRestore(nil) }()
		fmt.Fprint(os.Stderr, "Code (nothing is shown as you paste): ")
	}
	r2 := ue.in
	if r2 == nil {
		r2 = bufio.NewReader(os.Stdin)
	}
	line, lerr := readBoundedLine(r2, 512)
	if r.interactive {
		fmt.Fprintln(os.Stderr)
	}
	if errors.Is(lerr, errLineTooLong) {
		return nil, signinErr("claude_signin_code_long", "That is too long to be the code", exitUsage,
			[]string{"It was read to its end and thrown away.", "The sign-in was cancelled and nothing was saved."}, "that is too long to be the code; it was read to its end and thrown away. The sign-in was cancelled", "ks login claude")
	}
	code := cleanCode(line)
	wipe(line)
	if len(code) == 0 {
		return nil, signinCodeEmptyErr()
	}
	return code, nil
}

// cleanCode keeps the characters a code has; spaces and line ends around or
// inside a pasted code are dropped.
func cleanCode(b []byte) []byte {
	var out []byte
	for _, c := range b {
		if c >= 0x21 && c < 0x7f {
			out = append(out, c)
		}
	}
	return out
}

// signinLink accepts only an https link to Claude, printed and opened exactly.
func signinLink(raw string) (string, bool) {
	if raw == "" || len(raw) > 4096 {
		return "", false
	}
	for _, c := range raw {
		if c < 0x21 || c > 0x7e {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	if h != "claude.com" && h != "claude.ai" && !strings.HasSuffix(h, ".claude.com") && !strings.HasSuffix(h, ".claude.ai") {
		return "", false
	}
	return raw, true
}

// askYes asks one yes or no question on the terminal; --yes does not answer.
func (r *signinRun) askYes(q string, def bool) bool {
	ue := r.ue
	if !r.interactive || ue.in == nil {
		return false
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	ue.printf("%s %s %s ", ue.t.info("?"), ue.t.bold(q), ue.t.dim(hint))
	line, _ := ue.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "":
		return def
	}
	return false
}

// openLink opens a link with the system's opener (openBrowser, herdr.go).
func openLink(link string) error {
	if !openBrowser(link) {
		return errors.New("no way to open a browser here")
	}
	return nil
}

// sttyNoEcho turns echo off for a secret read and returns what puts it back;
// it installs no signal handler, because the caller's own handler restores it
// before it cancels the sign-in.
func sttyNoEcho() (func(), bool) {
	saved, err := sttyOut("-g")
	if err != nil || saved == "" {
		return func() {}, false
	}
	if err := sttyRun("-echo"); err != nil {
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { _ = sttyRun(saved) }) }, true
}

// ---- what the person sees while it waits ----------------------------------------

type signinProgress struct {
	r       *signinRun
	st      *stepList
	printed map[int]bool // steps shown as done
	active  int          // the step now spinning, -1 for none
	slowFor int          // the step the slow notice was shown for
	noted   map[int]bool
}

func (p *signinProgress) close() {
	if p.st != nil {
		p.st.Close()
		p.active = -1
	}
}

// show draws what changed since the last view: steps that finished, the step
// that is running with the service's words once, and an honest notice when a
// wait is long.
func (p *signinProgress) show(v signinView) {
	if p.st == nil {
		return
	}
	if p.noted == nil {
		p.noted = map[int]bool{}
	}
	for i, s := range v.Steps {
		if len(v.Steps) == 4 && i == 0 {
			continue // an older service's first step, the machine's start, is not shown
		}
		if s.State == "done" && !p.printed[i] {
			p.st.Done(s.Name, "")
			p.printed[i] = true
			if p.active == i {
				p.active = -1
			}
		}
	}
	if v.State == "starting" {
		// the machine's start is not a step: its words stand in its place
		if p.active != -2 {
			p.st.Start(signinStartingWords)
			p.active = -2
		}
		p.slow(v, 100, signinStartingWords)
		return
	}
	if v.State != "checking" {
		return
	}
	idx := v.Step - 1
	if idx < 0 || idx >= len(v.Steps) {
		return
	}
	name := v.Steps[idx].Name
	if p.active != idx {
		if w := sanitize(v.Words); w != "" && !p.noted[idx] {
			p.r.ue.Note(w)
			p.noted[idx] = true
		}
		p.st.Start(name)
		p.active = idx
	}
	p.slow(v, idx+1, name)
}

// slow says once, for a step, that a wait is long.
func (p *signinProgress) slow(v signinView, key int, name string) {
	if time.Duration(v.Seconds)*time.Second >= signinSlow && p.slowFor != key {
		p.slowFor = key
		p.st.Close()
		p.r.ue.Note("This is taking longer than usual. It is still working, and Ctrl-C cancels it and cleans up.")
		if p.r.ue.animate {
			p.st.Start(name)
		}
	}
}

// signinStartingWords is what a person reads while the machine starts and
// Claude makes the link; the console says the same.
const signinStartingWords = "Starting sign-in process, generating sign in URL"

// renderSigninLink is the link on a line of its own, never wrapped or cut.
func renderSigninLink(u *ui, link string) {
	u.Title("", toneNeutral, "Sign in at Claude")
	u.Note("Sign in to Claude, then copy the code it shows and paste it here.")
	u.Blank()
	u.lines(link)
	u.Blank()
}

// signinLinkWhole: the link carries what Claude's sign-in page needs (its
// state, the challenge, the client and the callback), each as long as Claude
// makes it. A link cut anywhere before its end fails, and is never shown or
// opened: Claude's page answers it with "Missing state parameter".
func signinLinkWhole(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	q := u.Query()
	tok := func(k string, min int) bool {
		v := q.Get(k)
		if len(v) < min {
			return false
		}
		for _, c := range v {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~') {
				return false
			}
		}
		return true
	}
	if !strings.Contains(u.Path, "authorize") && !strings.Contains(u.Path, "oauth") {
		return false
	}
	return tok("state", 20) && tok("code_challenge", 40) && q.Get("client_id") != "" && q.Get("redirect_uri") != ""
}
