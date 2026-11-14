// import_mcp.go: ks_import_session, the confirm tool that imports a
// conversation. The first call runs `ks import <tool> --plan --json` (nothing
// is created), and its digest from the service is what the confirm value is
// bound to; the confirmed call runs `--execute <digest>` for the same
// arguments, so what runs is exactly what the person was shown.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// importVerbOf is the verb for these arguments: import, the tool, and which
// session (the newest by default, or one by id; Cursor's is a chat).
func importVerbOf(a map[string]any) []string {
	s := strings.TrimSpace(strArg(a, "session"))
	flag := "--session"
	if strArg(a, "harness") == "cursor" {
		flag = "--chat"
	}
	v := []string{"import", strArg(a, "harness")}
	if s == "" || s == "latest" {
		return append(v, "--latest")
	}
	return append(v, flag+"="+s)
}

// importTargetArgv is the plan's command line: the same verb and flags, and
// --plan, which creates nothing.
func importTargetArgv(a map[string]any) []string {
	out := append(importVerbOf(a), "--json", "--no-input")
	for _, f := range [][2]string{{"project", "project"}, {"name", "name"}, {"model", "model"}} {
		if v := strArg(a, f[0]); v != "" {
			out = append(out, "--"+f[1]+"="+v)
		}
	}
	for _, f := range [][2]string{{"with_files", "with-files"}, {"brief", "brief"}} {
		if v, _ := a[f[0]].(bool); v {
			out = append(out, "--"+f[1])
		}
	}
	return append(out, "--plan")
}

var importDigests struct {
	sync.Mutex
	m map[string]string
}

func importKey(a map[string]any) string { return canonicalArgs(a) }

// importInspect turns the plan's answer into the words a person confirms, and
// binds the confirm value to the service's digest.
func importInspect(s *mcpServer, a map[string]any, doc []byte) (string, string, string) {
	var env struct {
		Data struct {
			Digest string   `json:"digest"`
			Mode   string   `json:"mode"`
			Lines  []string `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(doc, &env); err != nil || len(env.Data.Digest) != 64 {
		return "", "", "The import could not be planned (the plan carried no digest), so nothing was created."
	}
	importDigests.Lock()
	if importDigests.m == nil {
		importDigests.m = map[string]string{}
	}
	importDigests.m[importKey(a)] = env.Data.Digest
	importDigests.Unlock()
	what := "its conversation"
	if env.Data.Mode == "brief" {
		what = "a written summary of it"
	}
	plan := fmt.Sprintf("Import %s from this machine into a NEW KeepState agent session; %s goes up. Plan (digest %s): %s Confirming creates the session and uploads; no instruction is sent, so no model is called by the import.",
		strArg(a, "harness"), what, env.Data.Digest[:12], strings.Join(env.Data.Lines, " "))
	return "import=" + env.Data.Digest, sanitize(plan), ""
}

func importExtraOnConfirm(a map[string]any) []string {
	importDigests.Lock()
	d := importDigests.m[importKey(a)]
	importDigests.Unlock()
	return []string{"--execute=" + d}
}
