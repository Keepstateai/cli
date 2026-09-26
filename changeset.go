// changeset.go: reviewing and applying what an attempt changed (KS-057).
//
//	ks result diff <result>            what the ks-changeset.json result says
//	                                   changed, against the recorded base
//	ks result apply <result>           apply it to this project, only onto
//	                                   the recorded base
//	ks result apply --recover          put an interrupted apply back
//
// The changeset is untrusted data: every path is shown with terminal
// controls escaped, and a path that is absolute, climbs out, uses a
// backslash or passes through a link on this machine refuses the WHOLE
// changeset (QA-057-2). Before anything is written, every file the
// changeset touches is compared with its recorded before-state (digest and
// mode; an added path must not exist): one difference -- a local edit made
// since the base -- refuses the whole apply and changes nothing (QA-057-1).
//
// Applying is transactional. The new bytes are staged and verified, every
// file about to change is backed up, a journal records each step, and only
// then are files replaced; the result is verified against the after-states.
// An apply that is interrupted leaves its journal: the next apply refuses
// until `ks result apply --recover` restores every touched path from the
// backup (QA-057-3). Nothing is committed, pushed, run or deployed.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type csFileState struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode,omitempty"`
	Target string `json:"target,omitempty"`
}

type csChange struct {
	Path           string       `json:"path"`
	Op             string       `json:"op"`
	Before         *csFileState `json:"before,omitempty"`
	After          *csFileState `json:"after,omitempty"`
	Binary         bool         `json:"binary"`
	ContentB64     string       `json:"content_b64,omitempty"`
	ContentOmitted string       `json:"content_omitted,omitempty"`
}

type changeset struct {
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	Base      struct {
		Known      bool   `json:"known"`
		Digest     string `json:"digest,omitempty"`
		CapturedAt string `json:"captured_at,omitempty"`
		Truncated  bool   `json:"truncated"`
		Reason     string `json:"reason,omitempty"`
	} `json:"base"`
	Changes []csChange `json:"changes"`
	Note    string     `json:"note"`
}

// ---- reading and validating ------------------------------------------------------

func parseChangeset(raw []byte) (*changeset, error) {
	var cs changeset
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, fmt.Errorf("the result is not a changeset: %v", err)
	}
	if cs.Kind != "keepstate.changeset" || cs.Version != 1 {
		return nil, fmt.Errorf("the result is not a changeset this client reads (kind %q, version %d)", sanitize(cs.Kind), cs.Version)
	}
	seen := map[string]bool{}
	for i, c := range cs.Changes {
		if _, err := safeEntryName(c.Path); err != nil || strings.HasSuffix(c.Path, "/") || filepath.Clean(c.Path) != filepath.FromSlash(c.Path) {
			return nil, fmt.Errorf("change %d names %q, which is not a path inside the workspace; the whole changeset is refused", i, sanitize(c.Path))
		}
		if seen[c.Path] {
			return nil, fmt.Errorf("the path %q appears twice; the whole changeset is refused", sanitize(c.Path))
		}
		seen[c.Path] = true
		switch c.Op {
		case "add", "modify", "mode", "delete":
		default:
			return nil, fmt.Errorf("change %d has an unknown op %q", i, sanitize(c.Op))
		}
		if c.ContentB64 != "" {
			b, err := base64.StdEncoding.DecodeString(c.ContentB64)
			if err != nil || c.After == nil || digestOf(b) != c.After.SHA256 || int64(len(b)) != c.After.Bytes {
				return nil, fmt.Errorf("change %d's bytes do not match the digest and size it claims; the whole changeset is refused", i)
			}
		}
	}
	return &cs, nil
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fetchChangeset downloads and verifies the result, and reads it.
func fetchChangeset(cr hostedCreds, id string) (resultRow, *changeset) {
	r, err := fetchResult(cr, id)
	if err != nil {
		die(err)
	}
	if r.Name != "ks-changeset.json" {
		fail(&cliError{Code: exitUsage, Kind: "not_a_changeset", Message: fmt.Sprintf("result %s is %q, not the attempt's ks-changeset.json; use ks result download", r.ID, sanitize(r.Name))})
	}
	partial := filepath.Join(os.TempDir(), ".ks-changeset-"+r.ID)
	defer os.Remove(partial)
	if _, err := downloadVerified(cr, r, partial, 0, false); err != nil {
		die(err)
	}
	raw, err := os.ReadFile(partial)
	if err != nil {
		die(err)
	}
	cs, err := parseChangeset(raw)
	if err != nil {
		fail(integrity("changeset_refused", err.Error()))
	}
	return r, cs
}

