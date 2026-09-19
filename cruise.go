// ks cruise: accepted work on the fleet. A job is a goal, a check and a
// ladder of models, written into an acceptance manifest that the customer
// approves before anything runs. The fleet runs one attempt per rung from
// an exact save point, verifies the candidate in a verifier the agent
// cannot reach, and accepts only when the check passes; a failed rung
// escalates to the next one, and after the last rung the job waits in the
// customer's review queue. This file is the client side of
// docs/cruise-contract.md section 4: it drafts and locks the manifest,
// uploads the workspace and reads the job back. It never calls a model,
// and init and approve make no request at all.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	cruiseDir       = ".keepstate"
	cruiseDraft     = ".keepstate/cruise.json"
	cruiseLock      = ".keepstate/cruise.lock"
	cruiseVerifier  = ".keepstate/verifier.json"
	cruiseMaxBytes  = 50 << 20 // the contract's workspace limit
	cruiseTimeS     = 1800
	cruiseSpendUSD  = "2"
	cruiseReserve   = 200000
	cruiseCheapCap  = 4000
	cruiseEverySecs = 300
)

var cruiseUsageText = `ks cruise: accepted work on the fleet

usage:
  ks cruise init [--goal TEXT] [--tests CMD] [--paths GLOB...] [--ladder family:model,...] [--spend USD]
                              draft the acceptance manifest into .keepstate/cruise.json
                              (ks cruise init --help lists every option)
  ks cruise approve           lock the draft (digest and time) into .keepstate/cruise.lock
  ks cruise run               upload the workspace and the locked manifest; start the job
  ks cruise status [JOB]      the job (or the newest): rung, attempts, save points, spend, verdict
  ks cruise logs JOB          the event stream, one event per line
  ks cruise cancel JOB        cancel a job
  ks cruise resume JOB [--ladder family:model,...]   resume a job from review, on the same or a wider ladder
  ks cruise artifact JOB [--out FILE]               download the accepted tarball (sha256-verified)
  ks cruise models            the model table the control plane serves (families, rungs, default ladder)

A job is a goal, a check and a ladder of models. init reads the repository
and drafts the manifest; nothing runs until you approve it. The check is the
test command init finds (pytest, npm test, go test) or the one you name with
--tests; a goal with no check is not a job, and init stops and says so.
Every rung runs from an exact save point; a candidate is accepted only when
the check passes in a verifier the agent cannot reach. After the last rung
the job waits in your review queue.

Every help invocation makes no request; neither do init and approve. init
never calls a model. Defaults: 1800 s per attempt, a $2 spend ceiling
(--spend), a workspace of at most 50 MiB (.git and .keepstate excluded).
`

func cruiseUsage() { fmt.Print(cruiseUsageText) }

// sabotageHelpHook is gate CR-7's sabotage: with KS_CLI_SABOTAGE_HELP=1,
// "ks cruise run --help" performs one GET before printing help, which is
// the defect class the help guard exists to prevent, so the gate can
// prove its zero-request counter bites. Test-only; never set otherwise.
func sabotageHelpHook() {
	if os.Getenv("KS_CLI_SABOTAGE_HELP") != "1" || len(os.Args) < 3 || os.Args[2] != "run" {
		return
	}
	if c, ok := hostedToken(); ok {
		_ = hostedCall(c, "GET", "/api/models", nil, nil)
	}
}

// mustCreds is the sign-in gate the hosted verbs share. run calls it only
// after its local checks, so a refusal never needs a token or a request.
func mustCreds() hostedCreds {
	cr, ok := hostedToken()
	if !ok {
		fmt.Fprintln(os.Stderr, "Not signed in. Run: ks login")
		os.Exit(2)
	}
	return cr
}

func csvList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// The workspace: one walk, in the judge's order, for the digest and the tar
// ---------------------------------------------------------------------

type wsFile struct {
	rel  string // forward-slash path relative to the workspace root
	exec bool
	size int64
	sha  [32]byte // sha256 of the content
}

// excludedName is the set of entries the workspace never carries, at any
// depth: the repository's own history and the client's job files.
func excludedName(name string) bool { return name == ".git" || name == ".keepstate" }

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20)) }

// tooLarge is the contract's limit, stated in its own words so the
// refusal names the number and what it excludes.
func tooLarge(n int64) error {
	return fmt.Errorf("the workspace is %s of files (.git and .keepstate excluded), over the 50 MiB limit for a job; move large untracked directories out of the workspace", mib(n))
}

