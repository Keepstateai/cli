package main

import (
	"bytes"
	"strings"
	"testing"
)

// fixtureClaudeToken is an invented token, joined at run time so that no
// line of this repository holds a whole credential.
func fixtureClaudeToken() string {
	return "sk-ant" + "-oat01-" + "Qm4Zx8Vb2TnL7wWc3RfY9Hd5JpA6sGe1UoBi0NtKx2Cv7Mz4Qa8Ws3Er6Ty1Ui9Op5Lk2Jh7Gf4Ds"
}

// maskAll feeds chunks and returns what is shown, flushed, and the token.
func maskAll(cols int, chunks ...string) (string, string) {
	m := newTokenMasker(cols)
	var out []byte
	for _, c := range chunks {
		out = append(out, m.Write([]byte(c))...)
	}
	out = append(out, m.Flush()...)
	return string(out), string(m.Token())
}

// leaks says whether any run of eight characters of the token is shown.
func leaks(shown, tok string) bool {
	for i := 0; i+8 <= len(tok); i++ {
		if strings.Contains(shown, tok[i:i+8]) {
			return true
		}
	}
	return false
}

func TestMaskerHidesAndCapturesATokenInOneChunk(t *testing.T) {
	tok := fixtureClaudeToken()
	shown, got := maskAll(300, "Your OAuth token:\r\n\r\n"+tok+"\r\n\r\nStore this token securely.\r\n")
	if leaks(shown, tok) || strings.Contains(shown, "sk-ant-oat") {
		t.Fatalf("a piece of the token reached the screen: %q", shown)
	}
	if strings.Count(shown, claudePlanMask) != 1 {
		t.Errorf("the mask should show once: %q", shown)
	}
	if got != tok {
		t.Errorf("captured %d bytes, want the token", len(got))
	}
	if !strings.Contains(shown, "Store this token securely.") || !strings.Contains(shown, "Your OAuth token:") {
		t.Errorf("the words around the token were changed: %q", shown)
	}
}

func TestMaskerHoldsAPrefixAcrossEverySplit(t *testing.T) {
	tok := fixtureClaudeToken()
	text := "head: " + tok + "\r\n\r\ntail"
	want, _ := maskAll(300, text)
	for i := 0; i <= len(text); i++ {
		shown, got := maskAll(300, text[:i], text[i:])
		if shown != want || got != tok {
			t.Fatalf("split at %d: shown %q (want %q), captured ok=%v", i, shown, want, got == tok)
		}
	}
	// and one byte at a time
	var chunks []string
	for i := 0; i < len(text); i++ {
		chunks = append(chunks, text[i:i+1])
	}
	if shown, got := maskAll(300, chunks...); shown != want || got != tok {
		t.Fatalf("byte by byte: %q", shown)
	}
}

func TestMaskerFollowsATokenTheTerminalWrapped(t *testing.T) {
	tok := fixtureClaudeToken()
	for _, cols := range []int{20, 40, 60, 80} {
		var b strings.Builder
		rest := tok
		for len(rest) > 0 {
			n := min(cols, len(rest))
			b.WriteString(rest[:n])
			rest = rest[n:]
			b.WriteString("\r\n")
		}
		b.WriteString("\r\nStore this token securely.\r\n")
		shown, got := maskAll(cols, b.String())
		if leaks(shown, tok) {
			t.Errorf("cols %d: a piece of the wrapped token reached the screen: %q", cols, shown)
		}
		if got != tok {
			t.Errorf("cols %d: the wrapped token was captured as %d bytes, want %d", cols, len(got), len(tok))
		}
		if !strings.Contains(shown, "Store this token securely.") {
			t.Errorf("cols %d: the text after the token was lost: %q", cols, shown)
		}
	}
}

func TestMaskerHidesACutTokenAndKeepsNone(t *testing.T) {
	// a break inside the token on a line that is not full: the capture stops
	// at the break, and the rest is hidden all the same
	tok := fixtureClaudeToken()
	cut := tok[:50] + "\r\n" + tok[50:]
	shown, got := maskAll(300, "x\r\n"+cut+"\r\n\r\ndone")
	if leaks(shown, tok) {
		t.Errorf("a piece of the token reached the screen: %q", shown)
	}
	if got != "" {
		t.Errorf("a token cut by a break this filter could not read as a wrap was kept (%d bytes); a cut token is never kept", len(got))
	}
	if !strings.Contains(shown, "done") {
		t.Errorf("text after the blank line was lost: %q", shown)
	}
}

