// binding.go: which session a command means when it is not told (KS-022).
//
// The order is fixed and there is no fifth step:
//
//  1. an explicit --session, always first;
//  2. this project's binding, made by `ks session use`, stored in the
//     client's own configuration directory and keyed by the ACCOUNT, the
//     CANONICAL project root and the control plane's ORIGIN, so a binding
//     made in one repository, under one account or against one control
//     plane never selects anything anywhere else;
//  3. an interactive choice, at a terminal only, from a numbered list that
//     has no default;
//  4. otherwise an error that names the flag.
//
// The newest session is never chosen, and neither is the only one.
//
// A repository may ship a file that names a session
// (.keepstate/session.json). It is somebody else's statement: it is never
// used to select anything until a person confirms it with
// `ks session use --trust-repo --confirm <session>`, after which it becomes
// an ordinary local binding. The client's own binding store lives outside
// the project, and .keepstate is a mandatory upload exclusion (C09), so no
// binding travels in an upload.
//
// A target that did not come from --session is printed on stderr before
// the command acts, even under --quiet: a person who did not name the
// session sees which one is about to be used.
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
	"strconv"
	"strings"
	"time"
)

// c04Name is the C04 name rule, the one the service applies to sessions
// and agents: 1 to 48 ASCII letters, digits, hyphens or underscores,
// starting with a letter.
var c04Name = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,47}$`)

const repoBindingFile = ".keepstate/session.json"

type projectBinding struct {
	AccountID   string `json:"account_id"`
	Origin      string `json:"control_plane"`
	ProjectRoot string `json:"project_root"`
	SessionID   string `json:"session_id"`
	RecordID    string `json:"record_id,omitempty"`
	ShortID     string `json:"short_id"`
	Name        string `json:"name"`
	BoundAt     string `json:"bound_at"`
	Source      string `json:"source"` // "ks session use" | "repository file, confirmed"
}

func bindingsPath() string { return filepath.Join(configDir(), "bindings.json") }

// controlPlaneOrigin is scheme://host[:port] of the stored control plane.
func controlPlaneOrigin(ctl string) string {
	u, err := url.Parse(ctl)
	if err != nil || u.Host == "" {
		return strings.TrimRight(ctl, "/")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// projectRoot is the canonical root of the project the command runs in:
// the nearest directory upward holding .git or .keepstate, else the
// working directory, with every link resolved.
func projectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return "", err
	}
	for d := wd; ; {
		for _, marker := range []string{".git", ".keepstate"} {
			if _, err := os.Lstat(filepath.Join(d, marker)); err == nil {
				return d, nil
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return wd, nil
		}
		d = parent
	}
}

func readBindings() ([]projectBinding, error) {
	b, err := os.ReadFile(bindingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bs []projectBinding
	if err := json.Unmarshal(b, &bs); err != nil {
		return nil, fmt.Errorf("the binding store %s is unreadable (%v); remove it and bind again with ks session use", bindingsPath(), err)
	}
	return bs, nil
}

func writeBindings(bs []projectBinding) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(bs, "", " ")
	tmp, err := os.CreateTemp(configDir(), ".bindings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), bindingsPath())
}

func (b projectBinding) matches(account, origin, root string) bool {
	return account != "" && b.AccountID == account && b.Origin == origin && b.ProjectRoot == root
}

// currentBinding is this project's binding for this account on this control
// plane, or nil. A sign-in that does not record its account has no binding.
func currentBinding(cr hostedCreds) (*projectBinding, string, error) {
	root, err := projectRoot()
	if err != nil {
		return nil, "", err
	}
	if cr.AccountID == "" {
		return nil, root, nil
	}
	bs, err := readBindings()
	if err != nil {
		return nil, root, err
	}
	origin := controlPlaneOrigin(cr.CTL)
	for i := range bs {
		if bs[i].matches(cr.AccountID, origin, root) {
			return &bs[i], root, nil
		}
	}
	return nil, root, nil
}

// repoSuggestion reads the session a repository-shipped file names, only
// from a regular file (never through a link) and only a small one.
func repoSuggestion(root string) string {
	p := filepath.Join(root, repoBindingFile)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return ""
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var f struct {
		Session string `json:"session"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return ""
	}
	return sanitize(strings.TrimSpace(f.Session))
}

