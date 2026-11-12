// agent_files.go: ks agent files, an agent's reach into the files of the
// agent a person connected it to (the service's agent file access and its setting).
//
//	ks agent files list <adviser> [path]       a folder of the adviser's workspace
//	ks agent files get <adviser> <path>        one file, verified, into .shared-in/
//	ks agent files log                         every ask made and every ask received
//	ks agent files allow <adviser> read|none   what the connection allows
//
// The permission is the connection a person made (ks adviser connect), in
// one account and one direction, and it allows read unless a person turned it
// to none. The service is the only courier: the asker is never given a path
// into another machine, and a fetch is automatic (nobody is asked) and
// journalled (ks agent files log). The path rules are ks file's: relative,
// no .., no host path; a link, a secret name or a file holding a credential
// is refused by the service. A file is at most 250 MiB and a connection may
// fetch 1 GiB an hour.
//
// A file lands in .shared-in/<adviser session>-<adviser agent>/<path> under
// the current folder unless --to names another new file, so what an agent
// fetched is never mixed with its own files; nothing is ever replaced.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// sharedInDir is where fetched files land by default.
const sharedInDir = ".shared-in"

type agentFileFetch struct {
	ID      string `json:"id"`
	GrantID string `json:"grant_id"`
	Asker   string `json:"asker_agent_id"`
	Adviser string `json:"adviser_agent_id"`
	Op      string `json:"op"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256,omitempty"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	At      string `json:"at"`
	AskName string `json:"asker_name,omitempty"`
	AdvName string `json:"adviser_name,omitempty"`
}

// agentFilesAsker answers the agent that asks (--agent, default main, in the
// session --session names or the project's binding) and the session it is in.
func agentFilesAsker(cr hostedCreds, inv *Invocation) (agentRow, inventoryRow) {
	sess := agentSession(cr, inv)
	name := inv.Str("agent")
	if name == "" {
		name = "main"
	}
	a, err := resolveAgent(cr, sess, name)
	if err != nil {
		die(err)
	}
	return a, sess
}

func adviserFilesBase(asker, adviser agentRow) string {
	return "/api/v2/agents/" + url.PathEscape(asker.ID) + "/advisers/" + url.PathEscape(adviser.ID) + "/files"
}

// filesRefusal says what a refusal of the file reach means and what to do.
func filesRefusal(err error, adviserArg string, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_no_connection":
		ce.Code = exitFailed
		ce.NextAction = fmt.Sprintf("ks adviser connect %s --session %s (a person connects two agents; the other direction needs a connection of its own)", adviserArg, sess.ShortID)
	case "ks_files_not_allowed":
		ce.Code = exitConflict
		ce.NextAction = fmt.Sprintf("ks agent files allow %s read --session %s", adviserArg, sess.ShortID)
	case "ks_agent_files_off":
		ce.Code, ce.NextAction = exitConflict, ""
	case "ks_file_too_big":
		ce.Code = exitFailed
		ce.Message += " (a fetch is at most 250 MiB a file)"
	case "ks_files_hour_limit":
		ce.Code = exitTemporary
		ce.Message += " (a connection may fetch 1 GiB an hour)"
	case "ks_file_credential":
		ce.Code, ce.NextAction = exitIntegrity, ""
	case "ks_path_refused", "ks_path_redacted":
		ce.Code, ce.NextAction = exitIntegrity, ""
	case "ks_path_absent":
		ce.Code, ce.NextAction = exitFailed, "ks agent files list "+adviserArg+" --session "+sess.ShortID
	case "ks_path_not_a_file":
		ce.Code, ce.NextAction = exitUsage, "ks agent files list "+adviserArg+" --session "+sess.ShortID
	case "ks_session_not_running":
		ce.Code = exitConflict
		ce.NextAction = "the adviser's session is not running and a file read never wakes it; a person resumes it"
	case "ks_no_runtime":
		ce.Code, ce.NextAction = exitFailed, ""
	}
	return ce
}

func hostedAgentFilesList(cr hostedCreds, inv *Invocation) {
	p := workspacePath(inv.Arg(1))
	asker, sess := agentFilesAsker(cr, inv)
	adv, err := resolveAdviserTarget(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var env struct {
		Data fileList `json:"data"`
	}
	if err := hostedCall(cr, "GET", adviserFilesBase(asker, adv)+"?"+url.Values{"path": {p}}.Encode(), nil, &env); err != nil {
		die(filesRefusal(err, inv.Arg(0), sess))
	}
	l := env.Data
	emit(l, func() {
		fmt.Printf("%s (%s)\n", visible(l.Path), inv.Arg(0))
		for _, e := range l.Entries {
			name := visible(e.Name)
			if e.Type == "dir" {
				name += "/"
			}
			note := ""
			if e.Redacted {
				note = "  [redacted: its contents are never given]"
			}
			fmt.Printf("  %-5s %-6s %12s  %s%s\n", visible(e.Type), visible(e.Mode), commas(e.Size), name, note)
		}
		if l.Truncated {
			fmt.Printf("showing %d of %d entries; the list was cut at the service's bound\n", len(l.Entries), l.Total)
		}
	})
}

var sharedLabelUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// adviserLabel names the adviser for a folder: its session and agent names.
func adviserLabel(cr hostedCreds, adv agentRow) string {
	name, _, err := sessionRecord(cr, adv.SessionID)
	if err != nil || name == "" {
		name = adv.SessionID
	}
	l := sharedLabelUnsafe.ReplaceAllString(name+"-"+firstNonEmpty(adv.Name, "agent"), "_")
	if l == "" || strings.Trim(l, ".") == "" {
		l = "adviser"
	}
	return l
}

// makeFolders creates dir (relative) one component at a time, owner-only,
// refusing any existing component that is not a real folder: a link is never
// followed to somewhere else.
func makeFolders(dir string) error {
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			return &cliError{Code: exitIntegrity, Kind: "target_is_link", Message: fmt.Sprintf("%s is a link; a fetched file is never written through one. Nothing was downloaded", cur)}
		case err == nil && !fi.IsDir():
			return &cliError{Code: exitConflict, Kind: "target_exists", Message: fmt.Sprintf("%s is a file, not a folder; nothing was downloaded", cur)}
		case err != nil && errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		}
	}
	return nil
}

func hostedAgentFilesGet(cr hostedCreds, inv *Invocation) {
	p := workspacePath(inv.Arg(1))
	if p == "." {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name a file; ks agent files list shows a folder"})
	}
	asker, sess := agentFilesAsker(cr, inv)
	adv, err := resolveAdviserTarget(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	refuse := func(err error) error { return filesRefusal(err, inv.Arg(0), sess) }
	if inv.Bool("show") {
		showAdviserFile(cr, inv, asker, adv, p, refuse)
		return
	}
	dest := inv.Str("to")
	if dest == "" {
		dest = filepath.Join(sharedInDir, adviserLabel(cr, adv), filepath.FromSlash(p))
		if err := makeFolders(filepath.Dir(dest)); err != nil {
			die(err)
		}
	}
	if err := checkTarget(dest, false); err != nil {
		die(err)
	}
	route := adviserFilesBase(asker, adv) + "/download?" + url.Values{"path": {p}}.Encode()
	d := fetchVerifiedFile(cr, route, p, dest, inv.Arg(0), refuse)
	emit(d, func() {
		fmt.Printf("wrote %s bytes to %s (from %s, sha256 %s, checked)\n", commas(d.Bytes), dest, inv.Arg(0), short(d.SHA256))
	})
}

// showAdviserFile prints one text file of the adviser, up to 1 MiB, and writes nothing.
func showAdviserFile(cr hostedCreds, inv *Invocation, asker, adv agentRow, p string, refuse func(error) error) {
	var env struct {
		Data fileContent `json:"data"`
	}
	q := url.Values{"path": {p}}
	if err := hostedCall(cr, "GET", adviserFilesBase(asker, adv)+"/content?"+q.Encode(), nil, &env); err != nil {
		die(refuse(err))
	}
	c := env.Data
	raw, err := base64.StdEncoding.DecodeString(c.ContentB64)
	if err != nil {
		fail(integrity("content_unreadable", "the file's bytes did not arrive as base64; nothing is shown"))
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != c.Returned || (c.SHA256Returned != "" && hex.EncodeToString(sum[:]) != strings.ToLower(c.SHA256Returned)) {
		fail(integrity("digest_mismatch", "the bytes received do not match the size and sha256 the service stated for them; nothing is shown"))
	}
	emit(c, func() {
		progress("%s (%s): %s bytes shown, sha256 %s", visible(c.Path), inv.Arg(0), commas(c.Returned), short(c.SHA256Returned))
		if !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
			fmt.Printf("%s is not text (%s bytes); it is not printed. Fetch it: ks agent files get %s %s\n", visible(c.Path), commas(c.Returned), inv.Arg(0), p)
			return
		}
		fmt.Print(visible(string(raw)))
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			fmt.Println()
		}
		if c.Truncated {
			fmt.Printf("[cut at %s of %s bytes; ks agent files get brings the whole file]\n", commas(c.Returned), commas(c.Size))
		}
	})
}

func hostedAgentFilesLog(cr hostedCreds, inv *Invocation) {
	asker, _ := agentFilesAsker(cr, inv)
	var env struct {
		Data struct {
			Items []agentFileFetch `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/agents/"+url.PathEscape(asker.ID)+"/file-fetches", nil, &env); err != nil {
		die(err)
	}
	rows := env.Data.Items
	emit(map[string]any{"agent": asker.ID, "fetches": rows, "count": len(rows)}, func() {
		if len(rows) == 0 {
			fmt.Printf("no file has been asked for by or of %s yet\n", asker.Name)
			return
		}
		fmt.Printf("%-20s %-9s %-9s %-8s %12s  %s\n", "TIME", "DIRECTION", "OP", "OUTCOME", "BYTES", "PATH")
		for _, f := range rows {
			dir := "asked"
			if f.Adviser == asker.ID {
				dir = "was asked"
			}
			outcome := f.Outcome
			if f.Code != "" {
				outcome += " (" + f.Code + ")"
			}
			fmt.Printf("%-20s %-9s %-9s %-8s %12s  %s\n", sanitize(f.At), dir, sanitize(f.Op), sanitize(outcome), commas(f.Bytes), visible(path.Clean(f.Path)))
		}
	})
}

