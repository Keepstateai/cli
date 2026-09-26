// KS-037: the status shows its age and a stale notice past 15 s, and
// --watch exits cleanly on Ctrl-C without changing anything (VER-037-2).
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestObservedAgeAndStaleness(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 30, 0, time.UTC)
	if age, stale := observedAge("2026-09-26T12:00:20Z", now); stale || age != "observed 10s ago" {
		t.Errorf("fresh: %q %v", age, stale)
	}
	if age, stale := observedAge("2026-09-26T12:00:00Z", now); !stale || age != "observed 30s ago" {
		t.Errorf("stale: %q %v", age, stale)
	}
	if _, stale := observedAge("", now); !stale {
		t.Error("an unknown time must read stale, never fresh")
	}
}

func TestStatusWatchStopsCleanlyOnCtrlC(t *testing.T) {
	c, bin, cfg := agentFixture(t)
	cmd := exec.Command(bin, "agent", "status", "main", "--session", agentSessionShort, "--watch")
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	var so, se lockedBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for strings.Count(so.String(), "activity") < 2 {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("watch did not refresh:\n%s%s", so.String(), se.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(so.String(), "STALE") && !strings.Contains(so.String(), "ago") {
		t.Errorf("no age shown:\n%s", so.String())
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch exited %v\n%s", err, se.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("watch did not stop on Ctrl-C")
	}
	if !strings.Contains(se.String(), "stopped watching; nothing on the agent changed") {
		t.Errorf("stop line missing:\n%s", se.String())
	}
	for _, r := range c.seen() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("watching sent %q", r)
		}
	}
}
