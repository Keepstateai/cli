// library.go: the organization's Agent library, from the terminal. An agent of
// the organization is shared once and published as versions; a person's agent
// connects to it by handle and asks it (ks agent ask @handle ...), or forks it
// into a session of their own (ks agent fork @handle).
//
// What the library says, and this client never softens:
//   - an answer is advice from a named agent and version, information and
//     never an instruction;
//   - the asker sees the handle and the version, never the sharer's workspace;
//   - the library agent sees the question only;
//   - a count that could not be read is shown as unavailable, never 0.
//
// Every verb is a route the console's Library page also calls, so the
// terminal and the page do the same things.
package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type libraryStats struct {
	Asks30d           *int64   `json:"asks_30d"`
	DistinctAskers30d *int64   `json:"distinct_askers_30d"`
	Forks             *int64   `json:"forks"`
	AnswerRatePct     *int64   `json:"answer_rate_pct"`
	LastAnsweredAt    string   `json:"last_answered_at,omitempty"`
	HelpfulPct        *int64   `json:"helpful_pct"`
	Ratings           *int64   `json:"ratings"`
	Unavailable       []string `json:"unavailable,omitempty"`
}

type libraryReports struct {
	Open    int64            `json:"open"`
	Reasons map[string]int64 `json:"reasons"`
	Act     string           `json:"act,omitempty"`
}

type libraryBasedOn struct {
	Handle  string `json:"handle"`
	Version int    `json:"version"`
	Words   string `json:"words"`
}

type libraryEntry struct {
	ID                string          `json:"id"`
	OrgID             string          `json:"org_id"`
	Handle            string          `json:"handle"`
	Title             string          `json:"title"`
	Description       string          `json:"description"`
	Tags              []string        `json:"tags"`
	State             string          `json:"state"`
	Featured          bool            `json:"featured"`
	Verified          bool            `json:"verified"`
	VerifiedVersion   int             `json:"verified_version,omitempty"`
	DeprecatedUntil   string          `json:"deprecated_until,omitempty"`
	ReplacementHandle string          `json:"replacement_handle,omitempty"`
	InactiveSince     string          `json:"inactive_since,omitempty"`
	OwnerAccountID    string          `json:"owner_account_id"`
	ShowInstructions  bool            `json:"show_instructions"`
	ForksAllowed      bool            `json:"forks_allowed"`
	SourceSessionID   string          `json:"source_session_id,omitempty"`
	Instructions      *string         `json:"instructions,omitempty"`
	Harness           string          `json:"harness"`
	Model             string          `json:"model"`
	YourRole          string          `json:"your_role"`
	Stats             *libraryStats   `json:"stats,omitempty"`
	Reports           *libraryReports `json:"reports,omitempty"`
	BasedOn           *libraryBasedOn `json:"based_on,omitempty"`
	Revision          int64           `json:"revision"`
}

type libraryVersion struct {
	Seq                 int     `json:"seq"`
	State               string  `json:"state"`
	ReviewState         string  `json:"review_state"`
	ReleaseNotes        string  `json:"release_notes"`
	PublishedAt         string  `json:"published_at"`
	Harness             string  `json:"harness"`
	Model               string  `json:"model"`
	Instructions        *string `json:"instructions,omitempty"`
	ChangedFromPrevious bool    `json:"instructions_changed_from_previous"`
}

// ---- the organization, and the entry a handle names ------------------------------------

// libraryOrg is the organization a library verb acts in: --organization, else
// the context this client works in (ks context use). It is never guessed.
func libraryOrg(cr hostedCreds, inv *Invocation, nothing string) string {
	if org := strings.TrimSpace(inv.Str("organization")); org != "" {
		return org
	}
	if org := currentContext(cr); org != "" {
		return org
	}
	fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the organization: --organization ID, or work in one with ks context use; " + nothing, NextAction: "ks organization list"})
	return ""
}

