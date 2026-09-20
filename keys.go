// keys.go: the provider-key inventory on the command line (KS-023 to
// KS-025). Every verb here reads or changes metadata: an id, a provider,
// an alias, the last four characters, whether the key is enabled. The
// secret itself enters once, through standard input on ks key add, and
// never appears in an argument, a log, an error or an answer. A session
// is bound to a key by its id (ks session key set), so a rotation or a
// second key of the same provider can never be substituted for it.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

type customerKey struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Alias     string `json:"alias"`
	Last4     string `json:"last4"`
	Enabled   bool   `json:"enabled"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func fetchKeys(cr hostedCreds) ([]customerKey, error) {
	var env struct {
		Data struct {
			Items []customerKey `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/keys", nil, &env); err != nil {
		return nil, err
	}
	return env.Data.Items, nil
}

// resolveKey answers the one key an argument names: its id, or its alias
// when exactly one key carries it.
func resolveKey(cr hostedCreds, arg string) (customerKey, error) {
	keys, err := fetchKeys(cr)
	if err != nil {
		return customerKey{}, err
	}
	var byAlias []customerKey
	for _, k := range keys {
		if k.ID == arg {
			return k, nil
		}
		if k.Alias != "" && k.Alias == arg {
			byAlias = append(byAlias, k)
		}
	}
	switch len(byAlias) {
	case 1:
		return byAlias[0], nil
	case 0:
		return customerKey{}, &cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("no key of yours is %q (an id or an alias)", arg), NextAction: "ks key list"}
	}
	return customerKey{}, &cliError{Code: exitUsage, Kind: "ambiguous", Message: fmt.Sprintf("%d keys carry the alias %q; give the id", len(byAlias), arg), NextAction: "ks key list"}
}

func keyLine(k customerKey) string {
	state := "enabled"
	if !k.Enabled {
		state = "disabled"
	}
	alias := k.Alias
	if alias == "" {
		alias = "-"
	}
	return fmt.Sprintf("%-14s %-11s %-18s %-6s %s", k.ID, k.Provider, alias, "…"+k.Last4, state)
}

func hostedKeyList(cr hostedCreds, inv *Invocation) {
	keys, err := fetchKeys(cr)
	if err != nil {
		die(err)
	}
	emit(map[string]any{"keys": keys, "count": len(keys)}, func() {
		if len(keys) == 0 {
			fmt.Println("No provider keys. Add one: ks key add --provider anthropic < key.txt")
			return
		}
		fmt.Printf("%-14s %-11s %-18s %-6s %s\n", "KEY", "PROVIDER", "ALIAS", "LAST4", "STATE")
		for _, k := range keys {
			fmt.Println(keyLine(k))
		}
	})
}

func hostedKeyShow(cr hostedCreds, inv *Invocation) {
	k, err := resolveKey(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	emit(k, func() {
		fmt.Println(keyLine(k))
		fmt.Printf("  created %s · updated %s · revision %d\n", k.CreatedAt, k.UpdatedAt, k.Revision)
	})
}

// hostedKeyAdd reads the secret from standard input, whole, and sends it
// once. Nothing about the secret is echoed, logged or kept.
func hostedKeyAdd(cr hostedCreds, inv *Invocation) {
	provider := inv.Str("provider")
	if provider == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--provider is required (anthropic, openai or openrouter)", NextAction: "ks key add --provider anthropic < key.txt"})
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && !out.noInput {
		fmt.Fprintln(os.Stderr, "paste the key, then Enter and Ctrl-D (it is not echoed and not kept):")
	}
	raw, err := io.ReadAll(io.LimitReader(bufio.NewReader(os.Stdin), 8192))
	if err != nil {
		die(err)
	}
	secret := strings.TrimSpace(string(raw))
	if len(secret) < 8 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "no key was read from standard input", NextAction: "ks key add --provider " + provider + " < key.txt"})
	}
	var env struct {
		Data customerKey `json:"data"`
	}
	body := map[string]any{"provider": provider, "secret": secret}
	if a := inv.Str("alias"); a != "" {
		body["alias"] = a
	}
	if err := hostedCall(cr, "POST", "/api/v2/keys", body, &env); err != nil {
		die(err)
	}
	k := env.Data
	emit(k, func() {
		fmt.Printf("stored: %s\n", keyLine(k))
		fmt.Println("bind it to a session: ks session key set <session> --key " + k.ID)
	})
}

