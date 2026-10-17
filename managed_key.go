// managed_key.go: model calls on KeepState's key.
//
// A session's model calls for a provider go on one of your own keys or on
// KeepState's key. KeepState's key is paid from purchased credit (free credit
// never pays for it) at the provider's list price plus the spread, and each
// call is held before it is sent. The account keeps a default per provider
// for sessions created without a choice.
//
// The service lists KeepState's key as the capability managed_inference.
// Where it does not list it as available, nothing here asks the service
// anything new, and every command sends exactly what it sent before.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	managedCapability = "managed_inference"
	keepstateKeyArg   = "keepstate" // --key keepstate: KeepState's key
	ownKeyArg         = "own"       // --key own: your own key, whatever the account's default
	modelKeysPath     = "/api/v2/account/model-keys"
)

// managedProviders are the providers whose calls can go on KeepState's key.
var managedProviders = []string{"anthropic", "openai"}

type managedRates struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
}

type managedModel struct {
	ID       string       `json:"id"`
	Standing string       `json:"standing"`
	Note     string       `json:"note"`
	List     managedRates `json:"list_microusd_per_mtok"`
	Charged  managedRates `json:"charged_microusd_per_mtok"`
}

type managedProvider struct {
	Standing  string         `json:"standing"`
	Note      string         `json:"note"`
	SpreadPct float64        `json:"spread_pct"`
	Models    []managedModel `json:"models"`
}

// modelKeysDoc is the account's model keys: its default per provider and
// what KeepState's key offers it now.
type modelKeysDoc struct {
	Defaults map[string]string `json:"defaults"`
	Revision int64             `json:"revision"`
	Managed  struct {
		Available   bool                       `json:"available"`
		Note        string                     `json:"note"`
		Currency    string                     `json:"currency"`
		PriceBook   string                     `json:"price_book"`
		RateVersion string                     `json:"rate_version"`
		Providers   map[string]managedProvider `json:"providers"`
		Limits      struct {
			SessionDefault int64 `json:"session_default_microusd"`
			Daily          int64 `json:"daily_microusd"`
			Monthly        int64 `json:"monthly_microusd"`
			Concurrent     int64 `json:"concurrent_calls"`
		} `json:"limits"`
		Account struct {
			Purchased  int64 `json:"purchased_credit_microusd"`
			Headroom   int64 `json:"headroom_microusd"`
			SpentToday int64 `json:"spent_today_microusd"`
			SpentMonth int64 `json:"spent_this_month_microusd"`
			OpenHolds  int64 `json:"open_holds_microusd"`
			Held       bool  `json:"held_by_operator"`
		} `json:"account"`
	} `json:"managed"`
}

// defaultFor is the account's default for a provider: "managed" or "own"; a
// provider the document does not name reads own.
func (d *modelKeysDoc) defaultFor(p string) string {
	if d != nil && d.Defaults[p] == "managed" {
		return "managed"
	}
	return "own"
}

// offered is a provider's offer, and whether a call can go on KeepState's key
// for it now.
func (d *modelKeysDoc) offered(p string) (managedProvider, bool) {
	mp, ok := d.Managed.Providers[p]
	return mp, d.Managed.Available && ok && mp.Standing == "offered"
}

// notOfferedWhy is the service's own reason a provider cannot go on
// KeepState's key now.
func (d *modelKeysDoc) notOfferedWhy(p string) string {
	if !d.Managed.Available {
		return firstNonEmptyStr(sanitize(d.Managed.Note), "the service does not offer it now")
	}
	mp, ok := d.Managed.Providers[p]
	if !ok {
		return "the service names no offer for " + providerName(p)
	}
	return firstNonEmptyStr(sanitize(mp.Note), "its standing for "+providerName(p)+" is "+sanitize(mp.Standing))
}

// spreadText is what KeepState's key costs for a provider, in words.
func spreadText(p string, mp managedProvider) string {
	return providerName(p) + "'s list price plus " + strconv.FormatFloat(mp.SpreadPct, 'f', -1, 64) + "%"
}

var errModelKeysNotServed = errors.New("this service does not serve account model keys")

// fetchModelKeys reads the account's model keys. A service that does not
// serve the route answers errModelKeysNotServed.
func fetchModelKeys(cr hostedCreds) (*modelKeysDoc, error) {
	var env struct {
		Data modelKeysDoc `json:"data"`
	}
	if err := hostedCall(cr, "GET", modelKeysPath, nil, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
			return nil, errModelKeysNotServed
		}
		return nil, err
	}
	return &env.Data, nil
}

