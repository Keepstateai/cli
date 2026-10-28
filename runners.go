// runners.go: the Cruise check runners as the service lists them (the
// vendored runners_list.go). Discovery, the classification of a path and the
// checks a person may choose are read from that one list, so this client
// discovers and chooses what the service and its verifier do.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

type runnerDef struct {
	Ecosystem  string `json:"ecosystem"`
	Offered    bool   `json:"offered"`
	Runner     string `json:"runner"`
	Language   string `json:"language"`
	Legacy     bool   `json:"legacy"`
	Test       string `json:"test"`
	TestInDirs *struct {
		Name string   `json:"name"`
		Dirs []string `json:"dirs"`
	} `json:"testInDirs"`
	ConfigAnywhere   string   `json:"configAnywhere"`
	ConfigRoot       string   `json:"configRoot"`
	DepsAnywhere     string   `json:"depsAnywhere"`
	DepsRoot         string   `json:"depsRoot"`
	SupportInTestDir string   `json:"supportInTestDir"`
	TestPath         string   `json:"testPath"`
	ConfigPath       string   `json:"configPath"`
	SupportPath      string   `json:"supportPath"`
	SkipDirs         []string `json:"skipDirs"`
	RequiresDeps     string   `json:"requiresDeps"`
	Command          string   `json:"command"`

	re map[string]*regexp.Regexp
}

type runnerList struct {
	SchemaVersion int         `json:"schemaVersion"`
	TestDirs      []string    `json:"testDirs"`
	FixtureDirs   []string    `json:"fixtureDirs"`
	Ecosystems    []runnerDef `json:"ecosystems"`

	testDirs, fixtureDirs map[string]bool
	byRunner              map[string]*runnerDef
}

var runners = mustRunners([]byte(runnersJSON))

func mustRunners(b []byte) *runnerList {
	var l runnerList
	if err := json.Unmarshal(b, &l); err != nil {
		panic("the vendored runner list: " + err.Error())
	}
	if l.SchemaVersion != 1 {
		panic("the vendored runner list: schemaVersion must be 1")
	}
	l.testDirs, l.fixtureDirs, l.byRunner = map[string]bool{}, map[string]bool{}, map[string]*runnerDef{}
	for _, d := range l.TestDirs {
		l.testDirs[d] = true
	}
	for _, d := range l.FixtureDirs {
		l.fixtureDirs[d] = true
	}
	for i := range l.Ecosystems {
		e := &l.Ecosystems[i]
		e.re = map[string]*regexp.Regexp{}
		for k, v := range map[string]string{"test": e.Test, "configAnywhere": e.ConfigAnywhere, "configRoot": e.ConfigRoot,
			"depsAnywhere": e.DepsAnywhere, "depsRoot": e.DepsRoot, "supportInTestDir": e.SupportInTestDir,
			"testPath": e.TestPath, "configPath": e.ConfigPath, "supportPath": e.SupportPath} {
			if v != "" {
				e.re[k] = regexp.MustCompile(v)
			}
		}
		if e.TestInDirs != nil {
			e.re["testInDirs"] = regexp.MustCompile(e.TestInDirs.Name)
		}
		l.byRunner[e.Runner] = e
	}
	return &l
}

