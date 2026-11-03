// herdr.go: the KeepState plugin for herdr, a terminal workspace for coding
// agents. It is linked locally; nothing is downloaded:
//
//	ks herdr install     writes the plugin's manifest under this client's
//	                     config, shows it, asks, then runs
//	                     herdr plugin link <dir>
//	ks herdr uninstall   herdr plugin unlink keepstate, then removes the
//	                     files install wrote
//	ks herdr status      the herdr found, its version, and whether the
//	                     manifest is written and current
//
// The plugin's panes and its action run ks itself (herdr_pane.go): a list
// of your agents with their status, saved points and receipts, and the
// receipts of your Cruise jobs, followed as they run. A herdr plugin pane is
// a terminal program; herdr's plugin v1 has no other kind of list.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	herdrPluginID   = "keepstate"
	herdrMinVersion = "0.9.2"
	herdrCmdTimeout = 30 * time.Second
)

func herdrPluginDir() string { return filepath.Join(configDir(), "herdr-plugin") }

var semverShape = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// herdrPluginVersion is this client's version as the manifest states it;
// a build without a release version reads 0.0.0-dev.
func herdrPluginVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if !semverShape.MatchString(v) {
		return "0.0.0-dev"
	}
	return v
}

// herdrManifest is the plugin's herdr-plugin.toml. Pane and action ids carry
// no dots (herdr's rule for local ids); every command is an argv array that
// runs ks itself, with no shell.
func herdrManifest(v string) string {
	return `id = "keepstate"
name = "KeepState"
version = "` + herdrPluginVersion(v) + `"
min_herdr_version = "` + herdrMinVersion + `"
description = "Your KeepState sessions, agents, saved points and receipts"
platforms = ["linux", "macos"]

[[panes]]
id = "agents"
title = "KeepState"
placement = "overlay"
command = ["ks", "herdr", "pane", "agents"]

[[panes]]
id = "receipts"
title = "KeepState receipts"
placement = "split"
command = ["ks", "herdr", "pane", "receipts"]

[[actions]]
id = "console"
title = "Open the KeepState console"
contexts = ["workspace"]
command = ["ks", "herdr", "action", "console"]
`
}

// herdrBinary is the herdr to run: the pane's own when this runs inside
// herdr, else the one on PATH.
func herdrBinary() (string, bool) {
	if bin, _, ok := herdrPane(); ok {
		return bin, true
	}
	if p, err := exec.LookPath("herdr"); err == nil {
		return p, true
	}
	return "", false
}

var errNoHerdr = &cliError{Code: exitFailed, Kind: "herdr_missing", Message: "herdr is not installed on this machine; nothing was linked", NextAction: "install herdr, then run ks herdr install"}

// herdrWords is what herdr printed, fit to show: one line, no control
// characters, at most 2000 bytes.
func herdrWords(raw string) string { return oscText(strings.TrimSpace(sanitize(raw)), 2000) }

// runHerdr runs herdr once within limit and answers what it printed, as it
// printed it.
func runHerdr(bin string, limit time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var outb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &outb
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("herdr did not answer within %s", limit)
	}
	return outb.String(), err
}

func herdrInstall(inv *Invocation) {
	bin, ok := herdrBinary()
	if !ok {
		fail(errNoHerdr)
	}
	dir := herdrPluginDir()
	path := filepath.Join(dir, "herdr-plugin.toml")
	manifest := herdrManifest(version)
	if !out.json {
		fmt.Printf("This writes %s:\n\n", path)
		for _, l := range strings.Split(strings.TrimRight(manifest, "\n"), "\n") {
			fmt.Println("  " + l)
		}
		fmt.Printf("\nthen runs: %s plugin link %s\n", bin, dir)
		fmt.Println("Its panes and its action run ks on this machine; nothing is downloaded.")
	}
	switch {
	case out.yes:
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: "linking the plugin into herdr needs your yes; nothing was written or linked", NextAction: "ks herdr install --yes"})
	default:
		fmt.Fprint(os.Stderr, "Link it into herdr? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "not linked; nothing was written"})
		}
	}
	if err := writeFileAtomic(path, []byte(manifest)); err != nil {
		die(fmt.Errorf("the plugin manifest could not be written to %s: %w; nothing was linked", path, err))
	}
	raw, err := runHerdr(bin, herdrCmdTimeout, "plugin", "link", dir)
	said := herdrWords(raw)
	if err != nil {
		fail(&cliError{Code: exitFailed, Kind: "herdr_link_failed", WorkStarted: workYes,
			Message:    "the manifest is written, but herdr did not link it: " + notRecorded(said) + " (" + sanitize(err.Error()) + ")",
			NextAction: "ks herdr status"})
	}
	emit(map[string]any{"linked": true, "dir": dir, "manifest": path, "herdr": bin, "herdr_said": said}, func() {
		fmt.Println("Linked. In herdr, open it with: herdr plugin pane open --plugin keepstate --entrypoint agents")
		fmt.Println("Cruise receipts: herdr plugin pane open --plugin keepstate --entrypoint receipts")
	})
}