// managedListed reads the capability registry the command was just admitted
// by (the copy requireCapability refreshed), so it asks nothing: the row,
// and whether KeepState's key is listed as available to this client.
func managedListed(cr hostedCreds) (*capabilityRow, bool) {
	set := cachedCapabilities()
	if set == nil || set.ControlPlane != cr.CTL {
		return nil, false
	}
	row := set.find(managedCapability)
	return row, row != nil && row.Availability == "available" && row.SinceProtocol <= clientProtocol
}

// managedRowWhy is the registry's reason KeepState's key is not available.
func managedRowWhy(row *capabilityRow) string {
	switch {
	case row == nil:
		return "this service does not offer it"
	case row.Availability == "available":
		return fmt.Sprintf("it needs protocol %d and this client speaks %d (ks update)", row.SinceProtocol, clientProtocol)
	}
	if why := sanitize(row.why()); why != "" {
		return "the service says: " + why
	}
	return "the service reports it " + sanitize(row.Availability)
}

// parseDollars reads a spend limit typed in dollars ("20", "35.50", "$5"):
// positive, at most two decimals, as micro-dollars.
func parseDollars(flag, s string) (int64, *cliError) {
	bad := &cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--%s is an amount in dollars, more than zero, with at most two decimals (20 or 35.50); got %q. Nothing was sent", flag, s)}
	t := strings.TrimPrefix(strings.TrimSpace(s), "$")
	if t == "" {
		return 0, bad
	}
	whole, frac, dot := strings.Cut(t, ".")
	if whole == "" || len(frac) > 2 || (dot && frac == "") {
		return 0, bad
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, bad
		}
	}
	if len(whole) > 9 {
		return 0, bad
	}
	w, _ := strconv.ParseInt(whole, 10, 64)
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt((frac + "0")[:2], 10, 64)
	}
	micros := w*1_000_000 + f*10_000
	if micros <= 0 {
		return 0, bad
	}
	return micros, nil
}

// checkManagedFlags refuses, before any request, a choice of key or a spend
// limit that cannot be what was meant.
func checkManagedFlags(inv *Invocation) {
	if inv == nil || inv.Cmd == nil {
		return
	}
	usage := func(msg, next string) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: msg, NextAction: next})
	}
	key := inv.Str("key")
	switch inv.Cmd.Name() {
	case "run":
		if !inv.Bool("agent") {
			if inv.Set("spend-limit") || key == keepstateKeyArg || key == ownKeyArg {
				usage("--key "+firstNonEmptyStr(key, keepstateKeyArg)+" and --spend-limit belong to an agent session. Nothing was started", "ks run --agent --key keepstate")
			}
			return
		}
		if inv.Set("spend-limit") {
			if key != keepstateKeyArg {
				usage("--spend-limit is the session's spend limit on KeepState's key, so it goes with --key keepstate. Nothing was created", "ks run --agent --key keepstate --spend-limit 20")
			}
			if _, err := parseDollars("spend-limit", inv.Str("spend-limit")); err != nil {
				fail(err)
			}
		}
	case "session key set":
		if !inv.Set("key") && !inv.Set("spend-limit") {
			usage("name the key with --key (a key's id or alias, or keepstate for KeepState's key), or change the spend limit with --spend-limit. Nothing was changed", "ks session key set <session> --key keepstate")
		}
		if inv.Set("provider") {
			if key != keepstateKeyArg {
				usage("--provider goes with --key keepstate; an own key's provider is the key's. Nothing was changed", "ks session key set <session> --key keepstate --provider openai")
			}
			if !isManagedProvider(inv.Str("provider")) {
				usage("--provider is anthropic or openai, the providers KeepState's key serves; got "+sanitize(inv.Str("provider"))+". Nothing was changed", "")
			}
		}
		if inv.Set("spend-limit") {
			if inv.Set("key") && key != keepstateKeyArg {
				usage("--spend-limit is the session's spend limit on KeepState's key; it does not go with your own key. Nothing was changed", "ks session key set <session> --spend-limit 20")
			}
			if _, err := parseDollars("spend-limit", inv.Str("spend-limit")); err != nil {
				fail(err)
			}
		}
	case "preflight":
		if inv.Set("key") && key != keepstateKeyArg && key != ownKeyArg {
			usage("--key is keepstate (a run on KeepState's key) or own (a run on your own key); got "+sanitize(key)+". Nothing was checked", "ks preflight --key keepstate")
		}
	case "key default":
		p, v := inv.Arg(0), inv.Arg(1)
		switch {
		case p == "" && v == "":
		case v == "":
			usage("name the provider and the key: ks key default "+sanitize(p)+" own, or ks key default "+sanitize(p)+" keepstate. Nothing was changed", "ks key default")
		case !isManagedProvider(p):
			usage("the provider is anthropic or openai; got "+sanitize(p)+". Nothing was changed", "ks key default")
		case v != ownKeyArg && v != keepstateKeyArg:
			usage("the default is own (your own key) or keepstate (KeepState's key); got "+sanitize(v)+". Nothing was changed", "ks key default "+sanitize(p)+" keepstate")
		}
	}
}

