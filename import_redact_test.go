package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// ---- test helpers shared by the import tests ---------------------------------

var (
	fakesOnce sync.Once
	fakesMap  map[string][]string
)

// fakeValue is a throwaway credential-shaped string, joined from its parts at
// run time: no committed file holds a complete one.
func fakeValue(t testing.TB, kind string) string {
	t.Helper()
	fakesOnce.Do(func() {
		b, err := os.ReadFile("testdata/import/fakes.json")
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Fakes map[string][]string `json:"fakes"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		fakesMap = f.Fakes
	})
	parts, ok := fakesMap[kind]
	if !ok {
		t.Fatalf("no fake of kind %q", kind)
	}
	return strings.Join(parts, "")
}

var fakeMarker = regexp.MustCompile(`@@FAKE:([a-z_]+)@@`)

// expandFakes replaces the @@FAKE:kind@@ markers of a fixture with the fakes.
func expandFakes(t testing.TB, s string) string {
	t.Helper()
	return fakeMarker.ReplaceAllStringFunc(s, func(m string) string {
		return fakeValue(t, fakeMarker.FindStringSubmatch(m)[1])
	})
}

func fixtureText(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "import", name))
	if err != nil {
		t.Fatal(err)
	}
	return expandFakes(t, string(b))
}

// fixtureFile writes an expanded fixture to a new temp file and answers its path.
func fixtureFile(t testing.TB, name, as string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), as)
	if err := os.WriteFile(p, []byte(fixtureText(t, name)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- the shared vectors ------------------------------------------------------

type redactVectors struct {
	Cases []struct {
		Name  string         `json:"name"`
		In    []any          `json:"in"`
		Want  string         `json:"want"`
		Kinds map[string]int `json:"kinds"`
	} `json:"cases"`
}

func loadRedactVectors(t *testing.T) redactVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/import/import-redaction-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v redactVectors
	if err := json.Unmarshal(b, &v); err != nil || len(v.Cases) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	return v
}

func joinVectorInput(t *testing.T, in []any) string {
	var b strings.Builder
	for _, p := range in {
		switch x := p.(type) {
		case string:
			b.WriteString(x)
		case map[string]any:
			b.WriteString(fakeValue(t, x["fake"].(string)))
		default:
			t.Fatalf("a vector part is text or {fake}: %v", p)
		}
	}
	return b.String()
}

func TestImportRedactionPassesTheSharedVectors(t *testing.T) {
	v := loadRedactVectors(t)
	for _, c := range v.Cases {
		in := joinVectorInput(t, c.In)
		counts := redactCounts{}
		got := redactImportText(in, counts)
		if got != c.Want {
			t.Errorf("%s:\n got  %q\n want %q", c.Name, got, c.Want)
		}
		if len(counts) != len(c.Kinds) {
			t.Errorf("%s: kinds %v, want %v", c.Name, counts, c.Kinds)
		}
		for k, n := range c.Kinds {
			if counts[k] != n {
				t.Errorf("%s: %d of %s, want %d", c.Name, counts[k], k, n)
			}
		}
		again := redactCounts{}
		if redactImportText(got, again) != got || again.total() != 0 {
			t.Errorf("%s: redacting the redacted text changed it (%v)", c.Name, again)
		}
	}
}

func TestImportRedactionCoversTheNamedKinds(t *testing.T) {
	v := loadRedactVectors(t)
	seen := map[string]bool{}
	for _, c := range v.Cases {
		for k := range c.Kinds {
			seen[k] = true
		}
	}
	for _, k := range []string{"telegram_bot_token", "google_api_key", "jwt", "bearer_token", "aws_access_key", "github_token", "anthropic_key", "openai_key", "openrouter_key", "private_key_block", "connection_string", "assignment", "keepstate_key", "slack_token"} {
		if !seen[k] {
			t.Errorf("no vector holds the kind %s", k)
		}
	}
}

func TestImportRedactionWalksValuesAndSkipsKeys(t *testing.T) {
	tok := fakeValue(t, "github_token")
	rec, err := decodeImportLine([]byte(`{"` + tok + `":"k","a":["x ` + tok + ` y",{"b":"` + tok + `"}],"n":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	counts := redactCounts{}
	redactImportValue(rec, counts)
	b, _ := importJSON(rec)
	if counts["github_token"] != 2 || strings.Count(string(b), "[redacted:github_token]") != 2 {
		t.Fatalf("counts %v in %s", counts, b)
	}
	if !strings.Contains(string(b), "12345678901234567890") {
		t.Fatalf("a number was changed: %s", b)
	}
}

// ---- the drift guard ---------------------------------------------------------

func fileSHAimport(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type importSource struct {
	ServerCommit string `json:"server_commit"`
	Files        map[string]struct {
		Path     string `json:"path"`
		From     string `json:"from"`
		SHA256   string `json:"sha256"`
		Vendored string `json:"vendored_sha256"`
	} `json:"files"`
}

func loadImportSource(t *testing.T) importSource {
	t.Helper()
	b, err := os.ReadFile("testdata/import/SOURCE.json")
	var src importSource
	if err != nil || json.Unmarshal(b, &src) != nil || len(src.Files) == 0 {
		t.Fatalf("testdata/import/SOURCE.json: %v", err)
	}
	return src
}

func TestImportVendoredFilesMatchTheirSource(t *testing.T) {
	src := loadImportSource(t)
	for name, f := range src.Files {
		if got := fileSHAimport(t, f.Path); got != f.Vendored {
			t.Errorf("%s is %s, SOURCE.json records %s", name, got, f.Vendored)
		}
	}
}

func TestImportVectorsVendoredMatchTheServer(t *testing.T) {
	src := loadImportSource(t)
	server := os.Getenv("KS_SERVER_CHECKOUT")
	if server == "" {
		t.Skip("KS_SERVER_CHECKOUT not set: the comparison with the service runs in CI")
	}
	for name, f := range src.Files {
		raw, err := os.ReadFile(filepath.Join(server, f.From))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(publicScrub(raw))
		if got := hex.EncodeToString(sum[:]); got != f.Vendored {
			t.Fatalf("DRIFT: the service's %s, scrubbed for the public repository, is now %s, but this client vendored %s (%s) from %s; re-vendor and re-run the vectors",
				f.From, got, f.Vendored, name, src.ServerCommit)
		}
	}
}

// The cheap pre-check that keeps a 300 MB transcript fast must never say no
// to a text its rule would match.
func TestImportRedactionPrefilterNeverHidesAMatch(t *testing.T) {
	rs := loadImportRules()
	var texts []string
	for _, c := range loadRedactVectors(t).Cases {
		texts = append(texts, joinVectorInput(t, c.In))
	}
	for k := range fakesMap {
		texts = append(texts, fakeValue(t, k), "x "+fakeValue(t, k)+" y", "{\"a\":\""+fakeValue(t, k)+"\"}")
	}
	for _, in := range texts {
		for i := range rs.Rules {
			r := &rs.Rules[i]
			lower := ""
			if r.re.MatchString(in) && !couldMatch(r.Kind, in, &lower) {
				t.Errorf("rule %s matches %q but the pre-check says no", r.Kind, in)
			}
		}
	}
}
