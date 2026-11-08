// connect.go: repository connections to GitHub and GitLab, from the terminal.
// A person or an organization connects GitHub or GitLab once (through the
// provider's own approval page, or with a read-only token), and sessions and
// Cruise drafts then start from a repository it reaches.
//
// What these verbs hold to:
//   - a token is read from a prompt that does not echo it, or from standard
//     input when it is piped; never from a flag or an argument, never
//     printed, never kept here;
//   - the service keeps the credential sealed and never gives it to a
//     session's machine; this client only ever sees the card (who the
//     connection acts as, its status in words, the last four characters);
//   - when two connections can reach a repository, every choice is printed
//     with the way to pick one (--connection), and none is picked here.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type repoConnection struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	Kind         string   `json:"kind"`
	Owner        string   `json:"owner"`
	OrgID        string   `json:"org_id,omitempty"`
	Host         string   `json:"host"`
	BaseURL      string   `json:"base_url,omitempty"`
	ExternalName string   `json:"external_name"`
	Name         string   `json:"name,omitempty"`
	Scopes       []string `json:"scopes"`
	Status       string   `json:"status"`
	StatusWords  string   `json:"status_words"`
	Last4        string   `json:"last4,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	Warning      string   `json:"warning,omitempty"`
	CreatedBy    string   `json:"created_by"`
	CreatedAt    string   `json:"created_at"`
	Revision     int64    `json:"revision"`
}

type repoChoiceRow struct {
	ConnectionID string `json:"connection_id"`
	Provider     string `json:"provider"`
	Kind         string `json:"kind"`
	Owner        string `json:"owner"`
	ExternalName string `json:"external_name"`
	Words        string `json:"words"`
}

// ---- the context, the words ----------------------------------------------------------------

// connectOrg is the organization a connection verb acts in: --organization,
// else the context this client works in (ks context use), else "" for your
// personal account.
func connectOrg(cr hostedCreds, inv *Invocation) string {
	if org := strings.TrimSpace(inv.Str("organization")); org != "" {
		return org
	}
	return currentContext(cr)
}

func providerWord(p string) string {
	switch p {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	}
	return sanitize(p)
}

func connKindWord(k string) string {
	switch k {
	case "app":
		return "the KeepState app"
	case "oauth":
		return "a sign-in grant"
	case "token":
		return "a token"
	}
	return sanitize(k)
}

func connOwnerWord(c repoConnection) string {
	if c.Owner == "organization" {
		if c.OrgID != "" {
			return "the organization " + sanitize(c.OrgID)
		}
		return "an organization"
	}
	return "yours"
}

func connPath(id string, more ...string) string {
	return "/api/v2/connections/" + url.PathEscape(strings.TrimSpace(id)) + strings.Join(more, "")
}

// connCard prints one connection: provider, host, who it acts as, whose it
// is, its status in words, the token's last four characters and a warning.
func connCard(c repoConnection, full bool) {
	title := providerWord(c.Provider) + " on " + sanitize(c.Host)
	if c.Name != "" {
		title += " (" + sanitize(c.Name) + ")"
	}
	fmt.Printf("%s  %s\n", sanitize(c.ID), title)
	fmt.Printf("    acts as   %s, through %s\n", sanitize(c.ExternalName), connKindWord(c.Kind))
	fmt.Printf("    owner     %s\n", connOwnerWord(c))
	status := sanitize(c.Status)
	if c.StatusWords != "" {
		status += ": " + sanitize(c.StatusWords)
	}
	fmt.Printf("    status    %s\n", status)
	if c.Last4 != "" {
		line := "ending " + sanitize(c.Last4)
		if c.ExpiresAt != "" {
			line += ", expires " + sanitize(c.ExpiresAt)
		}
		fmt.Printf("    token     %s\n", line)
	} else if c.ExpiresAt != "" {
		fmt.Printf("    expires   %s\n", sanitize(c.ExpiresAt))
	}
	if c.Warning != "" {
		fmt.Printf("    warning   %s\n", sanitize(c.Warning))
	}
	if !full {
		return
	}
	if c.BaseURL != "" {
		fmt.Printf("    address   %s\n", sanitize(c.BaseURL))
	}
	if len(c.Scopes) > 0 {
		fmt.Printf("    scopes    %s\n", sanitize(strings.Join(c.Scopes, ", ")))
	}
	fmt.Printf("    made      %s by %s (revision %d)\n", sanitize(c.CreatedAt), sanitize(c.CreatedBy), c.Revision)
}

// ---- refusals in words ---------------------------------------------------------------------

// choiceDetail is a ks_connection_choice refusal's facts: every connection
// that can reach the repository, and how to pick one. None is picked here.
type choiceDetail struct {
	Choices   []repoChoiceRow `json:"choices"`
	HowToPick string          `json:"how_to_pick"`
}

func (d choiceDetail) detailLines() []string {
	lines := []string{fmt.Sprintf("%d connections can reach this repository:", len(d.Choices))}
	for _, c := range d.Choices {
		who := "yours"
		if c.Owner == "organization" {
			who = "the organization's"
		}
		line := fmt.Sprintf("  %s  %s, %s, %s, acting as %s", sanitize(c.ConnectionID), providerWord(c.Provider), connKindWord(c.Kind), who, sanitize(c.ExternalName))
		if c.Words != "" {
			line += ": " + sanitize(c.Words)
		}
		lines = append(lines, line)
	}
	return append(lines, d.HowToPick)
}

var httpRouteWords = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /`)

