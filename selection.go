// selection.go: the one file-set contract of a workspace upload (KS-026,
// leading into KS-027; C09). One workspace root, named explicitly and
// resolved; nothing outside it is read. The selection is built the same
// way by preview, init, approve and run, from the same policy, so what
// the preview lists is what the archive carries, path for path.
//
// Precedence, in this order and no other: mandatory exclusions (the
// repository's history and the client's own job files), sensitive-path
// blocks (names and directories that hold credentials), the project's
// ignore rules (.gitignore, honored for tracked paths too: this is an
// upload policy, not git's), then explicitly reviewed inclusion
// overrides (--allow), which can lift a sensitive block or an ignore rule
// and never a mandatory exclusion. Symlinks are refused, every one of
// them, with the list: the first certified uploader follows no link.
//
// The selection manifest records the policy version, every included path
// with size, mode and digest, every excluded path with its reason, the
// overrides, and a digest over all of it. Changed files or a changed
// policy change the digest; an approval is bound to it.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	selectionPolicyVersion = "ks-upload-policy/1"
	cruiseSelection        = ".keepstate/selection.json"
)

type selFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"` // 0644 or 0755: the executable bit only
	SHA256 string `json:"sha256"`
}

type selExcluded struct {
	Path   string `json:"path"`
	Reason string `json:"reason"` // mandatory | sensitive | ignored, with the rule
}

type selection struct {
	PolicyVersion string        `json:"policy_version"`
	Included      []selFile     `json:"included"`
	Excluded      []selExcluded `json:"excluded"`
	Symlinks      []string      `json:"symlinks"` // refused; present so the preview can name them
	Overrides     []string      `json:"overrides"`
	Files         int           `json:"files"`
	Bytes         int64         `json:"bytes"`
	Digest        string        `json:"digest"`
}

var errSymlinks = errors.New("symlinks are refused")

// ---- sensitive paths ----------------------------------------------------------

// sensitiveNames are file names that hold credentials by convention; a
// match is excluded with a warning, never printed, and may be lifted by
// an explicit --allow of that exact path.
var sensitiveNames = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.kdbx", "*.secret",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "credentials.json", "service-account*.json",
	".netrc", ".npmrc", ".pypirc", ".htpasswd", ".git-credentials", "secrets.yml", "secrets.yaml", "secrets.json",
}

// sensitiveExamples are the conventional sample files that carry no secret.
var sensitiveExamples = []string{".env.example", ".env.sample", ".env.template", ".env.dist"}

// sensitiveDirs are directories that hold credentials by convention.
var sensitiveDirs = []string{".aws", ".ssh", ".gnupg", ".kube", ".docker"}

func sensitiveReason(name string, isDir bool) string {
	if isDir {
		for _, d := range sensitiveDirs {
			if name == d {
				return "sensitive: directory " + d + "/ holds credentials by convention"
			}
		}
		return ""
	}
	for _, ex := range sensitiveExamples {
		if name == ex {
			return ""
		}
	}
	for _, p := range sensitiveNames {
		if ok, _ := path.Match(p, name); ok {
			return "sensitive: name matches " + p
		}
	}
	return ""
}

// ---- .gitignore rules -----------------------------------------------------------

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	base    string // the directory the .gitignore lives in, "" for the root
	text    string
}

// globToRegexp turns one gitignore pattern into a regexp over a path
// relative to the rule's directory (no leading slash).
func globToRegexp(pat string, anchored bool) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("^")
	if !anchored {
		sb.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch {
		case c == '*' && i+1 < len(pat) && pat[i+1] == '*':
			// ** : any depth; "**/" and "/**" absorb their slash
			if i+2 < len(pat) && pat[i+2] == '/' {
				sb.WriteString("(?:.*/)?")
				i += 2
			} else {
				sb.WriteString(".*")
				i++
			}
		case c == '*':
			sb.WriteString("[^/]*")
		case c == '?':
			sb.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(pat[i:], ']')
			if j < 0 {
				sb.WriteString(`\[`)
			} else {
				sb.WriteString(pat[i : i+j+1])
				i += j
			}
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return regexp.MustCompile(`^$`) // an unparseable rule matches nothing, and is reported by the preview
	}
	return re
}

