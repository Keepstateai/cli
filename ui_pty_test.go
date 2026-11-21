//go:build unix

package main

import (
	"regexp"
	"strings"
	"testing"
)

// At a real terminal (a pty) with colour on, a failure is the designed block;
// with NO_COLOR or --plain at the same terminal it is the contract lines. The
// fake control plane is local; nothing here reaches the service.
func TestErrorBlockOnATerminalAndContractLinesWhenPlain(t *testing.T) {
	srv := goldenServer()
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	base := []string{"XDG_CONFIG_HOME=" + cfg, "HOME=" + t.TempDir(), "TERM=xterm-256color", "COLORTERM=truecolor", "CI=", "NO_COLOR=", "KS_COLOR="}
	for _, cols := range []int{80, 60, 40} {
		got := inPTY(t, base, cols, 24, bin, "meter", "nope")
		bare := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(got, "")
		if !strings.Contains(bare, "✗ ") || !strings.Contains(bare, "│ ") || !strings.Contains(got, "\x1b[") {
			t.Errorf("%d columns: no designed block:\n%q", cols, got)
		}
		if strings.Contains(got, "error:") || strings.Contains(got, "Remote work started") {
			t.Errorf("%d columns: contract lines in the designed block:\n%q", cols, got)
		}
		for _, l := range strings.Split(strings.ReplaceAll(got, "\r", ""), "\n") {
			if displayWidth(regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(l, "")) > cols {
				t.Errorf("%d columns: %q is %d wide", cols, l, displayWidth(l))
			}
		}
	}
	for _, mode := range [][]string{{"NO_COLOR=1"}, nil} {
		args := []string{"meter", "nope"}
		if mode == nil {
			args = append(args, "--plain")
		}
		got := inPTY(t, append(append([]string{}, base...), mode...), 80, 24, bin, args...)
		if !strings.Contains(got, "error: ") || !strings.Contains(got, "Remote work started: no.") || strings.Contains(got, "\x1b[") {
			t.Errorf("%v: not the plain contract:\n%q", mode, got)
		}
	}
}
