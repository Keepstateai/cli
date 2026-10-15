// cruise_drafts.go: Cruise jobs drafted by the service from your files.
//
//	ks cruise draft create    a hosted draft from a folder (packed here) or an archive
//	ks cruise draft list      the account's open drafts
//	ks cruise draft show      one draft; --manifest prints its canonical bytes,
//	                          --verify downloads its archive and recomputes every digest
//	ks cruise draft set       change the goal, paths, check, ladder, spend limit or keys
//	ks cruise draft approve   run it, by confirming the digest of the manifest shown
//	ks cruise draft discard   delete it and its copy of the files
//
// A hosted draft is the service doing what ks cruise init and ks cruise
// approve do, on files it holds, so a job made in the console and a job made
// here are the same object. This client does not take the service's word for
// what it drafted: the digest it shows and approves is computed here from the
// manifest the service returned (the same canonical rule ks cruise approve
// uses), and --verify recomputes the archive, the tree, the selection, the
// check's inputs, the tests and the configuration digests from the archive
// itself.
//
// A folder is packed by this client's own selection (the one init and
// preview use), so only what a job may carry is sent; what the policy leaves
// out is named, never sent, and the service must leave it out again.
// ks cruise init, approve and run stay local, as they were.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	draftUploadMax = 100 << 20 // what an upload may send, the service's own bound
	draftsRoute    = "/api/v2/cruise-drafts"
)