func hostedAgentFilesAllow(cr hostedCreds, inv *Invocation) {
	setting := inv.Arg(1)
	if setting != "read" && setting != "none" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("%q is not a setting: files is read or none; nothing was changed", sanitize(setting))})
	}
	asker, sess := agentFilesAsker(cr, inv)
	adv, err := resolveAdviserTarget(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var list struct {
		Data struct {
			Items []adviserGrant `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/adviser-grants?"+url.Values{"agent_id": {asker.ID}}.Encode(), nil, &list); err != nil {
		die(err)
	}
	var live []adviserGrant
	for _, g := range list.Data.Items {
		if g.Live && g.SourceAgentID == asker.ID && g.TargetAgentID == adv.ID {
			live = append(live, g)
		}
	}
	if len(live) == 0 {
		fail(&cliError{Code: exitFailed, Kind: "no_connection",
			Message:    fmt.Sprintf("%s has no live connection to %s, so there is nothing to set; nothing was changed", asker.Name, inv.Arg(0)),
			NextAction: fmt.Sprintf("ks adviser connect %s --session %s", inv.Arg(0), sess.ShortID)})
	}
	var done []adviserGrant
	for _, g := range live {
		var env struct {
			Data adviserGrant `json:"data"`
		}
		if err := hostedMutate(cr, "PUT", "/api/v2/adviser-grants/"+url.PathEscape(g.ID)+"/files", map[string]any{"files": setting}, &env); err != nil {
			die(filesRefusal(err, inv.Arg(0), sess))
		}
		done = append(done, env.Data)
	}
	emit(map[string]any{"files": setting, "connections": done, "count": len(done)}, func() {
		for _, g := range done {
			fmt.Printf("connection %s: files %s (%s may now read %s's files: %v)\n", g.ID, setting, asker.Name, inv.Arg(0), setting == "read")
		}
		fmt.Println("takes effect on the next request; nothing already fetched is recalled")
	})
}