type capWriter struct {
	w io.Writer
	n int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > cruiseMaxBytes {
		return 0, tooLarge(c.n + int64(len(p)))
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// scanWorkspace lists the workspace files in exactly the order
// judge/manifest.py's tree_digest visits them and, when out is not nil,
// writes a gzip tarball of them to out in that same order. The order is
// os.walk's after sorted(): every directory sorted by its full path, then
// the files of each directory sorted by name. That is not a plain sort of
// file paths: "a-x/h" precedes "a/b/g" because the directory "a-x" sorts
// before the directory "a/b" ('-' is below '/'). A symlink to a directory
// is not descended (os.walk does not follow links); a symlink to a file is
// read through, and stored in the tarball as the file it points to, so
// the unpacked tree digests to the same value the client computed.
//
// The size limit is checked on the files themselves, before anything is
// packed, so an oversize workspace is refused without reading it through.
//
// The tarball is deterministic: fixed epoch mtime, uid and gid 0, no
// names, mode 0644 or 0755 (the executable bit only), entries in walk
// order, regular files only. Two packs of an unchanged tree are the same
// bytes, so the sha256 written at init is the sha256 checked at run.
func scanWorkspace(root string, out io.Writer) ([]wsFile, error) {
	dirFiles := map[string][]string{}
	var dirs []string
	var total int64
	var collect func(rel string) error
	collect = func(rel string) error {
		dirs = append(dirs, rel)
		ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := e.Name()
			if excludedName(name) {
				continue
			}
			crel := path.Join(rel, name)
			if e.Type()&fs.ModeSymlink != 0 {
				fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(crel)))
				if err != nil {
					return fmt.Errorf("%s: broken symlink; remove it or point it at a file", crel)
				}
				if fi.IsDir() {
					continue
				}
				if !fi.Mode().IsRegular() {
					return fmt.Errorf("%s: not a regular file", crel)
				}
				total += fi.Size()
				dirFiles[rel] = append(dirFiles[rel], name)
				continue
			}
			if e.IsDir() {
				if err := collect(crel); err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() {
				return fmt.Errorf("%s: not a regular file (sockets, devices and pipes cannot enter a job)", crel)
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
			dirFiles[rel] = append(dirFiles[rel], name)
		}
		return nil
	}
	if err := collect(""); err != nil {
		return nil, err
	}
	if total > cruiseMaxBytes {
		return nil, tooLarge(total)
	}
	sort.Strings(dirs)

	var gz *gzip.Writer
	var tw *tar.Writer
	if out != nil {
		gz = gzip.NewWriter(out) // zero header: no name, no mtime
		tw = tar.NewWriter(gz)
	}
	var files []wsFile
	for _, d := range dirs {
		names := dirFiles[d]
		sort.Strings(names)
		for _, name := range names {
			rel := path.Join(d, name)
			abs := filepath.Join(root, filepath.FromSlash(rel))
			fi, err := os.Stat(abs)
			if err != nil {
				return nil, err
			}
			f, err := os.Open(abs)
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			var w io.Writer = h
			if tw != nil {
				mode := int64(0o644)
				if fi.Mode()&0o111 != 0 {
					mode = 0o755
				}
				hdr := &tar.Header{
					Typeflag: tar.TypeReg,
					Name:     rel,
					Size:     fi.Size(),
					Mode:     mode,
					ModTime:  time.Unix(0, 0),
				}
				if err := tw.WriteHeader(hdr); err != nil {
					f.Close()
					return nil, err
				}
				w = io.MultiWriter(h, tw)
			}
			n, err := io.Copy(w, f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if n != fi.Size() {
				return nil, fmt.Errorf("%s changed while it was being read", rel)
			}
			wf := wsFile{rel: rel, exec: fi.Mode()&0o111 != 0, size: n}
			copy(wf.sha[:], h.Sum(nil))
			files = append(files, wf)
		}
	}
	if tw != nil {
		if err := tw.Close(); err != nil {
			return nil, err
		}
		if err := gz.Close(); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// treeDigest is judge/manifest.py tree_digest over the scanned files: for
// each file in walk order, sha256 absorbs the relative path, a NUL byte and
// the raw sha256 of the content. It is the manifest's initial_state_digest,
// which the fleet recomputes over the unpacked workspace.
func treeDigest(files []wsFile) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.rel))
		h.Write([]byte{0})
		h.Write(f.sha[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// testsDigest pins the check: sha256 over the test files in sorted path
// order, each as its path bytes followed by its content bytes. It is the
// manifest's verifier.tests_digest, recomputed by run and by gate CR-1
// with this same rule, so a test changed after approve is refused.
func testsDigest(root string, files []wsFile, isTest func(rel string) bool) (string, int, error) {
	var rels []string
	for _, f := range files {
		if isTest(f.rel) {
			rels = append(rels, f.rel)
		}
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", 0, err
		}
		h.Write([]byte(rel))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), len(rels), nil
}

// ---------------------------------------------------------------------
// Canonical JSON: the bytes the digest is over
// ---------------------------------------------------------------------

// canonicalJSON renders v as the bytes the manifest digest is taken over.
// The rule, byte for byte, is Python's
// json.dumps(m, sort_keys=True, separators=(",", ":")) as
// judge/manifest.py canonical() computes it, so the digest the customer
// sees at approve is the digest the fleet locks at intake. The rule: keys
// sorted by code point at every level; no whitespace; strings escape only
// the quote, the backslash and the five short escapes (\b \f \n \r \t),
// and render every other character outside 0x20..0x7e as lowercase
// \uXXXX (an astral character as a surrogate pair); no HTML escaping;
// whole numbers only (a fraction is refused, never rounded); true, false
// and null as words; no trailing newline. For a manifest whose text is
// plain ASCII this is exactly json.Marshal over sorted keys with HTML
// escaping off and no trailing newline.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		return writeCanonicalString(b, x)
	case json.Number:
		n, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return fmt.Errorf("number %q is not a whole number; the manifest carries whole numbers only", x)
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if x != math.Trunc(x) || math.Abs(x) >= 1<<53 {
			return fmt.Errorf("number %v is not a whole number; the manifest carries whole numbers only", x)
		}
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte order equals code-point order for UTF-8
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonicalString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []string:
		xs := make([]any, len(x))
		for i, s := range x {
			xs[i] = s
		}
		return writeCanonical(b, xs)
	default:
		// any other shape goes through encoding/json once and comes back
		// as the generic shapes above, with numbers kept verbatim
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var g any
		if err := dec.Decode(&g); err != nil {
			return err
		}
		return writeCanonical(b, g)
	}
	return nil
}

func writeCanonicalString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("text is not valid UTF-8")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800|((r>>10)&0x3ff), 0xdc00|(r&0x3ff))
			}
		}
	}
	b.WriteByte('"')
	return nil
}