func stateWord(s *csFileState) string {
	if s == nil {
		return "absent"
	}
	switch s.Kind {
	case "link":
		return "link -> " + visible(sanitize(s.Target))
	case "too_large":
		return fmt.Sprintf("too large to record (%s bytes)", commas(s.Bytes))
	}
	return fmt.Sprintf("%s %s bytes %s", short(strings.TrimPrefix(s.SHA256, "sha256:")), commas(s.Bytes), s.Mode)
}

func printChangeset(cs *changeset) {
	if cs.Base.Known {
		fmt.Printf("base %s captured %s\n", short(strings.TrimPrefix(cs.Base.Digest, "sha256:")), figure(cs.Base.CapturedAt))
	} else {
		fmt.Printf("base UNKNOWN: %s (applying needs a known base)\n", sanitize(cs.Base.Reason))
	}
	if len(cs.Changes) == 0 {
		fmt.Println("no changes")
	}
	for _, c := range cs.Changes {
		bin := ""
		if c.Binary {
			bin = " (binary)"
		}
		fmt.Printf("  %-6s %s%s\n", c.Op, visible(c.Path), bin)
		fmt.Printf("         %s  ->  %s\n", stateWord(c.Before), stateWord(c.After))
		if c.ContentOmitted != "" {
			fmt.Printf("         new bytes not carried: %s\n", sanitize(c.ContentOmitted))
		}
	}
}

func hostedResultDiff(cr hostedCreds, inv *Invocation) {
	r, cs := fetchChangeset(cr, inv.Arg(0))
	emit(map[string]any{"result": r.ID, "changeset": cs}, func() {
		fmt.Printf("changes of task %s, attempt %s (result %s)\n", cs.TaskID, cs.AttemptID, r.ID)
		printChangeset(cs)
		if inv.Bool("content") {
			for _, c := range cs.Changes {
				if c.ContentB64 == "" || c.Binary {
					continue
				}
				b, _ := base64.StdEncoding.DecodeString(c.ContentB64)
				fmt.Printf("---- %s (new content)\n%s\n", visible(c.Path), visible(string(b)))
			}
		}
		fmt.Printf("apply it here, onto the recorded base only: ks result apply %s\n", r.ID)
	})
}

// ---- applying --------------------------------------------------------------------

type applyJournal struct {
	ResultID  string      `json:"result_id"`
	Root      string      `json:"root"`
	State     string      `json:"state"` // applying | done | recovered
	StartedAt string      `json:"started_at"`
	Steps     []applyStep `json:"steps"`
}

type applyStep struct {
	Path    string `json:"path"`
	Op      string `json:"op"`
	Existed bool   `json:"existed"`
	Backup  string `json:"backup,omitempty"` // file under the backup dir
	Mode    string `json:"mode,omitempty"`   // the original mode
	Done    bool   `json:"done"`
}

func applyDir(root string) string { return filepath.Join(root, ".keepstate", "apply") }

func readJournal(root string) (*applyJournal, string, error) {
	p := filepath.Join(applyDir(root), "journal.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, p, nil
	}
	if err != nil {
		return nil, p, err
	}
	var j applyJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, p, fmt.Errorf("the apply journal %s is unreadable (%v); nothing is changed until it is repaired by hand", p, err)
	}
	return &j, p, nil
}

func writeJournal(p string, j *applyJournal) error {
	b, _ := json.MarshalIndent(j, "", " ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// noLinkOnTheWay refuses a path whose existing parents include a link.
func noLinkOnTheWay(root, rel string) error {
	parts := strings.Split(rel, "/")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%q passes through %q, which is not a plain directory here", rel, strings.TrimPrefix(cur, root+string(os.PathSeparator)))
		}
	}
	return nil
}

// localState is a path as it stands here, in the changeset's terms.
func localState(p string) (*csFileState, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, _ := os.Readlink(p)
		return &csFileState{Kind: "link", Target: t}, nil
	}
	if !fi.Mode().IsRegular() {
		return &csFileState{Kind: "other"}, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return &csFileState{Kind: "file", SHA256: digestOf(b), Bytes: int64(len(b)), Mode: fmt.Sprintf("%04o", fi.Mode().Perm())}, nil
}

func sameState(a, b *csFileState) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.SHA256 == b.SHA256 && a.Mode == b.Mode && a.Target == b.Target
}

