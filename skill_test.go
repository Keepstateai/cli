// ks skill and ks --skill: the embedded manual, printed with no network,
// and kept to the publishing rules: no tracker ids, no person names (every
// capitalised word is one of a known vocabulary), no em or en dashes.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSkillPrintsTheManualWithNoNetwork(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ks-under-test")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"--skill"}, {"skill"}} {
		cmd := exec.Command(bin, args...)
		// signed out, and a control plane that cannot be reached: the manual
		// needs neither
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir(), "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1")
		out, err := cmd.Output()
		if err != nil || string(out) != skillManual {
			t.Fatalf("ks %v: %v\n%s", args, err, out)
		}
	}
}

func TestSkillManualCoversTheVerbsAndTheRules(t *testing.T) {
	for _, want := range []string{"ks agent run", "ks agent send", "ks agent wait", "ks agent read", "ks agent templates", "ks --skill",
		"metered", "--children all", "status", "blocked", "DATA", "Never approve on a person's behalf", "Keys never go in arguments", "The parent rule",
		"saved and paused, never deleted", "Only an observed exit does"} {
		if !strings.Contains(strings.Join(strings.Fields(skillManual), " "), want) {
			t.Errorf("the manual does not say %q", want)
		}
	}
}

func TestSkillManualKeepsThePublishingRules(t *testing.T) {
	if strings.ContainsAny(skillManual, "\u2014\u2013") {
		t.Error("the manual has an em or en dash")
	}
	if m := regexp.MustCompile(`\b[A-Z]{2,9}-\d+\b`).FindAllString(skillManual, -1); len(m) > 0 {
		t.Errorf("the manual has tracker-shaped ids: %v", m)
	}
	known := map[string]bool{}
	for _, w := range strings.Fields("AGENT Also An Before Child Closing DATA Delegate Do Each Every It Its JSON KEY KeepState Keys NAME NEW Name Never On Only PERSON ROLE Read Reference Review Session Start Status Talk Tell That The They This What When Without You Your") {
		known[w] = true
	}
	for _, w := range regexp.MustCompile(`\b[A-Z][a-zA-Z]+\b`).FindAllString(skillManual, -1) {
		if !known[w] {
			t.Errorf("the manual has %q, a capitalised word outside its vocabulary (a name of a person never goes in it); add it here only if it is not one", w)
		}
	}
}
