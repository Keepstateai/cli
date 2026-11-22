//go:build !(darwin || linux)

package main

import (
	"io"
	"os"
	"time"
)

// captureSetupToken: this platform makes no pseudo-terminal for the child, so
// the sign-in asks for a pasted token instead.
func captureSetupToken(bin string, in io.Reader, userTTY *os.File, display io.Writer, cols, rows int, limit time.Duration) (setupResult, error) {
	return setupResult{}, errNoPTY
}