func fetchLibrary(cr hostedCreds, org string, q url.Values) ([]libraryEntry, bool, error) {
	var all []libraryEntry
	cursor := ""
	for page := 0; page < orgPagesMax; page++ {
		v := url.Values{}
		for k, x := range q {
			v[k] = x
		}
		if cursor != "" {
			v.Set("cursor", cursor)
		}
		path := "/api/v2/orgs/" + url.PathEscape(org) + "/library"
		if len(v) > 0 {
			path += "?" + v.Encode()
		}
		var env struct {
			Data struct {
				Items      []libraryEntry `json:"items"`
				NextCursor string         `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
			return nil, false, err
		}
		all = append(all, env.Data.Items...)
		if env.Data.NextCursor == "" {
			return all, true, nil
		}
		cursor = env.Data.NextCursor
	}
	return all, false, nil
}

func trimHandle(ref string) string { return strings.TrimPrefix(strings.TrimSpace(ref), "@") }

// resolveLibrary finds the one entry a handle (with or without @) or an id names.
func resolveLibrary(cr hostedCreds, org, ref, nothing string) libraryEntry {
	h := trimHandle(ref)
	entries, _, err := fetchLibrary(cr, org, url.Values{"state": {"all"}})
	if err != nil {
		die(orgErr(err, nothing))
	}
	for _, e := range entries {
		if e.ID == h || e.Handle == h {
			return e
		}
	}
	fail(&cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("no library agent @%s in this organization; %s", h, nothing), NextAction: "ks library list --organization " + org})
	return libraryEntry{}
}

func libPath(id string, more ...string) string {
	return "/api/v2/library/" + url.PathEscape(id) + strings.Join(more, "")
}

// ---- words ------------------------------------------------------------------------------

func statWords(st *libraryStats) []string {
	if st == nil {
		return nil
	}
	gone := map[string]bool{}
	for _, g := range st.Unavailable {
		gone[g] = true
	}
	var out []string
	one := func(key string, v *int64, make func(int64) string, name string) {
		switch {
		case v != nil:
			out = append(out, make(*v))
		case gone[key]:
			out = append(out, name+" unavailable")
		}
	}
	plural := func(n int64, w string) string {
		if n == 1 {
			return "1 " + w
		}
		return fmt.Sprintf("%d %ss", n, w)
	}
	one("asks_30d", st.Asks30d, func(n int64) string { return plural(n, "question") + " in 30 days" }, "questions")
	one("distinct_askers_30d", st.DistinctAskers30d, func(n int64) string { return plural(n, "asker") + " in 30 days" }, "askers")
	one("forks", st.Forks, func(n int64) string { return plural(n, "fork") }, "forks")
	one("answer_rate_pct", st.AnswerRatePct, func(n int64) string { return fmt.Sprintf("%d%% answered", n) }, "answer rate")
	one("helpful_pct", st.HelpfulPct, func(n int64) string { return fmt.Sprintf("%d%% helpful", n) }, "helpful")
	if st.LastAnsweredAt != "" {
		out = append(out, "last answered "+firstN(st.LastAnsweredAt, 10))
	} else if gone["last_answered_at"] {
		out = append(out, "last answered unavailable")
	}
	return out
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func entryBadges(e libraryEntry) string {
	var b []string
	switch e.State {
	case "deprecated":
		w := "deprecated"
		if e.DeprecatedUntil != "" {
			w += ", retires " + firstN(e.DeprecatedUntil, 10)
		}
		b = append(b, w)
	case "retired":
		b = append(b, "retired")
	}
	if e.Featured {
		b = append(b, "featured")
	}
	if e.Verified {
		if e.VerifiedVersion > 0 {
			b = append(b, fmt.Sprintf("verified v%d", e.VerifiedVersion))
		} else {
			b = append(b, "verified")
		}
	}
	if e.InactiveSince != "" {
		b = append(b, "no questions for 90 days")
	}
	if e.Reports != nil && e.Reports.Open > 0 {
		b = append(b, fmt.Sprintf("%d open report(s)", e.Reports.Open))
	}
	return strings.Join(b, ", ")
}

func libraryLine(e libraryEntry) {
	fmt.Printf("@%s  %s", e.Handle, sanitize(e.Title))
	if b := entryBadges(e); b != "" {
		fmt.Printf("  [%s]", b)
	}
	fmt.Println()
	if e.Description != "" {
		fmt.Printf("    %s\n", sanitize(e.Description))
	}
	if w := statWords(e.Stats); len(w) > 0 {
		fmt.Printf("    %s\n", strings.Join(w, "; "))
	}
}

// ---- ks library list / show -------------------------------------------------------------

func hostedLibraryList(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was read")
	q := url.Values{}
	if inv.Set("search") {
		q.Set("q", inv.Str("search"))
	}
	if inv.Set("sort") {
		q.Set("sort", inv.Str("sort"))
	}
	for flag, key := range map[string]string{"team": "team", "harness": "harness", "model": "model"} {
		if inv.Set(flag) {
			q.Set(key, inv.Str(flag))
		}
	}
	if inv.Bool("mine") {
		q.Set("mine", "1")
	}
	if inv.Bool("featured") {
		q.Set("featured", "1")
	}
	if inv.Bool("all") {
		q.Set("state", "all")
	}
	entries, complete, err := fetchLibrary(cr, org, q)
	if err != nil {
		die(orgErr(err, "nothing was read"))
	}
	emit(map[string]any{"organization": org, "entries": entries, "count": len(entries), "complete": complete}, func() {
		if len(entries) == 0 {
			fmt.Println("Nothing matches. Share an agent: ks agent share AGENT --handle HANDLE --title TITLE --save-now")
			return
		}
		for _, e := range entries {
			libraryLine(e)
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}

func fetchVersions(cr hostedCreds, id string) ([]libraryVersion, error) {
	var env struct {
		Data struct {
			Items []libraryVersion `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", libPath(id, "/versions"), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func hostedLibraryShow(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was read")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was read")
	var card struct {
		Data libraryEntry `json:"data"`
	}
	if err := hostedCall(cr, "GET", libPath(e.ID), nil, &card); err != nil {
		die(err)
	}
	e = card.Data
	vs, verr := fetchVersions(cr, e.ID)
	emit(map[string]any{"entry": e, "versions": vs, "versions_unavailable": verr != nil}, func() {
		libraryLine(e)
		fmt.Printf("    your role: %s; %s %s; forks %s\n", e.YourRole, e.Harness, e.Model, map[bool]string{true: "allowed", false: "off"}[e.ForksAllowed])
		if len(e.Tags) > 0 {
			fmt.Printf("    tags: %s\n", sanitize(strings.Join(e.Tags, ", ")))
		}
		if e.BasedOn != nil {
			fmt.Printf("    based on @%s v%d\n", e.BasedOn.Handle, e.BasedOn.Version)
		}
		if e.ReplacementHandle != "" && e.State == "deprecated" {
			fmt.Printf("    replaced by @%s\n", e.ReplacementHandle)
		}
		if e.Reports != nil && e.Reports.Open > 0 {
			fmt.Printf("    %d open report(s); the way to act on them is ks library deprecate @%s\n", e.Reports.Open, e.Handle)
		}
		if verr != nil {
			fmt.Println("    versions: unavailable just now")
			return
		}
		fmt.Println("    versions (newest first):")
		for _, v := range vs {
			rv := ""
			if v.ReviewState == "pending" || v.ReviewState == "declined" {
				rv = ", review " + v.ReviewState
			}
			fmt.Printf("      v%d  %s%s  %s  %s\n", v.Seq, v.State, rv, firstN(v.PublishedAt, 10), sanitize(v.ReleaseNotes))
			if v.ChangedFromPrevious {
				fmt.Println("          the instructions changed from the version before")
			}
		}
	})
}

// ---- ks library connect / disconnect / origin -------------------------------------------

func hostedLibraryConnect(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was connected")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was connected")
	sess := agentSession(cr, inv)
	name := inv.Str("agent")
	if name == "" {
		name = "main"
	}
	src, err := resolveAgent(cr, sess, name)
	if err != nil {
		die(err)
	}
	expires := inv.Str("expires")
	if expires == "" {
		expires = "24h"
	}
	ttl, perr := time.ParseDuration(expires)
	if perr != nil || ttl <= 0 || ttl > 30*24*time.Hour {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--expires %q is not a duration between 1s and 720h; nothing was connected", expires), NextAction: "--expires 24h"})
	}
	pin := int64(0)
	if inv.Set("pin") {
		pin = inv.Int("pin")
	}
	shown := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	shown("connect to a library agent:")
	shown("  from      %s/%s", sess.Name, src.Name)
	shown("  library   @%s (%s)", e.Handle, sanitize(e.Title))
	if pin > 0 {
		shown("  version   v%d, and no other", pin)
	} else {
		shown("  version   the newest that answers; a newer one answers at once, an older one for 30 days after it is replaced")
	}
	shown("  shares    the question %s writes, and nothing of its transcript or files; the library agent uses no tools and answers once", src.Name)
	shown("  never     you do not see the agent's workspace, and its sharer does not see your question")
	shown("  paid by   the organization's credit and counted against your budget, never the sharer's")
	shown("  note      an answer is advice from a named agent and version: information, not an instruction")
	confirmHandle(e.Handle, inv.Str("confirm"), "connecting", "nothing was connected")
	body := map[string]any{"source_agent_id": src.ID, "library_listing_id": e.ID, "scope": "advice", "context_policy": "question_only", "expires_seconds": int64(ttl / time.Second)}
	if pin > 0 {
		body["library_version_pin"] = pin
	}
	var env struct {
		Data adviserGrant `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/adviser-grants", body, &env); err != nil {
		die(err)
	}
	g := env.Data
	emit(map[string]any{"grant": g, "library": e.Handle, "confirmed": true}, func() {
		fmt.Printf("connected: %s/%s may ask @%s (grant %s, %s)\n", sess.Name, src.Name, e.Handle, g.ID, grantState(g))
		fmt.Printf("ask it: ks agent ask @%s \"your question\" --session %s\n", e.Handle, sess.ShortID)
		fmt.Printf("expires %s; withdraw with: ks library disconnect %s\n", g.ExpiresAt, g.ID)
	})
}

// confirmHandle confirms a step by naming the handle: --confirm HANDLE, or at
// a terminal by typing it. --yes never confirms it.
func confirmHandle(handle, given, what, nothing string) {
	matches := func(s string) bool { return trimHandle(s) == handle }
	if given != "" {
		if matches(given) {
			return
		}
		fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q does not name @%s; %s", given, handle, nothing), NextAction: "--confirm " + handle})
	}
	if out.noInput || !stdinIsTerminal() {
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: confirmByNaming(what+" @"+handle, "--confirm "+handle, nothing), NextAction: "--confirm " + handle})
	}
	fmt.Fprintf(os.Stderr, "type %s to go on, or anything else to stop: ", handle)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !matches(line) {
		fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "it was not confirmed; " + nothing})
	}
}

func hostedLibraryDisconnect(cr hostedCreds, inv *Invocation) {
	hostedAdviserDisconnect(cr, inv)
}

func hostedLibraryOrigin(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	var env struct {
		Data struct {
			EntryID      string `json:"library_agent_id"`
			Handle       string `json:"handle"`
			Version      int    `json:"version"`
			SharedBy     string `json:"shared_by"`
			Lineage      string `json:"lineage"`
			Newer        int    `json:"newer_version,omitempty"`
			NewerNote    string `json:"newer_note,omitempty"`
			CanForkNewer bool   `json:"can_fork_newer"`
			EntryState   string `json:"entry_state"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/library-origin", nil, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Println(sanitize(d.Lineage))
		if d.Newer > 0 {
			fmt.Printf("a newer version (v%d) exists: %s\n", d.Newer, sanitize(d.NewerNote))
		}
	})
}