// planApply checks everything before anything is written: a list of
// refusals (the whole apply is refused on any) and conflicts.
func planApply(root string, cs *changeset) (refusals, conflicts []string) {
	if !cs.Base.Known {
		refusals = append(refusals, "the base this attempt started from is unknown ("+sanitize(cs.Base.Reason)+"), so nothing can be checked against it")
	}
	for _, c := range cs.Changes {
		shown := visible(c.Path)
		if c.Path == ".keepstate" || strings.HasPrefix(c.Path, ".keepstate/") || c.Path == ".git" || strings.HasPrefix(c.Path, ".git/") {
			refusals = append(refusals, fmt.Sprintf("%s is inside the project's control files, which a changeset never changes", shown))
			continue
		}
		if err := noLinkOnTheWay(root, c.Path); err != nil {
			refusals = append(refusals, sanitize(err.Error()))
			continue
		}
		if c.After != nil && c.After.Kind != "file" {
			refusals = append(refusals, fmt.Sprintf("%s becomes a %s, which this client does not apply", shown, c.After.Kind))
			continue
		}
		if (c.Op == "add" || c.Op == "modify") && c.ContentB64 == "" && (c.After == nil || c.After.Bytes > 0) {
			refusals = append(refusals, fmt.Sprintf("%s: the changeset does not carry its new bytes (%s)", shown, figure(c.ContentOmitted)))
			continue
		}
		have, err := localState(filepath.Join(root, filepath.FromSlash(c.Path)))
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("%s could not be read here: %v", shown, err))
			continue
		}
		if !sameState(have, c.Before) {
			conflicts = append(conflicts, fmt.Sprintf("%s is %s here, and the change expects %s", shown, stateWord(have), stateWord(c.Before)))
		}
	}
	return
}

// applyFault lets a test interrupt an apply after n replaced paths.
var applyFault func(done int) error

