// project_defaults.go: a repository's .keepstate/project.json as a
// PROPOSAL, trusted only by a person (KS-039), and a session's idle policy.
//
//	ks project configure [--project ID] [--file PATH] [--confirm DIGEST]
//	ks session idle <session>
//
// The file is somebody's statement about defaults (a profile, key ALIASES,
// limits, adviser policy, idle settings). It is sent to the service as a
// proposal, which stores it with its digest and the exact changes it would
// make and authorizes nothing. The client shows those changes and trusts
// that digest only when the person confirms it (typed, or --confirm with
// the digest shown); --yes never trusts. Anything that looks like key
// material is refused here and never sent.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var keyMaterial = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{8,}|ks_sk_|ksk_[a-z0-9]+|-----BEGIN|AKIA[0-9A-Z]{12,}|xox[bap]-|gh[pousr]_[A-Za-z0-9]{20,}|bearer\s+[a-z0-9._-]{8,})`)

func projectOf(cr hostedCreds, inv *Invocation) string {
	if p := strings.TrimSpace(inv.Str("project")); p != "" {
		return p
	}
	sess := agentSession(cr, inv)
	var env struct {
		Data struct {
			ProjectID string `json:"project_id"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess)), nil, &env); err != nil {
		die(err)
	}
	if env.Data.ProjectID == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("session %s belongs to no project; name one with --project", sess.ShortID)})
	}
	return env.Data.ProjectID
}

func hostedProjectConfigure(cr hostedCreds, inv *Invocation) {
	path := inv.Str("file")
	if path == "" {
		root, err := projectRoot()
		if err != nil {
			die(err)
		}
		path = filepath.Join(root, ".keepstate", "project.json")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("no project defaults file at %s; nothing was proposed", path)})
	}
	if !fi.Mode().IsRegular() || fi.Size() > 64<<10 {
		fail(integrity("defaults_file_refused", path+" is not a regular file of at most 64 KiB (a link is never followed); nothing was proposed"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		die(err)
	}
	if keyMaterial.Match(raw) {
		fail(integrity("key_material", path+" contains something that looks like key material; defaults hold key ALIASES only, and nothing was sent"))
	}
	if !json.Valid(raw) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: path + " is not JSON; nothing was proposed"})
	}
	pid := projectOf(cr, inv)
	var env struct {
		Data struct {
			ID      string `json:"id"`
			Digest  string `json:"digest"`
			State   string `json:"state"`
			Changes []struct {
				Field string `json:"field"`
				From  any    `json:"from"`
				To    any    `json:"to"`
			} `json:"changes"`
			Note string `json:"note"`
		} `json:"data"`
	}
	base := "/api/v2/projects/" + url.PathEscape(pid) + "/defaults"
	if err := hostedCall(cr, "POST", base+"/proposals", map[string]any{"config": json.RawMessage(raw), "source": "repository"}, &env); err != nil {
		die(err)
	}
	p := env.Data
	hexd := strings.TrimPrefix(p.Digest, "sha256:")
	fmt.Fprintf(os.Stderr, "proposal %s from %s for project %s (digest %s); it changes nothing until trusted:\n", p.ID, path, pid, short(hexd))
	if len(p.Changes) == 0 {
		fmt.Fprintln(os.Stderr, "  no change from the project's current defaults")
	}
	for _, c := range p.Changes {
		from, _ := json.Marshal(c.From)
		to, _ := json.Marshal(c.To)
		fmt.Fprintf(os.Stderr, "  %-16s %s -> %s\n", visible(sanitize(c.Field)), visible(sanitize(string(from))), visible(sanitize(string(to))))
	}
	given := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(inv.Str("confirm"))), "sha256:")
	matches := func(s string) bool {
		s = strings.TrimPrefix(strings.TrimSpace(s), "sha256:")
		return s == hexd || s == short(hexd)
	}
	trusted := false
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not this proposal's digest (%s); it was proposed and NOT trusted", given, short(hexd))})
		}
		trusted = true
	case out.noInput || !stdinIsTerminal():
	default:
		fmt.Fprintf(os.Stderr, "type the digest (%s) to trust these defaults for new sessions, or anything else to leave them proposed: ", short(hexd))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		trusted = matches(line)
	}
	if !trusted {
		emit(map[string]any{"proposal": p, "trusted": false}, func() {
			fmt.Printf("proposed, NOT trusted: nothing changed. Trust exactly these changes: ks project configure --project %s --confirm %s\n", pid, short(hexd))
		})
		return
	}
	var tr struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", base+"/trust", map[string]any{"proposal_id": p.ID, "expected_digest": p.Digest}, &tr); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && (he.Type == "ks_digest_mismatch" || he.Type == "ks_defaults_not_proposed") {
			ce := classify(err)
			ce.Message = "nothing was trusted: " + ce.Message
			die(ce)
		}
		die(err)
	}
	emit(map[string]any{"proposal": p, "trusted": true, "trust": tr.Data}, func() {
		fmt.Printf("trusted: proposal %s (digest %s) is the project's defaults for sessions created from now on; existing sessions are unchanged\n", p.ID, short(hexd))
	})
}

func hostedSessionIdle(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var env struct {
		Data struct {
			Enabled        bool   `json:"enabled"`
			IdleMinutes    int    `json:"idle_minutes"`
			WarningSeconds int    `json:"warning_seconds"`
			State          string `json:"state"`
			IdleSince      string `json:"idle_since"`
			WarnedAt       string `json:"warned_at"`
			ParkAt         string `json:"park_at"`
			DeferredReason string `json:"deferred_reason"`
			Note           string `json:"note"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(agentSessionID(sess))+"/idle", nil, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		if !d.Enabled {
			fmt.Printf("session %s: idle parking is off (%s)\n", sess.ShortID, sanitize(d.Note))
			return
		}
		fmt.Printf("session %s: parks after %d min idle, with a %d s warning; an idle session is saved and paused, never killed\n", sess.ShortID, d.IdleMinutes, d.WarningSeconds)
		line := "  now      " + figure(d.State)
		if d.ParkAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, d.ParkAt); err == nil {
				left := time.Until(t).Round(time.Second)
				if left < 0 {
					left = 0
				}
				line += fmt.Sprintf(", parks at %s (in %s)", d.ParkAt, left)
			}
		}
		if d.DeferredReason != "" {
			line += ", deferred: " + sanitize(d.DeferredReason)
		}
		fmt.Println(line)
		if d.IdleSince != "" {
			fmt.Printf("  idle since %s\n", d.IdleSince)
		}
		if d.Note != "" {
			fmt.Printf("  %s\n", sanitize(d.Note))
		}
	})
}