// draftView is the draft as the service returns it.
type draftView struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	State    string `json:"state"`
	Source   struct {
		Kind           string `json:"kind"`
		Format         string `json:"format"`
		Bytes          int64  `json:"bytes"`
		Layout         string `json:"layout"`
		LeftOut        int64  `json:"left_out"`
		StrippedPrefix string `json:"stripped_prefix"`
		SessionID      string `json:"session_id"`
		ProposalID     string `json:"proposal_id"`
		SessionName    string `json:"session_name"`
		Root           string `json:"root"`
		CopiedAt       string `json:"copied_at"`
		AgentActivity  string `json:"agent_activity_at_copy"`
	} `json:"source"`
	Upload *struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		MaxBytes int64  `json:"max_bytes"`
	} `json:"upload"`
	Refusal *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"refusal"`
	Options struct {
		Goal          string            `json:"goal"`
		Paths         []string          `json:"paths"`
		Check         string            `json:"check"`
		Allow         []string          `json:"allow"`
		Ladder        []rung            `json:"ladder"`
		SpendMicrousd int64             `json:"spend_microusd"`
		Routes        map[string]string `json:"routes"`
	} `json:"options"`
	Files struct {
		PolicyVersion   string           `json:"policy_version"`
		Included        int64            `json:"included"`
		Bytes           int64            `json:"bytes"`
		Excluded        int64            `json:"excluded"`
		ExcludedByKind  map[string]int64 `json:"excluded_by_kind"`
		Symlinks        []string         `json:"symlinks"`
		SelectionDigest string           `json:"selection_digest"`
		Largest         []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"largest"`
	} `json:"files"`
	Selection *selection `json:"selection"`
	Check     struct {
		Status       string   `json:"status"`
		Runner       string   `json:"runner"`
		Command      string   `json:"command"`
		Source       string   `json:"source"`
		WithTests    []string `json:"with_tests"`
		Pinned       int64    `json:"pinned"`
		TestsDigest  string   `json:"tests_digest"`
		InputsDigest string   `json:"inputs_digest"`
	} `json:"check"`
	Manifest    json.RawMessage `json:"manifest"`
	ManifestSHA string          `json:"manifest_sha"`
	Archive     *struct {
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
		Path   string `json:"path"`
	} `json:"archive"`
	Readiness []struct {
		Check  string `json:"check"`
		Status string `json:"status"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"readiness"`
	SpendLimit *struct {
		Enforced bool   `json:"enforced"`
		Note     string `json:"note"`
	} `json:"spend_limit"`
	Warnings  []any  `json:"warnings"`
	Storage   string `json:"storage"`
	JobID     string `json:"job_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	ExpiresAt string `json:"expires_at"`
}

// hasManifest: the service drafted a manifest (its check is decided).
func (v *draftView) hasManifest() bool {
	s := strings.TrimSpace(string(v.Manifest))
	return s != "" && s != "null"
}

// manifestMap decodes the manifest with numbers kept as their literals.
func (v *draftView) manifestMap() (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(v.Manifest))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("the draft's manifest is not a JSON object")
	}
	return m, nil
}

// computedDigest is the manifest digest computed here, from the manifest
// shown, by the canonical rule ks cruise approve uses.
func (v *draftView) computedDigest() (string, error) {
	m, err := v.manifestMap()
	if err != nil {
		return "", err
	}
	return manifestSHA(m)
}

// checkedDigest computes the digest and refuses a draft whose stated digest
// is not the digest of the manifest it carries.
func (v *draftView) checkedDigest(nothing string) string {
	computed, err := v.computedDigest()
	if err != nil {
		fail(integrity("manifest_unreadable", err.Error()+"; "+nothing))
	}
	if computed != strings.ToLower(v.ManifestSHA) {
		fail(integrity("manifest_digest_mismatch", fmt.Sprintf("the service states digest %s for draft %s, but the manifest it returned hashes to %s here; %s", short(v.ManifestSHA), v.ID, short(computed), nothing)))
	}
	return computed
}

// draftEnvelope reads a v2 answer: the data as a view, and as the raw
// document --json emits.
type draftEnvelope struct {
	Data json.RawMessage `json:"data"`
}

func decodeDraft(raw json.RawMessage) (*draftView, error) {
	var v draftView
	if err := json.Unmarshal(raw, &v); err != nil || v.ID == "" {
		return nil, fmt.Errorf("the control plane answered with a draft this client could not read")
	}
	return &v, nil
}

func fetchDraft(cr hostedCreds, id string) (*draftView, json.RawMessage) {
	var env draftEnvelope
	if err := hostedCall(cr, "GET", draftsRoute+"/"+url.PathEscape(id), nil, &env); err != nil {
		die(err)
	}
	v, err := decodeDraft(env.Data)
	if err != nil {
		die(err)
	}
	return v, env.Data
}

// ---- printing ------------------------------------------------------------------

func draftLine(v *draftView) string {
	digest := "no manifest yet"
	if v.ManifestSHA != "" {
		digest = "digest " + short(v.ManifestSHA)
	}
	return sanitize(fmt.Sprintf("%-16s %-15s %s  %d files, %s  %s  kept until %s", v.ID, v.State, clip(notRecorded(v.Options.Goal), 40),
		v.Files.Included, mib(v.Files.Bytes), digest, figure(v.ExpiresAt)))
}

// printDraft writes the draft for review: where it stands, what was read,
// the check, the terms, and the digest computed here.
func printDraft(w io.Writer, v *draftView, computed string) {
	fmt.Fprintf(w, "draft %s  %s  (revision %d)\n", v.ID, strings.ToUpper(v.State), v.Revision)
	src := v.Source.Kind
	if v.Source.Format != "" {
		src += ", " + v.Source.Format
	}
	if v.Source.Layout != "" {
		src += ", " + v.Source.Layout
	}
	if v.Source.Kind == "proposal" {
		fmt.Fprintf(w, "  source       proposal %s, on the files of session %s", sanitize(v.Source.ProposalID), sanitize(v.Source.SessionID))
		if v.Source.CopiedAt != "" {
			fmt.Fprintf(w, ", copied at %s", sanitize(v.Source.CopiedAt))
		}
	} else if v.Source.Kind == "session" {
		fmt.Fprintf(w, "  source       session %s (%s), folder %s", sanitize(v.Source.SessionID), sanitize(notRecorded(v.Source.SessionName)), sanitize(notRecorded(v.Source.Root)))
		if v.Source.CopiedAt != "" {
			fmt.Fprintf(w, ", copied at %s while its agent was %s", sanitize(v.Source.CopiedAt), sanitize(notRecorded(v.Source.AgentActivity)))
		}
	} else {
		fmt.Fprintf(w, "  source       %s, %s bytes sent", src, commas(v.Source.Bytes))
	}
	if v.Source.LeftOut > 0 {
		fmt.Fprintf(w, ", %d path(s) left out before sending", v.Source.LeftOut)
	}
	if v.Source.StrippedPrefix != "" {
		fmt.Fprintf(w, ", top folder %s removed", sanitize(v.Source.StrippedPrefix))
	}
	fmt.Fprintln(w)
	if v.Refusal != nil {
		fmt.Fprintf(w, "  refused      %s: %s\n", sanitize(v.Refusal.Code), visible(sanitize(v.Refusal.Message)))
	}
	if v.Files.PolicyVersion != "" {
		fmt.Fprintf(w, "  files        %d included, %s; %d left out (policy %s)\n", v.Files.Included, mib(v.Files.Bytes), v.Files.Excluded, v.Files.PolicyVersion)
		var kinds []string
		for _, k := range []string{"mandatory", "sensitive", "ignored", "dependency"} {
			if n := v.Files.ExcludedByKind[k]; n > 0 {
				kinds = append(kinds, fmt.Sprintf("%d %s", n, k))
			}
		}
		if len(kinds) > 0 {
			fmt.Fprintf(w, "  left out     %s\n", strings.Join(kinds, ", "))
		}
		fmt.Fprintf(w, "  selection    %s\n", figure(v.Files.SelectionDigest))
	}
	if v.Check.Status != "" {
		switch v.Check.Status {
		case "found":
			fmt.Fprintf(w, "  check        %s (%s, %s); %d verifier inputs pinned\n", sanitize(v.Check.Command), sanitize(v.Check.Runner), sanitize(v.Check.Source), v.Check.Pinned)
		case "ambiguous":
			fmt.Fprintf(w, "  check        not chosen: tests of %s were found; name it: ks cruise draft set %s --check RUNNER\n", strings.Join(v.Check.WithTests, ", "), v.ID)
		default:
			fmt.Fprintf(w, "  check        %s\n", sanitize(v.Check.Status))
		}
	}
	if v.Options.Goal != "" {
		fmt.Fprintf(w, "  goal         %s\n", visible(sanitize(v.Options.Goal)))
	}
	if len(v.Options.Ladder) > 0 {
		fmt.Fprintf(w, "  ladder       %s\n", sanitize(ladderWords(v.Options.Ladder)))
	}
	if v.SpendLimit != nil {
		state := "not enforced"
		if v.SpendLimit.Enforced {
			state = "enforced"
		}
		fmt.Fprintf(w, "  spend limit  %s, %s: %s\n", dollars(v.Options.SpendMicrousd), state, visible(sanitize(v.SpendLimit.Note)))
	}
	for _, r := range v.Readiness {
		if r.Status != "ok" {
			fmt.Fprintf(w, "  %-12s %s: %s %s\n", sanitize(r.Status), sanitize(r.Check), sanitize(r.Code), visible(sanitize(r.Detail)))
		}
	}
	for _, x := range v.Warnings {
		fmt.Fprintf(w, "  warning      %s\n", visible(sanitize(fmt.Sprint(x))))
	}
	if v.Archive != nil {
		fmt.Fprintf(w, "  archive      %s bytes, sha256 %s\n", commas(v.Archive.Bytes), v.Archive.SHA256)
	}
	if v.JobID != "" {
		fmt.Fprintf(w, "  job          %s (ks cruise status %s)\n", v.JobID, v.JobID)
	}
	if v.Storage != "" {
		fmt.Fprintf(w, "  storage      %s\n", visible(sanitize(v.Storage)))
	}
	if v.ExpiresAt != "" && draftOpenState(v.State) {
		fmt.Fprintf(w, "  kept until   %s\n", v.ExpiresAt)
	}
	if computed != "" {
		fmt.Fprintf(w, "  digest       %s (the service)\n", v.ManifestSHA)
		fmt.Fprintf(w, "               %s (computed here from the manifest)\n", computed)
	}
}

func draftOpenState(s string) bool {
	switch s {
	case "awaiting_upload", "collecting", "ready", "approving":
		return true
	}
	return false
}

// ---- create ---------------------------------------------------------------------

type draftLeftOut struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
}

// draftTerms reads the options create and set share into a request body.
func draftTerms(inv *Invocation, body map[string]any) {
	if inv.Set("goal") {
		body["goal"] = inv.Str("goal")
	}
	if inv.Set("paths") {
		body["paths"] = inv.List("paths")
	}
	if inv.Set("check") {
		r := strings.TrimSpace(inv.Str("check"))
		if _, ok := ks072RunnerEco[r]; !ok {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--check is pytest, go or node (got %q)", r)})
		}
		body["check"] = r
	}
	if inv.Set("ladder") {
		l, err := parseLadder(csvList(inv.Str("ladder")))
		if err != nil {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: err.Error()})
		}
		if len(l) == 0 {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--ladder names no rung"})
		}
		body["ladder"] = l
	}
	if inv.Set("spend") {
		body["spend_microusd"] = inv.Int("spend")
	}
	if inv.Set("key") {
		named, err := parseKeyFlags(inv.List("key"))
		if err != nil {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: err.Error()})
		}
		body["routes"] = named
	}
}

// packFolder packs a folder by this client's own selection into a temp
// file: the included files in the deterministic archive init digests, and
// the paths the policy leaves out, named.
func packFolder(dir string, allow []string) (string, int64, []draftLeftOut, *selection, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", 0, nil, nil, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", 0, nil, nil, &cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--dir %s is not a folder; nothing was sent", dir)}
	}
	tmp, err := os.CreateTemp("", "ks-draft-*.tar.gz")
	if err != nil {
		return "", 0, nil, nil, err
	}
	cw := &capWriter{w: tmp}
	_, sel, err := scanSelected(root, cw, allow)
	cerr := tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		kind := "workspace_refused"
		if errors.Is(err, errSymlinks) {
			kind = "workspace_symlinks"
		}
		return "", 0, nil, nil, &cliError{Code: exitUsage, Kind: kind, Message: err.Error() + "; nothing was sent"}
	}
	if cerr != nil {
		os.Remove(tmp.Name())
		return "", 0, nil, nil, cerr
	}
	left := []draftLeftOut{}
	for _, x := range sel.Excluded {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(x.Path)))
		left = append(left, draftLeftOut{Path: x.Path, Dir: err == nil && fi.IsDir()})
	}
	return tmp.Name(), cw.n, left, sel, nil
}

// archiveFormat names an archive's format from its file name.
func archiveFormat(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	}
	return ""
}

func hostedCruiseDraftCreate(cr hostedCreds, inv *Invocation) {
	dir, archive, session := strings.TrimSpace(inv.Str("dir")), strings.TrimSpace(inv.Str("archive")), strings.TrimSpace(inv.Str("session"))
	proposal := strings.TrimSpace(inv.Str("proposal"))
	named := 0
	for _, x := range []string{dir, archive, session, proposal} {
		if x != "" {
			named++
		}
	}
	if named != 1 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name exactly one source: --dir FOLDER, --archive FILE, --session SESSION or --proposal PROPOSAL; nothing was sent"})
	}
	if proposal != "" {
		for _, f := range []string{"goal", "paths", "check", "ladder", "spend", "key"} {
			if inv.Set(f) {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("a draft from a proposal takes its terms from the proposal; --%s is changed afterwards with ks cruise draft set. Nothing was sent", f)})
			}
		}
	}
	if inv.Set("root") && session == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--root names a folder of a session's workspace and goes with --session; nothing was sent"})
	}
	body := map[string]any{"storage_consent": true}
	if session != "" || proposal != "" {
		createSessionDraft(cr, inv, body, proposal)
		return
	}
	source := map[string]any{"kind": "upload"}
	var file string
	var size int64
	if dir != "" {
		f, n, left, _, err := packFolder(dir, inv.List("allow"))
		if err != nil {
			die(err)
		}
		defer os.Remove(f)
		file, size = f, n
		source["format"], source["layout"] = "tar.gz", "folder"
		if len(left) > 0 {
			source["left_out"] = left
		}
	} else {
		format := archiveFormat(archive)
		if format == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--archive %s is not a .zip, .tar.gz, .tgz or .tar file; nothing was sent", archive)})
		}
		fi, err := os.Stat(archive)
		if err != nil || !fi.Mode().IsRegular() {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--archive %s is not a readable file; nothing was sent", archive)})
		}
		if fi.Size() > draftUploadMax {
			fail(&cliError{Code: exitUsage, Kind: "upload_too_large", Message: fmt.Sprintf("%s is %s, over the 100 MiB an upload may send; nothing was sent", archive, mib(fi.Size()))})
		}
		file, size = archive, fi.Size()
		source["format"], source["layout"] = format, "archive"
	}
	if size > draftUploadMax {
		fail(&cliError{Code: exitUsage, Kind: "upload_too_large", Message: fmt.Sprintf("the packed folder is %s, over the 100 MiB an upload may send; nothing was sent", mib(size))})
	}
	source["bytes"] = size
	body["source"] = source
	if a := inv.List("allow"); len(a) > 0 {
		body["allow"] = a
	}
	draftTerms(inv, body)
	progress("KeepState keeps these files for this draft only, does not bill for them, and deletes them when the draft is approved, discarded or expires")

	var env draftEnvelope
	if err := hostedMutate(cr, "POST", draftsRoute, body, &env); err != nil {
		die(err)
	}
	v, err := decodeDraft(env.Data)
	if err != nil {
		die(err)
	}
	if v.Upload == nil || v.Upload.Path == "" {
		fail(&cliError{Code: exitFailed, Kind: "upload_route_missing", WorkStarted: workYes,
			Message:    fmt.Sprintf("draft %s was created but the service named no upload route; nothing was sent", v.ID),
			NextAction: "ks cruise draft discard " + v.ID})
	}
	progress("draft %s created; sending %s bytes", v.ID, commas(size))
	raw, err := putDraftUpload(cr, v, file, size)
	if err != nil {
		die(err)
	}
	if v, err = decodeDraft(raw); err != nil {
		die(err)
	}
	finishDraftShow(v, raw, true)
}

// draftCollectWait is how long create waits for the service to copy and
// read a session's files before it hands back the draft id to follow.
var draftCollectWait = 5 * time.Minute

// createSessionDraft drafts from one of your running sessions: the service
// copies the files of the session's workspace (or of the folder --root names
// in it) and reads them as it reads an upload. A session that is not running
// is refused and not woken. Create waits while the draft is collecting.
//
// With a proposal named, the draft is from an agent's proposal: the service
// copies the proposal's session and drafts it on the proposal's terms, every
// digest recomputed from the files, so the digest to approve is the draft's.
func createSessionDraft(cr hostedCreds, inv *Invocation, body map[string]any, proposal string) {
	var source map[string]any
	if proposal != "" {
		source = map[string]any{"kind": "proposal", "proposal_id": proposal}
	} else {
		sess := agentSession(cr, inv)
		source = map[string]any{"kind": "session", "session_id": agentSessionID(sess)}
		if root := strings.TrimSpace(inv.Str("root")); root != "" {
			source["root"] = root
		}
	}
	body["source"] = source
	if a := inv.List("allow"); len(a) > 0 {
		body["allow"] = a
	}
	if proposal == "" {
		draftTerms(inv, body)
	}
	progress("KeepState copies the session's files into this draft only, does not bill for the copy, and deletes it when the draft is approved, discarded or expires")
	var env draftEnvelope
	if err := hostedMutate(cr, "POST", draftsRoute, body, &env); err != nil {
		die(err)
	}
	v, err := decodeDraft(env.Data)
	if err != nil {
		die(err)
	}
	raw := env.Data
	deadline := time.Now().Add(draftCollectWait)
	if v.State == "collecting" {
		progress("draft %s created; the service is copying and reading the session's files", v.ID)
	}
	for v.State == "collecting" {
		if time.Now().After(deadline) {
			fail(&cliError{Code: exitTemporary, Kind: "draft_collecting", WorkStarted: workYes,
				Message:    fmt.Sprintf("draft %s is still reading the session's files after %s", v.ID, draftCollectWait),
				NextAction: "ks cruise draft show " + v.ID})
		}
		time.Sleep(time.Second)
		v, raw = fetchDraft(cr, v.ID)
	}
	finishDraftShow(v, raw, true)
}

// putDraftUpload sends exactly the declared bytes, raw, on the draft's
// upload route (never through the idempotency layer, which would hold the
// whole body in memory).
func putDraftUpload(cr hostedCreds, v *draftView, file string, size int64) (json.RawMessage, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	resp, err := hostedDo(cr, "PUT", v.Upload.Path, "application/octet-stream", io.LimitReader(f, size))
	if err != nil {
		return nil, &cliError{Code: exitTemporary, Kind: "upload_uncertain", WorkStarted: workUnknown,
			Message:    fmt.Sprintf("the upload for draft %s got no answer (%s); whether it was read is not known", v.ID, sanitize(err.Error())),
			NextAction: "ks cruise draft show " + v.ID}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("PUT", v.Upload.Path, resp, raw)
	}
	var env draftEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
		return nil, &cliError{Code: exitTemporary, Kind: "upload_uncertain", WorkStarted: workUnknown,
			Message:    fmt.Sprintf("the service answered %s to the upload for draft %s with a result this client could not read", resp.Status, v.ID),
			NextAction: "ks cruise draft show " + v.ID}
	}
	return env.Data, nil
}

// finishDraftShow prints (or emits) a draft and, when it carries a
// manifest, the digest computed here, refusing a stated digest that is not
// its manifest's. A refused draft exits non-zero with the service's code.
func finishDraftShow(v *draftView, raw json.RawMessage, next bool) {
	computed := ""
	if v.hasManifest() {
		computed = v.checkedDigest("nothing was approved")
	}
	refused := v.State == "refused" && v.Refusal != nil
	// a refused draft is the refusal alone under --json: one document
	if !refused || !out.json {
		emit(raw, func() {
			printDraft(os.Stdout, v, computed)
			if next && computed != "" && v.State == "ready" {
				fmt.Printf("next: ks cruise draft approve %s --confirm %s\n", v.ID, short(computed))
			}
		})
	}
	if refused {
		fail(&cliError{Code: exitFailed, Kind: v.Refusal.Code, WorkStarted: workNo,
			Message:    fmt.Sprintf("draft %s was refused: %s; nothing runs, and its copy of the files is deleted", v.ID, sanitize(v.Refusal.Message)),
			NextAction: "change what was refused and run ks cruise draft create again"})
	}
}

// ---- list, show, set, discard ------------------------------------------------------

func hostedCruiseDraftList(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", draftsRoute, nil, &env); err != nil {
		die(err)
	}
	var vs []*draftView
	for _, it := range env.Data.Items {
		v, err := decodeDraft(it)
		if err != nil {
			die(err)
		}
		vs = append(vs, v)
	}
	emit(map[string]any{"drafts": env.Data.Items, "count": len(env.Data.Items)}, func() {
		if len(vs) == 0 {
			fmt.Println("no open drafts; start one with ks cruise draft create --dir FOLDER")
			return
		}
		for _, v := range vs {
			fmt.Println(draftLine(v))
		}
	})
}

func hostedCruiseDraftShow(cr hostedCreds, inv *Invocation) {
	v, raw := fetchDraft(cr, inv.Arg(0))
	switch {
	case inv.Bool("verify"):
		verifyDraft(cr, v, raw)
	case inv.Bool("manifest"):
		if !v.hasManifest() {
			fail(&cliError{Code: exitConflict, Kind: "draft_no_manifest", Message: fmt.Sprintf("draft %s carries no manifest yet (state %s, check %s)", v.ID, v.State, figure(v.Check.Status))})
		}
		m, err := v.manifestMap()
		if err != nil {
			fail(integrity("manifest_unreadable", err.Error()))
		}
		canon, err := canonicalJSON(m)
		if err != nil {
			fail(integrity("manifest_unreadable", err.Error()))
		}
		sum := sha256.Sum256(canon)
		if hex.EncodeToString(sum[:]) != strings.ToLower(v.ManifestSHA) {
			fail(integrity("manifest_digest_mismatch", fmt.Sprintf("the canonical bytes of draft %s's manifest hash to %s, and the service states %s", v.ID, short(hex.EncodeToString(sum[:])), short(v.ManifestSHA))))
		}
		emit(map[string]any{"draft_id": v.ID, "manifest_sha": v.ManifestSHA, "canonical": string(canon)}, func() {
			os.Stdout.Write(canon)
		})
	default:
		finishDraftShow(v, raw, true)
	}
}

func hostedCruiseDraftSet(cr hostedCreds, inv *Invocation) {
	body := map[string]any{}
	if inv.Bool("any-file") {
		if inv.Set("paths") {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--paths and --any-file say opposite things; give one. Nothing was changed"})
		}
		body["paths"] = []string{}
	}
	draftTerms(inv, body)
	if len(body) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name at least one change: --goal, --paths, --any-file, --check, --ladder, --spend or --key. Nothing was changed"})
	}
	v, _ := fetchDraft(cr, inv.Arg(0))
	body["expected_revision"] = v.Revision
	var env draftEnvelope
	if err := hostedMutate(cr, "PATCH", draftsRoute+"/"+url.PathEscape(v.ID), body, &env); err != nil {
		die(err)
	}
	after, err := decodeDraft(env.Data)
	if err != nil {
		die(err)
	}
	finishDraftShow(after, env.Data, true)
}

func hostedCruiseDraftDiscard(cr hostedCreds, inv *Invocation) {
	var env draftEnvelope
	if err := hostedMutate(cr, "DELETE", draftsRoute+"/"+url.PathEscape(inv.Arg(0)), nil, &env); err != nil {
		die(err)
	}
	v, err := decodeDraft(env.Data)
	if err != nil {
		die(err)
	}
	emit(env.Data, func() {
		fmt.Printf("discarded %s (state %s); its copy of the files is deleted and nothing runs\n", v.ID, v.State)
	})
}

// ---- approve ---------------------------------------------------------------------

func hostedCruiseDraftApprove(cr hostedCreds, inv *Invocation) {
	v, _ := fetchDraft(cr, inv.Arg(0))
	if v.State != "ready" && v.State != "approved" {
		fail(&cliError{Code: exitConflict, Kind: "draft_state", Message: fmt.Sprintf("draft %s is %s; only a ready draft can be approved, and nothing was created", v.ID, v.State)})
	}
	if !v.hasManifest() {
		next := "ks cruise draft show " + v.ID
		if v.Check.Status == "ambiguous" {
			next = "ks cruise draft set " + v.ID + " --check RUNNER"
		}
		fail(&cliError{Code: exitConflict, Kind: "draft_no_manifest", Message: fmt.Sprintf("draft %s carries no manifest to approve (check %s); nothing was created", v.ID, figure(v.Check.Status)), NextAction: next})
	}
	computed := v.checkedDigest("nothing was approved")
	// the review, before anything is sent (stderr, so --json stays one document)
	printDraft(os.Stderr, v, computed)
	given := strings.ToLower(strings.TrimSpace(inv.Str("confirm")))
	matches := func(s string) bool {
		s = strings.ToLower(strings.TrimSpace(s))
		return s == computed || (len(s) == 12 && s == short(computed))
	}
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch",
				Message: fmt.Sprintf("--confirm %q is not the digest of this draft's manifest (%s); nothing was approved", given, short(computed))})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
			Message:    "an approval is confirmed by the manifest's digest and none was given; --yes does not approve a Cruise job, and nothing was approved",
			NextAction: fmt.Sprintf("ks cruise draft approve %s --confirm %s", v.ID, short(computed))})
	default:
		fmt.Fprintf(os.Stderr, "type the digest (%s) to run this job, or anything else to stop: ", short(computed))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !matches(line) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the digest was not confirmed; nothing was approved"})
		}
	}
	var env struct {
		Data struct {
			Draft json.RawMessage `json:"draft"`
			Job   map[string]any  `json:"job"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", draftsRoute+"/"+url.PathEscape(v.ID)+"/approve", map[string]any{"manifest_sha": computed}, &env); err != nil {
		die(err)
	}
	job := env.Data.Job
	// a job of another digest than the one approved here is refused before
	// anything is printed as a success, so --json stays one document: the error
	if job != nil {
		if got, _ := job["manifest_sha"].(string); got != "" && got != computed {
			fail(integrity("job_digest_mismatch", fmt.Sprintf("job %s was created with manifest digest %s, not the %s approved here; it is not the job you approved, cancel it: ks cruise cancel %s",
				jstr(job, "id"), short(got), short(computed), jstr(job, "id"))))
		}
	}
	emit(env.Data, func() {
		if job == nil {
			fmt.Printf("approved %s; the service named no job in its answer: ks cruise draft show %s\n", v.ID, v.ID)
			return
		}
		fmt.Printf("approved %s: job %s (%s) runs the manifest with digest %s\n", v.ID, jstr(job, "id"), jstr(job, "state"), short(computed))
		fmt.Printf("follow it: ks cruise status %s; cancel it with ks cruise cancel %s\n", jstr(job, "id"), jstr(job, "id"))
	})
}

