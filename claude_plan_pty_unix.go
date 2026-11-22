//go:build darwin || linux

package main

// claude_plan_pty_unix.go: runs `claude setup-token` on a pseudo-terminal of
// its own, so the person does the browser step in their own terminal while
// the token it prints never reaches the screen. The child believes it is on a
// wide terminal (so its text layout does not wrap the token); the person's
// own terminal is put in raw mode so each keystroke, a pasted sign-in code
// included, goes straight to the child, and is put back on every way out.
//
// The token is read from the master side of the pair, passed through
// tokenMasker (claude_plan_mask.go) and written to the person's screen with
// the token replaced. Nothing here writes the token to a file, an argument or
// the environment of any process.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// setupTokenEnv is the environment the setup-token child gets: the person's own,
// with a terminal type that draws. It carries no credential of ours.
func setupTokenEnv() []string {
	env := os.Environ()
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// captureSetupToken runs bin with the argument setup-token on a new
// pseudo-terminal. in is the person's keyboard; userTTY, when it is a
// terminal, is put in raw mode for the run. display is where the child's
// screen goes, with the token masked. cols and rows are the width and height
// the child is given (at least claudePlanPTYCols wide). limit bounds the
// whole run.
func captureSetupToken(bin string, in io.Reader, userTTY *os.File, display io.Writer, cols, rows int, limit time.Duration) (setupResult, error) {
	var res setupResult
	if cols < claudePlanPTYCols {
		cols = claudePlanPTYCols
	}
	if rows < 10 {
		rows = 50
	}
	master, slave, err := ptyPair()
	if err != nil {
		return res, fmt.Errorf("%w: %v", errNoPTY, err)
	}
	// closed in the background: a read that a stray grandchild keeps open
	// must not hold this command at its last line
	defer func() { go master.Close() }()
	if err := setWinsize(master, rows, cols); err != nil {
		slave.Close()
		return res, fmt.Errorf("%w: %v", errNoPTY, err)
	}

	cmd := exec.Command(bin, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = setupTokenEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		return res, err
	}
	slave.Close() // the child holds its own; the master reads EOF when it is gone

	restore := func() {}
	if userTTY != nil && term.IsTerminal(int(userTTY.Fd())) {
		fd := int(userTTY.Fd())
		if old, merr := term.MakeRaw(fd); merr == nil {
			var once sync.Once
			restore = func() { once.Do(func() { _ = term.Restore(fd, old) }) }
		}
	}
	defer restore()
	// fail() ends in os.Exit, which runs no defers
	onExit(restore)

	// a signal that would kill this process first puts the terminal back and
	// takes the child down, then goes on to kill us as it was going to
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	stopSigs := make(chan struct{})
	var sigWG sync.WaitGroup
	sigWG.Add(1)
	go func() {
		defer sigWG.Done()
		for {
			select {
			case <-stopSigs:
				return
			case s := <-sigs:
				if s == syscall.SIGWINCH {
					if userTTY != nil {
						if _, r, e := term.GetSize(int(userTTY.Fd())); e == nil && r >= 10 {
							_ = setWinsize(master, r, cols)
						}
					}
					continue
				}
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				restore()
				signal.Stop(sigs)
				signal.Reset(s)
				if p, e := os.FindProcess(os.Getpid()); e == nil {
					_ = p.Signal(s)
				}
				return
			}
		}
	}()
	defer func() {
		signal.Stop(sigs)
		close(stopSigs)
		sigWG.Wait()
	}()

	// the person's keystrokes go to the child; this goroutine ends with the
	// process when the child is done, which is the end of this command
	go func() { _, _ = io.Copy(master, in) }()

	m := newTokenMasker(cols)
	chunks := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := master.Read(buf)
			if n > 0 {
				c := append([]byte(nil), buf[:n]...)
				wipe(buf[:n])
				chunks <- c
			}
			if rerr != nil {
				close(chunks)
				return
			}
		}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	show := func(c []byte) {
		shown := m.Write(c)
		wipe(c)
		if len(shown) > 0 {
			_, _ = display.Write(shown)
		}
	}
	var drain <-chan time.Time
	deadline := time.After(limit)
	exited := false
loop:
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				break loop
			}
			show(c)
		case werr := <-waitCh:
			res.ChildErr, exited, waitCh = werr, true, nil
			drain = time.After(400 * time.Millisecond)
		case <-drain:
			break loop
		case <-deadline:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if !exited {
				<-waitCh
			}
			_, _ = display.Write(m.Flush())
			m.Wipe()
			return res, errSetupTimedOut
		}
	}
	if !exited {
		select {
		case res.ChildErr = <-waitCh:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			res.ChildErr = <-waitCh
		}
	}
	_, _ = display.Write(m.Flush())
	res.Token = m.Token()
	res.Hidden = m.done || len(m.tok) > 0
	m.Wipe()
	return res, nil
}

// ioctlOn runs fn on the file's descriptor without taking the file out of
// the runtime's poller, so a close still ends a read that waits on it.
func ioctlOn(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func setWinsize(f *os.File, rows, cols int) error {
	return ioctlOn(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	})
}
