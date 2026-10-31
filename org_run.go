// org_run.go: the session verbs and ks run in an organization's context. With
// a context set (ks context use), the sessions these verbs list and resolve
// are the organization's session records you may read, and ks run makes the
// organization's session with the organization's key: your own keys and
// credit are never used for it.
package main

import (
	"fmt"
	"net/url"
	"strings"
)

// orgInventory reads the organization's session records as inventory rows,
// so every verb that resolves a session resolves among them.
func orgInventory(cr hostedCreds, org, state string) ([]inventoryRow, error) {
	var out []inventoryRow
	cursor := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"context": {org}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var env struct {
			Data struct {
				Items []struct {
					ID             string `json:"id"`
					Name           string `json:"name"`
					RuntimeState   string `json:"runtime_state"`
					Revision       int64  `json:"revision"`
					CreatedAt      string `json:"created_at"`
					UpdatedAt      string `json:"updated_at"`
					PrimaryAgentID string `json:"primary_agent_id"`
					ExecutionEpoch int64  `json:"execution_epoch"`
				} `json:"items"`
				NextCursor string `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", "/api/v2/sessions?"+q.Encode(), nil, &env); err != nil {
			return nil, orgErr(err, "nothing was read")
		}
		for _, x := range env.Data.Items {
			if state != "" && x.RuntimeState != state {
				continue
			}
			short := x.ID
			if len(short) > 20 {
				short = short[:20]
			}
			out = append(out, inventoryRow{ID: x.ID, ShortID: short, Name: x.Name, RuntimeState: x.RuntimeState, Revision: x.Revision,
				CreatedAt: x.CreatedAt, LastActivityAt: x.UpdatedAt, RecordID: x.ID, PrimaryAgentID: x.PrimaryAgentID, ExecutionEpoch: x.ExecutionEpoch,
				RecordState: x.RuntimeState})
		}
		cursor = env.Data.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// orgRunKey is the organization's key a run binds: the one named, or its one
// enabled key for the runner's provider, read from the organization's own
// preflight. A key of yours is never used for an organization's session.
func orgRunKey(pf map[string]any, org, keyArg, provider string) (customerKey, error) {
	var usable []customerKey
	keys, _ := pf["keys"].([]any)
	for _, x := range keys {
		m, _ := x.(map[string]any)
		k := customerKey{ID: fmt.Sprint(m["id"]), Provider: fmt.Sprint(m["provider"]), Last4: fmt.Sprint(m["last4"])}
		k.Enabled, _ = m["enabled"].(bool)
		if keyArg != "" && keyArg != ownKeyArg && k.ID == keyArg {
			return k, nil
		}
		if k.Provider == provider && k.Enabled {
			usable = append(usable, k)
		}
	}
	if keyArg != "" && keyArg != ownKeyArg {
		return customerKey{}, &cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("%s is not a key of the organization %s: an organization's session calls only the organization's own keys, never yours. Nothing was created", keyArg, org)}
	}
	switch len(usable) {
	case 1:
		return usable[0], nil
	case 0:
		return customerKey{}, &cliError{Code: exitConflict, Kind: "organization_no_key",
			Message:    fmt.Sprintf("the organization %s has no enabled %s key, and your own keys are never used for its sessions. Its owners and admins add the organization's own keys in its context (ks key add --provider %s). Nothing was created", org, provider, provider),
			NextAction: "ks key add --provider " + provider}
	}
	var ids []string
	for _, k := range usable {
		ids = append(ids, k.ID)
	}
	return customerKey{}, &cliError{Code: exitUsage, Kind: "key_ambiguous", Message: fmt.Sprintf("the organization %s has %d enabled %s keys (%s); name the one this agent calls with. Nothing was created", org, len(usable), provider, strings.Join(ids, ", ")),
		NextAction: "ks run --agent --key <id>"}
}