// writeFileAtomic writes path through a temporary file in its directory.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ks-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func herdrUninstall(inv *Invocation) {
	dir := herdrPluginDir()
	said, unlinkErr := "", error(nil)
	bin, ok := herdrBinary()
	if ok {
		var raw string
		raw, unlinkErr = runHerdr(bin, herdrCmdTimeout, "plugin", "unlink", herdrPluginID)
		said = herdrWords(raw)
	}
	_, statErr := os.Stat(dir)
	removed := false
	if statErr == nil {
		if err := os.RemoveAll(dir); err != nil {
			die(fmt.Errorf("%s could not be removed: %w", dir, err))
		}
		removed = true
	}
	emit(map[string]any{"unlinked": ok && unlinkErr == nil, "removed": removed, "dir": dir, "herdr_said": said}, func() {
		switch {
		case !ok:
			fmt.Println("herdr is not installed on this machine, so there was nothing to unlink.")
		case unlinkErr != nil:
			fmt.Println("herdr did not unlink the plugin (it may not have been linked): " + notRecorded(said))
		default:
			fmt.Println("Unlinked the KeepState plugin from herdr.")
		}
		if removed {
			fmt.Println("Removed " + dir + ".")
		} else {
			fmt.Println("There were no plugin files to remove.")
		}
	})
}

func herdrStatus(inv *Invocation) {
	doc := map[string]any{"plugin_dir": herdrPluginDir()}
	var lines []string
	bin, ok := herdrBinary()
	if ok {
		raw, err := runHerdr(bin, 10*time.Second, "--version")
		v := herdrWords(raw)
		if err != nil || v == "" {
			v = "unavailable"
		}
		doc["herdr"], doc["herdr_version"] = bin, v
		lines = append(lines, "herdr          "+bin+" ("+v+"); the plugin needs "+herdrMinVersion+" or later")
	} else {
		doc["herdr"] = nil
		lines = append(lines, "herdr          not installed on this machine")
	}
	path := filepath.Join(herdrPluginDir(), "herdr-plugin.toml")
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		doc["manifest"] = "absent"
		lines = append(lines, "plugin         not installed: ks herdr install")
	case err != nil:
		doc["manifest"] = "unreadable"
		lines = append(lines, "plugin         "+path+" could not be read: "+sanitize(err.Error()))
	case string(b) == herdrManifest(version):
		doc["manifest"] = "current"
		lines = append(lines, "plugin         "+path+" (this client's)")
	default:
		doc["manifest"] = "other"
		lines = append(lines, "plugin         "+path+" was written by another ks version: ks herdr install writes this one's")
	}
	if _, err := exec.LookPath("ks"); err != nil {
		doc["ks_on_path"] = false
		lines = append(lines, "ks on PATH     no: herdr runs the plugin's commands as ks, so put ks on PATH")
	} else {
		doc["ks_on_path"] = true
	}
	if _, pane, in := herdrPane(); in {
		doc["pane"] = pane
		lines = append(lines, "this terminal  a herdr pane ("+sanitize(pane)+"); streams report to herdr")
	}
	emit(doc, func() {
		for _, l := range lines {
			fmt.Println(l)
		}
	})
}

// consoleAgentsURL is the console's agents page, at one agent when fragment
// names one (#agent/<session>/<agent>).
func consoleAgentsURL(cr hostedCreds, fragment string) string {
	u := strings.TrimRight(cr.CTL, "/") + "/console/agents"
	if strings.HasPrefix(fragment, "#agent/") && oscText(fragment, 300) == fragment && !strings.ContainsAny(fragment, " \"'<>") {
		u += fragment
	}
	return u
}

// openBrowser opens url with the system's opener, in the background; false
// when there is none.
func openBrowser(url string) bool {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	p, err := exec.LookPath(opener)
	if err != nil {
		return false
	}
	cmd := exec.Command(p, url)
	if cmd.Start() != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// herdrActionConsole is the plugin's action: the console, in the browser.
func herdrActionConsole(cr hostedCreds, inv *Invocation) {
	u := consoleAgentsURL(cr, "")
	opened := openBrowser(u)
	emit(map[string]any{"url": u, "opened": opened}, func() {
		if opened {
			fmt.Println("Opened the KeepState console: " + u)
		} else {
			fmt.Println("Open the KeepState console: " + u)
		}
	})
}

// openAgentInHerdrTab opens a new herdr tab and runs the agent's window in
// it: tab create, then pane run on the tab's root pane. The command line is
// built only from names safe to type as they are.
func openAgentInHerdrTab(bin, name, session string) (string, error) {
	if !agentNameSafe.MatchString(name) || !agentNameSafe.MatchString(session) {
		return "", fmt.Errorf("the name %q cannot be typed into a terminal as it is; open it with ks agent open in a terminal", sanitize(name))
	}
	said, err := runHerdr(bin, 10*time.Second, "tab", "create", "--label", name, "--focus")
	if err != nil {
		return "", fmt.Errorf("herdr did not open a tab: %s", notRecorded(herdrWords(said)))
	}
	var resp struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	pane := ""
	if json.Unmarshal([]byte(said), &resp) == nil {
		pane = resp.Result.RootPane.PaneID
	}
	if pane == "" || oscText(pane, 128) != pane || strings.ContainsAny(pane, " \t") {
		return "", fmt.Errorf("herdr opened a tab but named no pane to run the agent in")
	}
	if said, err := runHerdr(bin, 10*time.Second, "pane", "run", pane, "ks agent open "+name+" --session "+session); err != nil {
		return pane, fmt.Errorf("herdr did not run the agent's window in the new tab: %s", notRecorded(herdrWords(said)))
	}
	return pane, nil
}
