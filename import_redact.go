// import_redact.go: the secrets an imported session never carries.
//
// A transcript holds what the tools printed: an environment dump, a curl
// header, a config file. Before anything leaves this machine, every string in
// it is read against one list of credential shapes (import_rules.json, the
// same list the service refuses on arrival and the same vectors hold every
// reader to) and a match is replaced by [redacted:KIND]. The person sees the
// counts by kind in the plan; the secrets themselves are never printed.
//
// A rule with keep leaves that many leading groups of its match in place and
// replaces the rest, so "API_KEY=" stays and only the value goes.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed import_rules.json
var importRulesJSON []byte

type importRule struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Keep    int    `json:"keep"`
	re      *regexp.Regexp
}

type importRuleSet struct {
	Version     int          `json:"version"`
	Replacement string       `json:"replacement"`
	Rules       []importRule `json:"rules"`
}

var (
	importRules     importRuleSet
	importRulesOnce sync.Once
)

func loadImportRules() *importRuleSet {
	importRulesOnce.Do(func() {
		if err := json.Unmarshal(importRulesJSON, &importRules); err != nil || importRules.Version != 1 || len(importRules.Rules) == 0 {
			panic("the import redaction rules are not readable: " + fmt.Sprint(err))
		}
		for i := range importRules.Rules {
			importRules.Rules[i].re = regexp.MustCompile(importRules.Rules[i].Pattern)
		}
	})
	return &importRules
}

// redactCounts is the redactions made, by kind.
type redactCounts map[string]int

func (c redactCounts) add(o redactCounts) {
	for k, n := range o {
		c[k] += n
	}
}

func (c redactCounts) total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

// words is "2 github_token, 1 jwt" in kind order, or "none".
func (c redactCounts) words() string {
	if len(c) == 0 {
		return "none"
	}
	kinds := make([]string, 0, len(c))
	for k := range c {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", c[k], k))
	}
	return strings.Join(parts, ", ")
}

// couldMatch is a cheap necessary condition for a rule: a text that fails it
// cannot match the rule, so the pattern is not run over it. Transcripts reach
// hundreds of megabytes, and a pattern run over every string would take
// minutes. A rule of a kind this does not know is always run.
func couldMatch(kind, s string, lower *string) bool {
	low := func() string {
		if *lower == "" {
			*lower = strings.ToLower(s)
		}
		return *lower
	}
	anyOf := func(hay string, needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(hay, n) {
				return true
			}
		}
		return false
	}
	switch kind {
	case "private_key_block":
		return strings.Contains(s, "PRIVATE KEY")
	case "anthropic_key":
		return strings.Contains(s, "sk-ant-")
	case "openrouter_key":
		return strings.Contains(s, "sk-or-")
	case "openai_key":
		return strings.Contains(s, "sk-")
	case "keepstate_key":
		return anyOf(s, "ks_sk_", "ksk_")
	case "github_token":
		return anyOf(s, "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_")
	case "aws_access_key":
		return anyOf(s, "AKIA", "ASIA", "AGPA", "AIDA", "AROA")
	case "google_api_key":
		return strings.Contains(s, "AIza")
	case "telegram_bot_token":
		for i := 8; i < len(s); i++ {
			if s[i] != ':' {
				continue
			}
			ok := true
			for _, c := range []byte(s[i-8 : i]) {
				if c < '0' || c > '9' {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	case "slack_token":
		return strings.Contains(s, "xox")
	case "jwt":
		return strings.Contains(s, "eyJ")
	case "connection_string":
		return strings.Contains(s, "://")
	case "azure_account_key":
		return anyOf(low(), "accountkey", "sharedaccesskey")
	case "authorization_header":
		return strings.Contains(low(), "authorization")
	case "bearer_token":
		return strings.Contains(low(), "bearer")
	case "assignment":
		return (strings.Contains(s, "=") || strings.Contains(s, ":")) &&
			anyOf(low(), "api_key", "api-key", "apikey", "secret", "token", "password", "passwd", "credential", "private_key", "private-key", "privatekey")
	}
	return true
}

// redactImportText replaces every credential-shaped match in s, rule by rule
// in the list's order, and counts them by kind.
func redactImportText(s string, counts redactCounts) string {
	rs := loadImportRules()
	lower := ""
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if len(s) < 8 || !couldMatch(r.Kind, s, &lower) || !r.re.MatchString(s) {
			continue
		}
		lower = "" // the text may change
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			counts[r.Kind]++
			head := ""
			if r.Keep > 0 {
				sub := r.re.FindStringSubmatch(m)
				for g := 1; g <= r.Keep && g < len(sub); g++ {
					head += sub[g]
				}
			}
			return head + fmt.Sprintf(rs.Replacement, r.Kind)
		})
	}
	return s
}

// redactImportValue walks a decoded JSON value and redacts every string in
// it (never a key), in place.
func redactImportValue(v any, counts redactCounts) any {
	switch x := v.(type) {
	case string:
		return redactImportText(x, counts)
	case []any:
		for i := range x {
			x[i] = redactImportValue(x[i], counts)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = redactImportValue(x[k], counts)
		}
		return x
	}
	return v
}

// importJSON writes v as one compact line: map keys sorted (so the same
// record always makes the same bytes), no HTML escaping, a newline.
func importJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// decodeImportLine reads one record, numbers kept as written.
func decodeImportLine(line []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}