// manifestSHA is the digest printed by init and approve, sent by run, and
// recomputed by the control plane: sha256 over canonicalJSON(m).
func manifestSHA(m map[string]any) (string, error) {
	b, err := canonicalJSON(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------------
// The check: what decides done
// ---------------------------------------------------------------------

type checks struct {
	command string
	kind    string // pytest | npm | go | named
	isTest  func(rel string) bool
}

func hasDirComponent(rel string, names ...string) bool {
	for _, comp := range strings.Split(path.Dir(rel), "/") {
		for _, n := range names {
			if comp == n {
				return true
			}
		}
	}
	return false
}

// the test files each runner reads: pinned into tests_digest, listed as
// prohibited for the attempt, and recomputed at run
func isPytestFile(rel string) bool {
	base := path.Base(rel)
	return strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")
}

func isGoTestFile(rel string) bool {
	return strings.HasSuffix(path.Base(rel), "_test.go") || hasDirComponent(rel, "testdata")
}

func isNpmTestFile(rel string) bool {
	base := path.Base(rel)
	return rel == "package.json" || hasDirComponent(rel, "test", "tests", "__tests__") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func isAnyTestFile(rel string) bool {
	return isPytestFile(rel) || isGoTestFile(rel) || isNpmTestFile(rel)
}

// detectChecks finds the test command from the scanned files: pytest when
// test_*.py files exist, npm test when package.json has a real
// scripts.test, go test when go.mod exists alongside at least one _test.go
// file. Nothing is inferred from a goal.
func detectChecks(root string, files []wsFile) checks {
	pyTests, goTests, goMod, pkgJSON := 0, 0, false, false
	for _, f := range files {
		switch {
		case isPytestFile(f.rel):
			pyTests++
		case strings.HasSuffix(path.Base(f.rel), "_test.go"):
			goTests++
		case f.rel == "go.mod":
			goMod = true
		case f.rel == "package.json":
			pkgJSON = true
		}
	}
	if pyTests > 0 {
		return checks{command: "python3 -m pytest -q", kind: "pytest", isTest: isPytestFile}
	}
	if pkgJSON {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil && json.Unmarshal(b, &pkg) == nil {
			if s := strings.TrimSpace(pkg.Scripts["test"]); s != "" && !strings.Contains(s, "no test specified") {
				return checks{command: "npm test", kind: "npm", isTest: isNpmTestFile}
			}
		}
	}
	if goMod && goTests > 0 {
		return checks{command: "go test ./...", kind: "go", isTest: isGoTestFile}
	}
	return checks{}
}

// checksFor recovers the test-file rule from a manifest's verifier.command,
// so run pins the same files init did.
func checksFor(command string) func(rel string) bool {
	switch {
	case strings.Contains(command, "pytest"):
		return isPytestFile
	case strings.HasPrefix(command, "npm "):
		return isNpmTestFile
	case strings.HasPrefix(command, "go test"):
		return isGoTestFile
	}
	return isAnyTestFile
}

// ---------------------------------------------------------------------
// The model table and the ladder
// ---------------------------------------------------------------------

type rung struct {
	Family string `json:"family"`
	Model  string `json:"model"`
}

type modelFamily struct {
	Provider string   `json:"provider"`
	Rungs    []string `json:"rungs"`
}

type modelTable struct {
	Version       string                 `json:"version"`
	Families      map[string]modelFamily `json:"families"`
	DefaultLadder []rung                 `json:"default_ladder"`
}

// embeddedModels mirrors ctl/models.json v1, the table GET /api/models
// serves (docs/cruise-contract.md section 1.5). init drafts from it so it
// makes no request; ks cruise models shows the live table, and the control
// plane decides a ladder's validity at run.
var embeddedModels = modelTable{
	Version: "v1",
	Families: map[string]modelFamily{
		"anthropic": {Provider: "anthropic", Rungs: []string{"claude-haiku-4-5-20251001", "claude-sonnet-5"}},
		"openai":    {Provider: "openrouter", Rungs: []string{"openai/gpt-4o-mini", "openai/gpt-4o"}},
	},
	DefaultLadder: []rung{
		{Family: "anthropic", Model: "claude-haiku-4-5-20251001"},
		{Family: "anthropic", Model: "claude-sonnet-5"},
	},
}

// parseLadder reads --ladder: "family:model" pairs in order, or a bare
// model id when the embedded table knows it. A repeated rung is refused
// (a ladder never retries a rung); a pair the table does not carry is
// passed through with a note, since the control plane holds the live
// table.
func parseLadder(specs []string) ([]rung, error) {
	var out []rung
	seen := map[string]bool{}
	for _, s := range specs {
		var r rung
		if fam, model, ok := strings.Cut(s, ":"); ok {
			if fam == "" || model == "" {
				return nil, fmt.Errorf("rung %q: write it as family:model", s)
			}
			r = rung{Family: fam, Model: model}
		} else {
			for _, fam := range familyNames(embeddedModels) {
				for _, m := range embeddedModels.Families[fam].Rungs {
					if m == s {
						r = rung{Family: fam, Model: m}
					}
				}
			}
			if r.Model == "" {
				return nil, fmt.Errorf("rung %q is not in the model table this client carries; write it as family:model (ks cruise models shows the live table)", s)
			}
		}
		key := r.Family + ":" + r.Model
		if seen[key] {
			return nil, fmt.Errorf("rung %s is listed twice; a ladder never repeats a rung", key)
		}
		seen[key] = true
		if !embeddedModels.knows(r) {
			fmt.Fprintf(os.Stderr, "note: rung %s is not in the model table this client carries (%s); the control plane decides at run\n", key, embeddedModels.Version)
		}
		out = append(out, r)
	}
	return out, nil
}

func (t modelTable) knows(r rung) bool {
	for _, m := range t.Families[r.Family].Rungs {
		if m == r.Model {
			return true
		}
	}
	return false
}

func familyNames(t modelTable) []string {
	names := make([]string, 0, len(t.Families))
	for f := range t.Families {
		names = append(names, f)
	}
	sort.Strings(names)
	return names
}

// ladderProviders is permitted.providers: the provider of each family in
// the ladder, in ladder order, once each. A family the table does not
// carry names itself.
func ladderProviders(t modelTable, ladder []rung) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, r := range ladder {
		p := t.Families[r.Family].Provider
		if p == "" {
			p = r.Family
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func ladderWords(ladder []rung) string {
	var s []string
	for _, r := range ladder {
		s = append(s, r.Family+":"+r.Model)
	}
	return strings.Join(s, ", ")
}

func cruiseModels(inv *Invocation) {
	c := mustCreds()
	var t modelTable
	if err := hostedCall(c, "GET", "/api/models", nil, &t); err != nil {
		die(err)
	}
	fmt.Printf("model table %s (from %s)\n", t.Version, c.CTL)
	for _, f := range familyNames(t) {
		fmt.Printf("  %s (provider %s): %s\n", f, t.Families[f].Provider, strings.Join(t.Families[f].Rungs, ", "))
	}
	if len(t.DefaultLadder) == 0 {
		fmt.Println("default ladder: unavailable")
	} else {
		fmt.Printf("default ladder: %s\n", ladderWords(t.DefaultLadder))
	}
	if t.Version != embeddedModels.Version {
		fmt.Printf("this client drafts from table %s; name rungs with --ladder family:model to use the live one\n", embeddedModels.Version)
	}
}

// ---------------------------------------------------------------------
// init
// ---------------------------------------------------------------------

func cruiseInit(inv *Invocation) {
	root, err := os.Getwd()
	if err != nil {
		die(err)
	}
	spend, _ := parseUSD(cruiseSpendUSD)
	if inv.Set("spend") {
		spend = inv.Int("spend") // parsed exactly by the schema
	}
	paths := inv.List("paths")
	ladder, err := parseLadder(csvList(inv.Str("ladder")))
	if err != nil {
		die(err)
	}
	fromTable := len(ladder) == 0
	if fromTable {
		ladder = embeddedModels.DefaultLadder
	}

	// 1. the workspace, packed once to learn the tarball's digest and size
	h := sha256.New()
	cw := &capWriter{w: h}
	files, err := scanWorkspace(root, cw)
	if err != nil {
		die(err)
	}
	if len(files) == 0 {
		die(fmt.Errorf("the workspace has no files; run init inside the repository"))
	}
	tarSha := hex.EncodeToString(h.Sum(nil))

	// 2. the check; without one there is no job (ADR-030), and init stops
	//    here, before anything is written
	ck := detectChecks(root, files)
	named := strings.TrimSpace(inv.Str("tests"))
	if named != "" {
		ck = checks{command: named, kind: "named", isTest: checksFor(named)}
	}
	if ck.command == "" {
		fmt.Fprintln(os.Stderr, "no check found: a goal with no check is not a job.")
		fmt.Fprintln(os.Stderr, "init looks for pytest (test_*.py), npm test (package.json scripts.test) or go test (go.mod with _test.go files).")
		fmt.Fprintln(os.Stderr, "Name the command that decides done: ks cruise init --tests CMD. This version does not draft checks.")
		os.Exit(2)
	}
	tests, testCount, err := testsDigest(root, files, ck.isTest)
	if err != nil {
		die(err)
	}
	// v1 pins root-level test files only: the verifier copies each pinned
	// test flat into the checked tree, and the worker refuses a nested one.
	for _, f := range files {
		if ck.isTest(f.rel) && strings.Contains(f.rel, "/") {
			fmt.Fprintf(os.Stderr, "ks cruise init: test file %s is nested; this version pins test files at the repository root only.\n", f.rel)
			os.Exit(2)
		}
	}

	// 3. the goal: named, kept from the previous draft, or the check itself
	goal := strings.TrimSpace(inv.Str("goal"))
	if goal == "" {
		if prior, err := readDraft(); err == nil {
			goal, _ = prior["goal"].(string)
		}
	}
	if goal == "" {
		goal = "make the check pass: " + ck.command
	}

	// 4. the verifier config, written first so its hash is of the file
	allowed := []any{"**"}
	boundary := "every tracked file in the workspace may change; the test files and .keepstate may not"
	if len(paths) > 0 {
		allowed = allowed[:0]
		for _, p := range paths {
			allowed = append(allowed, p)
		}
		boundary = fmt.Sprintf("only files matching %s may change; the test files and .keepstate may not", strings.Join(paths, ", "))
	}
	prohibited := []any{".keepstate/"}
	for _, f := range files {
		if ck.isTest(f.rel) {
			prohibited = append(prohibited, f.rel)
		}
	}
	cfgBytes, err := canonicalJSON(map[string]any{"allowed_paths": allowed, "prohibited_paths": prohibited})
	if err != nil {
		die(err)
	}
	cfgSum := sha256.Sum256(cfgBytes)
	if err := os.MkdirAll(filepath.Join(root, cruiseDir), 0o755); err != nil {
		die(err)
	}
	if err := os.WriteFile(filepath.Join(root, cruiseVerifier), cfgBytes, 0o644); err != nil {
		die(err)
	}

	// 5. the manifest draft
	ladderAny := []any{}
	for _, r := range ladder {
		ladderAny = append(ladderAny, map[string]any{"family": r.Family, "model": r.Model})
	}
	m := map[string]any{
		"family":               "job",
		"version":              1,
		"goal":                 goal,
		"task_boundary":        boundary,
		"initial_state_digest": treeDigest(files),
		"workspace":            map[string]any{"sha256": tarSha, "bytes": cw.n, "root": "."},
		"image":                "claude",
		"permitted": map[string]any{
			"tools":            []any{"edit-source"},
			"providers":        ladderProviders(embeddedModels, ladder),
			"regions":          []any{"westeurope"},
			"external_effects": []any{},
		},
		"data_classification": "customer",
		"storage_consent":     "the account that submits this job authorizes KeepState to store this workspace, its save points and the accepted candidate for this job",
		"done_condition":      "verifier checks pass on the candidate",
		"verifier": map[string]any{
			"implementation": "judge/verifier.py",
			"command":        ck.command,
			"config_hash":    hex.EncodeToString(cfgSum[:]),
			"tests_digest":   tests,
			"checks_origin":  "repo",
			// the policy the control plane and the worker enforce; its
			// canonical bytes are what config_hash is the sha256 of
			"allowed_paths":    allowed,
			"prohibited_paths": prohibited,
		},
		"limits": map[string]any{
			"time_s":                  cruiseTimeS,
			"spend_microusd":          spend,
			"attempts":                len(ladder),
			"cheap_max_output_tokens": cruiseCheapCap,
			"reserve_microusd":        cruiseReserve,
		},
		"escalation": map[string]any{
			"human_review_on":   []any{"strong-tier-failure", "policy-violation"},
			"on_strong_failure": "human-review",
		},
		"ladder": ladderAny,
		"checkpoint": map[string]any{
			"before_attempt":   true,
			"every_s":          cruiseEverySecs,
			"after_pass":       true,
			"on_call_boundary": false,
		},
	}
	if err := writeDraft(root, m); err != nil {
		die(err)
	}
	lockRemoved := os.Remove(filepath.Join(root, cruiseLock)) == nil
	sha, err := manifestSHA(m)
	if err != nil {
		die(err)
	}

	// 6. the digest first, on its own line, then the summary
	fmt.Println(sha)
	fmt.Printf("manifest: %s (draft, version 1)\n", cruiseDraft)
	fmt.Printf("goal: %s\n", goal)
	how := "detected; override with --tests"
	if ck.kind == "named" {
		how = "named with --tests"
	}
	fmt.Printf("check: %s (%s)\n", ck.command, how)
	fmt.Printf("tests pinned: %d files, tests_digest %s\n", testCount, short(tests))
	fmt.Printf("boundary: %s\n", boundary)
	src := "from --ladder"
	if fromTable {
		src = "the default ladder of model table " + embeddedModels.Version
	}
	fmt.Printf("ladder: %s (%d rungs, %s)\n", ladderWords(ladder), len(ladder), src)
	fmt.Printf("limits: %d s per attempt, spend ceiling %s, reserve %s\n", cruiseTimeS, dollars(spend), dollars(cruiseReserve))
	fmt.Printf("workspace: %d files, %s bytes packed, tree digest %s\n", len(files), commas(cw.n), short(treeDigest(files)))
	fmt.Printf("save points: before every attempt, every %d s, after a pass\n", cruiseEverySecs)
	if lockRemoved {
		fmt.Println("previous approval removed: the draft changed")
	}
	fmt.Printf("next: review %s, then: ks cruise approve\n", cruiseDraft)
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func writeDraft(root string, m map[string]any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, cruiseDraft), b.Bytes(), 0o644)
}

// readDraft decodes the draft with numbers kept verbatim, so a whole
// number stays the whole number the digest is over.
func readDraft() (map[string]any, error) {
	b, err := os.ReadFile(cruiseDraft)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", cruiseDraft, err)
	}
	return m, nil
}

// validateManifest mirrors judge/manifest.py validate plus the v2 keys, so
// a draft the fleet would refuse at intake is refused here with the same
// words, before anything is uploaded.
func validateManifest(m map[string]any) error {
	var errs []string
	for _, k := range []string{"family", "version", "task_boundary", "initial_state_digest", "permitted",
		"data_classification", "storage_consent", "done_condition", "verifier", "limits", "escalation",
		"goal", "ladder", "checkpoint", "workspace", "image"} {
		if _, ok := m[k]; !ok {
			errs = append(errs, "missing '"+k+"'")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	perm, _ := m["permitted"].(map[string]any)
	for _, k := range []string{"tools", "providers", "regions", "external_effects"} {
		if _, ok := perm[k]; !ok {
			errs = append(errs, "permitted."+k+" missing")
		}
	}
	class, _ := m["data_classification"].(string)
	consent, _ := m["storage_consent"].(string)
	if class == "customer" && strings.TrimSpace(consent) == "" {
		errs = append(errs, "customer data requires a non-empty storage_consent naming the authorizing account")
	}
	if class != "public" && class != "synthetic" && class != "customer" {
		errs = append(errs, fmt.Sprintf("data_classification %q refused: only public, synthetic or customer may enter a session", class))
	}
	ver, _ := m["verifier"].(map[string]any)
	for _, k := range []string{"implementation", "config_hash", "tests_digest"} {
		if _, ok := ver[k]; !ok {
			errs = append(errs, "verifier."+k+" missing")
		}
	}
	lim, _ := m["limits"].(map[string]any)
	for _, k := range []string{"time_s", "spend_microusd", "attempts"} {
		if _, ok := lim[k]; !ok {
			errs = append(errs, "limits."+k+" missing")
		}
	}
	esc, _ := m["escalation"].(map[string]any)
	if _, ok := esc["human_review_on"]; !ok {
		errs = append(errs, "escalation.human_review_on missing")
	}
	if s, _ := esc["on_strong_failure"].(string); s != "human-review" {
		errs = append(errs, "escalation.on_strong_failure must be 'human-review' (never a silent retry)")
	}
	if l, _ := m["ladder"].([]any); len(l) == 0 {
		errs = append(errs, "ladder must list at least one rung")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------
// approve and run
// ---------------------------------------------------------------------

type lockFile struct {
	SHA256     string `json:"sha256"`
	ApprovedAt string `json:"approved_at"`
	Version    any    `json:"version,omitempty"`
}

func cruiseApprove(inv *Invocation) {
	root, err := os.Getwd()
	if err != nil {
		die(err)
	}
	m, err := readDraft()
	if err != nil {
		if os.IsNotExist(err) {
			die(fmt.Errorf("no draft at %s; run: ks cruise init", cruiseDraft))
		}
		die(err)
	}
	if err := validateManifest(m); err != nil {
		die(fmt.Errorf("draft refused: %v", err))
	}
	files, err := scanWorkspace(root, nil)
	if err != nil {
		die(err)
	}
	if want, _ := m["initial_state_digest"].(string); want != treeDigest(files) {
		die(fmt.Errorf("the workspace changed since init (tree digest %s, draft says %s); run ks cruise init again",
			short(treeDigest(files)), short(want)))
	}
	sha, err := manifestSHA(m)
	if err != nil {
		die(err)
	}
	lk := lockFile{SHA256: sha, ApprovedAt: time.Now().UTC().Format(time.RFC3339), Version: m["version"]}
	b, _ := json.MarshalIndent(lk, "", "  ")
	if err := os.WriteFile(filepath.Join(root, cruiseLock), append(b, '\n'), 0o644); err != nil {
		die(err)
	}
	fmt.Println(sha)
	fmt.Printf("approved: %s locks manifest version %v at %s\n", cruiseLock, m["version"], lk.ApprovedAt)
	fmt.Println("next: ks cruise run")
}

func readLock() (lockFile, error) {
	var lk lockFile
	b, err := os.ReadFile(cruiseLock)
	if err != nil {
		return lk, err
	}
	if err := json.Unmarshal(b, &lk); err != nil || lk.SHA256 == "" {
		return lk, fmt.Errorf("%s is not a lock file", cruiseLock)
	}
	return lk, nil
}

// cruiseRun refuses, in this order and before any request: no lock, an
// oversize workspace, a changed test, a changed draft, a changed
// workspace. Only then does it upload. It returns rather than exits so
// the packed temp file is always removed.
func cruiseRun(inv *Invocation) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	m, err := readDraft()
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no draft at %s; run: ks cruise init", cruiseDraft)
		}
		return err
	}
	lk, err := readLock()
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the draft is not approved; review %s, then: ks cruise approve", cruiseDraft)
		}
		return err
	}
	if err := validateManifest(m); err != nil {
		return fmt.Errorf("draft refused: %v", err)
	}
	c := mustCreds() // no request; exits before any temp file exists

	// the workspace, packed to a temp file; the size limit is checked on
	// the files before anything is packed
	tmp, err := os.CreateTemp("", "ks-cruise-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	cw := &capWriter{w: io.MultiWriter(tmp, h)}
	files, err := scanWorkspace(root, cw)
	if err != nil {
		return err
	}

	// the pinned tests, recomputed
	ver, _ := m["verifier"].(map[string]any)
	command, _ := ver["command"].(string)
	tests, _, err := testsDigest(root, files, checksFor(command))
	if err != nil {
		return err
	}
	if want, _ := ver["tests_digest"].(string); want != tests {
		return fmt.Errorf("the test files changed since approve: tests_digest is now %s, the locked manifest says %s; a changed check is a new job (ks cruise init, then approve)",
			short(tests), short(want))
	}

	// the draft, against the lock
	sha, err := manifestSHA(m)
	if err != nil {
		return err
	}
	if sha != lk.SHA256 {
		return fmt.Errorf("the draft changed since approve (digest %s, approved %s); review it, then: ks cruise approve", short(sha), short(lk.SHA256))
	}

	// the workspace, against the draft
	if want, _ := m["initial_state_digest"].(string); want != treeDigest(files) {
		return fmt.Errorf("the workspace changed since approve (tree digest %s, approved %s); run ks cruise init and approve again",
			short(treeDigest(files)), short(want))
	}
	tarSha := hex.EncodeToString(h.Sum(nil))
	ws, _ := m["workspace"].(map[string]any)
	if ws == nil {
		ws = map[string]any{}
		m["workspace"] = ws
	}
	filled := false
	if have, _ := ws["sha256"].(string); have != "" {
		if have != tarSha {
			return fmt.Errorf("the packed workspace (%s) is not the one approved (%s); run ks cruise init and approve again", short(tarSha), short(have))
		}
	} else {
		// the draft was approved without a workspace digest (a hand-written
		// draft): fill it now; the digest sent is of the filled manifest
		ws["sha256"], ws["bytes"], ws["root"] = tarSha, cw.n, "."
		filled = true
		if sha, err = manifestSHA(m); err != nil {
			return err
		}
	}
	manifestBytes, err := canonicalJSON(m)
	if err != nil {
		return err
	}

	// POST /api/jobs { manifest, manifest_sha, workspace_b64 }: the
	// manifest goes over the wire as its canonical bytes, so what the
	// control plane hashes is what the customer approved.
	var body bytes.Buffer
	body.WriteString(`{"manifest":`)
	body.Write(manifestBytes)
	body.WriteString(`,"manifest_sha":"` + sha + `","workspace_b64":"`)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	b64 := base64.NewEncoder(base64.StdEncoding, &body)
	if _, err := io.Copy(b64, tmp); err != nil {
		return err
	}
	b64.Close()
	body.WriteString(`"}`)

	var job map[string]any
	if err := hostedCall(c, "POST", "/api/jobs", body.Bytes(), &job); err != nil {
		return err
	}
	ceiling := "unavailable"
	if n, ok := jnum(job, "spend_ceiling_microusd"); ok {
		ceiling = dollars(n)
	} else if lim, _ := m["limits"].(map[string]any); lim != nil {
		if n, ok := jnum(lim, "spend_microusd"); ok {
			ceiling = dollars(n)
		}
	}
	if filled {
		fmt.Fprintf(os.Stderr, "workspace digest filled at run; the manifest digest sent is %s\n", sha)
	}
	ladder, _ := m["ladder"].([]any)
	fmt.Fprintf(os.Stderr, "job %s %s: spend ceiling %s, %d rungs, %s bytes packed (on %s)\n",
		jstr(job, "id"), jstr(job, "state"), ceiling, len(ladder), commas(cw.n), c.CTL)
	fmt.Println(jstr(job, "id"))
	return nil
}

// ---------------------------------------------------------------------
// status, logs, cancel, resume, artifact
// ---------------------------------------------------------------------

func jstr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return "unavailable"
}

func jnum(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// dollars renders microdollars as a dollar figure with at least two and
// at most six decimals: 2000000 is $2.00, 12345 is $0.012345.
func dollars(micro int64) string {
	sign := ""
	if micro < 0 {
		sign = "-"
		micro = -micro
	}
	frac := fmt.Sprintf("%06d", micro%1000000)
	for len(frac) > 2 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	return fmt.Sprintf("%s$%d.%s", sign, micro/1000000, frac)
}

// parseUSD reads a dollar amount ("2", "2.50", "$0.25") into microdollars
// exactly, with no float in the path. Six decimals at most.
func parseUSD(s string) (int64, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("%q has more than six decimals", s)
	}
	for _, part := range []string{whole, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("%q is not a dollar amount", s)
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 9_000_000_000_000 {
		return 0, fmt.Errorf("%q is not a dollar amount", s)
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
	}
	micro := w*1000000 + f
	if micro <= 0 {
		return 0, fmt.Errorf("the spend ceiling must be above zero")
	}
	return micro, nil
}

func fetchJob(c hostedCreds, id string) map[string]any {
	var job map[string]any
	if err := hostedCall(c, "GET", "/api/jobs/"+id, nil, &job); err != nil {
		die(err)
	}
	return job
}

func cruiseStatus(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	var job map[string]any
	if id == "" {
		var jobs []map[string]any
		if err := hostedCall(c, "GET", "/api/jobs", nil, &jobs); err != nil {
			die(err)
		}
		if len(jobs) == 0 {
			fmt.Println("no jobs on this account; start one with ks cruise init")
			return
		}
		job = jobs[0]
		for _, j := range jobs[1:] {
			if jstr(j, "created_at") > jstr(job, "created_at") {
				job = j
			}
		}
		if s, _ := job["id"].(string); s != "" && job["attempts"] == nil {
			job = fetchJob(c, s) // the list form has no attempts
		}
	} else {
		job = fetchJob(c, id)
	}
	printJob(job)
}

// printJob renders the contract's words as the control plane sent them:
// job states queued, running, verifying, escalating, accepted, review,
// cancelled, failed; attempt states intent, dispatched, acknowledged,
// usage-received, reconciled, ambiguous; verdicts accepted,
// ladder-exhausted, policy-violation, limit-reached, cancelled. Anything
// absent reads "unavailable", never zero.
func printJob(job map[string]any) {
	fmt.Printf("job %s  state %s\n", jstr(job, "id"), jstr(job, "state"))
	fmt.Printf("goal: %s\n", jstr(job, "goal"))

	manifest, _ := job["manifest"].(map[string]any)
	ladder, _ := manifest["ladder"].([]any)
	if pos, ok := jnum(job, "ladder_pos"); ok && len(ladder) > 0 {
		model := "unavailable"
		if int(pos) >= 0 && int(pos) < len(ladder) {
			if r, _ := ladder[pos].(map[string]any); r != nil {
				model = jstr(r, "model")
			}
		}
		fmt.Printf("rung %d of %d (%s)\n", pos+1, len(ladder), model)
	} else {
		fmt.Println("rung: unavailable")
	}

	attempts, haveAttempts := job["attempts"].([]any)
	savePoints := 0
	if haveAttempts && len(attempts) == 0 {
		fmt.Println("attempts: none yet")
	}
	for _, a := range attempts {
		at, _ := a.(map[string]any)
		if at == nil {
			continue
		}
		rungText := "unavailable"
		if r, ok := jnum(at, "rung"); ok {
			rungText = strconv.FormatInt(r+1, 10)
		}
		cost := "unavailable"
		if n, ok := jnum(at, "cost_microusd"); ok {
			cost = dollars(n)
		}
		sp, _ := at["save_points"].([]any)
		savePoints += len(sp)
		fmt.Printf("  attempt %s  rung %s  %s  state %s  verdict %s  cost %s  save points %d\n",
			jstr(at, "id"), rungText, jstr(at, "model"), jstr(at, "state"), jstr(at, "verdict"), cost, len(sp))
	}
	if haveAttempts {
		fmt.Printf("save points: %d\n", savePoints)
	} else {
		fmt.Println("save points: unavailable")
	}

	spent, ceiling := "unavailable", "unavailable"
	if n, ok := jnum(job, "spent_microusd"); ok {
		spent = dollars(n)
	}
	if n, ok := jnum(job, "spend_ceiling_microusd"); ok {
		ceiling = dollars(n)
	}
	fmt.Printf("spent %s of %s ceiling\n", spent, ceiling)
	fmt.Printf("verdict: %s\n", jstr(job, "verdict"))
	if sha, _ := job["artifact_sha"].(string); sha != "" {
		fmt.Printf("artifact: ks cruise artifact %s  (sha256 %s)\n", jstr(job, "id"), short(sha))
	} else {
		fmt.Println("artifact: unavailable")
	}
	fmt.Printf("record: %s\n", jstr(job, "record_url"))
	fmt.Printf("updated: %s\n", jstr(job, "updated_at"))
}

func cruiseLogs(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	var events []map[string]any
	if err := hostedCall(c, "GET", "/api/jobs/"+id+"/events", nil, &events); err != nil {
		die(err)
	}
	if len(events) == 0 {
		fmt.Println("no events yet")
		return
	}
	for _, e := range events {
		seq := "unavailable"
		if n, ok := jnum(e, "seq"); ok {
			seq = strconv.FormatInt(n, 10)
		}
		fmt.Printf("%s %s %s%s\n", seq, jstr(e, "ts"), jstr(e, "type"), eventDetail(e))
	}
}

// eventDetail renders an event's attempt id, when there is one, and its
// detail as compact JSON, after the three fixed columns.
func eventDetail(e map[string]any) string {
	s := ""
	if a, _ := e["attempt_id"].(string); a != "" {
		s += " " + a
	}
	if d, ok := e["detail"]; ok && d != nil {
		if b, err := json.Marshal(d); err == nil && string(b) != "{}" {
			s += " " + string(b)
		}
	}
	return s
}

func cruiseCancel(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	var job map[string]any
	if err := hostedCall(c, "POST", "/api/jobs/"+id+"/cancel", map[string]any{}, &job); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "job %s %s\n", id, jstr(job, "state"))
	fmt.Println(id)
}

func cruiseResume(inv *Invocation) {
	c := mustCreds()
	id := inv.Arg(0)
	req := map[string]any{}
	if specs := csvList(inv.Str("ladder")); len(specs) > 0 {
		ladder, err := parseLadder(specs)
		if err != nil {
			die(err)
		}
		req["ladder"] = ladder
	}
	var job map[string]any
	if err := hostedCall(c, "POST", "/api/jobs/"+id+"/resume", req, &job); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "job %s %s\n", id, jstr(job, "state"))
	fmt.Println(id)
}

func cruiseArtifact(inv *Invocation) error {
	c := mustCreds()
	id := inv.Arg(0)
	job := fetchJob(c, id)
	want, _ := job["artifact_sha"].(string)
	if want == "" {
		return fmt.Errorf("no artifact for job %s: state %s, verdict %s", id, jstr(job, "state"), jstr(job, "verdict"))
	}
	out := id + ".tar.gz"
	if inv.Set("out") {
		out = inv.Str("out")
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s exists; choose another name with --out", out)
	}
	resp, err := hostedDo(c, "GET", "/api/jobs/"+id+"/artifact", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return hostedError("GET", "/api/jobs/"+id+"/artifact", resp, raw)
	}
	// downloaded next to the target, verified, then renamed into place:
	// the named file exists only once its sha256 matches the job's record
	dir := filepath.Dir(out)
	tmp, err := os.CreateTemp(dir, ".ks-artifact-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	tmp.Close()
	if err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("artifact refused: sha256 %s does not match the job's record %s (nothing written)", short(got), short(want))
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "artifact %s: %s bytes, sha256 %s verified\n", id, commas(n), short(want))
	fmt.Println(out)
	return nil
}
