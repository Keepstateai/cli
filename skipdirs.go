// skipdirs.go: the verifier's SKIP_DIRS, vendored (verifier_skip_dirs.json,
// with testdata/skipdirs/SOURCE.json and a drift guard). One list serves
// the KS-072 discovery port and upload policy ks-upload-policy/2, so what
// is uploaded, what is discovered and what the verifier reads cannot
// diverge.
package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed verifier_skip_dirs.json
var verifierSkipDirsJSON []byte

// verifierSkipDirs is the vendored set.
var verifierSkipDirs = func() map[string]bool {
	var doc struct {
		Dirs []string `json:"dirs"`
	}
	if err := json.Unmarshal(verifierSkipDirsJSON, &doc); err != nil || len(doc.Dirs) == 0 {
		panic("verifier_skip_dirs.json is unreadable: the upload policy cannot be built")
	}
	m := map[string]bool{}
	for _, d := range doc.Dirs {
		m[d] = true
	}
	return m
}()