// findBound answers the session a binding names, by its exact id: a binding
// never falls back to a prefix, a name, or anything else.
func findBound(cr hostedCreds, b *projectBinding) (inventoryRow, error) {
	rows, err := fetchInventory(cr, "", true)
	if err != nil {
		return inventoryRow{}, err
	}
	for _, r := range rows {
		if r.ID == b.SessionID || (b.RecordID != "" && r.RecordID == b.RecordID) {
			return r, nil
		}
	}
	return inventoryRow{}, &cliError{Code: exitUsage, Kind: "binding_stale",
		Message: fmt.Sprintf("this project is bound to session %s (%s), which this account no longer has; nothing was chosen instead",
			b.ShortID, b.Name),
		NextAction: "ks session use <session>, or ks session use --clear"}
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if dn, derr := os.Stat(os.DevNull); derr == nil && os.SameFile(fi, dn) {
		return false
	}
	return true
}

// chooseSession is the interactive selector's decision over a list already
// shown: a number from the list and nothing else. An empty answer chooses
// nothing: there is no default.
func chooseSession(rows []inventoryRow, answer string) (inventoryRow, error) {
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(rows) {
		return inventoryRow{}, &cliError{Code: exitUsage, Kind: "selection_declined",
			Message: "no session was chosen; nothing was done", NextAction: "pass --session <session>"}
	}
	return rows[n-1], nil
}