func parseIgnoreFile(base, file string) []ignoreRule {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []ignoreRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{base: base, text: line}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasPrefix(line, `\`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		r.re = globToRegexp(line, anchored)
		rules = append(rules, r)
	}
	return rules
}

// ignoredBy answers the last rule that decides rel (a path relative to
// the root), or "" when none does; the last matching rule wins, as in git.
func ignoredBy(rules []ignoreRule, rel string, isDir bool) string {
	decision := ""
	for _, r := range rules {
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = strings.TrimPrefix(rel, r.base+"/")
		}
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(sub) {
			if r.negate {
				decision = ""
			} else {
				decision = r.text
				if r.base != "" {
					decision = r.base + "/.gitignore: " + r.text
				} else {
					decision = ".gitignore: " + r.text
				}
			}
		}
	}
	return decision
}

// ---- the selection --------------------------------------------------------------

func allowed(overrides []string, rel string) bool {
	for _, o := range overrides {
		o = strings.TrimSuffix(strings.TrimPrefix(o, "./"), "/")
		if o == rel || strings.HasPrefix(rel, o+"/") {
			return true
		}
	}
	return false
}

// buildSelection walks the root in the judge's order and decides every
// entry once. It reads no file content; packSelected does, by descriptor,
// and reconciles sizes. Symlinks are collected and the walk continues so
// the preview can list them all; the error names them.
func buildSelection(root string, overrides []string) (*selection, error) {
	sel := &selection{PolicyVersion: selectionPolicyVersion, Included: []selFile{}, Excluded: []selExcluded{}, Symlinks: []string{}, Overrides: append([]string{}, overrides...)}
	sort.Strings(sel.Overrides)
	var rules []ignoreRule
	if fi, err := os.Lstat(filepath.Join(root, ".gitignore")); err == nil && fi.Mode().IsRegular() {
		rules = parseIgnoreFile("", filepath.Join(root, ".gitignore"))
	}
	dirFiles := map[string][]string{}
	var dirs []string
	var walk func(rel string) error
	walk = func(rel string) error {
		dirs = append(dirs, rel)
		ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := e.Name()
			crel := path.Join(rel, name)
			if e.Type()&fs.ModeSymlink != 0 {
				sel.Symlinks = append(sel.Symlinks, crel)
				continue
			}
			isDir := e.IsDir()
			if excludedName(name) {
				sel.Excluded = append(sel.Excluded, selExcluded{Path: crel, Reason: "mandatory: " + map[string]string{".git": "the repository's history", ".keepstate": "the client's job files"}[name]})
				continue
			}
			if reason := sensitiveReason(name, isDir); reason != "" && !allowed(overrides, crel) {
				sel.Excluded = append(sel.Excluded, selExcluded{Path: crel, Reason: reason})
				continue
			}
			if rule := ignoredBy(rules, crel, isDir); rule != "" && !allowed(overrides, crel) {
				sel.Excluded = append(sel.Excluded, selExcluded{Path: crel, Reason: "ignored: " + rule})
				continue
			}
			if isDir {
				if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(crel), ".gitignore")); err == nil && fi.Mode().IsRegular() {
					rules = append(rules, parseIgnoreFile(crel, filepath.Join(root, filepath.FromSlash(crel), ".gitignore"))...)
				}
				if err := walk(crel); err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() {
				return fmt.Errorf("%s: not a regular file (sockets, devices and pipes cannot enter a job)", crel)
			}
			fi, err := e.Info()
			if err != nil {
				return err
			}
			mode := "0644"
			if fi.Mode()&0o111 != 0 {
				mode = "0755"
			}
			sel.Included = append(sel.Included, selFile{Path: crel, Size: fi.Size(), Mode: mode})
			sel.Bytes += fi.Size()
			dirFiles[rel] = append(dirFiles[rel], name)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	// the judge's order: directories by full path, then names within each
	sort.Strings(dirs)
	ordered := make([]selFile, 0, len(sel.Included))
	byPath := map[string]selFile{}
	for _, f := range sel.Included {
		byPath[f.Path] = f
	}
	for _, d := range dirs {
		names := dirFiles[d]
		sort.Strings(names)
		for _, n := range names {
			ordered = append(ordered, byPath[path.Join(d, n)])
		}
	}
	sel.Included = ordered
	sel.Files = len(ordered)
	sort.Slice(sel.Excluded, func(i, j int) bool { return sel.Excluded[i].Path < sel.Excluded[j].Path })
	sort.Strings(sel.Symlinks)
	if sel.Bytes > cruiseMaxBytes {
		return sel, tooLarge(sel.Bytes)
	}
	if len(sel.Symlinks) > 0 {
		return sel, fmt.Errorf("%w: %s; a link is never followed into or out of the workspace, remove it or replace it with the file", errSymlinks, strings.Join(sel.Symlinks, ", "))
	}
	return sel, nil
}

// selectionDigest is sha256 over the canonical selection: policy,
// included paths with size, mode and content digest, exclusions with
// reasons, overrides. Two unchanged previews yield the same digest.
func selectionDigest(sel *selection) string {
	type canon struct {
		Policy    string        `json:"policy_version"`
		Included  []selFile     `json:"included"`
		Excluded  []selExcluded `json:"excluded"`
		Overrides []string      `json:"overrides"`
	}
	b, _ := json.Marshal(canon{sel.PolicyVersion, sel.Included, sel.Excluded, sel.Overrides})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readSelectionOverrides(root string) []string {
	b, err := os.ReadFile(filepath.Join(root, cruiseSelection))
	if err != nil {
		return nil
	}
	var s selection
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return s.Overrides
}

func writeSelection(root string, sel *selection) error {
	b, err := json.MarshalIndent(sel, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, cruiseSelection), append(b, '\n'), 0o644)
}

// editScopeReport names the allowed-to-edit globs of the draft that reach
// no included file: an edit scope outside the upload is a mistake the
// preview should show, distinctly from the exclusions.
func editScopeReport(sel *selection, allowedPaths []any) []string {
	var out []string
	for _, a := range allowedPaths {
		g, _ := a.(string)
		if g == "" || g == "**" {
			continue
		}
		hits, blocked := 0, 0
		matches := func(p string) bool {
			if ok, _ := path.Match(g, p); ok {
				return true
			}
			gg := strings.TrimSuffix(g, "/")
			return strings.HasPrefix(p, gg+"/") || strings.HasPrefix(p, strings.TrimSuffix(gg, "/**")+"/")
		}
		for _, f := range sel.Included {
			if matches(f.Path) {
				hits++
			}
		}
		for _, x := range sel.Excluded {
			if matches(x.Path) {
				blocked++
			}
		}
		if hits == 0 {
			out = append(out, fmt.Sprintf("%s: may be edited but reaches no uploaded file (%d excluded path(s) match it)", g, blocked))
		}
	}
	return out
}

// ---- ks cruise preview ------------------------------------------------------------

// cruisePreview shows exactly what an upload would carry, and what it
// would not and why, from the same selection the upload uses. It makes
// no request and writes nothing. Names and metadata only: no content of
// any file, sensitive or not, is printed.
func cruisePreview(inv *Invocation) {
	root, err := os.Getwd()
	if err != nil {
		die(err)
	}
	overrides := inv.List("allow")
	if len(overrides) == 0 {
		overrides = readSelectionOverrides(root)
	}
	files, sel, err := scanSelected(root, nil, overrides)
	if err != nil && !errors.Is(err, errSymlinks) {
		die(err)
	}
	if sel == nil {
		die(err)
	}
	// the verifier inputs that would be pinned (KS-072): the draft's pinned
	// set when there is a draft, else what discovery would pin -- every
	// input of the one ecosystem with tests, or of all of them when more
	// than one has tests (init then asks which)
	shas := map[string]string{}
	for _, f := range sel.Included {
		shas[f.Path] = f.SHA256
	}
	disc := ks072Discover(shas, nil)
	pinnedSet := map[string]bool{}
	for _, e := range disc.WithTests {
		for _, r := range ks072InputsOf(disc, e) {
			pinnedSet[r.Path] = true
		}
	}
	isTest := func(rel string) bool { return pinnedSet[rel] }
	var allowedPaths []any
	if m, derr := readDraft(); derr == nil {
		if ver, _ := m["verifier"].(map[string]any); ver != nil {
			if pp, ok := ver["prohibited_paths"].([]any); ok {
				draftSet := map[string]bool{}
				for _, x := range pp {
					if r, _ := x.(string); r != "" && !strings.HasSuffix(r, "/") {
						draftSet[r] = true
					}
				}
				if len(draftSet) > 0 {
					isTest = func(rel string) bool { return draftSet[rel] }
				}
			}
			allowedPaths, _ = ver["allowed_paths"].([]any)
		}
	}
	var tests []string
	for _, f := range sel.Included {
		if isTest(f.Path) {
			tests = append(tests, f.Path)
		}
	}
	largest := append([]selFile{}, sel.Included...)
	sort.SliceStable(largest, func(i, j int) bool { return largest[i].Size > largest[j].Size })
	if len(largest) > 5 {
		largest = largest[:5]
	}
	byKind := map[string][]selExcluded{}
	for _, x := range sel.Excluded {
		kind := strings.SplitN(x.Reason, ":", 2)[0]
		byKind[kind] = append(byKind[kind], x)
	}
	scope := editScopeReport(sel, allowedPaths)
	blocked := len(sel.Symlinks) > 0
	_ = files
	report := map[string]any{
		"policy_version":     sel.PolicyVersion,
		"root":               root,
		"included":           sel.Included,
		"files":              sel.Files,
		"bytes":              sel.Bytes,
		"largest":            largest,
		"tests":              tests,
		"excluded":           sel.Excluded,
		"sensitive_warnings": byKind["sensitive"],
		"symlinks_refused":   sel.Symlinks,
		"edit_scope_outside": scope,
		"overrides":          sel.Overrides,
		"selection_digest":   sel.Digest,
		"uploadable":         !blocked && sel.Bytes <= cruiseMaxBytes,
	}
	emit(report, func() {
		fmt.Printf("upload preview (%s), root %s\n", sel.PolicyVersion, root)
		fmt.Printf("included: %d files, %s\n", sel.Files, mib(sel.Bytes))
		for _, f := range largest {
			fmt.Printf("  largest: %s (%d bytes)\n", f.Path, f.Size)
		}
		fmt.Printf("tests pinned by the check: %d\n", len(tests))
		for _, kind := range []string{"mandatory", "sensitive", "ignored"} {
			if xs := byKind[kind]; len(xs) > 0 {
				fmt.Printf("excluded, %s (%d):\n", kind, len(xs))
				for _, x := range xs {
					fmt.Printf("  %s  [%s]\n", x.Path, strings.TrimSpace(strings.SplitN(x.Reason, ":", 2)[1]))
				}
				if kind == "sensitive" {
					fmt.Println("  a sensitive path is uploaded only by an explicit review: ks cruise init --allow PATH")
				}
			}
		}
		if blocked {
			fmt.Printf("symlinks, refused (%d): %s\n", len(sel.Symlinks), strings.Join(sel.Symlinks, ", "))
			fmt.Println("  a link is never followed; remove it or replace it with the file before init")
		}
		if len(sel.Overrides) > 0 {
			fmt.Printf("overrides in force: %s\n", strings.Join(sel.Overrides, ", "))
		}
		for _, s := range scope {
			fmt.Printf("edit scope outside the upload: %s\n", s)
		}
		fmt.Printf("selection digest: %s\n", sel.Digest)
		if blocked {
			fmt.Println("this workspace cannot be uploaded as it is")
		}
	})
	if blocked {
		os.Exit(exitUsage)
	}
}
