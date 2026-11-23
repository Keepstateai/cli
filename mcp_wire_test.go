// mcp_wire_test: the wire behaviour of the MCP servers, written once as
// vectors (testdata/mcpwire/mcp-wire-v1.json) and run here against ks mcp's
// stdio server. The hosted KeepState MCP, a remote server run by the service,
// runs the same file as HTTP posts, so both servers answer the discovery, the
// handshake, the version rules, the tool list's shape and the refusals the same
// way. The file is the service's; this repository vendors it
// (testdata/mcpwire/SOURCE.json), and TestMCPWireVectorsVendoredMatchTheServer
// holds the copy to the service's own.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type wireCase struct {
	Name   string            `json:"name"`
	Send   []json.RawMessage `json:"send"`
	Expect []json.RawMessage `json:"expect"`
}

type wireFile struct {
	Version int        `json:"vectors_version"`
	Cases   []wireCase `json:"cases"`
}

// wireMatch reports why a value does not match a pattern, or "" when it does.
// A pattern object matches an object holding every key it names (an extra key
// is allowed); a key starting with $ is an operator over the value itself.
func wireMatch(pat, val any, path string) string {
	switch p := pat.(type) {
	case map[string]any:
		for k, pv := range p {
			if strings.HasPrefix(k, "$") {
				if why := wireOp(k, pv, val, path); why != "" {
					return why
				}
				continue
			}
			vm, ok := val.(map[string]any)
			if !ok {
				return fmt.Sprintf("%s: want an object holding %q, got %v", path, k, wireShort(val))
			}
			got, has := vm[k]
			if abs, isAbs := pv.(map[string]any); isAbs && abs["$absent"] == true {
				if has {
					return fmt.Sprintf("%s.%s: must be absent", path, k)
				}
				continue
			}
			if !has {
				return fmt.Sprintf("%s: missing %q", path, k)
			}
			if why := wireMatch(pv, got, path+"."+k); why != "" {
				return why
			}
		}
		return ""
	case []any:
		va, ok := val.([]any)
		if !ok || len(va) != len(p) {
			return fmt.Sprintf("%s: want an array of %d, got %v", path, len(p), wireShort(val))
		}
		for i := range p {
			if why := wireMatch(p[i], va[i], fmt.Sprintf("%s[%d]", path, i)); why != "" {
				return why
			}
		}
		return ""
	}
	if fmt.Sprint(pat) != fmt.Sprint(val) {
		return fmt.Sprintf("%s: want %v, got %v", path, wireShort(pat), wireShort(val))
	}
	return ""
}

func wireShort(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 160 {
		return string(b[:160]) + "..."
	}
	return string(b)
}

func wireOp(op string, arg, val any, path string) string {
	switch op {
	case "$type":
		want, _ := arg.(string)
		got := "null"
		switch val.(type) {
		case string:
			got = "string"
		case float64:
			got = "number"
		case bool:
			got = "boolean"
		case map[string]any:
			got = "object"
		case []any:
			got = "array"
		}
		if got != want {
			return fmt.Sprintf("%s: want %s, got %s", path, want, wireShort(val))
		}
	case "$includes":
		va, ok := val.([]any)
		if !ok {
			return fmt.Sprintf("%s: want an array, got %v", path, wireShort(val))
		}
		for _, w := range arg.([]any) {
			found := false
			for _, x := range va {
				found = found || fmt.Sprint(x) == fmt.Sprint(w)
			}
			if !found {
				return fmt.Sprintf("%s: %v is not among %s", path, w, wireShort(val))
			}
		}
	case "$matches":
		s, ok := val.(string)
		if !ok || !regexp.MustCompile(arg.(string)).MatchString(s) {
			return fmt.Sprintf("%s: %v does not match %v", path, wireShort(val), arg)
		}
	case "$some", "$every":
		va, ok := val.([]any)
		if !ok {
			return fmt.Sprintf("%s: want an array, got %v", path, wireShort(val))
		}
		any1 := false
		for i, x := range va {
			why := wireMatch(arg, x, fmt.Sprintf("%s[%d]", path, i))
			if op == "$every" && why != "" {
				return why
			}
			any1 = any1 || why == ""
		}
		if op == "$some" && !any1 {
			return fmt.Sprintf("%s: no element matches %s", path, wireShort(arg))
		}
	case "$all_of":
		for _, p := range arg.([]any) {
			if why := wireMatch(p, val, path); why != "" {
				return why
			}
		}
	case "$empty":
		if m, ok := val.(map[string]any); !ok || len(m) != 0 {
			return fmt.Sprintf("%s: want an empty object, got %v", path, wireShort(val))
		}
	case "$no_key_matching":
		re := regexp.MustCompile(arg.(string))
		if m, ok := val.(map[string]any); ok {
			for k := range m {
				if re.MatchString(k) {
					return fmt.Sprintf("%s: the key %q matches %s", path, k, arg)
				}
			}
		}
	case "$absent":
		// handled by the parent key
	default:
		return fmt.Sprintf("%s: unknown operator %s", path, op)
	}
	return ""
}

