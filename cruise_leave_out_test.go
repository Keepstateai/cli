// cruise_leave_out_test: the leave-out choice's terminal half. When Cruise's check
// names project test files that stop it and can be left out, the console
// offers one button; here the same choice is `ks cruise tests show`'s next
// line and `ks cruise draft set --leave-out` (or --keep-all-tests to put them
// back), and `ks cruise draft show` names what the check leaves out.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCruiseTestsShowNamesTheLeaveOutChoiceAndDraftSetSendsIt(t *testing.T) {
	f, bin, cfg := startTestsFake(t)
	f.mu.Lock()
	f.state = "refused"
	f.refusal = map[string]any{"code": "ks_repository_tests_unloadable", "message": "the repository's own tests stop pytest before any test runs",
		"leave_out": map[string]any{"label": "Leave out these 2 files and continue", "note": "the files stay in your project, unchanged and protected",
			"files": []any{map[string]any{"path": "test_browser_multi.py", "why": "ModuleNotFoundError: No module named 'playwright'"},
				map[string]any{"path": "test_browser_simple.py", "why": "ModuleNotFoundError: No module named 'playwright'"}}}}
	f.mu.Unlock()
	out, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "tests", "show", fakeDraftID)
	if code != 0 || !strings.Contains(out, "can leave out test_browser_multi.py (ModuleNotFoundError: No module named 'playwright')") ||
		!strings.Contains(out, "next: ks cruise draft set "+fakeDraftID+" --leave-out test_browser_multi.py test_browser_simple.py, then ks cruise tests generate "+fakeDraftID) {
		t.Fatalf("show: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--leave-out", "test_browser_multi.py", "test_browser_simple.py")
	var sent map[string]any
	_ = json.Unmarshal(f.patched, &sent)
	if code != 0 || string(mustJSONcli(sent["leave_out"])) != `["test_browser_multi.py","test_browser_simple.py"]` || sent["expected_revision"] != float64(4) ||
		!strings.Contains(out, "left out     test_browser_multi.py (ModuleNotFoundError: No module named 'playwright'): kept as it is and not run by the check") {
		t.Fatalf("set --leave-out: exit %d sent %s\n%s%s", code, f.patched, out, errs)
	}
	if _, _, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--keep-all-tests"); code != 0 {
		t.Fatalf("set --keep-all-tests: exit %d", code)
	}
	_ = json.Unmarshal(f.patched, &sent)
	if lo, ok := sent["leave_out"].([]any); !ok || len(lo) != 0 {
		t.Fatalf("--keep-all-tests sent %s", f.patched)
	}
	if _, errs, code := ksIn(t, bin, cfg, t.TempDir(), "cruise", "draft", "set", fakeDraftID, "--keep-all-tests", "--leave-out", "a.py"); code == 0 || !strings.Contains(errs, "say opposite things") {
		t.Fatalf("both flags: exit %d %s", code, errs)
	}
}

func mustJSONcli(v any) []byte { b, _ := json.Marshal(v); return b }
