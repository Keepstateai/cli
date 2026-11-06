// The listing files for ks mcp: server.json for the MCP Registry, the
// mcpName the registry checks in the npm package, the Claude Code plugin in
// plugins/keepstate and the marketplace that offers it. Each is read from
// the repository as published, so a listing can not drift from the client
// it lists: the registry's own schema (vendored with its SOURCE.json), the
// npm package's name and mcpName, the verb the server runs, a title on every
// tool tools/list answers, and the plugin directory's file rules.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const pluginDir = "plugins/keepstate"

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}
	return m
}

// --- a JSON Schema (draft-07) validator for the keywords the registry's
// schema uses; "format" and the annotations are not assertions.

type jsonSchema struct{ root map[string]any }

func (s jsonSchema) resolve(n map[string]any) map[string]any {
	for {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		cur := any(s.root)
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = cur.(map[string]any)[part]
		}
		n = cur.(map[string]any)
	}
}

func jsonTypeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func (s jsonSchema) validate(n map[string]any, v any, at string) []string {
	n = s.resolve(n)
	var errs []string
	if ty, ok := n["type"].(string); ok {
		got := jsonTypeOf(v)
		if !(got == ty || (ty == "number" && got == "integer")) {
			return []string{fmt.Sprintf("%s: is %s, the schema wants %s", at, got, ty)}
		}
	}
	if c, ok := n["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		errs = append(errs, fmt.Sprintf("%s: is not %v", at, c))
	}
	if en, ok := n["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", at, v, en))
		}
	}
	if str, ok := v.(string); ok {
		if p, ok := n["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			errs = append(errs, fmt.Sprintf("%s: %q does not match %s", at, str, p))
		}
		if m, ok := n["maxLength"].(float64); ok && len([]rune(str)) > int(m) {
			errs = append(errs, fmt.Sprintf("%s: %d characters, at most %d", at, len([]rune(str)), int(m)))
		}
		if m, ok := n["minLength"].(float64); ok && len([]rune(str)) < int(m) {
			errs = append(errs, fmt.Sprintf("%s: %d characters, at least %d", at, len([]rune(str)), int(m)))
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, r := range n["required"].([]any) {
			if _, ok := obj[r.(string)]; !ok {
				errs = append(errs, fmt.Sprintf("%s: %s is required", at, r))
			}
		}
		props, _ := n["properties"].(map[string]any)
		for k, val := range obj {
			if ps, ok := props[k].(map[string]any); ok {
				errs = append(errs, s.validate(ps, val, at+"."+k)...)
			} else if ap, ok := n["additionalProperties"]; ok {
				if b, ok := ap.(bool); ok && !b {
					errs = append(errs, fmt.Sprintf("%s: %s is not allowed", at, k))
				} else if apm, ok := ap.(map[string]any); ok {
					errs = append(errs, s.validate(apm, val, at+"."+k)...)
				}
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if it, ok := n["items"].(map[string]any); ok {
			for i, e := range arr {
				errs = append(errs, s.validate(it, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	if all, ok := n["allOf"].([]any); ok {
		for _, sub := range all {
			errs = append(errs, s.validate(sub.(map[string]any), v, at)...)
		}
	}
	if anyOf, ok := n["anyOf"].([]any); ok {
		pass := false
		for _, sub := range anyOf {
			if len(s.validate(sub.(map[string]any), v, at)) == 0 {
				pass = true
				break
			}
		}
		if !pass {
			errs = append(errs, fmt.Sprintf("%s: matches none of anyOf", at))
		}
	}
	if not, ok := n["not"].(map[string]any); ok && len(s.validate(not, v, at)) == 0 {
		errs = append(errs, fmt.Sprintf("%s: matches what the schema forbids", at))
	}
	return errs
}

func (n jsonSchema) check(v any) []string { return n.validate(n.root, v, "server.json") }

func registrySchema(t *testing.T) (jsonSchema, string) {
	t.Helper()
	src := readJSONFile(t, "testdata/mcp-registry/SOURCE.json")
	files := src["files"].(map[string]any)
	row := files["testdata/mcp-registry/server.schema.json"].(map[string]any)
	b, err := os.ReadFile("testdata/mcp-registry/server.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != row["sha256"] {
		t.Fatalf("the vendored registry schema does not hash to what SOURCE.json records")
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	// every "required" is read as a list; normalise the absent ones
	var norm func(any)
	norm = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			if _, ok := y["required"]; !ok {
				y["required"] = []any{}
			}
			for k, z := range y {
				if k != "required" && k != "enum" && k != "examples" && k != "example" && k != "default" && k != "const" {
					norm(z)
				}
			}
		case []any:
			for _, z := range y {
				norm(z)
			}
		}
	}
	norm(root)
	return jsonSchema{root: root}, src["url"].(string)
}

// newestReleased is the newest "## vX.Y.Z" heading of docs/changelog.md.
func newestReleased(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("docs/changelog.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+) `).FindSubmatch(b)
	if m == nil {
		t.Fatal("docs/changelog.md names no released version")
	}
	return string(m[1])
}

func releasedVersions(t *testing.T) map[string]bool {
	t.Helper()
	b, _ := os.ReadFile("docs/changelog.md")
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+) `).FindAllSubmatch(b, -1) {
		out[string(m[1])] = true
	}
	return out
}

func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

var exactSemver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func TestMCPListingServerJSON(t *testing.T) {
	schema, schemaURL := registrySchema(t)
	sj := readJSONFile(t, "server.json")
	if errs := schema.check(sj); len(errs) > 0 {
		t.Fatalf("server.json fails the registry's schema:\n%s", strings.Join(errs, "\n"))
	}
	if sj["$schema"] != schemaURL {
		t.Errorf("server.json names the schema %v, not the vendored %s", sj["$schema"], schemaURL)
	}
	npm := readJSONFile(t, "npm/package.json")
	if sj["name"] != npm["mcpName"] || sj["name"] == nil {
		t.Errorf("the registry checks that the npm package's mcpName (%v) is the server's name (%v)", npm["mcpName"], sj["name"])
	}
	if title, _ := sj["title"].(string); title == "" {
		t.Error("server.json has no title")
	}
	pkgs, _ := sj["packages"].([]any)
	if len(pkgs) != 1 {
		t.Fatalf("server.json lists %d packages, want the one npm package", len(pkgs))
	}
	p := pkgs[0].(map[string]any)
	if p["registryType"] != "npm" || p["identifier"] != npm["name"] || p["registryBaseUrl"] != "https://registry.npmjs.org" {
		t.Errorf("the package is not the npm package %v on the public registry: %v", npm["name"], p)
	}
	if tr := p["transport"].(map[string]any); tr["type"] != "stdio" || len(tr) != 1 {
		t.Errorf("the transport is not stdio: %v", tr)
	}
	args, _ := json.Marshal(p["packageArguments"])
	if string(args) != `[{"type":"positional","value":"mcp"}]` {
		t.Errorf("the package's argument is not the one verb mcp: %s", args)
	}
	if _, ok := p["environmentVariables"]; ok {
		t.Error("server.json asks for an environment variable; ks mcp signs in with ks login and takes no key")
	}
	man, err := os.ReadFile("commands.json")
	if err != nil || !bytes.Contains(man, []byte(`"usage": "ks mcp [--read-only]"`)) {
		t.Error("commands.json has no ks mcp verb for the listing to run")
	}
	v, _ := sj["version"].(string)
	if !exactSemver.MatchString(v) || p["version"] != v {
		t.Errorf("server.json's version %q and its package's version %v must be one exact version", v, p["version"])
	}
	if nr := newestReleased(t); semverLess(v, nr) {
		t.Errorf("server.json names version %s, older than the newest release %s", v, nr)
	}
	repo := sj["repository"].(map[string]any)
	npmRepo := npm["repository"].(map[string]any)["url"].(string)
	if want := strings.TrimSuffix(strings.TrimPrefix(npmRepo, "git+"), ".git"); repo["url"] != want || repo["source"] != "github" {
		t.Errorf("server.json's repository %v is not the npm package's %s", repo["url"], want)
	}
	for _, ic := range sj["icons"].([]any) {
		src := ic.(map[string]any)["src"].(string)
		if !strings.HasPrefix(src, "https://") {
			t.Errorf("icon %s is not an https URL", src)
		}
		if strings.HasPrefix(src, "https://raw.githubusercontent.com/Keepstateai/cli/main/") {
			if _, err := os.Stat(strings.TrimPrefix(src, "https://raw.githubusercontent.com/Keepstateai/cli/main/")); err != nil {
				t.Errorf("icon %s names a file this repository does not have", src)
			}
		}
	}
}

// The validator itself refuses what the registry refuses, so a pass above
// is a pass of the schema and not of a validator that accepts anything.
func TestMCPListingSchemaRefusesWhatTheRegistryRefuses(t *testing.T) {
	schema, _ := registrySchema(t)
	good := readJSONFile(t, "server.json")
	cases := map[string]func(m map[string]any){
		"a description over 100 characters": func(m map[string]any) { m["description"] = strings.Repeat("x", 101) },
		"a name without its namespace":      func(m map[string]any) { m["name"] = "ks" },
		"no version":                        func(m map[string]any) { delete(m, "version") },
		"a package at version latest": func(m map[string]any) {
			m["packages"].([]any)[0].(map[string]any)["version"] = "latest"
		},
		"a transport the schema does not know": func(m map[string]any) {
			m["packages"].([]any)[0].(map[string]any)["transport"] = map[string]any{"type": "carrier-pigeon"}
		},
		"an argument with neither value nor valueHint": func(m map[string]any) {
			m["packages"].([]any)[0].(map[string]any)["packageArguments"] = []any{map[string]any{"type": "positional"}}
		},
		"an icon size that is not WxH": func(m map[string]any) {
			m["icons"].([]any)[0].(map[string]any)["sizes"] = []any{"big"}
		},
	}
	for name, mutate := range cases {
		b, _ := json.Marshal(good)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mutate(m)
		if len(schema.check(m)) == 0 {
			t.Errorf("the schema check accepted %s", name)
		}
	}
}

func TestMCPListingEveryToolHasATitle(t *testing.T) {
	seen := map[string]string{}
	for _, tool := range mcpTools {
		if strings.TrimSpace(tool.Title) == "" {
			t.Errorf("%s has no title", tool.Name)
			continue
		}
		if len(tool.Title) > 64 || strings.HasPrefix(tool.Title, "ks_") {
			t.Errorf("%s's title %q is not a short name a person reads", tool.Name, tool.Title)
		}
		if other, ok := seen[tool.Title]; ok {
			t.Errorf("%s and %s share the title %q", tool.Name, other, tool.Title)
		}
		seen[tool.Title] = tool.Name
	}
	// through the real server: tools/list carries the title beside the
	// annotations it always had, in both eras
	_, bin, env := mcpEnv(t)
	c := startMCP(t, bin, env)
	resultOf(t, c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "x", "version": "1"}}))
	for era, params := range map[string]map[string]any{"legacy": nil, "modern": {"_meta": modernMeta(map[string]any{})}} {
		tools, _ := resultOf(t, c.call("tools/list", params))["tools"].([]any)
		if len(tools) != len(mcpTools) {
			t.Fatalf("%s tools/list answered %d tools, want %d", era, len(tools), len(mcpTools))
		}
		for _, x := range tools {
			d := x.(map[string]any)
			tool := findMCPTool(d["name"].(string))
			ann, _ := d["annotations"].(map[string]any)
			if d["title"] != tool.Title || ann["title"] != tool.Title {
				t.Errorf("%s tools/list: %s carries title %v and annotations.title %v, want %q", era, tool.Name, d["title"], ann["title"], tool.Title)
			}
			for _, k := range []string{"readOnlyHint", "destructiveHint", "openWorldHint"} {
				if _, ok := ann[k]; !ok {
					t.Errorf("%s tools/list: %s lost its %s", era, tool.Name, k)
				}
			}
			if ann["readOnlyHint"] != (tool.Kind == "read") || ann["destructiveHint"] != tool.Destructive || ann["openWorldHint"] != false {
				t.Errorf("%s tools/list: %s's annotations changed: %v", era, tool.Name, ann)
			}
		}
	}
}

// wordsOutsideCode counts the words of a markdown file, leaving out fenced
// code blocks as the plugin directory does.
func wordsOutsideCode(md string) int {
	n, fenced := 0, false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			n += len(strings.Fields(line))
		}
	}
	return n
}

func TestMCPListingClaudePlugin(t *testing.T) {
	pj := readJSONFile(t, filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))
	name, _ := pj["name"].(string)
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`).MatchString(name) {
		t.Errorf("plugin name %q is not lowercase letters, digits and hyphens", name)
	}
	for _, reserved := range []string{"claude", "anthropic", "official", "plugin", "mcp", "test"} {
		if name == reserved || strings.HasPrefix(name, "claude-") || strings.HasPrefix(name, "anthropic-") {
			t.Errorf("plugin name %q is reserved (%s)", name, reserved)
		}
	}
	for _, k := range []string{"description", "version", "license", "homepage", "repository"} {
		if s, _ := pj[k].(string); s == "" {
			t.Errorf("plugin.json has no %s", k)
		}
	}
	if a, _ := pj["author"].(map[string]any); a == nil || a["name"] == "" {
		t.Error("plugin.json has no author name")
	}
	if pj["license"] != "MIT" {
		t.Errorf("plugin.json's license is %v; this repository is MIT", pj["license"])
	}
	for _, k := range []string{"homepage", "documentationUrl", "supportUrl", "privacyPolicyUrl", "termsOfServiceUrl"} {
		if s, _ := pj[k].(string); !strings.HasPrefix(s, "https://") {
			t.Errorf("plugin.json's %s %q is not an https URL", k, s)
		}
	}

	// the MCP server: npx running the npm package at one exact released
	// version, and the verb mcp; the plugin's version is that version
	mj := readJSONFile(t, filepath.Join(pluginDir, ".mcp.json"))
	servers, _ := mj["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Fatalf(".mcp.json declares %d servers, want one", len(servers))
	}
	srv, _ := servers["keepstate"].(map[string]any)
	if srv == nil || srv["command"] != "npx" {
		t.Fatalf(".mcp.json's keepstate server does not run npx: %v", servers)
	}
	npm := readJSONFile(t, "npm/package.json")
	args, _ := srv["args"].([]any)
	if len(args) != 3 || args[0] != "-y" || args[2] != "mcp" {
		t.Fatalf(".mcp.json's arguments are not -y <package>@<version> mcp: %v", args)
	}
	pkg, _ := args[1].(string)
	at := strings.LastIndex(pkg, "@")
	if at <= 0 || pkg[:at] != npm["name"] {
		t.Fatalf(".mcp.json runs %q, not the npm package %v", pkg, npm["name"])
	}
	pin := pkg[at+1:]
	if !exactSemver.MatchString(pin) {
		t.Errorf("the launcher's version %q is not one exact version; the directory refuses a range or latest", pin)
	}
	if !releasedVersions(t)[pin] {
		t.Errorf("the launcher pins %s, which docs/changelog.md does not name as released", pin)
	}
	if pj["version"] != pin {
		t.Errorf("plugin.json's version %v is not the version its server runs, %s", pj["version"], pin)
	}
	if _, ok := srv["env"]; ok {
		t.Error(".mcp.json sets an environment for the server; it signs in with ks login and takes no credential")
	}

	// the README: at least 40 words outside code, says that approving needs
	// a person through elicitation, and names the command it runs
	rb, err := os.ReadFile(filepath.Join(pluginDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(rb)
	if n := wordsOutsideCode(readme); n < 40 {
		t.Errorf("the README has %d words outside code; the directory needs 40", n)
	}
	for _, want := range []string{"ks_agent_approve", "elicitation", "a person", "npx -y " + pkg + " mcp", "ctl.keepstate.ai", "SHA256SUMS", "/plugin marketplace add Keepstateai/cli"} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README does not say %q", want)
		}
	}

	// the license is this repository's own, as a regular file
	lic, err := os.ReadFile(filepath.Join(pluginDir, "LICENSE"))
	root, _ := os.ReadFile("LICENSE")
	if err != nil || !bytes.Equal(lic, root) {
		t.Error("the plugin's LICENSE is not this repository's LICENSE")
	}

	// the icon: a complete 512x512 PNG inside the plugin
	icon, _ := pj["icon"].(string)
	if !strings.HasPrefix(icon, "./") {
		t.Fatalf("plugin.json's icon %q is not a path inside the plugin", icon)
	}
	f, err := os.Open(filepath.Join(pluginDir, icon))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatalf("the icon is not a complete PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Errorf("the icon is %dx%d, want 512x512", b.Dx(), b.Dy())
	}

	// the folder holds exactly these regular files, none over the limits
	want := []string{".claude-plugin/plugin.json", ".mcp.json", "LICENSE", "README.md", strings.TrimPrefix(icon, "./")}
	var got []string
	_ = filepath.Walk(pluginDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(pluginDir, p)
		got = append(got, filepath.ToSlash(rel))
		if fi.Mode()&os.ModeType != 0 {
			t.Errorf("%s is not a regular file", rel)
		}
		if !strings.HasSuffix(rel, ".png") && fi.Size() >= 256<<10 {
			t.Errorf("%s is %d bytes; the directory holds files over 256 KiB for a reviewer", rel, fi.Size())
		}
		return nil
	})
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the plugin folder holds %v, want %v (no system files, no package-manager configuration)", got, want)
	}

	// the marketplace at the repository root offers this one plugin, so
	// /plugin marketplace add Keepstateai/cli finds it
	mk := readJSONFile(t, ".claude-plugin/marketplace.json")
	if mk["name"] != "keepstate" {
		t.Errorf("the marketplace is named %v", mk["name"])
	}
	if o, _ := mk["owner"].(map[string]any); o == nil || o["name"] == "" {
		t.Error("the marketplace has no owner name")
	}
	plugins, _ := mk["plugins"].([]any)
	if len(plugins) != 1 {
		t.Fatalf("the marketplace lists %d plugins, want one", len(plugins))
	}
	entry := plugins[0].(map[string]any)
	if entry["name"] != name || entry["source"] != "./"+pluginDir {
		t.Errorf("the marketplace entry %v does not point at %s by its name %s", entry, pluginDir, name)
	}
}