func hostedKeySetEnabled(enabled bool) func(cr hostedCreds, inv *Invocation) {
	return func(cr hostedCreds, inv *Invocation) {
		k, err := resolveKey(cr, inv.Arg(0))
		if err != nil {
			die(err)
		}
		var env struct {
			Data customerKey `json:"data"`
		}
		if err := hostedMutate(cr, "PATCH", "/api/v2/keys/"+k.ID, map[string]any{"enabled": enabled, "expected_revision": k.Revision}, &env); err != nil {
			die(err)
		}
		emit(env.Data, func() { fmt.Println(keyLine(env.Data)) })
	}
}

func hostedKeyDelete(cr hostedCreds, inv *Invocation) {
	k, err := resolveKey(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var plan struct {
		Data struct {
			ID   string         `json:"id"`
			Plan map[string]any `json:"plan"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/keys/"+k.ID+"/deletion-plan", map[string]any{}, &plan); err != nil {
		die(err)
	}
	bound := 0
	if b, ok := plan.Data.Plan["bindings"].(map[string]any); ok {
		if ss, ok := b["sessions"].([]any); ok {
			bound = len(ss)
		}
	}
	if !out.yes {
		progress("plan %s: delete %s (%s …%s); %d session binding(s) would be dropped; confirm with --yes", plan.Data.ID, k.ID, k.Provider, k.Last4, bound)
		emit(map[string]any{"plan_id": plan.Data.ID, "key": k, "bindings_dropped": bound, "confirmed": false}, func() {
			fmt.Println("nothing deleted; run again with --yes to execute this plan")
		})
		return
	}
	var res map[string]any
	if err := hostedMutate(cr, "DELETE", "/api/v2/keys/"+k.ID+"?plan_id="+plan.Data.ID, nil, &res); err != nil {
		die(err)
	}
	emit(res, func() {
		fmt.Printf("deleted %s (%s …%s); %v binding(s) dropped\n", k.ID, k.Provider, k.Last4, figure(res["bindings_dropped"]))
	})
}

// hostedSessionKeySet binds a session's provider to one key by id.
func hostedSessionKeySet(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: "this session has no workspace record to bind a key to yet", NextAction: "ks session show " + sess.ShortID})
	}
	k, err := resolveKey(cr, inv.Str("key"))
	if err != nil {
		die(err)
	}
	var rec struct {
		Data struct {
			Revision int64 `json:"revision"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+sess.RecordID, nil, &rec); err != nil {
		die(err)
	}
	var res map[string]any
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+sess.RecordID+"/bindings", map[string]any{"keys": map[string]string{k.Provider: k.ID}, "expected_revision": rec.Data.Revision}, &res); err != nil {
		die(err)
	}
	emit(res, func() { fmt.Printf("session %s: %s bound to %s (…%s)\n", sess.ShortID, k.Provider, k.ID, k.Last4) })
}

// hostedPreflight is the run-readiness check: the service's facts and
// the local upload preview, no request that starts anything.
func hostedPreflight(cr hostedCreds, inv *Invocation) {
	body := map[string]any{}
	if p := inv.Str("provider"); p != "" {
		body["provider"] = p
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := hostedCall(cr, "POST", "/api/v2/preflight", body, &env); err != nil {
		die(err)
	}
	d := env.Data
	local := map[string]any{}
	if root, err := os.Getwd(); err == nil {
		if _, sel, err := scanSelected(root, nil, readSelectionOverrides(root)); err == nil {
			local = map[string]any{"files": sel.Files, "bytes": sel.Bytes, "excluded": len(sel.Excluded), "selection_digest": sel.Digest}
		} else {
			local = map[string]any{"error": err.Error()}
		}
	}
	emit(map[string]any{"service": d, "workspace": local}, func() {
		fmt.Printf("account %v (%v) · credit %s %v · registry %v\n", d["account_id"], d["cohort_state"], figure(d["credit_microusd"]), d["currency"], d["registry_version"])
		if keys, ok := d["keys"].([]any); ok {
			fmt.Printf("keys: %d\n", len(keys))
		}
		if b, ok := d["blockers"].([]any); ok && len(b) > 0 {
			for _, x := range b {
				fmt.Printf("blocker: %v\n", x)
			}
		}
		if n, ok := d["next_actions"].(map[string]any); ok {
			for k, v := range n {
				fmt.Printf("next (%s): %v\n", k, v)
			}
		}
		if e, ok := local["error"]; ok {
			fmt.Printf("workspace: %v\n", e)
		} else if f, ok := local["files"]; ok {
			fmt.Printf("workspace: %v files, %v bytes, %v excluded (ks cruise preview for the list)\n", f, local["bytes"], local["excluded"])
		}
		ready, _ := d["ready"].(bool)
		if ready {
			fmt.Println("ready for cruise jobs; agent mode is not available in this release")
		} else {
			fmt.Println("not ready; clear the blockers above")
		}
	})
}
