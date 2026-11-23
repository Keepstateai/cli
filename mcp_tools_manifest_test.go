// mcp_tools_manifest_test: mcp-tools.json is the machine-readable statement
// of the tools ks mcp serves, written from the mcpTools table. The hosted
// KeepState MCP (a remote server run by the service) serves the same tools,
// so it vendors this file and holds its own table to it: a tool's name, title,
// description, properties, kind and destructive and human flags are decided
// here and nowhere else. This test is the guard that the file was
// regenerated: go test -run TestMCPToolsManifestIsCurrent -update
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateMCPTools = flag.Bool("update", false, "rewrite mcp-tools.json from the mcpTools table")

type mcpToolsManifestProp struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
	Required    bool     `json:"required"`
}

type mcpToolsManifestTool struct {
	Name        string                 `json:"name"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	Kind        string                 `json:"kind"`
	ReadOnly    bool                   `json:"read_only"`
	Destructive bool                   `json:"destructive"`
	Human       bool                   `json:"human"`
	Verb        []string               `json:"verb"`
	TimeoutArg  string                 `json:"timeout_arg,omitempty"`
	Properties  []mcpToolsManifestProp `json:"properties"`
}

func mcpToolsManifestBytes() []byte {
	var tools []mcpToolsManifestTool
	for _, t := range mcpTools {
		row := mcpToolsManifestTool{Name: t.Name, Title: t.Title, Description: t.Desc, Kind: t.Kind, ReadOnly: t.Kind == "read",
			Destructive: t.Destructive, Human: t.Human, Verb: t.Verb, TimeoutArg: t.TimeoutArg, Properties: []mcpToolsManifestProp{}}
		for _, p := range t.Props {
			row.Properties = append(row.Properties, mcpToolsManifestProp{Name: p.Name, Type: p.Type, Description: p.Desc, Enum: p.Enum, Required: p.Required})
		}
		tools = append(tools, row)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	_ = enc.Encode(map[string]any{"manifest_version": 1, "tools": tools})
	return buf.Bytes()
}

func TestMCPToolsManifestIsCurrent(t *testing.T) {
	want := mcpToolsManifestBytes()
	if *updateMCPTools {
		if err := os.WriteFile("mcp-tools.json", want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile("mcp-tools.json")
	if err != nil {
		t.Fatalf("mcp-tools.json: %v (go test -run TestMCPToolsManifestIsCurrent -update writes it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("mcp-tools.json is out of date with the mcpTools table; regenerate it: go test -run TestMCPToolsManifestIsCurrent -update")
	}
}

// the manifest is the contract the hosted server is held to, so it must say
// what the hosted server's directory and parity checks read
func TestMCPToolsManifestShape(t *testing.T) {
	var m struct {
		Version int                    `json:"manifest_version"`
		Tools   []mcpToolsManifestTool `json:"tools"`
	}
	raw, err := os.ReadFile("mcp-tools.json")
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Version != 1 || len(m.Tools) == 0 {
		t.Fatalf("mcp-tools.json: %v", err)
	}
	seen := map[string]bool{}
	for _, tl := range m.Tools {
		if seen[tl.Name] || len(tl.Name) > 64 || !strings.HasPrefix(tl.Name, "ks_") {
			t.Errorf("tool name %q: duplicate, over 64 characters or not ks_", tl.Name)
		}
		seen[tl.Name] = true
		if tl.Title == "" || tl.Description == "" || len(tl.Verb) == 0 {
			t.Errorf("%s: a tool needs a title, a description and a verb", tl.Name)
		}
		if tl.ReadOnly != (tl.Kind == "read") {
			t.Errorf("%s: read_only disagrees with its kind", tl.Name)
		}
		if tl.Human && tl.Kind != "confirm" {
			t.Errorf("%s: only a confirm tool is human", tl.Name)
		}
		for _, p := range tl.Properties {
			l := strings.ToLower(p.Name)
			for _, bad := range []string{"key", "token", "secret", "password"} {
				if strings.Contains(l, bad) {
					t.Errorf("%s: property %q looks like a credential field", tl.Name, p.Name)
				}
			}
		}
		if strings.Contains(tl.Description, "—") {
			t.Errorf("%s: an em dash in a description", tl.Name)
		}
	}
}
