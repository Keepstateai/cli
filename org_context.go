// org_context.go: which organization this client acts in (the context), and
// an organization's session records. The context is a local setting of this
// client, per control plane, never stored on the service; it is always
// shown, never inferred. In this release `ks organization sessions` reads
// it; the verbs that run work read it as the service admits organization
// work into them.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type customerOrgSession struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	RuntimeState string  `json:"runtime_state"`
	ProjectID    *string `json:"project_id"`
	OrgID        string  `json:"org_id"`
	CreatedBy    string  `json:"created_by"`
	CreatedAt    string  `json:"created_at"`
	Transferred  string  `json:"transferred_from,omitempty"`
}

func contextPath() string { return filepath.Join(configDir(), "context.json") }

// readContexts is the stored context per control plane; a missing or
// unreadable file is no context anywhere: personal.
func readContexts() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(contextPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func currentContext(cr hostedCreds) string {
	return readContexts()[strings.TrimRight(cr.CTL, "/")]
}

func writeContext(cr hostedCreds, org string) error {
	all := readContexts()
	key := strings.TrimRight(cr.CTL, "/")
	if org == "" {
		delete(all, key)
	} else {
		all[key] = org
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(all, "", " ")
	tmp := contextPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, contextPath())
}

func hostedContextUse(cr hostedCreds, inv *Invocation) {
	target := strings.TrimSpace(inv.Arg(0))
	if target == "personal" {
		if err := writeContext(cr, ""); err != nil {
			die(err)
		}
		emit(map[string]any{"context": "personal"}, func() { fmt.Println("Context: personal (your own account).") })
		return
	}
	if !strings.HasPrefix(target, "org_") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the context is personal or an organization's id (ks organization list shows yours); nothing was changed", NextAction: "ks organization list"})
	}
	var env struct {
		Data customerOrg `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/orgs/"+url.PathEscape(target), nil, &env); err != nil {
		die(orgErr(err, "nothing was changed"))
	}
	if err := writeContext(cr, target); err != nil {
		die(err)
	}
	o := env.Data
	emit(map[string]any{"context": o.ID, "organization": o}, func() {
		fmt.Printf("Context: %s (%s), as %s; %s.\n", o.Name, o.ID, o.YourRole, orgYourSeatText(o.YourSeat))
	})
}

func hostedContextShow(cr hostedCreds, inv *Invocation) {
	c := currentContext(cr)
	if c == "" {
		emit(map[string]any{"context": "personal"}, func() { fmt.Println("Context: personal (your own account).") })
		return
	}
	emit(map[string]any{"context": c}, func() { fmt.Printf("Context: organization %s (ks context use personal to leave it).\n", c) })
}

func hostedOrgSessions(cr hostedCreds, inv *Invocation) {
	org := strings.TrimSpace(inv.Arg(0))
	if org == "" {
		org = currentContext(cr)
	}
	if org == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the organization, or choose it first: ks context use ORGANIZATION; nothing was read", NextAction: "ks organization list"})
	}
	var all []customerOrgSession
	cursor, complete := "", false
	for page := 0; page < orgPagesMax; page++ {
		q := url.Values{"context": {org}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var env struct {
			Data struct {
				Items      []customerOrgSession `json:"items"`
				NextCursor string               `json:"next_cursor"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", "/api/v2/sessions?"+q.Encode(), nil, &env); err != nil {
			die(orgErr(err, "nothing was read"))
		}
		all = append(all, env.Data.Items...)
		if env.Data.NextCursor == "" {
			complete = true
			break
		}
		cursor = env.Data.NextCursor
	}
	emit(map[string]any{"organization": org, "sessions": all, "count": len(all), "complete": complete}, func() {
		fmt.Printf("Context: organization %s\n", org)
		if len(all) == 0 {
			fmt.Println("No organization sessions you may read.")
			return
		}
		fmt.Printf("%-33s %-20s %-14s %s\n", "SESSION", "NAME", "STATE", "MADE BY")
		for _, s := range all {
			by := s.CreatedBy
			if s.Transferred != "" {
				by += " (transferred from " + s.Transferred + ")"
			}
			fmt.Printf("%-33s %-20s %-14s %s\n", s.ID, clip(s.Name, 20), s.RuntimeState, by)
		}
		if !complete {
			fmt.Printf("Stopped after %d pages; the list continues on the service.\n", orgPagesMax)
		}
	})
}