// ---- ks library usage / report ----------------------------------------------------------

func hostedLibraryUsage(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was read")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was read")
	var env struct {
		Data struct {
			Stats  *libraryStats `json:"stats"`
			Askers []struct {
				Name string `json:"name"`
				Asks int64  `json:"asks_30d"`
				Up   int64  `json:"thumbs_up"`
				Down int64  `json:"thumbs_down"`
			} `json:"askers"`
			Note string `json:"note"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", libPath(e.ID, "/usage"), nil, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("@%s\n", e.Handle)
		for _, w := range statWords(d.Stats) {
			fmt.Printf("  %s\n", w)
		}
		for _, a := range d.Askers {
			fmt.Printf("  %s: %d question(s), %d helpful, %d not helpful\n", sanitize(a.Name), a.Asks, a.Up, a.Down)
		}
		if d.Note != "" {
			fmt.Printf("  %s\n", sanitize(d.Note))
		}
	})
}

func hostedLibraryReport(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was reported")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was reported")
	reason := inv.Str("reason")
	if reason == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--reason says what is wrong: wrong, harmful, leaks, spam or other; nothing was reported"})
	}
	var env struct {
		Data struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			Note   string `json:"note"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/reports"), map[string]any{"reason": reason}, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() { fmt.Printf("reported @%s (%s). %s\n", e.Handle, d.Reason, sanitize(d.Note)) })
}

// ---- the entry's lifecycle --------------------------------------------------------------

func lifecycle(cr hostedCreds, e libraryEntry, verb string, body map[string]any) libraryEntry {
	body["expected_revision"] = e.Revision
	var env struct {
		Data libraryEntry `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/"+verb), body, &env); err != nil {
		die(err)
	}
	return env.Data
}

func hostedLibraryDeprecate(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	body := map[string]any{}
	if inv.Set("replacement") {
		r := resolveLibrary(cr, org, inv.Str("replacement"), "nothing was changed")
		body["replacement_id"] = r.ID
	}
	r := lifecycle(cr, e, "deprecate", body)
	emit(r, func() {
		fmt.Printf("@%s is deprecated: it keeps answering until %s, then retires.\n", r.Handle, firstN(r.DeprecatedUntil, 10))
	})
}

func hostedLibraryRetire(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	confirmHandle(e.Handle, inv.Str("confirm"), "retiring", "nothing was changed")
	r := lifecycle(cr, e, "retire", map[string]any{})
	emit(r, func() {
		fmt.Printf("@%s is retired. It stops answering; its answer copies are deleted after 30 days; sessions already forked from it are not touched.\n", r.Handle)
	})
}

func hostedLibraryTransfer(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	r := lifecycle(cr, e, "transfer", map[string]any{"account_id": strings.TrimSpace(inv.Arg(1))})
	emit(r, func() { fmt.Printf("@%s is now owned by %s, with the agent behind it.\n", r.Handle, r.OwnerAccountID) })
}

func hostedLibraryFeature(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	r := lifecycle(cr, e, "feature", map[string]any{"featured": !inv.Bool("off")})
	emit(r, func() {
		fmt.Printf("@%s is %s.\n", r.Handle, map[bool]string{true: "featured", false: "no longer featured"}[r.Featured])
	})
}

func hostedLibraryVerify(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	body := map[string]any{"verified": !inv.Bool("off")}
	if inv.Set("version") {
		body["version"] = inv.Int("version")
	}
	r := lifecycle(cr, e, "verify", body)
	emit(r, func() {
		if r.Verified {
			fmt.Printf("@%s v%d is verified. A newer version does not inherit the badge.\n", r.Handle, r.VerifiedVersion)
		} else {
			fmt.Printf("@%s has no verified version.\n", r.Handle)
		}
	})
}

func hostedLibraryForks(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	var allowed bool
	switch strings.ToLower(inv.Arg(1)) {
	case "on":
		allowed = true
	case "off":
	default:
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "say on or off; nothing was changed", NextAction: "ks library forks @" + e.Handle + " on"})
	}
	var env struct {
		Data struct {
			ForksAllowed bool `json:"forks_allowed"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/forks"), map[string]any{"allowed": allowed, "expected_revision": e.Revision}, &env); err != nil {
		die(err)
	}
	emit(env.Data, func() {
		fmt.Printf("forks of @%s are %s.\n", e.Handle, map[bool]string{true: "on", false: "off"}[env.Data.ForksAllowed])
	})
}

// ---- publishing and editing ---------------------------------------------------------------

// pointBody is what to publish: exactly one of a saved point or "save it now";
// the client never chooses one.
func pointBody(inv *Invocation, nothing string) map[string]any {
	ck := strings.TrimSpace(inv.Str("checkpoint"))
	if (ck == "") == !inv.Bool("save-now") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name what to publish: --checkpoint ID (a saved point; ks session checkpoints lists them) or --save-now (save the agent now); one of the two; " + nothing})
	}
	body := map[string]any{}
	if ck != "" {
		body["checkpoint_id"] = ck
	} else {
		body["save_now"] = true
	}
	if inv.Set("notes") {
		body["release_notes"] = inv.Str("notes")
	}
	return body
}

