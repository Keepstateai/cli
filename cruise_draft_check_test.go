// cruise_draft_check_test: BACKLOG-493's terminal half. On the "I have tests"
// path the service checks a draft's own tests before approval, and the
// console offers leaving out the files that stop the check. In the terminal:
// `ks cruise draft check` asks for the check, waits for its answer, names the
// files with why and prints the way on; `ks cruise draft show` says it.
package main

import (
	"strings"
	"testing"
)

func TestCruiseDraftCheckNamesTheFilesAndTheWayOn(t *testing.T) {
	f, bin, cfg := startTestsFake(t)
	f.mu.Lock()
	f.run = map[string]any{"id": "chk_1", "state": "refused", "current": true, "code": "ks_repository_tests_unloadable",
		"message": "your project's own tests stop pytest before any test runs",
		"leave_out": map[string]any{"label": "Leave out these 2 files and continue", "files": []any{
			map[string]any{"path": "test_browser_multi.py", "why": "ModuleNotFoundError: No module named 'playwright'"},
			map[string]any{"path": "test_browser_simple.py", "why": "ModuleNotFoundError: No module named 'playwright'"}}}}
	f.mu.Unlock()
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "check", fakeDraftID)
	if code == 0 || f.checked != 1 || !strings.Contains(out, "ks_repository_tests_unloadable") ||
		!strings.Contains(out, "can leave out test_browser_multi.py (ModuleNotFoundError: No module named 'playwright')") ||
		!strings.Contains(out, "next: ks cruise draft set "+fakeDraftID+" --leave-out test_browser_multi.py test_browser_simple.py, then ks cruise draft check "+fakeDraftID) {
		t.Fatalf("check, refused: exit %d asked %d\n%s%s", code, f.checked, out, errs)
	}
	f.mu.Lock()
	f.run = map[string]any{"id": "chk_2", "state": "loads", "current": true, "tests": 7}
	f.mu.Unlock()
	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "check", fakeDraftID)
	if code != 0 || !strings.Contains(out, "they load in Cruise's check: 7 ran") || !strings.Contains(out, "next: ks cruise draft approve "+fakeDraftID) {
		t.Fatalf("check, loads: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "show", fakeDraftID)
	if code != 0 || !strings.Contains(out, "your tests   they load in Cruise's check: 7 ran") {
		t.Fatalf("show: exit %d\n%s%s", code, out, errs)
	}
}