// ---- verify ------------------------------------------------------------------------

// archiveEntryFiles reads a draft's archive: its regular files in the order
// they arrived, with their contents.
func archiveEntryFiles(data []byte) ([]wsFile, map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("the archive is not a gzip stream")
	}
	tr := tar.NewReader(gz)
	var files []wsFile
	contents := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("the archive's tar stream is malformed")
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, nil, fmt.Errorf("the archive holds %q, which is not a regular file", hdr.Name)
		}
		if _, dup := contents[hdr.Name]; dup {
			return nil, nil, fmt.Errorf("the archive holds %q twice", hdr.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, cruiseMaxBytes+1))
		if err != nil || int64(len(b)) != hdr.Size {
			return nil, nil, fmt.Errorf("the archive's entry %q is not the size it declares", hdr.Name)
		}
		f := wsFile{rel: hdr.Name, exec: hdr.Mode&0o111 != 0, size: hdr.Size, sha: sha256.Sum256(b)}
		files = append(files, f)
		contents[hdr.Name] = b
	}
	return files, contents, nil
}

// judgeSorted is the tree digest's order: every directory by its full path,
// then the files of each directory by name.
func judgeSorted(files []wsFile) []wsFile {
	out := append([]wsFile{}, files...)
	dir := func(p string) string {
		d := path.Dir(p)
		if d == "." {
			return ""
		}
		return d
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := dir(out[i].rel), dir(out[j].rel)
		if di != dj {
			return di < dj
		}
		return path.Base(out[i].rel) < path.Base(out[j].rel)
	})
	return out
}