// runWireCase sends a case's messages to a fresh server and returns its
// answers by id.
func runWireCase(t *testing.T, c wireCase) map[string]map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, m := range c.Send {
		var one bytes.Buffer
		if err := json.Compact(&one, m); err != nil {
			t.Fatal(err)
		}
		in.Write(one.Bytes()) // one message a line
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	s := &mcpServer{out: &out, self: "ks", confirms: map[string]pendingConfirm{}, waiting: map[string]chan rpcIn{}}
	if rc := s.serve(&in, io.Discard); rc != 0 {
		t.Fatalf("%s: the server exited %d", c.Name, rc)
	}
	got := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: the server wrote a line that is not JSON: %q", c.Name, line)
		}
		got[fmt.Sprint(m["id"])] = m
	}
	return got
}

func loadWireVectors(t *testing.T) wireFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/mcpwire/mcp-wire-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f wireFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Cases) == 0 {
		t.Fatalf("mcp-wire-v1.json: %v", err)
	}
	return f
}

func TestMCPWireVectors(t *testing.T) {
	for _, c := range loadWireVectors(t).Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			got := runWireCase(t, c)
			if len(got) != len(c.Expect) {
				t.Fatalf("%d answers, want %d: %v", len(got), len(c.Expect), got)
			}
			for _, raw := range c.Expect {
				var want map[string]any
				if err := json.Unmarshal(raw, &want); err != nil {
					t.Fatal(err)
				}
				m, ok := got[fmt.Sprint(want["id"])]
				if !ok {
					t.Fatalf("no answer to id %v", want["id"])
				}
				if why := wireMatch(want, m, "$"); why != "" {
					t.Errorf("%s", why)
				}
			}
		})
	}
}

// the vectors can fail: a server that answers wrongly does not pass them
func TestMCPWireVectorsCanFail(t *testing.T) {
	f := loadWireVectors(t)
	c := f.Cases[0]
	got := runWireCase(t, c)
	var want map[string]any
	_ = json.Unmarshal(c.Expect[0], &want)
	if why := wireMatch(want, got["1"], "$"); why != "" {
		t.Fatalf("the first case should pass: %s", why)
	}
	bad := map[string]any{"jsonrpc": "2.0", "id": float64(1), "result": map[string]any{"resultType": "complete", "supportedVersions": []any{"1999-01-01"}}}
	if why := wireMatch(want, bad, "$"); why == "" {
		t.Errorf("a result that lists no supported version matched")
	}
}

func TestMCPWireVectorsVendoredMatchTheServer(t *testing.T) {
	var src struct {
		ServerCommit string `json:"server_commit"`
		From         string `json:"from"`
		Vendored     string `json:"vendored_sha256"`
	}
	b, err := os.ReadFile("testdata/mcpwire/SOURCE.json")
	if err != nil || json.Unmarshal(b, &src) != nil || src.ServerCommit == "" || src.From == "" {
		t.Fatalf("SOURCE.json: %v", err)
	}
	raw, err := os.ReadFile("testdata/mcpwire/mcp-wire-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != src.Vendored {
		t.Errorf("testdata/mcpwire/mcp-wire-v1.json is %s, SOURCE.json records %s", got, src.Vendored)
	}
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in CI")
	}
	theirs, err := os.ReadFile(filepath.Join(server, src.From))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(theirs, raw) {
		t.Fatalf("DRIFT: the service's %s differs from this client's copy; re-vendor it (cp the file, update SOURCE.json) and re-run the vectors", src.From)
	}
}