func publishVersion(cr hostedCreds, e libraryEntry, body map[string]any) libraryVersion {
	var env struct {
		Data libraryVersion `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/versions"), body, &env); err != nil {
		die(err)
	}
	return env.Data
}

func hostedLibraryPublish(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was published")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was published")
	body := pointBody(inv, "nothing was published")
	v := publishVersion(cr, e, body)
	emit(v, func() { fmt.Printf("published @%s v%d (%s).\n", e.Handle, v.Seq, v.State) })
}

func hostedLibraryEdit(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	body := map[string]any{"expected_revision": e.Revision}
	if inv.Set("title") {
		body["title"] = inv.Str("title")
	}
	if inv.Set("description") {
		body["description"] = inv.Str("description")
	}
	if inv.Set("tag") {
		body["tags"] = inv.List("tag")
	}
	if inv.Bool("show-instructions") == inv.Bool("hide-instructions") && inv.Bool("show-instructions") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--show-instructions and --hide-instructions are two different changes; give one; nothing was changed"})
	}
	if inv.Bool("show-instructions") {
		body["show_instructions"] = true
	}
	if inv.Bool("hide-instructions") {
		body["show_instructions"] = false
	}
	var env struct {
		Data libraryEntry `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", libPath(e.ID), body, &env); err != nil {
		die(err)
	}
	emit(env.Data, func() { fmt.Printf("changed @%s.\n", env.Data.Handle) })
}

