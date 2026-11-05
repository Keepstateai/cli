// The plugin as herdr's marketplace publishes it: a static manifest in
// herdr/ that is the generated one at a released version (the release
// checks that version against its tag), no other file herdr would list,
// and ks herdr install, status and uninstall living with a plugin that
// herdr plugin install fetched from GitHub. No real herdr is run.
package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const herdrListCall = "|plugin|list|--json|--plugin|keepstate"

func publishedHerdrManifest(t *testing.T) (manifest, version string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("herdr", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version = "([^"]*)"$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("herdr/herdr-plugin.toml states no version:\n%s", b)
	}
	return string(b), m[1]
}

// The published manifest is the generated one at the version it states, so
// the panes and the action herdr's marketplace installs are the ones this
// client serves; that version is a release's (never 0.0.0-dev), and on a
// release run (KS_RELEASE_VERSION, the tag) it is the release's own.
func TestThePublishedHerdrManifestIsTheGeneratedOneAtTheReleaseVersion(t *testing.T) {
	if w := os.Getenv("KS_HERDR_MANIFEST_WRITE"); w != "" {
		// writes the manifest at the version given (the release's, while
		// making the release commit), then checks it as below
		if err := os.WriteFile(filepath.Join("herdr", "herdr-plugin.toml"), []byte(herdrPublishedManifest(strings.TrimPrefix(w, "v"))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, v := publishedHerdrManifest(t)
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(v) {
		t.Errorf("the published manifest's version %q is not a release version", v)
	}
	if want := herdrPublishedManifest(v); m != want {
		t.Errorf("herdr/herdr-plugin.toml differs from the generated manifest at %s; write it with KS_HERDR_MANIFEST_WRITE=<version> go test -run TestThePublishedHerdrManifest .:\n%s", v, want)
	}
	if rel := os.Getenv("KS_RELEASE_VERSION"); rel != "" && strings.TrimPrefix(rel, "v") != v {
		t.Errorf("herdr/herdr-plugin.toml states version %s; this release is %s. Set it before tagging: KS_HERDR_MANIFEST_WRITE=%s go test -run TestThePublishedHerdrManifest . (RELEASING.md)", v, rel, rel)
	}
	// it is the local manifest plus one build step that runs ks itself, so
	// herdr plugin install stops at once when ks is not on PATH
	if !strings.HasPrefix(m, herdrManifest(v)) || !strings.Contains(m, "\n[[build]]\ncommand = [\"ks\", \"herdr\", \"pane\", \"agents\", \"--help\"]\n") {
		t.Errorf("published manifest:\n%s", m)
	}
}

func TestTheReleaseChecksTheHerdrManifestAgainstItsTag(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "KS_RELEASE_VERSION: ${{ github.event_name == 'push' && github.ref_name || '' }}") {
		t.Error("release.yml's tests do not get the tag as KS_RELEASE_VERSION")
	}
	r, err := os.ReadFile("RELEASING.md")
	if err != nil || !strings.Contains(string(r), "herdr/herdr-plugin.toml") {
		t.Error("RELEASING.md does not say to set the herdr manifest's version")
	}
}

// herdr's marketplace lists every herdr-plugin.toml of a tagged repository,
// so the only one in this repository is the published one.
func TestTheOnlyHerdrPluginManifestInTheRepositoryIsThePublishedOne(t *testing.T) {
	var found []string
	_ = filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "herdr-plugin.toml" {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 1 || found[0] != filepath.Join("herdr", "herdr-plugin.toml") {
		t.Errorf("herdr-plugin.toml files: %v", found)
	}
}

func TestTheHerdrPluginReadmeSaysWhatItRunsAndNeeds(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("herdr", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	r := string(b)
	for _, need := range []string{
		"herdr plugin install Keepstateai/cli/herdr",
		"runs only `ks`",
		"`ks` on your PATH",
		"KeepState account",
		"[ui.toast]",
		`delivery = "herdr"`,
		"[ui.sidebar.agents]",
		`"state_text"`,
		"herdr plugin uninstall keepstate",
	} {
		if !strings.Contains(r, need) {
			t.Errorf("herdr/README.md lacks %q", need)
		}
	}
}

// herdr's answer to plugin list --json for a plugin herdr plugin install
// fetched from this repository (herdr 0.9.5's shape).
const herdrGitHubListed = `{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[{"plugin_id":"keepstate","name":"KeepState","version":"0.1.38","min_herdr_version":"0.9.2","manifest_path":"/h/.config/herdr/plugins/github-installations/keepstate/abc/herdr/herdr-plugin.toml","plugin_root":"/h/.config/herdr/plugins/github-installations/keepstate/abc/herdr","enabled":true,"source":{"kind":"github","owner":"Keepstateai","repo":"cli","subdir":"herdr","resolved_commit":"0123456789abcdef0123456789abcdef01234567","managed_path":"/h/.config/herdr/plugins/github-installations/keepstate/abc","installed_unix_ms":1}}]}}`

func TestHerdrInstallLeavesAGitHubInstalledPluginAlone(t *testing.T) {
	bin, cfg := herdrClientFixture(t)
	dir, _, log := fakeHerdrCLI(t)
	if err := os.WriteFile(filepath.Join(dir, "list.out"), []byte(herdrGitHubListed+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(append(fastEnv(cfg), noHerdrEnv()...), "PATH="+dir+":"+os.Getenv("PATH"))
	pluginDir := filepath.Join(cfg, "keepstate", "herdr-plugin")

	// install: nothing written, nothing linked over it, exit 0
	for _, args := range [][]string{{"herdr", "install", "--yes"}, {"herdr", "install", "--no-input"}} {
		_ = os.Remove(log)
		out := runOKEnv(t, bin, cfg, env, args...)
		if calls := herdrCalls(t, log); len(calls) != 1 || calls[0] != herdrListCall {
			t.Errorf("%v: herdr calls %v", args, calls)
		}
		if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
			t.Errorf("%v wrote %s", args, pluginDir)
		}
		if !strings.Contains(out, "already installed from GitHub (Keepstateai/cli/herdr at 0123456789ab), version 0.1.38; nothing was written or linked") || !strings.Contains(out, "herdr plugin update keepstate") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
	js := runOKEnv(t, bin, cfg, env, "herdr", "install", "--yes", "--json")
	if !strings.Contains(js, `"linked":false`) || !strings.Contains(js, `"source":"github"`) {
		t.Errorf("install --json:\n%s", js)
	}

	// status: where it came from and its version
	out := runOKEnv(t, bin, cfg, env, "herdr", "status")
	if !strings.Contains(out, "plugin         installed from GitHub: Keepstateai/cli/herdr at 0123456789ab, version 0.1.38") || strings.Contains(out, "not installed") {
		t.Errorf("status:\n%s", out)
	}
	js = runOKEnv(t, bin, cfg, env, "herdr", "status", "--json")
	for _, need := range []string{`"source":"github"`, `"version":"0.1.38"`, `"repo":"Keepstateai/cli/herdr"`, `"commit":"0123456789abcdef0123456789abcdef01234567"`} {
		if !strings.Contains(js, need) {
			t.Errorf("status --json lacks %s:\n%s", need, js)
		}
	}

	// uninstall: herdr's own uninstall, which owns the fetched copy
	_ = os.Remove(log)
	if err := os.WriteFile(filepath.Join(dir, "uninstall.out"), []byte("Uninstalled keepstate.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = runOKEnv(t, bin, cfg, env, "herdr", "uninstall")
	if calls := herdrCalls(t, log); len(calls) != 2 || calls[0] != herdrListCall || calls[1] != "|plugin|uninstall|keepstate" {
		t.Errorf("uninstall calls %v", calls)
	}
	if !strings.Contains(out, "Uninstalled the KeepState plugin that herdr installed from GitHub.") {
		t.Errorf("uninstall:\n%s", out)
	}
}

// With nothing in herdr, status names both ways in.
func TestHerdrStatusWithNoPluginNamesBothWaysToInstall(t *testing.T) {
	bin, cfg := herdrClientFixture(t)
	dir, _, _ := fakeHerdrCLI(t)
	env := append(append(fastEnv(cfg), noHerdrEnv()...), "PATH="+dir+":"+os.Getenv("PATH"))
	out := runOKEnv(t, bin, cfg, env, "herdr", "status")
	if !strings.Contains(out, "plugin         not installed: herdr plugin install Keepstateai/cli/herdr, or ks herdr install") {
		t.Errorf("status:\n%s", out)
	}
}
