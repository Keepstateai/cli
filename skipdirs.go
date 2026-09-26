// skipdirs.go: the verifier's SKIP_DIRS (judge/checks.py), the directory
// names the verifier never reads. One list serves the discovery port and
// upload policy ks-upload-policy/2, so what is uploaded, what is discovered
// and what the verifier reads cannot diverge. The vendored record is
// testdata/skipdirs/verifier_skip_dirs.json (with SOURCE.json); a test keeps
// this list equal to it, and a drift guard compares both with the service.
package main

var verifierSkipDirList = []string{".claude", ".git", ".hg", ".keepstate", ".mypy_cache", ".nox", ".pytest_cache", ".ruff_cache", ".svn", ".tox", ".venv", "__pycache__", "build", "dist", "node_modules", "venv"}

// verifierSkipDirs is the list as a set.
var verifierSkipDirs = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range verifierSkipDirList {
		m[d] = true
	}
	return m
}()
