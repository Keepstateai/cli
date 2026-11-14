// import_summary.go: the written summary of a conversation.
//
// A conversation that cannot be resumed (a Cursor chat, or a Claude Code
// session going into a Codex agent) goes across as a short written summary,
// never as a transcript. It is built here, on the person's machine, from the
// text of the conversation by fixed rules: no model reads or writes it. It
// holds the person's own words, the decisions the assistant stated, where the
// work stands, the open tasks and the files mentioned, and nothing else.
// Memory stays small however long the conversation is: only what each
// section can show is kept as the turns go by.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	summaryMaxChars     = 6000
	summaryUserClip     = 600
	summaryUserMax      = 12
	summaryDecisionMax  = 8
	summaryDecisionClip = 300
	summaryStandsClip   = 1200
	summaryOpenMax      = 10
	summaryOpenClip     = 240
	summaryFilesMax     = 40
)

var (
	decisionCue = regexp.MustCompile(`(?i)\b(decided|decision|we will|we'll|i will use|i'll use|chose|chosen|instead of|going with|switch(?:ed)? to)\b`)
	openCue     = regexp.MustCompile(`(?i)(\bTODO\b|still to do|\bnext:|^\s*[-*] \[ \])`)
	pathToken   = regexp.MustCompile(`(?:~|\.{1,2})?/?[A-Za-z0-9_@+.\-]+(?:/[A-Za-z0-9_@+.\-]+)+`)
	sentenceEnd = regexp.MustCompile(`[.!?]\s+|\n+`)
)

// summaryBuilder takes the conversation's turns in order and keeps what the
// summary needs.
type summaryBuilder struct {
	rewrite     func(string) string
	firstUser   string
	hasFirst    bool
	recentUsers []string
	decisions   []string
	lastStands  string
	open        []string
	files       []string
	fileSeen    map[string]bool
	Turns       int
}

func newSummaryBuilder(rewrite func(string) string) *summaryBuilder {
	if rewrite == nil {
		rewrite = func(s string) string { return s }
	}
	return &summaryBuilder{rewrite: rewrite, fileSeen: map[string]bool{}}
}

func clipWords(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + " [cut]"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// add takes one turn: role is "user" or "assistant".
func (b *summaryBuilder) add(role, text string) {
	text = strings.TrimSpace(b.rewrite(text))
	if text == "" {
		return
	}
	switch role {
	case "user":
		b.Turns++
		t := clipWords(text, summaryUserClip)
		if !b.hasFirst {
			b.firstUser, b.hasFirst = t, true
		} else {
			b.recentUsers = append(b.recentUsers, t)
			if len(b.recentUsers) > summaryUserMax-1 {
				b.recentUsers = b.recentUsers[1:]
			}
		}
	case "assistant":
		b.lastStands = text
		for _, s := range sentenceEnd.Split(text, -1) {
			s = oneLine(s)
			if len(s) > 12 && decisionCue.MatchString(s) {
				b.decisions = append(b.decisions, clipWords(s, summaryDecisionClip))
				if len(b.decisions) > summaryDecisionMax {
					b.decisions = b.decisions[1:]
				}
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if openCue.MatchString(line) {
			b.open = append(b.open, clipWords(oneLine(line), summaryOpenClip))
			if len(b.open) > summaryOpenMax {
				b.open = b.open[1:]
			}
		}
	}
	for _, loc := range pathToken.FindAllStringIndex(text, -1) {
		tok := strings.Trim(text[loc[0]:loc[1]], ".,;:)(\"'`")
		if loc[0] >= 3 && strings.HasSuffix(text[:loc[0]], "://") || strings.Contains(tok, "://") {
			continue
		}
		if !pathLike(tok) || b.fileSeen[tok] || len(b.files) >= summaryFilesMax {
			continue
		}
		b.fileSeen[tok] = true
		b.files = append(b.files, tok)
	}
}

// pathLike: an absolute or home path, or a relative one that ends in a file
// name with an extension.
func pathLike(t string) bool {
	if len(t) < 3 || len(t) > 200 {
		return false
	}
	if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~/") {
		return strings.Count(t, "/") >= 2
	}
	last := t[strings.LastIndex(t, "/")+1:]
	i := strings.LastIndex(last, ".")
	return i > 0 && len(last)-i-1 >= 1 && len(last)-i-1 <= 8
}

// build writes the summary, redacted and within its cap, and counts the
// redactions by kind into counts.
func (b *summaryBuilder) build(source string, counts redactCounts) string {
	var sec []string
	head := fmt.Sprintf("Written summary of an earlier conversation in %s (%d of the person's messages). It was built on the person's own machine from the conversation's text by fixed rules; no model wrote it. It is not a transcript: check anything it says against the files.", source, b.Turns)
	list := func(title string, items []string, numbered bool) {
		var s strings.Builder
		s.WriteString(title + "\n")
		if len(items) == 0 {
			s.WriteString("- none found\n")
		}
		for i, it := range items {
			if numbered {
				fmt.Fprintf(&s, "%d. %s\n", i+1, it)
			} else {
				fmt.Fprintf(&s, "- %s\n", it)
			}
		}
		sec = append(sec, s.String())
	}
	var asked []string
	if b.hasFirst {
		asked = append(asked, b.firstUser)
	}
	asked = append(asked, b.recentUsers...)
	list("What the person asked for (their own words, oldest first; the first message, then the most recent):", asked, true)
	list("Decisions the assistant stated:", b.decisions, false)
	stands := []string{}
	if b.lastStands != "" {
		stands = append(stands, clipWords(b.lastStands, summaryStandsClip))
	}
	list("Where it stands (the assistant's last message):", stands, false)
	list("Open tasks:", b.open, false)
	files := append([]string(nil), b.files...)
	sort.Strings(files)
	list("Files mentioned:", files, false)
	out := head + "\n\n" + strings.Join(sec, "\n")
	out = redactImportText(out, counts)
	if r := []rune(out); len(r) > summaryMaxChars {
		out = string(r[:summaryMaxChars-30]) + "\n[the summary was cut]"
	}
	return out
}