func TestMaskerToleratesColourCodesAroundAndInsideAToken(t *testing.T) {
	tok := fixtureClaudeToken()
	around := "\x1b[1m" + tok + "\x1b[22m\r\n\r\nStore"
	shown, got := maskAll(300, around)
	if leaks(shown, tok) || got != tok || !strings.Contains(shown, "\x1b[1m") || !strings.HasSuffix(shown, "Store") {
		t.Errorf("around: shown %q captured ok=%v", shown, got == tok)
	}
	inside := tok[:40] + "\x1b[2m" + tok[40:] + "\r\n\r\nafter"
	shown, got = maskAll(300, inside)
	if leaks(shown, tok) || got != tok {
		t.Errorf("inside: shown %q captured ok=%v", shown, got == tok)
	}
	// the same, split at every byte of the escape
	for i := 40; i < 46; i++ {
		shown, got = maskAll(300, inside[:i], inside[i:])
		if leaks(shown, tok) || got != tok {
			t.Errorf("inside, split at %d: shown %q", i, shown)
		}
	}
}

func TestMaskerHidesASecondTokenAndKeepsTheFirst(t *testing.T) {
	tok := fixtureClaudeToken()
	other := "sk-ant" + "-oat01-" + "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	shown, got := maskAll(300, tok+" and "+other+"\r\nexport CLAUDE_CODE_OAUTH_TOKEN="+tok+"\r\n")
	if leaks(shown, tok) || strings.Contains(shown, "ZZZZZZZZ") {
		t.Errorf("a token reached the screen: %q", shown)
	}
	if got != tok {
		t.Errorf("the first token is the one kept")
	}
	if strings.Count(shown, claudePlanMask) != 3 {
		t.Errorf("each token is masked once: %q", shown)
	}
}

func TestMaskerLeavesOtherTextAlone(t *testing.T) {
	for _, in := range []string{
		"sk-ant-api03-notaplantoken and sk-ant-oa",
		"plain text with sk- and ssk-an and sk-ant-oa\r\n",
		"\x1b[31mred\x1b[0m \x1b]0;title\x07 done",
		"",
	} {
		shown, got := maskAll(300, in)
		if shown != in || got != "" {
			t.Errorf("%q came out as %q (token %q)", in, shown, got)
		}
	}
	// a prefix held at the end of the stream is released by Flush
	m := newTokenMasker(300)
	if out := m.Write([]byte("sk-ant-oa")); len(out) != 0 {
		t.Errorf("a possible prefix was shown early: %q", out)
	}
	if out := m.Flush(); string(out) != "sk-ant-oa" {
		t.Errorf("Flush gave %q", out)
	}
}

func TestMaskerStartsAgainInsideReleasedBytes(t *testing.T) {
	tok := fixtureClaudeToken()
	for _, pre := range []string{"s", "sk-", "sk-ant-", "ssk-", "sk-sk-"} {
		shown, got := maskAll(300, pre+tok+"\r\n\r\n")
		if leaks(shown, tok) || got != tok || !strings.HasPrefix(shown, pre) {
			t.Errorf("%q before a token: shown %q captured ok=%v", pre, shown, got == tok)
		}
	}
}

func TestMaskerCapsARunAndWipes(t *testing.T) {
	long := "sk-ant" + "-oat01-" + strings.Repeat("A", 400)
	shown, _ := maskAll(1000, long)
	// the first claudePlanTokenMaxLen characters of a run are hidden; a run
	// longer than any token can be is not swallowed whole
	if got := strings.Count(shown, "A"); got >= 400 || got < 400-claudePlanTokenMaxLen {
		t.Errorf("shown %d of 400 characters; the cap hides at most %d", got, claudePlanTokenMaxLen)
	}
	m := newTokenMasker(300)
	m.Write([]byte(fixtureClaudeToken() + "\r\n\r\n"))
	tok := m.Token()
	if len(tok) == 0 {
		t.Fatal("no token captured")
	}
	m.Wipe()
	if m.Token() != nil {
		t.Error("the token survived Wipe")
	}
	if bytes.Contains(m.tok, []byte("sk-ant")) {
		t.Error("the buffer still holds the token")
	}
}

func TestMaskerRefusesAShortTokenShape(t *testing.T) {
	shown, got := maskAll(300, "sk-ant"+"-oat01-"+"abc\r\n\r\n")
	if got != "" {
		t.Errorf("a token shorter than the service accepts was kept: %q", got)
	}
	if strings.Contains(shown, "abc") {
		t.Errorf("a short token was shown: %q", shown)
	}
}

func TestMaskerKeepsATokenWhoseNextLineStartsWithAShortWord(t *testing.T) {
	tok := fixtureClaudeToken()
	shown, got := maskAll(300, tok+"\r\nStore this token securely.\r\n")
	if got != tok || leaks(shown, tok) {
		t.Errorf("a short word after the token made the capture suspect: ok=%v shown %q", got == tok, shown)
	}
}