// applyChangeset performs a checked plan. On any error part-way the journal
// stays "applying", and recoverApply restores it.
func applyChangeset(root, resultID string, cs *changeset) error {
	dir := applyDir(root)
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "staged"), 0o700); err != nil {
		return err
	}
	jp := filepath.Join(dir, "journal.json")
	j := &applyJournal{ResultID: resultID, Root: root, State: "applying", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	// 1. stage the new bytes and back up what will change, before anything moves
	staged := map[int]string{}
	for i, c := range cs.Changes {
		target := filepath.Join(root, filepath.FromSlash(c.Path))
		st := applyStep{Path: c.Path, Op: c.Op}
		if fi, err := os.Lstat(target); err == nil {
			st.Existed, st.Mode = true, fmt.Sprintf("%04o", fi.Mode().Perm())
			b, err := os.ReadFile(target)
			if err != nil {
				return err
			}
			st.Backup = filepath.Join("backup", strconv.Itoa(i))
			if err := os.WriteFile(filepath.Join(dir, st.Backup), b, 0o600); err != nil {
				return err
			}
		}
		if c.ContentB64 != "" || ((c.Op == "add" || c.Op == "modify") && c.After != nil && c.After.Bytes == 0) {
			b, _ := base64.StdEncoding.DecodeString(c.ContentB64)
			sp := filepath.Join(dir, "staged", strconv.Itoa(i))
			if err := os.WriteFile(sp, b, 0o600); err != nil {
				return err
			}
			if digestOf(b) != c.After.SHA256 {
				return fmt.Errorf("staged bytes for %q do not match their digest", c.Path)
			}
			staged[i] = sp
		}
		j.Steps = append(j.Steps, st)
	}
	if err := writeJournal(jp, j); err != nil {
		return err
	}
	// 2. replace, one path at a time, journalled
	for i, c := range cs.Changes {
		if applyFault != nil {
			if err := applyFault(i); err != nil {
				return err
			}
		}
		target := filepath.Join(root, filepath.FromSlash(c.Path))
		// the path is read again at the moment it is replaced: a file that
		// appeared or changed since the preview is never overwritten (F04
		// pre-existing-target-race); the paths already replaced are in the
		// journal for --recover
		if have, err := localState(target); err != nil || !sameState(have, c.Before) {
			return fmt.Errorf("%q changed after the preview (it is %s now); it was not touched", c.Path, stateWord(have))
		}
		switch c.Op {
		case "add", "modify":
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if c.Op == "add" {
				// a link fails if anything is at the target: an add never replaces
				if err := os.Link(staged[i], target); err != nil {
					return fmt.Errorf("%q appeared after the preview; it was not touched: %v", c.Path, err)
				}
				_ = os.Remove(staged[i])
			} else if err := os.Rename(staged[i], target); err != nil {
				return err
			}
			if err := os.Chmod(target, parseMode(c.After.Mode, 0o644)); err != nil {
				return err
			}
		case "mode":
			if err := os.Chmod(target, parseMode(c.After.Mode, 0o644)); err != nil {
				return err
			}
		case "delete":
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		j.Steps[i].Done = true
		if err := writeJournal(jp, j); err != nil {
			return err
		}
	}
	// 3. verify every path against its after-state
	for _, c := range cs.Changes {
		have, err := localState(filepath.Join(root, filepath.FromSlash(c.Path)))
		if err != nil || (c.After == nil && have != nil) || (c.After != nil && (have == nil || have.SHA256 != c.After.SHA256)) {
			return fmt.Errorf("after applying, %q is not what the changeset says it should be", c.Path)
		}
	}
	j.State = "done"
	_ = os.RemoveAll(filepath.Join(dir, "staged"))
	return writeJournal(jp, j)
}

func parseMode(s string, def os.FileMode) os.FileMode {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n == 0 {
		return def
	}
	return os.FileMode(n) & 0o777
}

// recoverApply puts every path an interrupted apply touched back as it was.
func recoverApply(root string) (*applyJournal, error) {
	j, jp, err := readJournal(root)
	if err != nil {
		return nil, err
	}
	if j == nil || j.State != "applying" {
		return j, nil
	}
	dir := applyDir(root)
	for i := len(j.Steps) - 1; i >= 0; i-- {
		st := j.Steps[i]
		target := filepath.Join(root, filepath.FromSlash(st.Path))
		if st.Existed {
			b, err := os.ReadFile(filepath.Join(dir, st.Backup))
			if err != nil {
				return j, fmt.Errorf("the backup of %q is missing (%v); restore it by hand from version control", st.Path, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return j, err
			}
			if err := os.WriteFile(target, b, parseMode(st.Mode, 0o644)); err != nil {
				return j, err
			}
			_ = os.Chmod(target, parseMode(st.Mode, 0o644))
		} else if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return j, err
		}
	}
	j.State = "recovered"
	_ = os.RemoveAll(filepath.Join(dir, "staged"))
	return j, writeJournal(jp, j)
}

func hostedResultApply(cr hostedCreds, inv *Invocation) {
	root := inv.Str("dir")
	if root == "" {
		var err error
		if root, err = projectRoot(); err != nil {
			die(err)
		}
	}
	root, _ = filepath.Abs(root)
	if inv.Bool("recover") {
		j, err := recoverApply(root)
		if err != nil {
			die(err)
		}
		emit(j, func() {
			switch {
			case j == nil:
				fmt.Println("no apply was ever started here; nothing to recover")
			case j.State == "recovered":
				fmt.Printf("recovered: every path the interrupted apply of %s touched is back as it was before it began\n", j.ResultID)
			default:
				fmt.Printf("the last apply (%s) is %s; nothing to recover\n", j.ResultID, j.State)
			}
		})
		return
	}
	if inv.Arg(0) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the changeset result to apply, or --recover"})
	}
	refuseIfInterrupted(root)
	r, cs := fetchChangeset(cr, inv.Arg(0))
	confirmAndApply(root, r.ID, r.SHA256, cs, inv, "ks result apply "+r.ID)
}

// refuseIfInterrupted stops any apply while an earlier one is unrecovered.
func refuseIfInterrupted(root string) {
	if j, _, err := readJournal(root); err != nil {
		die(err)
	} else if j != nil && j.State == "applying" {
		var lines []string
		for _, st := range j.Steps {
			w := "restore from backup"
			if !st.Existed {
				w = "remove (it did not exist before)"
			}
			lines = append(lines, visible(st.Path)+": "+w)
		}
		fail(&cliError{Code: exitConflict, Kind: "apply_interrupted",
			Message:    fmt.Sprintf("an apply of %s was interrupted here and must be recovered first; the repair plan: %s", j.ResultID, strings.Join(lines, "; ")),
			NextAction: "ks result apply --recover"})
	}
}