func hostedLibraryWithdraw(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	v := strings.TrimPrefix(strings.TrimSpace(inv.Arg(1)), "v")
	var env struct {
		Data libraryVersion `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/versions/"+url.PathEscape(v)+"/withdraw"), map[string]any{}, &env); err != nil {
		die(err)
	}
	emit(env.Data, func() { fmt.Printf("@%s v%d is withdrawn.\n", e.Handle, env.Data.Seq) })
}

func hostedLibraryReview(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was changed")
	v := strings.TrimPrefix(strings.TrimSpace(inv.Arg(1)), "v")
	decision := strings.ToLower(inv.Arg(2))
	if decision != "approve" && decision != "decline" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "say approve or decline; nothing was changed"})
	}
	var env struct {
		Data libraryVersion `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/versions/"+url.PathEscape(v)+"/review"), map[string]any{"decision": decision}, &env); err != nil {
		die(err)
	}
	emit(env.Data, func() { fmt.Printf("@%s v%d: %s (%s).\n", e.Handle, env.Data.Seq, decision, env.Data.ReviewState) })
}

func hostedLibrarySettings(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was changed")
	path := "/api/v2/orgs/" + url.PathEscape(org) + "/library-settings"
	var cur struct {
		Data struct {
			WhoMayShare    string `json:"who_may_share"`
			ReviewRequired bool   `json:"review_required"`
			Revision       int64  `json:"revision"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &cur); err != nil {
		die(orgErr(err, "nothing was read"))
	}
	who, review := cur.Data.WhoMayShare, cur.Data.ReviewRequired
	changed := false
	if inv.Set("who-may-share") {
		who = inv.Str("who-may-share")
		changed = true
	}
	if inv.Bool("review-required") && inv.Bool("no-review-required") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--review-required and --no-review-required are two different changes; give one; nothing was changed"})
	}
	if inv.Bool("review-required") {
		review, changed = true, true
	}
	if inv.Bool("no-review-required") {
		review, changed = false, true
	}
	out2 := cur.Data
	if changed {
		var env struct {
			Data struct {
				WhoMayShare    string `json:"who_may_share"`
				ReviewRequired bool   `json:"review_required"`
				Revision       int64  `json:"revision"`
			} `json:"data"`
		}
		if err := hostedMutate(cr, "PUT", path, map[string]any{"who_may_share": who, "review_required": review, "expected_revision": cur.Data.Revision}, &env); err != nil {
			die(err)
		}
		out2 = env.Data
	}
	emit(out2, func() {
		fmt.Printf("who may share: %s; a new version waits for review: %t\n", out2.WhoMayShare, out2.ReviewRequired)
	})
}

// ---- ks agent share / ks agent fork / ks advice rate --------------------------------------

func hostedAgentShare(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was shared")
	handle := trimHandle(inv.Str("handle"))
	if handle == "" || strings.TrimSpace(inv.Str("title")) == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--handle and --title name the entry; nothing was shared"})
	}
	pointBody(inv, "nothing was shared") // refuses before anything is sent
	sess := agentSession(cr, inv)
	src, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	add := map[string]any{"agent_id": src.ID, "handle": handle, "title": inv.Str("title")}
	if inv.Set("description") {
		add["description"] = inv.Str("description")
	}
	if inv.Set("tag") {
		add["tags"] = inv.List("tag")
	}
	if inv.Bool("show-instructions") {
		add["show_instructions"] = true
	}
	var env struct {
		Data libraryEntry `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/orgs/"+url.PathEscape(org)+"/library", add, &env); err != nil {
		die(orgErr(err, "nothing was shared"))
	}
	e := env.Data
	body := pointBody(inv, "")
	var v libraryVersion
	func() {
		var venv struct {
			Data libraryVersion `json:"data"`
		}
		if err := hostedMutate(cr, "POST", libPath(e.ID, "/versions"), body, &venv); err != nil {
			fail(&cliError{Code: exitFailed, Kind: "partly_done", Message: fmt.Sprintf("@%s is in the library, but its first version was not published: %s", e.Handle, sanitize(err.Error())), NextAction: "ks library publish @" + e.Handle + " --save-now"})
		}
		v = venv.Data
	}()
	emit(map[string]any{"entry": e, "version": v}, func() {
		fmt.Printf("shared %s/%s as @%s, version v%d (%s).\n", sess.Name, src.Name, e.Handle, v.Seq, v.State)
		fmt.Println("People in the organization can ask it (ks library connect) or fork it (ks agent fork).")
		fmt.Println("Asking reveals what the agent knows; a fork copies its workspace, but no keys, approvals or connections.")
	})
}

func hostedAgentFork(cr hostedCreds, inv *Invocation) {
	org := libraryOrg(cr, inv, "nothing was forked")
	e := resolveLibrary(cr, org, inv.Arg(0), "nothing was forked")
	exec, planID := strings.TrimSpace(inv.Str("execute")), strings.TrimSpace(inv.Str("plan-id"))
	if exec == "" {
		body := map[string]any{}
		if inv.Set("version") {
			body["version"] = inv.Int("version")
		}
		if inv.Set("name") {
			body["name"] = inv.Str("name")
		}
		if inv.Set("project") {
			body["project_id"] = inv.Str("project")
		}
		var env struct {
			Data struct {
				ID          string   `json:"id"`
				PlanDigest  string   `json:"plan_digest"`
				Handle      string   `json:"handle"`
				Version     int      `json:"version"`
				Name        string   `json:"name"`
				Copies      []string `json:"copies"`
				DoesNotCopy []string `json:"does_not_copy"`
				Keys        struct {
					Note string `json:"note"`
				} `json:"keys"`
				Cost    string `json:"cost"`
				Lineage string `json:"lineage"`
			} `json:"data"`
		}
		if err := hostedMutate(cr, "POST", libPath(e.ID, "/fork-plan"), body, &env); err != nil {
			die(err)
		}
		d := env.Data
		emit(d, func() {
			fmt.Printf("fork plan for @%s v%d: NOTHING was created\n", d.Handle, d.Version)
			fmt.Printf("  lineage    %s\n", sanitize(d.Lineage))
			fmt.Printf("  session    %s\n", sanitize(d.Name))
			for _, c := range d.Copies {
				fmt.Printf("  copies     %s\n", sanitize(c))
			}
			for _, c := range d.DoesNotCopy {
				fmt.Printf("  not copied %s\n", sanitize(c))
			}
			fmt.Printf("  keys       %s\n", sanitize(d.Keys.Note))
			fmt.Printf("  cost       %s\n", sanitize(d.Cost))
			fmt.Printf("  digest     %s\n", d.PlanDigest)
			fmt.Printf("fork exactly this plan: ks agent fork @%s --execute %s --plan-id %s\n", e.Handle, d.PlanDigest, d.ID)
		})
		return
	}
	if planID == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--plan-id names the plan the digest belongs to (ks agent fork @" + e.Handle + " prints both); nothing was forked"})
	}
	var env struct {
		Data struct {
			SessionID string   `json:"session_id"`
			Name      string   `json:"name"`
			AgentIDs  []string `json:"agent_ids"`
			Lineage   string   `json:"lineage"`
			Note      string   `json:"note"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", libPath(e.ID, "/fork"), map[string]any{"plan_id": planID, "plan_digest": exec}, &env); err != nil {
		die(err)
	}
	d := env.Data
	emit(d, func() {
		fmt.Printf("forked: session %s %q. %s\n", d.SessionID, sanitize(d.Name), sanitize(d.Lineage))
		if d.Note != "" {
			fmt.Println(sanitize(d.Note))
		}
		fmt.Printf("open it: ks session show %s\n", d.SessionID)
	})
}

func hostedAdviceRate(cr hostedCreds, inv *Invocation) {
	rating := strings.ToLower(inv.Arg(1))
	if rating != "up" && rating != "down" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "say up or down; nothing was recorded"})
	}
	var env struct {
		Data struct {
			ConsultationID string `json:"consultation_id"`
			Rating         string `json:"rating"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/consultations/"+url.PathEscape(inv.Arg(0))+"/rating", map[string]any{"rating": rating}, &env); err != nil {
		die(err)
	}
	emit(env.Data, func() { fmt.Printf("recorded: %s for %s.\n", env.Data.Rating, env.Data.ConsultationID) })
}