func verifyDraft(cr hostedCreds, v *draftView, raw json.RawMessage) {
	if !v.hasManifest() {
		fail(&cliError{Code: exitConflict, Kind: "draft_no_manifest", Message: fmt.Sprintf("draft %s carries no manifest to verify (state %s)", v.ID, v.State)})
	}
	computed := v.checkedDigest("nothing was verified")
	m, _ := v.manifestMap()
	ws, _ := m["workspace"].(map[string]any)
	ver, _ := m["verifier"].(map[string]any)
	if ws == nil || ver == nil {
		fail(integrity("manifest_incomplete", "the draft's manifest names no workspace or verifier, so nothing can be verified"))
	}
	if v.Selection == nil {
		fail(integrity("selection_missing", "the service returned no selection with this draft, so its selection digest cannot be recomputed"))
	}
	route := draftsRoute + "/" + url.PathEscape(v.ID) + "/archive"
	if v.Archive != nil && v.Archive.Path != "" {
		route = v.Archive.Path
	}
	resp, err := hostedDo(cr, "GET", route, "", nil)
	if err != nil {
		die(err)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, draftUploadMax))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		die(hostedError("GET", route, resp, data))
	}
	type check struct {
		Name string `json:"check"`
		OK   bool   `json:"ok"`
		Got  string `json:"got"`
		Want string `json:"want"`
	}
	var checks []check
	add := func(name, got, want string) {
		checks = append(checks, check{name, got == want && want != "", got, want})
	}
	sum := sha256.Sum256(data)
	archSHA := hex.EncodeToString(sum[:])
	wantSHA, _ := ws["sha256"].(string)
	add("archive sha256 is the manifest's workspace.sha256", archSHA, strings.ToLower(wantSHA))
	add("archive sha256 is the X-KS-Sha256 the service sent", archSHA, strings.ToLower(resp.Header.Get("X-KS-Sha256")))
	wantBytes, _ := jnum(ws, "bytes")
	add("archive size is the manifest's workspace.bytes", fmt.Sprint(len(data)), fmt.Sprint(wantBytes))
	files, contents, aerr := archiveEntryFiles(data)
	if aerr != nil {
		add("archive reads as regular files", aerr.Error(), "a readable archive")
	} else {
		add("tree digest is the manifest's initial_state_digest", treeDigest(judgeSorted(files)), fmt.Sprint(m["initial_state_digest"]))
		bySHA := map[string]string{}
		for _, f := range files {
			bySHA[f.rel] = hex.EncodeToString(f.sha[:])
		}
		bad := 0
		for _, f := range v.Selection.Included {
			if bySHA[f.Path] != f.SHA256 {
				bad++
			}
		}
		add("every selected file is in the archive with the sha256 the selection records",
			fmt.Sprintf("%d of %d differ, %d archive entries", bad, len(v.Selection.Included), len(files)),
			fmt.Sprintf("0 of %d differ, %d archive entries", len(v.Selection.Included), len(v.Selection.Included)))
		add("selection digest is the manifest's workspace.selection_digest", selectionDigest(v.Selection), fmt.Sprint(ws["selection_digest"]))
		inputs, _, ierr := bindingInputs(judgeSorted(files), checkRunnerOf(ver))
		if ierr != nil {
			inputs = ierr.Error()
		}
		add("check inputs digest is the manifest's verifier.inputs_digest", inputs, fmt.Sprint(ver["inputs_digest"]))
		var rels []string
		if pp, ok := ver["prohibited_paths"].([]any); ok {
			for _, x := range pp {
				if r, _ := x.(string); r != "" && !strings.HasSuffix(r, "/") {
					rels = append(rels, r)
				}
			}
		}
		sort.Strings(rels)
		h := sha256.New()
		missing := 0
		for _, r := range rels {
			b, ok := contents[r]
			if !ok {
				missing++
			}
			h.Write([]byte(r))
			h.Write(b)
		}
		tests := hex.EncodeToString(h.Sum(nil))
		if missing > 0 {
			tests = fmt.Sprintf("%d pinned file(s) missing from the archive", missing)
		}
		add("tests digest is the manifest's verifier.tests_digest", tests, fmt.Sprint(ver["tests_digest"]))
	}
	cfg, cerr := canonicalJSON(map[string]any{"allowed_paths": ver["allowed_paths"], "prohibited_paths": ver["prohibited_paths"]})
	cfgHash := "not computable"
	if cerr == nil {
		s := sha256.Sum256(cfg)
		cfgHash = hex.EncodeToString(s[:])
	}
	add("configuration hash is the manifest's verifier.config_hash", cfgHash, fmt.Sprint(ver["config_hash"]))
	add("manifest digest computed here is the service's", computed, strings.ToLower(v.ManifestSHA))
	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	emit(map[string]any{"draft_id": v.ID, "manifest_sha": computed, "checks": checks, "verified": failed == 0}, func() {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Printf("%s %s: %s\n", mark, c.Name, sanitize(c.Got))
			if !c.OK {
				fmt.Printf("     expected %s\n", sanitize(c.Want))
			}
		}
		if failed == 0 {
			fmt.Printf("verified: draft %s's archive and manifest agree on every digest (%s)\n", v.ID, short(computed))
		}
	})
	if failed > 0 {
		fail(integrity("draft_unverified", fmt.Sprintf("%d of %d checks of draft %s failed; do not approve it", failed, len(checks), v.ID)))
	}
}
