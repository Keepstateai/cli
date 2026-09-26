// Hosted `ks attach`: an interactive terminal into a running fleet
// session, tunneled through the control plane. The client dials ctl,
// asks for the ks-attach upgrade with its bearer token, and on a 101
// puts the local terminal in raw mode and relays bytes both ways. The
// remote PTY is allocated by ksd's `ssh -tt`; this side only needs to
// stop the local tty from cooking the stream. Terminal control uses
// stty (present on macOS and Linux) so the client keeps zero module
// dependencies. Live resize is a documented v1 limitation; the initial
// size is sent at connect.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// sttyOut and sttyRun run stty against THE TERMINAL, which means wiring
// the child's stdin to ours. `stty` takes the terminal from its own
// standard input; an exec.Command with no Stdin set hands the child
// /dev/null, where every stty invocation fails with "stdin isn't a
// terminal". session.go's termColumns has always done this correctly and
// is the precedent; termSize and rawMode below did not, and both were
// therefore inert in exactly the way a discarded error hides.
func sttyOut(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func sttyRun(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// termSize is the real window, for sizing the remote tmux. It answered
// 80x24 for every window ever attached, because its stty child read
// /dev/null and the error was swallowed by the `err == nil` guard.
func termSize() (cols, rows int) {
	cols, rows = 80, 24
	out, err := sttyOut("size") // "rows cols"
	if err == nil {
		_, _ = fmt.Sscanf(out, "%d %d", &rows, &cols)
	}
	return
}

// rawMode puts the controlling terminal into raw, no-echo mode and
// returns a restore func. A no-op (with a restore no-op) when stdin is
// not a terminal, so the gate can drive attach with piped stdin.
func rawMode() func() {
	saved, err := sttyOut("-g")
	if err != nil || saved == "" {
		return func() {} // not a terminal: the piped path the gates drive
	}
	// -icanon -echo -isig off so keystrokes (incl. Ctrl-C, tmux prefix)
	// reach the remote unmangled; the remote tmux/shell owns them.
	if err := sttyRun("raw", "-echo"); err != nil {
		return func() {} // nothing was changed, so there is nothing to undo
	}
	var once sync.Once
	restore := func() { once.Do(func() { _ = sttyRun(saved) }) }

	// A terminal left raw with echo off is far worse than one that never
	// entered it, and a deferred restore does not run when a signal ends
	// the process. Restore first, then re-raise so the exit status is
	// still the signal's. Ctrl-C is NOT among these in practice: raw mode
	// turns off isig precisely so Ctrl-C reaches the remote instead.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	go func() {
		s, ok := <-ch
		if !ok {
			return
		}
		restore()
		signal.Stop(ch)
		signal.Reset(s)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(s)
		}
	}()
	return func() { signal.Stop(ch); restore() }
}

func hostedAttach(cr hostedCreds, id string) error {
	u, err := url.Parse(strings.TrimRight(cr.CTL, "/"))
	if err != nil {
		return err
	}
	cols, rows := termSize()
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}
	path := fmt.Sprintf("/api/sessions/%s/attach?cols=%d&rows=%d&term=%s", id, cols, rows, url.QueryEscape(term))

	// raw dial so we can hijack: TLS for https, plain TCP for http (dev).
	var conn net.Conn
	if u.Scheme == "https" {
		host := u.Host
		if !strings.Contains(host, ":") {
			host += ":443"
		}
		conn, err = tls.Dial("tcp", host, &tls.Config{
			ServerName: strings.Split(u.Host, ":")[0],
			NextProtos: []string{"http/1.1"}, // hijack needs HTTP/1.1
		})
	} else {
		host := u.Host
		if !strings.Contains(host, ":") {
			host += ":80"
		}
		conn, err = net.Dial("tcp", host)
	}
	if err != nil {
		return fmt.Errorf("control plane unreachable: %w", err)
	}
	defer conn.Close()

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Authorization: Bearer " + cr.Token + "\r\n" +
		"Upgrade: ks-attach\r\nConnection: Upgrade\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("attach handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct{ Message, Error string }
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			if e.Error != "" {
				// a named refusal keeps its name (BACKLOG-170)
				return hostedError("GET", "/api/sessions/"+id+"/attach", resp, raw)
			}
			return fmt.Errorf("%s", e.Message)
		}
		return fmt.Errorf("attach refused: %s", resp.Status)
	}

	restore := rawMode()
	defer restore()
	fmt.Fprintf(os.Stderr, "\r\n[attached to %s · detach with your tmux prefix then d]\r\n", id)

	// The attach ends when the REMOTE side closes (tmux detach or the shell
	// exits), never when local stdin ends — a piped or ^D'd stdin must not
	// tear the session down before the guest has processed it.
	remoteDone := make(chan struct{})
	go func() {
		if n := br.Buffered(); n > 0 {
			b := make([]byte, n)
			_, _ = io.ReadFull(br, b)
			_, _ = os.Stdout.Write(b)
		}
		_, _ = io.Copy(os.Stdout, conn) // remote -> local, incl. bytes after the 101
		close(remoteDone)
	}()
	go func() {
		_, _ = io.Copy(conn, os.Stdin) // local -> remote
		// stdin ended: half-close the write side so the guest sees EOF but
		// its output still reaches us until it closes.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	<-remoteDone
	restore()
	fmt.Fprintf(os.Stderr, "\r\n[detached from %s]\r\n", id)
	return nil
}
