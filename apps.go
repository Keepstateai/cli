package main

// apps.go: the apps connected to your account (an app such as claude.ai that
// you allowed to use KeepState for you). ks apps list shows them and
// ks apps disconnect ends one at once: its sign-in, every token and every
// request it had asked you to confirm. The word is "apps": "connections" is
// the adviser connections between your agents.

import (
	"fmt"
	"net/url"
	"strings"
)

type connectedApp struct {
	ID           string   `json:"id"`
	Client       string   `json:"client"`
	Verification string   `json:"verification"`
	Verified     bool     `json:"verified"`
	SendsCodesTo string   `json:"sends_codes_to"`
	Context      string   `json:"context"`
	ContextName  string   `json:"context_name"`
	Scopes       []string `json:"scopes"`
	ScopeWords   []string `json:"scope_words"`
	ConnectedAt  string   `json:"connected_at"`
	LastUsedAt   string   `json:"last_used_at"`
}

func hostedAppsList(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data struct {
			Items []connectedApp `json:"items"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/connected-apps", nil, &env); err != nil {
		die(err)
	}
	apps := env.Data.Items
	emit(map[string]any{"apps": apps, "count": len(apps)}, func() {
		o, _ := cmdUI()
		o.List(appsListScreen(apps))
	})
}

// appsListScreen is ks apps list: the apps, then what each may do.
func appsListScreen(apps []connectedApp) listScreen {
	s := listScreen{Title: "Apps connected to your account", Detail: listCount(len(apps), "app", "apps"),
		Cols: []Col{{Title: "App"}, {Title: "Acts in"}, {Title: "Last used"}, {Title: "Verified"}, {Title: "Id", Second: true}}}
	for _, a := range apps {
		s.Rows = append(s.Rows, []string{sanitize(a.Client), sanitize(a.ContextName), firstNonEmpty(firstN10(a.LastUsedAt), "never"), sanitize(a.Verification), a.ID})
	}
	if len(apps) == 0 {
		s.Empty = "No app is connected to your account."
		return s
	}
	s.Groups = []listGroup{{Rows: s.Rows}}
	for _, a := range apps {
		g := listGroup{Heading: sanitize(a.Client) + " (" + a.ID + ")", Lead: []string{"Sends its sign-in codes to " + sanitize(a.SendsCodesTo) + " and may:"}}
		for _, w := range a.ScopeWords {
			g.Lead = append(g.Lead, "- "+sanitize(w))
		}
		s.Groups = append(s.Groups, g)
	}
	s.Next = []string{"ks apps disconnect <id>"}
	return s
}

func firstN10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func hostedAppsDisconnect(cr hostedCreds, inv *Invocation) {
	id := strings.TrimSpace(inv.Arg(0))
	var env struct {
		Data connectedApp `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", "/api/v2/connected-apps/"+url.PathEscape(id), nil, &env); err != nil {
		die(err)
	}
	emit(map[string]any{"disconnected": id, "app": env.Data.Client,
		"stops": "the app's sign-in, every token it holds and every request it had asked you to confirm, at once"}, func() {
		fmt.Printf("disconnected %s (%s)\n", id, sanitize(env.Data.Client))
		fmt.Println("stops: the app's sign-in, every token it holds and every request it had asked you to confirm, at once")
	})
}