// classify is the verifier's classify: the ecosystems and role a relative
// path plays in a check, or nil.
func (l *runnerList) classify(rel string, ignored func(string) bool) []ecoRole {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	if ignored(rel) {
		return nil
	}
	inFixture, inTestdir := false, false
	for _, d := range dirs {
		inFixture = inFixture || l.fixtureDirs[d]
		inTestdir = inTestdir || l.testDirs[d]
	}
	atRoot := len(parts) == 1
	match := func(e *runnerDef, k string) bool { r := e.re[k]; return r != nil && r.MatchString(name) }
	// testPath, configPath and supportPath match the whole relative path (a
	// language whose layout says what a test is: src/test/java for Maven)
	matchPath := func(e *runnerDef, k string) bool { r := e.re[k]; return r != nil && r.MatchString(rel) }
	var out []ecoRole
	for i := range l.Ecosystems {
		e := &l.Ecosystems[i]
		if e.skips(dirs) {
			// a dependency tree of this ecosystem (PHP's vendor/)
			continue
		}
		inDirs := false
		if e.TestInDirs != nil && match(e, "testInDirs") {
			for _, d := range dirs {
				for _, w := range e.TestInDirs.Dirs {
					inDirs = inDirs || d == w
				}
			}
		}
		var role string
		switch {
		case match(e, "test") || inDirs || matchPath(e, "testPath"):
			role = "test"
		case match(e, "configAnywhere") || (atRoot && match(e, "configRoot")) || matchPath(e, "configPath"):
			role = "config"
		case match(e, "depsAnywhere") || (atRoot && match(e, "depsRoot")):
			role = "deps"
		case (inTestdir && match(e, "supportInTestDir")) || matchPath(e, "supportPath"):
			role = "test_support"
		default:
			continue
		}
		out = append(out, ecoRole{e.Ecosystem, role})
	}
	// the first ecosystems (legacy) decide "any other file a test package
	// carries" among themselves only, so their pinned inputs read as before
	// the later languages existed
	legacyRoles, has := 0, map[string]bool{}
	for _, o := range out {
		has[o.eco] = true
		if l.legacy(o.eco) {
			legacyRoles++
		}
	}
	all := len(out)
	for i := range l.Ecosystems {
		e := &l.Ecosystems[i]
		if has[e.Ecosystem] || e.skips(dirs) {
			continue
		}
		basis := all
		if e.Legacy {
			basis = legacyRoles
		}
		if inFixture || (inTestdir && basis == 0) {
			out = append(out, ecoRole{e.Ecosystem, "fixture"})
		}
	}
	return out
}

// skips says a path in dirs lies in a directory this ecosystem classifies
// nothing in (skipDirs: its dependency tree).
func (e *runnerDef) skips(dirs []string) bool {
	for _, d := range dirs {
		for _, s := range e.SkipDirs {
			if d == s {
				return true
			}
		}
	}
	return false
}

func (l *runnerList) legacy(eco string) bool {
	for i := range l.Ecosystems {
		if l.Ecosystems[i].Ecosystem == eco {
			return l.Ecosystems[i].Legacy
		}
	}
	return false
}

func (l *runnerList) ecosystems() []string {
	out := make([]string, 0, len(l.Ecosystems))
	for _, e := range l.Ecosystems {
		out = append(out, e.Ecosystem)
	}
	return out
}

// offered: the runners a person may choose, in the list's order.
func (l *runnerList) offered() []string {
	var out []string
	for _, e := range l.Ecosystems {
		if e.Offered {
			out = append(out, e.Runner)
		}
	}
	return out
}

// offeredRunner: the offered definition of a runner, or nil.
func (l *runnerList) offeredRunner(r string) *runnerDef {
	if d := l.byRunner[r]; d != nil && d.Offered {
		return d
	}
	return nil
}

func (l *runnerList) offeredEco(eco string) (string, bool) {
	for _, e := range l.Ecosystems {
		if e.Ecosystem == eco && e.Offered {
			return e.Runner, true
		}
	}
	return "", false
}

func orList(o []string) string {
	if len(o) < 2 {
		return strings.Join(o, "")
	}
	return strings.Join(o[:len(o)-1], ", ") + " or " + o[len(o)-1]
}

// choices: "pytest, go, node, cargo or minitest", for a refusal.
func (l *runnerList) choices() string { return orList(l.offered()) }

// languages: "Python, Go, Node, Rust or Ruby", the offered languages.
func (l *runnerList) languages() string {
	var o []string
	for _, e := range l.Ecosystems {
		if e.Offered {
			o = append(o, e.Language)
		}
	}
	return orList(o)
}