func isManagedProvider(p string) bool {
	for _, x := range managedProviders {
		if x == p {
			return true
		}
	}
	return false
}

// managedRefusal words the five refusals of a call or a choice on
// KeepState's key: the service's words, its code, and the next step.
func managedRefusal(he *hostedErr) *cliError {
	next := map[string]string{
		"ks_insufficient_credit":       "add credit in the console (Billing); free credit does not pay for model use on KeepState's key. Or use your own key: --key own",
		"ks_managed_unavailable":       "use your own key: --key own (ks key list shows your keys)",
		"ks_managed_model_not_offered": "choose a model KeepState's key offers (ks key list), or use your own key: --key own",
		"ks_managed_cap_reached":       "use your own key until the account's limit on KeepState's key resets: --key own",
		"ks_spend_limit_reached":       "raise the session's spend limit: ks session key set <session> --spend-limit DOLLARS",
	}[he.Type]
	if next == "" {
		return nil
	}
	return &cliError{Code: exitConflict, Kind: he.Type, HTTPStatus: he.Status, WorkStarted: workNo,
		Message: sanitize(he.Message) + " [" + he.Type + "]", NextAction: next}
}

// harnessProvider is the provider a session's harness calls.
func harnessProvider(h string) string {
	switch h {
	case "", defaultHarness:
		return "anthropic"
	case "codex":
		return "openai"
	}
	return ""
}

// ---- ks key list: KeepState's key beside your own -------------------------------

// managedKeyRows are the rows ks key list prints for KeepState's key: one per
// provider it is offered for now.
func managedKeyRows(d *modelKeysDoc) []string {
	if d == nil || !d.Managed.Available {
		return nil
	}
	var ps []string
	for p := range d.Managed.Providers {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	var out []string
	for _, p := range ps {
		mp, ok := d.offered(p)
		if !ok {
			continue
		}
		state := "list price plus " + strconv.FormatFloat(mp.SpreadPct, 'f', -1, 64) + "%, from purchased credit"
		if d.defaultFor(p) == "managed" {
			state += "; the default for new sessions"
		}
		out = append(out, fmt.Sprintf("%-14s %-11s %-18s %-6s %s", keepstateKeyArg, sanitize(p), "KeepState's key", "-", state))
	}
	return out
}

// ---- ks key default ----------------------------------------------------------------

func hostedKeyDefault(cr hostedCreds, inv *Invocation) {
	doc, err := fetchModelKeys(cr)
	if errors.Is(err, errModelKeysNotServed) {
		fail(&cliError{Code: exitFailed, Kind: "not_served", Message: "this service keeps no default key per provider: every session uses your own key. Nothing was changed", NextAction: "ks key list"})
	}
	if err != nil {
		die(err)
	}
	p, v := inv.Arg(0), inv.Arg(1)
	if p == "" {
		emit(map[string]any{"defaults": doc.Defaults, "revision": doc.Revision, "managed": doc.Managed}, func() { printKeyDefaults(doc) })
		return
	}
	want := "own"
	if v == keepstateKeyArg {
		want = "managed"
	}
	var env struct {
		Data modelKeysDoc `json:"data"`
	}
	if err := hostedMutate(cr, "PUT", modelKeysPath, map[string]any{"defaults": map[string]string{p: want}, "expected_revision": doc.Revision}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_revision_conflict" {
			fail(&cliError{Code: exitConflict, Kind: he.Type, HTTPStatus: he.Status, Message: "the defaults changed since they were read, so nothing changed; read them again and retry: " + sanitize(he.Message), NextAction: "ks key default"})
		}
		die(err)
	}
	nd := &env.Data
	emit(map[string]any{"defaults": nd.Defaults, "revision": nd.Revision, "managed": nd.Managed}, func() {
		on := "your own key"
		if nd.defaultFor(p) == "managed" {
			on = "KeepState's key"
		}
		fmt.Printf("new sessions start their %s calls on %s; existing sessions are unchanged (ks session key set changes one)\n", providerName(p), on)
		printKeyDefaults(nd)
	})
}

