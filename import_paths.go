// import_paths.go: the paths of the old machine, said as paths of the new one.
//
// A transcript names the project by absolute paths: in what a tool was asked
// to read, in what it printed, in the working directory of every record. The
// session continues in a workspace whose root is /home/specimen/workspace, so
// every mention of the project's root becomes the folder it was uploaded to
// (or the workspace itself when no files are uploaded), longest root first
// and only at a path boundary: /Users/alice/shop2 is not a mention of
// /Users/alice/shop.
package main

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// importGuestWorkspace is where a session's workspace is, for every session.
const importGuestWorkspace = "/home/specimen/workspace"

type pathRoot struct{ From, To string }

// pathRewriter replaces old roots with new ones in text.
type pathRewriter struct {
	roots []pathRoot
	re    *regexp.Regexp
	to    map[string]string
	Count int
}

func newPathRewriter(roots []pathRoot) *pathRewriter {
	seen := map[string]bool{}
	var rs []pathRoot
	for _, r := range roots {
		r.From = strings.TrimRight(r.From, "/")
		if r.From == "" || r.From == "/" || seen[r.From] {
			continue
		}
		seen[r.From] = true
		rs = append(rs, r)
	}
	sort.SliceStable(rs, func(i, j int) bool { return len(rs[i].From) > len(rs[j].From) })
	w := &pathRewriter{roots: rs, to: map[string]string{}}
	var alts []string
	for _, r := range rs {
		alts = append(alts, regexp.QuoteMeta(r.From))
		w.to[r.From] = r.To
	}
	if len(alts) > 0 {
		w.re = regexp.MustCompile(strings.Join(alts, "|"))
	}
	return w
}

func isNameByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-' || c == '.'
}

// apply rewrites every mention of a root in s that stands at path boundaries.
func (w *pathRewriter) apply(s string) string {
	if w == nil || w.re == nil || !strings.Contains(s, "/") && !strings.Contains(s, "~") {
		return s
	}
	found := false
	for _, r := range w.roots {
		if strings.Contains(s, r.From) {
			found = true
			break
		}
	}
	if !found {
		return s
	}
	locs := w.re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		start, end := l[0], l[1]
		if start < last {
			continue
		}
		if start > 0 {
			p := s[start-1]
			if isNameByte(p) || p == '~' || (p == '/' && !strings.HasSuffix(s[:start], "file://")) {
				continue
			}
		}
		if end < len(s) && isNameByte(s[end]) {
			// a longer name (shop2, shop-old); a dot that ends a sentence is not a name
			if !(s[end] == '.' && (end+1 == len(s) || !isNameByte(s[end+1]))) {
				continue
			}
		}
		b.WriteString(s[last:start])
		b.WriteString(w.to[s[start:end]])
		last = end
		w.Count++
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// importRoots are the roots to rewrite: the project's root (the git top
// level when its files go along, else the working directory) to dest, any
// other working directory the session used to the workspace itself, and the
// home-relative spelling of the project's root.
func importRoots(root string, others []string, home, dest string) []pathRoot {
	rs := []pathRoot{{root, dest}}
	if home != "" {
		if rel, err := filepath.Rel(home, root); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			rs = append(rs, pathRoot{"~/" + filepath.ToSlash(rel), dest})
		}
	}
	for _, o := range others {
		if o == "" || o == root || under(o, root) {
			continue
		}
		rs = append(rs, pathRoot{o, importGuestWorkspace})
	}
	return rs
}

// under: p is inside dir (at a path boundary).
func under(p, dir string) bool {
	dir = strings.TrimRight(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}
