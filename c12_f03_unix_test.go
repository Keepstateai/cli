//go:build unix

// KS-091 F03 (unix: the fixture plants a FIFO and symlinks): the service's
// hostile workspace against the client's upload selection (KS-026).
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrygrtc/cli/internal/c12"
)

// selectionWithin runs buildSelection, failing the test if it takes long:
// a selection that OPENED the fixture's FIFO would block here for ever.
func selectionWithin(t *testing.T, root string, overrides []string) (*selection, error) {
	t.Helper()
	type res struct {
		s   *selection
		err error
	}
	ch := make(chan res, 1)
	go func() { s, err := buildSelection(root, overrides); ch <- res{s, err} }()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("the selection blocked: it opened the FIFO")
	}
	return nil, nil
}

func TestC12F03HostileWorkspaceAgainstTheUploadSelection(t *testing.T) {
	dir := t.TempDir()
	root, entries, err := c12.BuildF03(dir, c12.DefaultSeed)
	if err != nil {
		t.Skipf("F03 cannot be built here: %v", err)
	}
	kinds := map[string]string{}
	for _, e := range entries {
		kinds[e.Path] = e.Kind
	}
	// the refusals come first, one at a time, each naming its path; the
	// FIFO is refused without being opened
	_, err = selectionWithin(t, root, nil)
	if err == nil || !strings.Contains(err.Error(), "pipe: not a regular file") {
		t.Fatalf("the FIFO was not refused by name: %v", err)
	}
	_ = os.Remove(filepath.Join(root, "pipe"))
	_, err = selectionWithin(t, root, nil)
	if err == nil || !strings.Contains(err.Error(), "over the 50 MiB limit") {
		t.Fatalf("the oversized file was not refused: %v", err)
	}
	_ = os.Remove(filepath.Join(root, "data", "huge.bin"))
	_, err = selectionWithin(t, root, nil)
	if !errors.Is(err, errSymlinks) || !strings.Contains(err.Error(), "link-inside") || !strings.Contains(err.Error(), "link-outside") {
		t.Fatalf("both links (inside and outside) must be refused by name: %v", err)
	}
	_ = os.Remove(filepath.Join(root, "link-inside"))
	_ = os.Remove(filepath.Join(root, "link-outside"))
	sel, err := selectionWithin(t, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	included := map[string]bool{}
	for _, f := range sel.Included {
		included[f.Path] = true
	}
	excluded := map[string]string{}
	for _, x := range sel.Excluded {
		excluded[x.Path] = x.Reason
	}
	isExcluded := func(p string) (string, bool) {
		for q := p; q != "." && q != ""; q = filepath.Dir(q) {
			if r, ok := excluded[filepath.ToSlash(q)]; ok {
				return r, true
			}
		}
		return "", false
	}
	for p, kind := range kinds {
		reason, ex := isExcluded(p)
		switch kind {
		case c12.KindOrdinary, c12.KindNestedTest, c12.KindUnicodeName, c12.KindRaceTarget:
			if !included[p] {
				t.Errorf("%s (%s) is not uploaded (excluded: %q)", p, kind, reason)
			}
		case c12.KindSensitive:
			if included[p] || !ex || !strings.Contains(reason, "sensitive") {
				t.Errorf("%s (sensitive) is not blocked as sensitive: included %v, reason %q", p, included[p], reason)
			}
		case c12.KindExcluded:
			if included[p] || !strings.HasPrefix(reason, "mandatory") {
				t.Errorf("%s is not a mandatory exclusion: %q", p, reason)
			}
		case c12.KindIgnoredDep:
			// under ks-upload-policy/2 a dependency or tool tree is excluded
			// by the verifier's SKIP_DIRS, and ignored files by .gitignore
			if included[p] || !(strings.HasPrefix(reason, "ignored") || strings.HasPrefix(reason, "dependency or tool tree")) {
				t.Errorf("%s (ignored dependency) is not excluded: included %v, %q", p, included[p], reason)
			}
		}
	}
	for _, f := range sel.Included {
		if strings.Contains(f.Path, "outside") || strings.Contains(f.Path, "secret") {
			t.Errorf("something outside the root was selected: %s", f.Path)
		}
	}
	// the Unicode name survives byte-exact
	if !included["docs/çalışma notları.md"] {
		t.Error("the Unicode name did not survive byte-exact")
	}
	// an explicit, exact override lifts one sensitive block and nothing else
	sel2, err := selectionWithin(t, root, []string{".env"})
	if err != nil {
		t.Fatal(err)
	}
	lifted := map[string]bool{}
	for _, f := range sel2.Included {
		lifted[f.Path] = true
	}
	if !lifted[".env"] || lifted["config/.env.production"] || lifted["keys/id_ed25519"] {
		t.Errorf("the override lifted the wrong set: .env %v, .env.production %v, id_ed25519 %v", lifted[".env"], lifted["config/.env.production"], lifted["keys/id_ed25519"])
	}
	// J09 race: after the preview, the selected file becomes a link leaving
	// the root; the old consent does not cover it
	before := selectionDigest(sel)
	if err := c12.SwapToOutsideLink(root); err != nil {
		t.Fatal(err)
	}
	after, err := selectionWithin(t, root, nil)
	if err == nil {
		t.Fatalf("the swapped target was selected (digest %s, before %s)", selectionDigest(after), before)
	}
	if !errors.Is(err, errSymlinks) || !strings.Contains(err.Error(), "src/swap-me.py") {
		t.Errorf("the swapped target is not refused by name: %v", err)
	}
}