func printKeyDefaults(d *modelKeysDoc) {
	for _, p := range managedProviders {
		mp, offered := d.offered(p)
		var line string
		switch {
		case d.defaultFor(p) == "managed" && offered:
			line = fmt.Sprintf("%-11s %-16s %s, from purchased credit", p, "KeepState's key", spreadText(p, mp))
		case d.defaultFor(p) == "managed":
			line = fmt.Sprintf("%-11s %-16s not offered now: %s", p, "KeepState's key", d.notOfferedWhy(p))
		case offered:
			line = fmt.Sprintf("%-11s %-16s KeepState's key is offered too, at %s", p, "your own key", spreadText(p, mp))
		default:
			line = fmt.Sprintf("%-11s %s", p, "your own key")
		}
		fmt.Println(line)
	}
	if !d.Managed.Available {
		fmt.Println("KeepState's key is not offered now: " + firstNonEmptyStr(sanitize(d.Managed.Note), "the service does not offer it") + ". Your own key works as before.")
	}
}

// ---- ks session key set: your key or KeepState's key ------------------------------

// hostedSessionKeySet binds a session's provider to one of your keys by id,
// or to KeepState's key, and changes its spend limit on KeepState's key.
func hostedSessionKeySet(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: "this session has no workspace record to bind a key to yet", NextAction: "ks session show " + sess.ShortID})
	}
	keyArg := inv.Str("key")
	managed := keyArg == keepstateKeyArg
	var k customerKey
	if keyArg != "" && !managed {
		if k, err = resolveKey(cr, keyArg); err != nil {
			die(err)
		}
	}
	var rec struct {
		Data struct {
			Revision    int64               `json:"revision"`
			Harness     string              `json:"harness"`
			Managed     *[]string           `json:"managed"`
			KeyBindings *sessionKeyBindings `json:"key_bindings"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), nil, &rec); err != nil {
		die(err)
	}
	rev := rec.Data.Revision
	// the providers on KeepState's key now, when the record says (nil: a
	// record that predates the choice)
	current := rec.Data.Managed
	if rec.Data.KeyBindings != nil {
		current = &rec.Data.KeyBindings.Managed
	}
	facts := map[string]any{}
	var said []string
	var res map[string]any
	if keyArg != "" {
		body := map[string]any{"expected_revision": rev}
		provider := k.Provider
		var offer managedProvider
		if managed {
			provider = inv.Str("provider")
			if provider == "" {
				provider = harnessProvider(rec.Data.Harness)
			}
			if provider == "" {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: "this session runs " + sanitize(rec.Data.Harness) + "; name the provider whose calls go on KeepState's key with --provider. Nothing was changed", NextAction: "ks session key set " + sess.ShortID + " --key keepstate --provider openai"})
			}
			if row, listed := managedListed(cr); !listed {
				fail(&cliError{Code: exitConflict, Kind: "managed_unavailable", Message: "KeepState's key cannot be used with this service; " + managedRowWhy(row) + ". Nothing was changed", NextAction: "ks key list"})
			}
			doc, err := fetchModelKeys(cr)
			if err != nil {
				die(err)
			}
			mp, ok := doc.offered(provider)
			if !ok {
				fail(&cliError{Code: exitConflict, Kind: "managed_unavailable", Message: "KeepState's key is not offered for " + providerName(provider) + " now: " + doc.notOfferedWhy(provider) + ". Nothing was changed", NextAction: "ks key list"})
			}
			offer = mp
			// only managed is sent: the service keeps every other
			// provider's own key
			body["managed"] = append(withoutStr(current, provider), provider)
		} else {
			// keys replaces the own-key bindings: every other provider's
			// own key is sent as it is bound now
			keys := map[string]string{}
			if rec.Data.KeyBindings != nil {
				for p, id := range rec.Data.KeyBindings.Keys {
					keys[p] = id
				}
			}
			keys[k.Provider] = k.ID
			body["keys"] = keys
			if current != nil {
				// a session that knows KeepState's key: this provider leaves it
				body["managed"] = withoutStr(current, k.Provider)
			}
		}
		if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/bindings", body, &res); err != nil {
			die(sessionKeyRefusal(err, sess))
		}
		var whole *sessionKeyBindings
		if d, ok := res["data"].(map[string]any); ok {
			if n, ok := d["revision"].(float64); ok {
				rev = int64(n)
			}
			whole = bindingsOf(d)
		}
		facts["binding"] = res
		if managed {
			said = append(said, fmt.Sprintf("session %s: %s calls go on KeepState's key (%s, paid from your purchased credit) from the first call after the session next wakes", sess.ShortID, providerName(provider), spreadText(provider, offer)))
		} else {
			said = append(said, fmt.Sprintf("session %s: %s bound to %s (…%s)", sess.ShortID, k.Provider, k.ID, k.Last4))
		}
		if whole != nil {
			said = append(said, "  now: "+whole.words())
		}
	}
	if inv.Set("spend-limit") {
		micros, perr := parseDollars("spend-limit", inv.Str("spend-limit"))
		if perr != nil {
			fail(perr)
		}
		var patched map[string]any
		if err := hostedMutate(cr, "PATCH", "/api/v2/sessions/"+url.PathEscape(sess.RecordID), map[string]any{"spend_limit_microusd": micros, "expected_revision": rev}, &patched); err != nil {
			if keyArg != "" {
				fmt.Fprintln(os.Stderr, said[0]+"; the spend limit was not changed:")
			}
			die(sessionKeyRefusal(err, sess))
		}
		facts["limits"] = patched
		said = append(said, fmt.Sprintf("session %s: its spend limit on KeepState's key is %s", sess.ShortID, receiptMoney(micros)))
	}
	out := any(facts)
	if len(facts) == 1 && facts["binding"] != nil {
		out = res // the answer as it always was
	}
	emit(out, func() {
		for _, s := range said {
			fmt.Println(s)
		}
	})
}

// sessionKeyRefusal words a refusal of a binding or a limit change.
func sessionKeyRefusal(err error, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	switch he.Type {
	case "ks_session_not_paused":
		ce := classify(err)
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "pause the session first (ks agent pause <agent> --session " + sess.ShortID + "), then ks session key set again"
		return ce
	case "ks_revision_conflict":
		ce := classify(err)
		ce.Message = "the record changed since it was read, so nothing changed; read it again and retry: " + sanitize(he.Message)
		ce.NextAction = "ks session key set " + sess.ShortID
		return ce
	}
	return err
}

// sessionKeyBindings is what a session calls each provider with: one of your
// keys by id, or KeepState's key.
type sessionKeyBindings struct {
	Keys    map[string]string `json:"keys"`
	Managed []string          `json:"managed"`
}

// words is the binding per provider, as a person reads it.
func (b *sessionKeyBindings) words() string {
	seen := map[string]bool{}
	var ps []string
	for p := range b.Keys {
		ps, seen[p] = append(ps, p), true
	}
	for _, p := range b.Managed {
		if !seen[p] {
			ps, seen[p] = append(ps, p), true
		}
	}
	if len(ps) == 0 {
		return "no key bound"
	}
	sort.Strings(ps)
	var parts []string
	for _, p := range ps {
		if containsStr(b.Managed, p) {
			parts = append(parts, sanitize(p)+" on KeepState's key")
		} else {
			parts = append(parts, sanitize(p)+" on your key "+sanitize(b.Keys[p]))
		}
	}
	return strings.Join(parts, "; ")
}

// bindingsOf reads the whole binding from a bindings answer; nil from a
// service that answers only what it was sent.
func bindingsOf(d map[string]any) *sessionKeyBindings {
	m, ok := d["managed"].([]any)
	if !ok {
		return nil
	}
	b := &sessionKeyBindings{Keys: map[string]string{}}
	for _, x := range m {
		b.Managed = append(b.Managed, fmt.Sprint(x))
	}
	if ks, ok := d["keys"].(map[string]any); ok {
		for p, id := range ks {
			b.Keys[p] = fmt.Sprint(id)
		}
	}
	return b
}

// withoutStr is a list without one entry; nil reads as empty.
func withoutStr(xs *[]string, s string) []string {
	out := []string{}
	if xs == nil {
		return out
	}
	for _, x := range *xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
