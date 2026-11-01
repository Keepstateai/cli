// The team verbs and the home team against a fake control plane: what each
// sends (a team named by id, path or a name only one team has; each change
// names the revision the service last answered), what each prints, and the
// refusals made before anything is sent.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type orgTeamsCtl struct {
	mu   sync.Mutex
	sent []string
	keys map[string]string
}

func (c *orgTeamsCtl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.sent = append(c.sent, r.Method+" "+r.URL.Path+" "+string(b))
	c.keys[r.Method+" "+r.URL.Path] = r.Header.Get("Idempotency-Key")
	w.Header().Set("Content-Type", "application/json")
	env := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 2, "request_id": "r", "data": data})
	}
	eng := map[string]any{"id": "oteam_eng", "name": "Engineering", "parent_team_id": "", "path": []string{"oteam_eng"}, "path_names": []string{"Engineering"}, "depth": 1, "revision": 3,
		"members": []any{map[string]any{"account_id": "acct_bob", "role": "lead", "home": true}}}
	plat := map[string]any{"id": "oteam_plat", "name": "Platform", "parent_team_id": "oteam_eng", "path": []string{"oteam_eng", "oteam_plat"}, "path_names": []string{"Engineering", "Platform"}, "depth": 2, "revision": 5, "members": []any{}}
	plat2 := map[string]any{"id": "oteam_plat2", "name": "Platform", "parent_team_id": "", "path": []string{"oteam_plat2"}, "path_names": []string{"Platform"}, "depth": 1, "revision": 1, "members": []any{}}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/teams":
		env(200, map[string]any{"items": []any{eng, plat, plat2}, "next_cursor": ""})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/teams":
		env(201, map[string]any{"id": "oteam_new", "name": "Infra", "parent_team_id": "oteam_plat", "path_names": []string{"Engineering", "Platform", "Infra"}, "depth": 3, "revision": 1, "members": []any{}})
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/orgs/org_a/teams/oteam_plat":
		env(200, plat)
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/teams/oteam_plat":
		env(200, map[string]any{"id": "oteam_plat", "deleted": true, "members_left": 2})
	case r.Method == "POST" && r.URL.Path == "/api/v2/orgs/org_a/teams/oteam_eng/members":
		env(200, eng)
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/orgs/org_a/teams/oteam_eng/members/acct_bob":
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_team_is_home","type":"ks_team_is_home","message":"this is their home team; choose another home team for them first; nothing was changed","retryable":false,"work_started":"no","next_action":""}}`))
	case r.URL.Path == "/api/v2/orgs/org_a/members":
		env(200, map[string]any{"items": []any{map[string]any{"account_id": "acct_bob", "role": "member", "seat_state": "assigned", "revision": 4}}, "next_cursor": ""})
	case r.Method == "PATCH" && r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob":
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		env(200, map[string]any{"account_id": "acct_bob", "role": "member", "seat_state": "assigned", "revision": 5, "home_team_id": p["home_team_id"]})
	case r.Method == "GET" && r.URL.Path == "/api/v2/orgs/org_a/members/acct_bob":
		env(200, map[string]any{"account_id": "acct_bob", "role": "member", "seat_state": "assigned", "revision": 4, "home_team_id": "oteam_eng",
			"teams": []any{map[string]any{"team_id": "oteam_eng", "name": "Engineering", "role": "lead", "home": true}}})
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"schema_version":2,"request_id":"r","error":{"code":"ks_not_found","type":"ks_not_found","message":"no such resource","retryable":false,"work_started":"no","next_action":""}}`))
	}
}

func (c *orgTeamsCtl) last(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.sent[i], prefix) {
			return c.sent[i]
		}
	}
	return ""
}

func TestOrganizationTeamVerbs(t *testing.T) {
	c := &orgTeamsCtl{keys: map[string]string{}}
	syncFixture(t, &c.mu)
	srv := httptest.NewServer(c)
	defer srv.Close()
	bin, cfg := buildAndAuth(t, srv)
	env := fastEnv(cfg)
	dir := t.TempDir()
	run := func(args ...string) (string, string, int) { return auditExec(t, bin, cfg, dir, env, args...) }
	count := func() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

	// list: the tree, with who is in each and their home team
	out, errs, code := run("team", "list", "--organization", "org_a")
	if code != 0 || !strings.Contains(out, "Engineering  oteam_eng") || !strings.Contains(out, "  Platform  oteam_plat") || !strings.Contains(out, "acct_bob (lead, home team)") {
		t.Fatalf("team list: %d\n%s%s", code, out, errs)
	}
	// create under a team named by its path; sent once under a replay key
	out, errs, code = run("team", "create", "Infra", "--parent", "Engineering/Platform", "--organization", "org_a")
	if code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/teams"), `"parent_team_id":"oteam_plat"`) || !strings.Contains(out, "Engineering/Platform/Infra") {
		t.Fatalf("team create: %d %q\n%s%s", code, c.last("POST /api/v2/orgs/org_a/teams"), out, errs)
	}
	if c.keys["POST /api/v2/orgs/org_a/teams"] == "" {
		t.Error("team create was sent without an idempotency key")
	}
	// a name two teams have is refused before anything is sent, naming both
	n := count()
	if _, errs, code := run("team", "rename", "Platform", "Tools", "--organization", "org_a"); code != exitUsage || !strings.Contains(errs, "2 teams are called") || count() != n+1 {
		t.Errorf("an ambiguous name: %d %s (%d requests)", code, errs, count()-n)
	}
	// rename and move by id and path, naming the revision read
	if out, _, code := run("team", "rename", "oteam_plat", "Tools", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/teams/oteam_plat"), `"expected_revision":5`) || !strings.Contains(c.last("PATCH"), `"name":"Tools"`) || !strings.Contains(out, "Renamed") {
		t.Errorf("team rename: %d %s %q", code, out, c.last("PATCH"))
	}
	if _, _, code := run("team", "move", "Engineering/Platform", "--top", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/teams/oteam_plat"), `"parent_team_id":""`) {
		t.Errorf("team move --top: %d %q", code, c.last("PATCH"))
	}
	if _, _, code := run("team", "move", "Engineering/Platform", "--parent", "oteam_plat2", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/teams/oteam_plat"), `"parent_team_id":"oteam_plat2"`) {
		t.Errorf("team move --parent: %d %q", code, c.last("PATCH"))
	}
	// delete
	if out, _, code := run("team", "delete", "Engineering/Platform", "--organization", "org_a"); code != 0 || !strings.Contains(out, "The 2 people in it stay in the organization") {
		t.Errorf("team delete: %d %s", code, out)
	}
	// add as lead; remove, refused with the service's words
	if _, _, code := run("team", "add", "Engineering", "acct_bob", "--lead", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("POST /api/v2/orgs/org_a/teams/oteam_eng/members"), `"role":"lead"`) {
		t.Errorf("team add --lead: %d %q", code, c.last("POST /api/v2/orgs/org_a/teams/oteam_eng/members"))
	}
	if _, errs, code := run("team", "remove", "Engineering", "acct_bob", "--organization", "org_a"); code == 0 || !strings.Contains(errs, "home team") {
		t.Errorf("team remove of a home team: %d %s", code, errs)
	}
	// the home team, through member set; and none
	if out, _, code := run("member", "set", "acct_bob", "--home-team", "Engineering", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"), `"home_team_id":"oteam_eng"`) || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"), `"expected_revision":4`) || !strings.Contains(out, "Home team oteam_eng") {
		t.Errorf("member set --home-team: %d %s %q", code, out, c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"))
	}
	if out, _, code := run("member", "set", "acct_bob", "--no-home-team", "--organization", "org_a"); code != 0 || !strings.Contains(c.last("PATCH /api/v2/orgs/org_a/members/acct_bob"), `"home_team_id":""`) || !strings.Contains(out, "No home team") {
		t.Errorf("member set --no-home-team: %d %s", code, out)
	}
	if out, _, code := run("member", "show", "acct_bob", "--organization", "org_a"); code != 0 || !strings.Contains(out, "team Engineering (lead, home team)") || !strings.Contains(out, "home team Engineering (oteam_eng)") {
		t.Errorf("member show: %d %s", code, out)
	}
	// refused before anything is sent
	for _, bad := range [][]string{
		{"team", "create", " Infra", "--organization", "org_a"},
		{"team", "list"},
		{"team", "move", "Engineering", "--organization", "org_a"},
		{"team", "move", "Engineering", "--top", "--parent", "Platform", "--organization", "org_a"},
		{"member", "set", "acct_bob", "--home-team", "Engineering", "--no-home-team", "--organization", "org_a"},
	} {
		n := count()
		if _, _, code := run(bad...); code != exitUsage || count() != n {
			t.Errorf("%v: exit %d, %d requests", bad, code, count()-n)
		}
	}
	// an unknown team is refused after reading the teams, before any change
	n = count()
	if _, errs, code := run("team", "delete", "Sales", "--organization", "org_a"); code != exitUsage || !strings.Contains(errs, `no team "Sales"`) || count() != n+1 {
		t.Errorf("an unknown team: %d %s", code, errs)
	}
}

func TestPickTeam(t *testing.T) {
	teams := []customerOrgTeam{
		{ID: "oteam_a", Name: "Eng", PathNames: []string{"Eng"}},
		{ID: "oteam_b", Name: "Ops", PathNames: []string{"Eng", "Ops"}},
		{ID: "oteam_c", Name: "Ops", PathNames: []string{"Ops"}},
	}
	for ref, want := range map[string]string{"oteam_b": "oteam_b", "eng/ops": "oteam_b", "/Ops/": "oteam_c", "Eng": "oteam_a", "Ops": "", "Nope": ""} {
		got, _ := pickTeam(teams, ref)
		id := ""
		if got != nil {
			id = got.ID
		}
		if id != want {
			t.Errorf("pickTeam(%q) = %q, want %q", ref, id, want)
		}
	}
}
