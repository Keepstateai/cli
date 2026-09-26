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
