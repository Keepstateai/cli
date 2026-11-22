package main

// claude_plan_capture.go: what running `claude setup-token` for a person
// answers, on every platform. The pseudo-terminal itself is in
// claude_plan_pty_unix.go; a platform without one answers errNoPTY and the
// sign-in falls back to a pasted token.

import (
	"errors"
	"time"
)

// errSetupTimedOut says the browser step took longer than claudePlanLimit.
var errSetupTimedOut = errors.New("claude setup-token did not finish in time")

// errSetupInterrupted says the person stopped the sign-in with Ctrl-C.
var errSetupInterrupted = errors.New("the sign-in was stopped")

// errNoPTY says no pseudo-terminal could be made, so nothing was run.
var errNoPTY = errors.New("no pseudo-terminal could be made")

// setupResult is what one run of the setup-token command left.
type setupResult struct {
	// Token is the token the child printed, nil when none was read. The
	// caller wipes it after the one request that stores it.
	Token []byte
	// Hidden: the child printed something shaped like a token, and it was
	// kept off the screen, whether or not it could be read whole.
	Hidden bool
	// ChildErr is how the child ended, nil for a clean exit.
	ChildErr error
}

const (
	// claudePlanPTYCols is the least width the child is given: wide enough
	// that the child's own text layout never wraps a token.
	claudePlanPTYCols = 300
	// claudePlanLimit is how long the browser step may take.
	claudePlanLimit = 20 * time.Minute
)