// targetSession resolves the session a command means, in the fixed order.
// how names where it came from; explicit is true only for --session.
func targetSession(cr hostedCreds, explicitArg string) (r inventoryRow, how string, explicit bool) {
	if explicitArg != "" {
		r, err := resolveSession(cr, explicitArg)
		if err != nil {
			die(err)
		}
		return r, "--session", true
	}
	b, root, err := currentBinding(cr)
	if err != nil {
		die(err)
	}
	if b != nil {
		r, err := findBound(cr, b)
		if err != nil {
			die(err)
		}
		return r, "the binding for " + root, false
	}
	suggestion := repoSuggestion(root)
	if stdinIsTerminal() && !out.noInput {
		rows, err := fetchInventory(cr, "", false)
		if err != nil {
			die(err)
		}
		if len(rows) > 0 {
			fmt.Fprintln(os.Stderr, "no session was named and this project has no binding; choose one (there is no default):")
			for i, x := range rows {
				fmt.Fprintf(os.Stderr, "  %d) %s  %-20s %s\n", i+1, x.ShortID, clip(x.Name, 20), x.RuntimeState)
			}
			if suggestion != "" {
				fmt.Fprintf(os.Stderr, "(this repository's %s names %q; it is not trusted and is not preselected)\n", repoBindingFile, suggestion)
			}
			fmt.Fprint(os.Stderr, "number: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			r, err := chooseSession(rows, line)
			if err != nil {
				die(err)
			}
			return r, "your choice", false
		}
	}
	msg := "no session was named and this project has no binding; nothing is chosen for you"
	if cr.AccountID == "" {
		msg += " (this sign-in does not record its account, so no binding can apply; ks login again to record it)"
	}
	next := "pass --session <session>, or bind this project: ks session use <session>"
	if suggestion != "" {
		msg += fmt.Sprintf("; this repository's %s names %q, which is not trusted until you confirm it", repoBindingFile, suggestion)
		next = "ks session use --trust-repo --confirm " + suggestion
	}
	fail(&cliError{Code: exitUsage, Kind: "session_required", Message: msg, NextAction: next})
	return
}

// showTarget prints the resolved target before anything acts on it. It goes
// to stderr so --json stdout stays one document, and it is printed even
// under --quiet: it is the decision's own display, not progress.
func showTarget(r inventoryRow, how string) {
	fmt.Fprintf(os.Stderr, "target: session %s %q (%s), from %s\n", r.ShortID, r.Name, r.RuntimeState, how)
}

// ---- ks session use -----------------------------------------------------------

func hostedSessionUse(cr hostedCreds, inv *Invocation) {
	root, err := projectRoot()
	if err != nil {
		die(err)
	}
	origin := controlPlaneOrigin(cr.CTL)
	bs, err := readBindings()
	if err != nil {
		die(err)
	}
	idx := -1
	for i := range bs {
		if bs[i].matches(cr.AccountID, origin, root) {
			idx = i
		}
	}
	arg := inv.Arg(0)
	modes := 0
	for _, m := range []bool{arg != "", inv.Bool("clear"), inv.Bool("trust-repo")} {
		if m {
			modes++
		}
	}
	if modes > 1 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name a session, or --clear, or --trust-repo: one at a time"})
	}
	if inv.Set("confirm") && !inv.Bool("trust-repo") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--confirm confirms a repository's binding and goes with --trust-repo"})
	}

	switch {
	case inv.Bool("clear"):
		if idx < 0 {
			emit(map[string]any{"project_root": root, "cleared": false}, func() {
				fmt.Printf("this project (%s) has no binding; nothing to clear\n", root)
			})
			return
		}
		was := bs[idx]
		bs = append(bs[:idx], bs[idx+1:]...)
		if err := writeBindings(bs); err != nil {
			die(err)
		}
		emit(map[string]any{"project_root": root, "cleared": true, "was": was}, func() {
			fmt.Printf("cleared: %s is no longer bound (it was session %s %q)\n", root, was.ShortID, was.Name)
		})
		return
	case arg == "" && !inv.Bool("trust-repo"):
		// the reading form: what this project is bound to, and what the
		// repository suggests, side by side and never merged
		suggestion := repoSuggestion(root)
		var cur any
		if idx >= 0 {
			cur = bs[idx]
		}
		emit(map[string]any{"project_root": root, "account_id": cr.AccountID, "control_plane": origin, "binding": cur,
			"repository_suggestion": suggestion, "repository_suggestion_trusted": false}, func() {
			if idx >= 0 {
				b := bs[idx]
				fmt.Printf("%s is bound to session %s %q (%s) for %s on %s, since %s\n", root, b.ShortID, b.Name, b.SessionID, b.AccountID, b.Origin, b.BoundAt)
			} else {
				fmt.Printf("%s has no binding; commands without --session ask or refuse\n", root)
			}
			if suggestion != "" {
				fmt.Printf("this repository's %s names %q: not trusted; to use it: ks session use --trust-repo --confirm %s\n", repoBindingFile, suggestion, suggestion)
			}
		})
		return
	}

	if cr.AccountID == "" {
		fail(&cliError{Code: exitUsage, Kind: "account_unrecorded",
			Message:    "this sign-in does not record its account, and a binding is kept per account; nothing was bound",
			NextAction: "ks login"})
	}
	name, source := arg, "ks session use"
	if inv.Bool("trust-repo") {
		name = repoSuggestion(root)
		if name == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("this project has no readable %s to trust; nothing was bound", repoBindingFile)})
		}
		source = "repository file, confirmed"
	}
	r, err := resolveSession(cr, name)
	if err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "bind this project:\n  project   %s\n  account   %s on %s\n  session   %s %q (%s, %s)\n", root, cr.AccountID, origin, r.ShortID, r.Name, r.ID, r.RuntimeState)
	if inv.Bool("trust-repo") {
		// trusting a statement somebody else wrote is a decision --yes never
		// makes: the session must be confirmed by its id, exactly
		want := inv.Str("confirm")
		if want == "" || (want != r.ID && want != r.ShortID && (r.RecordID == "" || want != r.RecordID)) {
			msg := "a repository's binding is trusted only when you confirm the session it names by its id; nothing was bound"
			if want != "" {
				msg = fmt.Sprintf("--confirm %q is not the session the repository names (%s); nothing was bound", want, r.ShortID)
			}
			fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: msg, NextAction: "ks session use --trust-repo --confirm " + r.ShortID})
		}
	}
	nb := projectBinding{AccountID: cr.AccountID, Origin: origin, ProjectRoot: root, SessionID: r.ID, RecordID: r.RecordID,
		ShortID: r.ShortID, Name: r.Name, BoundAt: time.Now().UTC().Format(time.RFC3339), Source: source}
	if idx >= 0 {
		bs[idx] = nb
	} else {
		bs = append(bs, nb)
	}
	if err := writeBindings(bs); err != nil {
		die(err)
	}
	emit(nb, func() {
		fmt.Printf("bound: %s -> session %s %q, for %s on %s\n", root, r.ShortID, r.Name, cr.AccountID, origin)
	})
}

// ---- duplicate active work --------------------------------------------------