// repoErr turns a refusal of a repository route into the service's words
// and its next step. A choice between connections lists every choice and
// says how to pick one; retry is the command to run again with --connection.
func repoErr(err error, retry, nothing string) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	var doc struct {
		Error struct {
			NextAction string          `json:"next_action"`
			Choices    []repoChoiceRow `json:"choices"`
		} `json:"error"`
	}
	_ = json.Unmarshal(he.Raw, &doc)
	ce := classify(err)
	if ce.Message != "" && nothing != "" && !strings.Contains(strings.ToLower(ce.Message), strings.ToLower(nothing)) {
		ce.Message += "; " + nothing
	}
	if next := strings.TrimSpace(doc.Error.NextAction); next != "" && !httpRouteWords.MatchString(next) {
		ce.NextAction = sanitize(next)
	}
	if he.Type == "ks_connection_choice" {
		pick := "pick one by its id: " + retry + " --connection CONNECTION"
		ce.Detail = choiceDetail{Choices: doc.Error.Choices, HowToPick: pick}
		ce.NextAction = retry + " --connection CONNECTION (one of the connections above)"
		if len(doc.Error.Choices) == 0 {
			ce.Detail = nil
		}
	}
	return ce
}

// isRepoRefusal says whether a refusal belongs to the repository routes, so
// a caller that also has its own refusal words (a Cruise draft) knows whose
// words to use.
func isRepoRefusal(err error) bool {
	var he *hostedErr
	if !errors.As(err, &he) {
		return false
	}
	for _, p := range []string{"ks_connection", "ks_repositor", "ks_branch", "ks_address", "ks_provider", "ks_token", "ks_base_url"} {
		if strings.HasPrefix(he.Type, p) {
			return true
		}
	}
	return false
}

// ---- ks connect list / show ----------------------------------------------------------------

