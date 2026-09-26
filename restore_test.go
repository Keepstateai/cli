package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// KS-052: the continuation is printed exactly; unknown is never success.
func TestARestorePrintsItsContinuationAndUnknownIsNeverSuccess(t *testing.T) {
	c := &forkCtl{continuation: "exact_runtime"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	dir := t.TempDir()
	env := fastEnv(cfg)
	out, errs, code := auditExec(t, bin, cfg, dir, env, "session", "restore", "fleetfrk", "--checkpoint", "ck_1", "--reason", "broke")
	if code != 0 || !strings.Contains(out, "continuation   exact_runtime") || !strings.Contains(out, "loaded the whole machine") {
		t.Fatalf("exact: %d\n%s%s", code, out, errs)
	}
	c.continuation = "unknown"
	out, errs, code = auditExec(t, bin, cfg, dir, env, "session", "restore", "fleetfrk", "--checkpoint", "ck_1", "--reason", "broke")
	if code != exitTemporary || !strings.Contains(out, "continuation   unknown") || !strings.Contains(errs, "UNKNOWN") || !strings.Contains(errs, "Remote work started: unknown") {
		t.Fatalf("unknown: %d\n%s%s", code, out, errs)
	}
	if _, _, code := auditExec(t, bin, cfg, dir, env, "session", "restore", "fleetfrk", "--reason", "broke"); code != exitUsage {
		t.Fatalf("no checkpoint: %d", code)
	}
}

// KS-052: the saved point's stack -- verified, not established, or
// incompatible with its named differences (refused, nothing restored).
func TestARestoreSaysWhetherTheStackWasVerified(t *testing.T) {
	c := &forkCtl{continuation: "exact_runtime", stack: "verified"}
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	restore := func() (string, string, int) {
		return auditExec(t, bin, cfg, t.TempDir(), env, "session", "restore", "fleetfrk", "--checkpoint", "ck_1", "--reason", "r")
	}
	if out, _, code := restore(); code != 0 || !strings.Contains(out, "stack          verified") {
		t.Fatalf("verified: %d\n%s", code, out)
	}
	c.stack = ""
	if out, _, code := restore(); code != 0 || !strings.Contains(out, "stack          NOT established") || strings.Contains(out, "stack          verified") {
		t.Fatalf("unknown: %d\n%s", code, out)
	}
	c.stack, c.continuation = "incompatible", "none"
	out, errs, code := restore()
	if code != exitConflict || !strings.Contains(out, "INCOMPATIBLE") || !strings.Contains(errs, "runner 2.1.240 != 2.1.251") || !strings.Contains(errs, "NOT restored") {
		t.Fatalf("incompatible: %d\n%s%s", code, out, errs)
	}
}