// warnDuplicateWork tells a person starting a new session that this
// project is already bound to one doing work. It never blocks and never
// merges: two sessions are two sessions. It asks nothing of the service
// when there is no binding. It sees only this client's bindings: a
// session started for the same project from another machine is not known.
func warnDuplicateWork(cr hostedCreds) {
	b, root, err := currentBinding(cr)
	if err != nil || b == nil {
		return
	}
	r, err := findBound(cr, b)
	if err != nil {
		return
	}
	active := r.RuntimeState == "running" || r.RuntimeState == "provisioning" ||
		r.AgentActivity == "working" || r.TaskState == "running" || r.TaskState == "queued"
	if !active {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %s is bound to session %s %q, which is %s (agent %s, task %s). "+
		"This starts a SEPARATE session; nothing is merged. To give the existing session the work instead: ks agent tell main \"...\" --session %s\n",
		root, r.ShortID, r.Name, r.RuntimeState, r.AgentActivity, r.TaskState, r.ShortID)
}

// ---- renames ----------------------------------------------------------------

func checkName(name string) {
	if !c04Name.MatchString(name) {
		fail(&cliError{Code: exitUsage, Kind: "name_invalid",
			Message: fmt.Sprintf("%q is not a name: 1 to 48 ASCII letters, digits, hyphens or underscores, starting with a letter; nothing was renamed", sanitize(name))})
	}
}

func hostedAgentRename(cr hostedCreds, inv *Invocation) {
	to := inv.Arg(1)
	checkName(to)
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if a.Name == to {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("agent %s is already named %q; nothing was renamed", a.ID, to)})
	}
	fmt.Fprintf(os.Stderr, "rename agent %q (%s) in session %s %q to %q, against revision %d; its tasks, approvals and adviser connections keep referring to it by id\n",
		a.Name, a.ID, sess.ShortID, sess.Name, to, a.Revision)
	var env struct {
		Data agentRow `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/agents/"+url.PathEscape(a.ID), map[string]any{"name": to, "expected_revision": a.Revision}, &env); err != nil {
		die(renameRefusal(err, "ks agent list --session "+sess.ShortID))
	}
	emit(map[string]any{"agent": env.Data, "from": a.Name, "session_id": agentSessionID(sess)}, func() {
		fmt.Printf("renamed agent %s: %q -> %q (revision %d); its id, tasks and connections are unchanged\n", env.Data.ID, a.Name, env.Data.Name, env.Data.Revision)
	})
}

// renameRefusal names what a refused rename means, keeping the service's
// own code and exit class.
func renameRefusal(err error, next string) error {
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_revision_conflict":
			ce := classify(err)
			ce.Message = "the record changed since it was read, so nothing was renamed; read it again and retry: " + ce.Message
			ce.NextAction = next
			return ce
		case "ks_forbidden":
			ce := classify(err)
			ce.Message = "renaming needs the operator role on this session; nothing was renamed: " + ce.Message
			ce.NextAction = ""
			return ce
		}
	}
	return err
}

func hostedSessionRename(cr hostedCreds, inv *Invocation) {
	to := inv.Arg(1)
	checkName(to)
	r, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if r.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: fmt.Sprintf("session %s has no workspace record to rename; nothing was renamed", r.ShortID)})
	}
	var rec struct {
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Revision int64  `json:"revision"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(r.RecordID), nil, &rec); err != nil {
		die(err)
	}
	if rec.Data.Name == to {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("session %s is already named %q; nothing was renamed", r.ShortID, to)})
	}
	fmt.Fprintf(os.Stderr, "rename session %s %q (%s) to %q, against revision %d; its id and everything in it are unchanged\n",
		r.ShortID, rec.Data.Name, r.RecordID, to, rec.Data.Revision)
	var env struct {
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Revision int64  `json:"revision"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/sessions/"+url.PathEscape(r.RecordID), map[string]any{"name": to, "expected_revision": rec.Data.Revision}, &env); err != nil {
		die(renameRefusal(err, "ks session show "+r.ShortID))
	}
	emit(map[string]any{"session": env.Data, "from": rec.Data.Name}, func() {
		fmt.Printf("renamed session %s: %q -> %q (revision %d)\n", r.ShortID, rec.Data.Name, env.Data.Name, env.Data.Revision)
	})
}