func fetchConnections(cr hostedCreds, org, provider string) ([]repoConnection, error) {
	path := "/api/v2/connections"
	if provider != "" {
		path += "?provider=" + url.QueryEscape(provider)
	}
	var env struct {
		Data struct {
			Items      []repoConnection `json:"items"`
			NextCursor string           `json:"next_cursor"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", keyPath(org, path), nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

func checkProvider(p, nothing string) {
	if p != "" && p != "github" && p != "gitlab" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--provider %q is github or gitlab; %s", p, nothing)})
	}
}

func hostedConnectList(cr hostedCreds, inv *Invocation) {
	org := connectOrg(cr, inv)
	provider := strings.TrimSpace(inv.Str("provider"))
	checkProvider(provider, "nothing was read")
	conns, err := fetchConnections(cr, org, provider)
	if err != nil {
		die(repoErr(err, "", "nothing was read"))
	}
	emit(map[string]any{"context": contextOf(org), "connections": conns, "count": len(conns)}, func() {
		where := "your personal account"
		if org != "" {
			where = "the organization " + org
		}
		if len(conns) == 0 {
			fmt.Printf("No connections in %s. Connect one: ks connect github, or ks connect gitlab\n", where)
			return
		}
		fmt.Printf("Connections of %s:\n", where)
		for i, c := range conns {
			if i > 0 {
				fmt.Println()
			}
			connCard(c, false)
		}
	})
}

func contextOf(org string) string {
	if org == "" {
		return "personal"
	}
	return org
}

func hostedConnectShow(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data repoConnection `json:"data"`
	}
	if err := hostedCall(cr, "GET", connPath(inv.Arg(0)), nil, &env); err != nil {
		die(repoErr(err, "", "nothing was read"))
	}
	c := env.Data
	emit(c, func() { connCard(c, true) })
}

// ---- ks connect github / gitlab ------------------------------------------------------------

func hostedConnectGitHub(cr hostedCreds, inv *Invocation) { connectProvider(cr, inv, "github") }
func hostedConnectGitLab(cr hostedCreds, inv *Invocation) { connectProvider(cr, inv, "gitlab") }

func connectProvider(cr hostedCreds, inv *Invocation, provider string) {
	org := connectOrg(cr, inv)
	base := strings.TrimSpace(inv.Str("url"))
	if base != "" && !inv.Bool("token") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--url names a self-managed GitLab instance, which connects with a token only; add --token. Nothing was connected", NextAction: "ks connect gitlab --url " + base + " --token"})
	}
	if base != "" && !strings.HasPrefix(base, "https://") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--url is the instance's https address, like https://gitlab.example.com; nothing was connected"})
	}
	if inv.Set("name") && !inv.Bool("token") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--name names a connection made with a token; the provider's page names the other kind. Add --token, or leave out --name. Nothing was connected"})
	}
	if inv.Bool("token") {
		connectWithToken(cr, inv, org, provider, base)
		return
	}
	connectThroughBrowser(cr, inv, org, provider)
}

// readSecretToken reads a token from a prompt that does not echo it when
// standard input is a terminal, or whole from standard input when it is
// piped. It is never an argument and never printed.
func readSecretToken(provider string) string {
	tty := stdinIsTerminal()
	if tty && out.noInput {
		fail(&cliError{Code: exitUsage, Kind: "needs_input", Message: "--no-input: the token is read from standard input, so pipe it in; nothing was connected", NextAction: "ks connect " + provider + " --token < token.txt"})
	}
	var raw string
	if tty {
		// suppress first, then say what is true
		restore, off := noEcho()
		if off {
			fmt.Fprintln(os.Stderr, "paste the token, then Enter (it is not echoed and not kept here):")
		} else {
			fmt.Fprintln(os.Stderr, "paste the token, then Enter (this terminal will SHOW it; it is not kept here):")
		}
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 8192)).ReadString('\n')
		// before anything that can exit: fail() runs no defers
		restore()
		fmt.Fprintln(os.Stderr)
		if err != nil && err != io.EOF {
			die(err)
		}
		raw = line
	} else {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if err != nil {
			die(err)
		}
		raw = string(b)
	}
	tok := strings.TrimSpace(raw)
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "no token was read: paste one token at the prompt, or pipe it on standard input; nothing was connected", NextAction: "ks connect " + provider + " --token < token.txt"})
	}
	return tok
}

func connectWithToken(cr hostedCreds, inv *Invocation, org, provider, base string) {
	tok := readSecretToken(provider)
	body := map[string]any{"token": tok}
	tok = ""
	if base != "" {
		body["base_url"] = base
	}
	if n := strings.TrimSpace(inv.Str("name")); n != "" {
		body["name"] = n
	}
	var env struct {
		Data repoConnection `json:"data"`
	}
	err := hostedCall(cr, "POST", keyPath(org, "/api/v2/connections/"+provider+"/token"), body, &env)
	body["token"] = ""
	if err != nil {
		die(repoErr(err, "", "nothing was connected"))
	}
	c := env.Data
	emit(c, func() {
		fmt.Printf("connected %s; KeepState keeps the token sealed and never gives it to a session's machine\n", providerWord(provider))
		connCard(c, false)
		fmt.Println("its repositories: ks connect repos " + sanitize(c.ID))
	})
}

type connectStart struct {
	State     string `json:"state"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	Poll      string `json:"poll"`
	Note      string `json:"note"`
}

type connectStartPoll struct {
	State      string          `json:"state"`
	Message    string          `json:"message,omitempty"`
	Connection *repoConnection `json:"connection,omitempty"`
}

func connectThroughBrowser(cr hostedCreds, inv *Invocation, org, provider string) {
	var env struct {
		Data connectStart `json:"data"`
	}
	if err := hostedCall(cr, "POST", keyPath(org, "/api/v2/connections/"+provider+"/start"), map[string]any{}, &env); err != nil {
		die(repoErr(err, "", "nothing was connected"))
	}
	st := env.Data
	if st.State == "" || !strings.HasPrefix(st.URL, "https://") {
		fail(&cliError{Code: exitFailed, Kind: "connect_link_missing", Message: "the service named no https link to approve the connection at; nothing was connected"})
	}
	// the link and the note on stderr, so --json stdout is the outcome alone
	fmt.Fprintf(os.Stderr, "Open this link to connect %s:\n  %s\n", providerWord(provider), sanitize(st.URL))
	if st.Note != "" {
		fmt.Fprintln(os.Stderr, sanitize(st.Note))
	}
	if !out.noInput && openBrowser(st.URL) {
		fmt.Fprintln(os.Stderr, "Opened it in your browser.")
	}
	deadline := time.Now().Add(10 * time.Minute)
	if t, err := time.Parse(time.RFC3339, st.ExpiresAt); err == nil {
		deadline = t
	}
	fmt.Fprintln(os.Stderr, "Waiting for the approval...")
	statePath := "/api/v2/connection-starts/" + url.PathEscape(st.State)
	for {
		var p struct {
			Data connectStartPoll `json:"data"`
		}
		err := hostedCall(cr, "GET", statePath, nil, &p)
		if err == nil {
			switch r := p.Data; r.State {
			case "connected":
				emit(r, func() {
					fmt.Printf("connected %s\n", providerWord(provider))
					if r.Message != "" {
						fmt.Println(sanitize(r.Message))
					}
					if r.Connection != nil {
						connCard(*r.Connection, false)
						fmt.Println("its repositories: ks connect repos " + sanitize(r.Connection.ID))
					}
				})
				return
			case "refused", "expired":
				msg := "the connection was " + r.State
				if r.Message != "" {
					msg += ": " + sanitize(r.Message)
				}
				fail(&cliError{Code: exitFailed, Kind: "connect_" + r.State, Message: msg + "; nothing was connected", NextAction: "ks connect " + provider})
			}
		} else {
			var he *hostedErr
			if errors.As(err, &he) && he.Status/100 == 4 && he.Status != 429 {
				die(repoErr(err, "", "nothing was connected"))
			}
			// anything else is transient; the deadline bounds the wait
		}
		if time.Now().After(deadline) {
			fail(&cliError{Code: exitFailed, Kind: "connect_expired", Message: "the link expired before the connection was approved; nothing was connected", NextAction: "ks connect " + provider})
		}
		time.Sleep(waitPoll())
	}
}

// ---- ks connect remove ---------------------------------------------------------------------

func hostedConnectRemove(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	var env struct {
		Data repoConnection `json:"data"`
	}
	if err := hostedCall(cr, "GET", connPath(id), nil, &env); err != nil {
		die(repoErr(err, "", "nothing was removed"))
	}
	c := env.Data
	if !out.json {
		fmt.Fprintln(os.Stderr, "remove this connection:")
		fmt.Fprintf(os.Stderr, "  %s  %s on %s, acting as %s, %s\n", sanitize(c.ID), providerWord(c.Provider), sanitize(c.Host), sanitize(c.ExternalName), connOwnerWord(c))
		fmt.Fprintln(os.Stderr, "  KeepState deletes what it holds of it; work that names it is refused afterwards")
	}
	confirmByID(c.ID, inv.Str("confirm"), "removing the connection", "nothing was removed")
	var res struct {
		Data struct {
			ID      string `json:"id"`
			Removed bool   `json:"removed"`
			Note    string `json:"note"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "DELETE", connPath(c.ID), nil, &res); err != nil {
		die(repoErr(err, "", "nothing was removed"))
	}
	d := res.Data
	emit(d, func() {
		fmt.Printf("removed %s\n", sanitize(d.ID))
		if d.Note != "" {
			fmt.Println(sanitize(d.Note))
		}
	})
}

// confirmByID confirms a step by naming the id: --confirm ID, or at a
// terminal by typing it. --yes never confirms it.
func confirmByID(id, given, what, nothing string) {
	if given != "" {
		if strings.TrimSpace(given) == id {
			return
		}
		fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q does not name %s; %s", sanitize(given), id, nothing), NextAction: "--confirm " + id})
	}
	if out.noInput || !stdinIsTerminal() {
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: confirmByNaming(what+" "+id, "--confirm "+id, nothing), NextAction: "--confirm " + id})
	}
	fmt.Fprintf(os.Stderr, "type %s to go on, or anything else to stop: ", id)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != id {
		fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "it was not confirmed; " + nothing})
	}
}

// ---- ks connect repos / branches -----------------------------------------------------------

type repoListRow struct {
	ProviderID    any    `json:"provider_id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	WebURL        string `json:"web_url"`
	ConnectionID  string `json:"connection_id"`
}

func hostedConnectRepos(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	path := connPath(id, "/repositories")
	if q := inv.Str("search"); q != "" {
		path += "?" + url.Values{"q": {q}}.Encode()
	}
	var env struct {
		Data struct {
			Items []repoListRow `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		die(repoErr(err, "", "nothing was read"))
	}
	rows := env.Data.Items
	emit(map[string]any{"connection_id": id, "repositories": rows, "count": len(rows)}, func() {
		if len(rows) == 0 {
			fmt.Printf("The connection %s reaches no repository that matches.\n", id)
			return
		}
		for _, r := range rows {
			vis := "public"
			if r.Private {
				vis = "private"
			}
			fmt.Printf("%s  default branch %s, %s\n", sanitize(r.FullName), sanitize(r.DefaultBranch), vis)
		}
		fmt.Printf("%d repositories; its branches: ks connect branches %s REPOSITORY\n", len(rows), id)
	})
}

func hostedConnectBranches(cr hostedCreds, inv *Invocation) {
	id, repo := strings.TrimSpace(inv.Arg(0)), strings.TrimSpace(inv.Arg(1))
	q := url.Values{"repository": {repo}}
	if s := inv.Str("search"); s != "" {
		q.Set("q", s)
	}
	var env struct {
		Data struct {
			Items []struct {
				Name   string `json:"name"`
				Commit string `json:"commit"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", connPath(id, "/branches")+"?"+q.Encode(), nil, &env); err != nil {
		die(repoErr(err, "", "nothing was read"))
	}
	rows := env.Data.Items
	emit(map[string]any{"connection_id": id, "repository": repo, "branches": rows, "count": len(rows)}, func() {
		if len(rows) == 0 {
			fmt.Printf("%s has no branch that matches.\n", sanitize(repo))
			return
		}
		fmt.Printf("branches of %s (the default branch first):\n", sanitize(repo))
		for _, b := range rows {
			fmt.Printf("  %s  %s\n", sanitize(b.Name), firstN(sanitize(b.Commit), 12))
		}
	})
}

// ---- ks organization connections -----------------------------------------------------------

type connPolicy struct {
	OrgID     string `json:"org_id"`
	Providers []struct {
		Provider         string `json:"provider"`
		OrganizationOnly bool   `json:"organization_only"`
		UpdatedBy        string `json:"updated_by,omitempty"`
		UpdatedAt        string `json:"updated_at,omitempty"`
	} `json:"providers"`
}

func hostedOrgConnections(cr hostedCreds, inv *Invocation) {
	setting := inv.Set("organization-only")
	nothing := "nothing was read"
	if setting {
		nothing = "nothing was changed"
	}
	org := libraryOrg(cr, inv, nothing)
	provider := strings.TrimSpace(inv.Str("provider"))
	checkProvider(provider, nothing)
	path := "/api/v2/orgs/" + url.PathEscape(org) + "/connection-policy"
	var env struct {
		Data connPolicy `json:"data"`
	}
	if setting || provider != "" {
		if !setting || provider == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "set the policy for one provider: --provider github|gitlab together with --organization-only on|off; " + nothing, NextAction: "ks organization connections --provider github --organization-only on"})
		}
		var only bool
		switch inv.Str("organization-only") {
		case "on":
			only = true
		case "off":
		default:
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--organization-only %q is on or off; %s", sanitize(inv.Str("organization-only")), nothing)})
		}
		if err := hostedCall(cr, "PUT", path, map[string]any{"provider": provider, "organization_only": only}, &env); err != nil {
			die(repoErr(orgErr(err, nothing), "", nothing))
		}
	} else if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		die(repoErr(orgErr(err, nothing), "", nothing))
	}
	p := env.Data
	emit(p, func() {
		fmt.Printf("repository connections of the organization %s:\n", sanitize(firstNonEmptyStr(p.OrgID, org)))
		for _, it := range p.Providers {
			words := "its work may also use a member's own connection, where the member allows it"
			if it.OrganizationOnly {
				words = "its work uses only the organization's own connections"
			}
			line := fmt.Sprintf("  %-7s organization only %s: %s", providerWord(it.Provider), map[bool]string{true: "on", false: "off"}[it.OrganizationOnly], words)
			if it.UpdatedAt != "" {
				line += fmt.Sprintf(" (set %s by %s)", sanitize(it.UpdatedAt), sanitize(it.UpdatedBy))
			}
			fmt.Println(line)
		}
	})
}

// ---- ks session repo -----------------------------------------------------------------------

type repoPlacement struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	State        string   `json:"state"`
	Provider     string   `json:"provider"`
	Repository   string   `json:"repository"`
	Branch       string   `json:"branch"`
	Commit       string   `json:"commit"`
	ConnectionID string   `json:"connection_id"`
	To           string   `json:"to"`
	Files        int64    `json:"files"`
	Bytes        int64    `json:"bytes"`
	Notes        []string `json:"notes"`
	Refusal      *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		NextAction string `json:"next_action"`
	} `json:"refusal,omitempty"`
	Message string `json:"message"`
}