// confirmAndApply is the apply itself, shared by ks result apply and ks cruise
// apply: every file checked against its recorded before-state, the whole
// changeset refused on any refusal or conflict, confirmed by its digest, then
// applied transactionally. label names it in the journal; fullSHA is its
// sha256; confirmCmd is what a non-interactive caller repeats with --confirm.
func confirmAndApply(root, label, fullSHA string, cs *changeset, inv *Invocation, confirmCmd string) {
	refusals, conflicts := planApply(root, cs)
	refusals = append(refusals, collisionRefusals(root, cs)...)
	if len(refusals) > 0 {
		fail(integrity("changeset_refused", "the whole changeset is refused and nothing was changed: "+strings.Join(refusals, "; ")))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		fail(&cliError{Code: exitConflict, Kind: "apply_conflict",
			Message: "the files here are not the base the changes were made against, so nothing was changed: " + strings.Join(conflicts, "; ")})
	}
	fmt.Fprintf(os.Stderr, "apply to %s (each file checked against its recorded base):\n", root)
	for _, c := range cs.Changes {
		fmt.Fprintf(os.Stderr, "  %-6s %s\n", c.Op, visible(c.Path))
	}
	want := short(fullSHA)
	given := strings.TrimSpace(inv.Str("confirm"))
	switch {
	case given != "":
		if given != want && given != fullSHA {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not this changeset (%s); nothing was changed", given, want)})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: "applying changes files here and is confirmed by the changeset's digest; --yes does not confirm it, and nothing was changed",
			NextAction: fmt.Sprintf("%s --confirm %s", confirmCmd, want)})
	default:
		fmt.Fprintf(os.Stderr, "type %s to apply, or anything else to stop: ", want)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != want {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "not confirmed; nothing was changed"})
		}
	}
	// the files are read again after the confirmation: anything that
	// changed while a person was deciding refuses the apply, unchanged
	if r2, c2 := planApply(root, cs); len(r2)+len(c2) > 0 {
		fail(&cliError{Code: exitConflict, Kind: "apply_conflict",
			Message: "the files here changed after the preview, so nothing was changed: " + strings.Join(append(r2, c2...), "; ")})
	}
	if err := applyChangeset(root, label, cs); err != nil {
		fail(&cliError{Code: exitFailed, Kind: "apply_interrupted", WorkStarted: workYes,
			Message: "the apply stopped part-way (" + sanitize(err.Error()) + "); every file it touched is backed up and journalled", NextAction: "ks result apply --recover"})
	}
	emit(map[string]any{"result": label, "applied": len(cs.Changes), "backup": filepath.Join(applyDir(root), "backup")}, func() {
		fmt.Printf("applied %d change(s) from %s and verified each against its recorded after-state; the previous files are backed up in %s\n", len(cs.Changes), label, filepath.Join(applyDir(root), "backup"))
		fmt.Println("nothing was committed, pushed or run")
	})
}

// collisionRefusals asks THIS filesystem whether two of the changeset's
// paths are the same file here (a case-insensitive or normalizing
// filesystem folds README.md and readme.md, or an NFC and an NFD name,
// together): each path is created exclusively in a scratch directory under
// the project's .keepstate, and a create that finds an earlier one names
// both (F04 case-collision and unicode-normalization-pair). The scratch
// directory is removed afterwards.
func collisionRefusals(root string, cs *changeset) []string {
	base := filepath.Join(root, ".keepstate")
	_, statErr := os.Stat(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return []string{"the filesystem's name folding could not be checked: " + sanitize(err.Error())}
	}
	probe, err := os.MkdirTemp(base, "collision-probe-")
	if err != nil {
		return []string{"the filesystem's name folding could not be checked: " + sanitize(err.Error())}
	}
	defer func() {
		os.RemoveAll(probe)
		if statErr != nil {
			os.Remove(base) // only if this created it and it is empty
		}
	}()
	var out []string
	made := map[string]string{} // probe file -> change path
	for _, c := range cs.Changes {
		p := filepath.Join(probe, filepath.FromSlash(c.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			out = append(out, fmt.Sprintf("%s cannot sit where the changeset puts it: %s", visible(c.Path), sanitize(err.Error())))
			continue
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			other := "an earlier path"
			if ni, serr := os.Stat(p); serr == nil {
				for mp, name := range made {
					if mi, merr := os.Stat(mp); merr == nil && os.SameFile(ni, mi) {
						other = visible(name)
					}
				}
			}
			out = append(out, fmt.Sprintf("%s and %s are the same file on this filesystem (it folds case or Unicode normalization); applying both would lose one", other, visible(c.Path)))
			continue
		}
		f.Close()
		made[p] = c.Path
	}
	return out
}
