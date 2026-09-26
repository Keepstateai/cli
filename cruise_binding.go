// cruise_binding.go: an approval that binds exactly what will run (KS-074,
// the client's half; the service half is ctl/jobs_binding.go).
//
// ks cruise approve writes `binding_version: 1` into the draft BEFORE it is
// digested, so the approved bytes carry:
//
//	workspace.selection_digest  the upload-selection this client applied
//	verifier.inputs_digest      the pinned-file manifest of the check's
//	                            inputs (the judge's inputs_digest)
//	verifier.check              the explicit execution definition
//	routes                      [{provider, key_id, key_created_at}] for
//	                            exactly the providers the ladder uses
//
// ks cruise run recomputes each of them and sends NOTHING when the
// selection, the check's inputs or a route's key changed since approval; and
// it sends the approved canonical bytes themselves, checked against the lock.
package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const cruiseBindingVersion = 1

// checkRunnerOf answers the runner a draft's check is, from its explicit
// verifier.check or its command.
func checkRunnerOf(ver map[string]any) string {
	if c, ok := ver["check"].(map[string]any); ok {
		if r, _ := c["runner"].(string); r != "" {
			return r
		}
	}
	cmd, _ := ver["command"].(string)
	switch {
	case ks072LegacyPytest.MatchString(cmd):
		return "pytest"
	case strings.HasPrefix(cmd, "go test"):
		return "go"
	case strings.HasPrefix(cmd, "npm ") || strings.HasPrefix(cmd, "node "):
		return "node"
	}
	return ""
}

// bindingInputs recomputes the check's pinned-inputs digest over the files
// as they are now.
func bindingInputs(files []wsFile, runner string) (string, []pinnedInput, error) {
	shas := map[string]string{}
	for _, f := range files {
		shas[f.rel] = fmt.Sprintf("%x", f.sha)
	}
	d := ks072Discover(shas, nil)
	eco := ks072RunnerEco[runner]
	if eco == "" || d.Ecosystems[eco] == nil {
		return "", nil, fmt.Errorf("the check's %s tests are no longer in the workspace", figure(runner))
	}
	rows := ks072InputsOf(d, eco)
	return ks072InputsDigest(rows), rows, nil
}

// bindingProviders is the set of providers the draft's ladder uses.
func bindingProviders(m map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	if l, ok := m["ladder"].([]any); ok {
		for _, x := range l {
			r, _ := x.(map[string]any)
			fam, _ := r["family"].(string)
			p := embeddedModels.Families[fam].Provider
			if p == "" {
				p = fam
			}
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// chooseRoutes binds one enabled key per provider: the one named with
// --key provider=KEY, or the only enabled one; several and none named is
// refused with the choices, never guessed.
func chooseRoutes(keys []customerKey, providers []string, named map[string]string) ([]any, error) {
	var routes []any
	var problems []string
	for _, p := range providers {
		var enabled []customerKey
		for _, k := range keys {
			if k.Provider == p && k.Enabled {
				enabled = append(enabled, k)
			}
		}
		var pick *customerKey
		if want := named[p]; want != "" {
			for i := range enabled {
				if enabled[i].ID == want || (enabled[i].Alias != "" && enabled[i].Alias == want) {
					pick = &enabled[i]
				}
			}
			if pick == nil {
				problems = append(problems, fmt.Sprintf("--key %s=%s names no enabled %s key", p, want, p))
				continue
			}
		} else {
			switch len(enabled) {
			case 0:
				problems = append(problems, fmt.Sprintf("no enabled %s key: add one (ks key add --provider %s)", p, p))
				continue
			case 1:
				pick = &enabled[0]
			default:
				var ids []string
				for _, k := range enabled {
					ids = append(ids, k.ID)
				}
				problems = append(problems, fmt.Sprintf("%d enabled %s keys (%s): choose one with --key %s=<key>", len(enabled), p, strings.Join(ids, ", "), p))
				continue
			}
		}
		routes = append(routes, map[string]any{"provider": p, "key_id": pick.ID, "key_created_at": pick.CreatedAt})
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return routes, nil
}

// routeChanges names every bound route whose key is no longer the one
// approved: gone, on another provider, disabled, or rotated.
func routeChanges(keys []customerKey, routes []any) []string {
	var out []string
	for _, x := range routes {
		r, _ := x.(map[string]any)
		p, _ := r["provider"].(string)
		id, _ := r["key_id"].(string)
		at, _ := r["key_created_at"].(string)
		var k *customerKey
		for i := range keys {
			if keys[i].ID == id {
				k = &keys[i]
			}
		}
		switch {
		case k == nil:
			out = append(out, fmt.Sprintf("the %s key the approval names (%s) no longer exists", p, id))
		case k.Provider != p:
			out = append(out, fmt.Sprintf("key %s is a %s key, not the %s key approved", id, k.Provider, p))
		case !k.Enabled:
			out = append(out, fmt.Sprintf("the %s key %s is disabled", p, id))
		case k.CreatedAt != at:
			out = append(out, fmt.Sprintf("the %s key %s was replaced after approval (approved version %s, now %s)", p, id, at, k.CreatedAt))
		}
	}
	return out
}

func parseKeyFlags(list []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range list {
		p, k, ok := strings.Cut(s, "=")
		if !ok || p == "" || k == "" {
			return nil, fmt.Errorf("--key is provider=KEY (got %q)", s)
		}
		out[p] = k
	}
	return out, nil
}