// placementWait bounds how long session repo follows a placement: the
// global --wait-timeout when given, else ten minutes.
var placementWait = 10 * time.Minute

func hostedSessionRepo(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	repo := strings.TrimSpace(inv.Arg(1))
	if repo == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the repository: github.com/owner/name, gitlab.com/group/project, an https address, or owner/name with --connection; nothing was placed"})
	}
	sid := agentSessionID(sess)
	body := map[string]any{"repository": repo}
	for flag, key := range map[string]string{"branch": "branch", "connection": "connection_id", "to": "to"} {
		if v := strings.TrimSpace(inv.Str(flag)); v != "" {
			body[key] = v
		}
	}
	if inv.Bool("replace") {
		body["replace"] = true
	}
	retry := "ks session repo " + sess.ShortID + " " + repo
	var env struct {
		Data repoPlacement `json:"data"`
	}
	base := "/api/v2/sessions/" + url.PathEscape(sid) + "/repository"
	if err := hostedCall(cr, "POST", base, body, &env); err != nil {
		die(repoErr(err, retry, "nothing was placed"))
	}
	p := env.Data
	if p.State == "fetching" && !inv.Bool("no-wait") {
		progress("fetching %s into session %s; KeepState reads with the connection and never gives it to the session's machine", sanitize(repo), sess.ShortID)
		bound := placementWait
		if inv.Set("wait-timeout") {
			bound = waitBound
		}
		deadline := time.Now().Add(bound)
		for p.State == "fetching" {
			if time.Now().After(deadline) {
				fail(&cliError{Code: exitTemporary, Kind: "placement_fetching", WorkStarted: workYes,
					Message:    fmt.Sprintf("placement %s is still fetching after %s; the service carries on", sanitize(p.ID), bound),
					NextAction: "ks session show " + sess.ShortID})
			}
			time.Sleep(waitPoll())
			var again struct {
				Data repoPlacement `json:"data"`
			}
			if err := hostedCall(cr, "GET", base+"/"+url.PathEscape(p.ID), nil, &again); err != nil {
				var he *hostedErr
				if errors.As(err, &he) && he.Status/100 == 4 && he.Status != 429 {
					die(repoErr(err, retry, ""))
				}
				continue // transient; the deadline bounds the wait
			}
			p = again.Data
		}
	}
	if p.State == "refused" {
		code, msg, next := "ks_repository_refused", "the repository was not placed", ""
		if p.Refusal != nil {
			code = firstNonEmptyStr(p.Refusal.Code, code)
			msg = firstNonEmptyStr(p.Refusal.Message, msg)
			next = p.Refusal.NextAction
		}
		if !out.json {
			if p.Message != "" {
				fmt.Println(sanitize(p.Message))
			}
			placementNotes(p)
		}
		ce := &cliError{Code: exitFailed, Kind: code, Message: sanitize(msg) + "; nothing was placed", WorkStarted: workNo, Detail: p}
		if next != "" && !httpRouteWords.MatchString(next) {
			ce.NextAction = sanitize(next)
		}
		fail(ce)
	}
	emit(p, func() {
		switch p.State {
		case "placed":
			fmt.Printf("placed %s at %s (%s) into %s: %d files, %s bytes\n", sanitize(p.Repository), sanitize(p.Branch), firstN(sanitize(p.Commit), 12), sanitize(notRecorded(p.To)), p.Files, commas(p.Bytes))
		default:
			fmt.Printf("placement %s is %s: %s into session %s\n", sanitize(p.ID), sanitize(p.State), sanitize(repo), sess.ShortID)
		}
		if p.Message != "" {
			fmt.Println(sanitize(p.Message))
		}
		placementNotes(p)
		if p.ConnectionID != "" {
			fmt.Printf("through the connection %s\n", sanitize(p.ConnectionID))
		}
	})
}

func placementNotes(p repoPlacement) {
	for _, n := range p.Notes {
		fmt.Printf("  note: %s\n", sanitize(n))
	}
}

// orgConnFlag is the --organization flag of the connection verbs.
var orgConnFlag = Flag{Name: "organization", Kind: flagString, Value: "ID", Summary: "the organization whose connections these are (ks organization list shows yours); default the context this client works in (ks context use), else your personal account"}
