package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

func TestMaskerRefusesATokenLongerThanTheBoundAndHidesItWhole(t *testing.T) {
	long := "sk-ant" + "-oat01-" + strings.Repeat("A", claudePlanTokenMaxLen+88)
	shown, got := maskAll(1000, "x "+long+" y")
	if got != "" {
		t.Errorf("a token over the bound was kept (%d bytes); it is refused, never cut", len(got))
	}
	if strings.Contains(shown, "AAAA") || !strings.HasSuffix(shown, " y") {
		t.Errorf("the long run was not hidden whole, or the text after it was lost: %q", shown)
	}
	m := newTokenMasker(1000)
	m.Write([]byte(long + "\r\n\r\n"))
	m.Flush()
	if _, why := m.Result(); why != "overflow" {
		t.Errorf("reason = %q, want overflow", why)
	}
}

func TestMaskerWipes(t *testing.T) {
	m := newTokenMasker(300)
	m.Write([]byte(fixtureClaudeToken() + "\r\n\r\n"))
	if len(m.Token()) == 0 {
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

// Anything the filter is not sure of is hidden and not kept, with its reason.
func TestMaskerFailsClosedWithAReasonForEveryDoubt(t *testing.T) {
	tok := fixtureClaudeToken()
	for _, tc := range []struct{ name, in, why string }{
		{"an erase inside the token", "a\r\n" + tok[:40] + "\x1b[K" + tok[40:] + "\r\n\r\nafter", "cut"},
		{"a cursor move inside the token", tok[:30] + "\x1b[9G" + tok[30:] + "\r\n\r\n", "cut"},
		{"a break that is not a wrap", tok[:50] + "\r\n" + tok[50:] + "\r\n\r\n", "cut"},
		{"too short to be a token", "sk-ant" + "-oat01-" + "abc\r\n\r\n", "unreadable"},
		{"a prefix at the end of the output", "token: sk-ant-oa", "partial"},
		{"a prefix of three characters at the end", "ask-", "partial"},
	} {
		m := newTokenMasker(300)
		out := append(m.Write([]byte(tc.in)), m.Flush()...)
		got, why := m.Result()
		if got != nil || why != tc.why {
			t.Errorf("%s: kept=%v reason=%q, want nothing kept and %q", tc.name, got != nil, why, tc.why)
		}
		if tc.why != "partial" && leaks(string(out), tok[13:]) {
			t.Errorf("%s: a piece of the token reached the screen: %q", tc.name, out)
		}
	}
	// and the doubts that are not doubts
	for _, in := range []string{
		"\x1b[1m" + tok + "\x1b[22m\r\n\r\n",         // a colour code around
		tok[:40] + "\x1b[2m" + tok[40:] + "\r\n\r\n", // a colour code inside
		tok + "\x1b[9Gto\x1b[12Gnext\r\n",            // words placed by a cursor move after it
		tok + "\r\nStore this token securely.\r\n",   // a short word on the next line
	} {
		if got, why := func() ([]byte, string) {
			m := newTokenMasker(300)
			m.Write([]byte(in))
			m.Flush()
			return m.Result()
		}(); string(got) != tok || why != "" {
			t.Errorf("%q: kept=%v reason=%q", in, string(got) == tok, why)
		}
	}
}

func TestMaskerDoesNotKeepAShortToken(t *testing.T) {
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

// ---- the real screen -------------------------------------------------------------

// recordedSetupFrames are the bytes the real claude setup-token wrote, from
// its start to the sign-in URL and the code prompt, recorded offline with no
// sign-in and so no token (testdata/setup-token). The screen after a token is
// NOT recorded: no token was ever made. The tail used below is written by
// hand in the same style (words placed by cursor moves, a colour code around
// the token) and is labelled as synthetic.
func recordedSetupFrames(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/setup-token/frames-2.1.316-300.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Frames []string `json:"frames"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Frames) < 10 {
		t.Fatalf("the recording is unreadable: %v", err)
	}
	var out [][]byte
	for _, h := range d.Frames {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestMaskerLeavesTheRecordedRealScreenUntouched(t *testing.T) {
	var all []byte
	m := newTokenMasker(300)
	var shown []byte
	for _, f := range recordedSetupFrames(t) {
		all = append(all, f...)
		shown = append(shown, m.Write(f)...)
	}
	shown = append(shown, m.Flush()...)
	if !bytes.Equal(shown, all) {
		t.Fatalf("the filter changed a screen that holds no token (%d bytes in, %d out)", len(all), len(shown))
	}
	if tok, why := m.Result(); tok != nil || why != "" {
		t.Errorf("a token was read from a screen with none: %q", why)
	}
	if !bytes.Contains(shown, []byte("https://claude.com/cai/oauth/authorize")) || !bytes.Contains(shown, []byte("Paste")) {
		t.Error("the recording lacks the sign-in URL or the code prompt")
	}
}

func TestMaskerHidesATokenAfterTheRecordedRealScreenAtEverySplit(t *testing.T) {
	tok := fixtureClaudeToken()
	// SYNTHETIC: what Claude Code would print once a sign-in finished, in the
	// style of the recorded screen: words placed by cursor moves, the token in
	// a colour, a blank line, and the closing words
	tail := "\r\r\n\x1b[2G\x1b[1mYour\x1b[7GOAuth\x1b[13Gtoken\x1b[19G(valid\x1b[26Gfor\x1b[30G1\x1b[32Gyear):\x1b[22m\r\r\n\r\r\n" +
		"\x1b[2G\x1b[38;5;231m" + tok + "\x1b[39m\r\r\n\r\r\n\x1b[2GStore\x1b[8Gthis\x1b[13Gtoken\x1b[19Gsecurely.\r\r\n"
	var stream []byte
	for _, f := range recordedSetupFrames(t) {
		stream = append(stream, f...)
	}
	stream = append(stream, tail...)
	check := func(label string, chunks [][]byte) {
		m := newTokenMasker(300)
		var shown []byte
		for _, c := range chunks {
			shown = append(shown, m.Write(c)...)
		}
		shown = append(shown, m.Flush()...)
		got, why := m.Result()
		if leaks(string(shown), tok) || bytes.Contains(shown, []byte("sk-ant-oat")) {
			t.Fatalf("%s: a piece of the token reached the screen", label)
		}
		if string(got) != tok || why != "" {
			t.Fatalf("%s: kept=%v reason=%q", label, string(got) == tok, why)
		}
		if !bytes.Contains(shown, []byte("Store")) || !bytes.Contains(shown, []byte(claudePlanMask)) {
			t.Fatalf("%s: the words around the token were lost", label)
		}
	}
	for cut := 0; cut <= len(tail); cut++ {
		split := len(stream) - len(tail) + cut
		check(fmt.Sprintf("split at %d", cut), [][]byte{stream[:split], stream[split:]})
	}
	var bytewise [][]byte
	for i := range stream {
		bytewise = append(bytewise, stream[i:i+1])
	}
	check("byte by byte", bytewise)
	// and in the chunks the child wrote them, with the tail as one more read
	check("as recorded", append(recordedSetupFrames(t), []byte(tail)))
}

// ---- codes inside the prefix --------------------------------------------------------

// A colour code, a cursor move or an erase can fall inside "sk-ant-oat" as
// readily as inside the rest of a token. The prefix is read across them, and a
// token whose prefix was completed across a code that is not a colour is hidden
// and not kept.
func TestMaskerReadsThePrefixAcrossCodesAndHidesTheToken(t *testing.T) {
	tok := fixtureClaudeToken()
	for _, tc := range []struct {
		name, code string
		kept       bool
	}{
		{"a colour code", "\x1b[2m", true},
		{"a colour reset", "\x1b[0m", true},
		{"an erase", "\x1b[K", false},
		{"a cursor move", "\x1b[9G", false},
		{"a link", "\x1b]8;;\x07", false},
		{"a two byte sequence", "\x1b7", false},
	} {
		for at := 1; at < len(claudePlanTokenPrefix); at++ {
			in := tok[:at] + tc.code + tok[at:] + "\r\n\r\nafter"
			// at every split of the stream, and byte by byte
			splits := [][]string{{in}}
			for cut := 1; cut < len(in); cut++ {
				splits = append(splits, []string{in[:cut], in[cut:]})
			}
			var bytewise []string
			for i := range in {
				bytewise = append(bytewise, in[i:i+1])
			}
			splits = append(splits, bytewise)
			for _, chunks := range splits {
				m := newTokenMasker(300)
				var shown []byte
				for _, c := range chunks {
					shown = append(shown, m.Write([]byte(c))...)
				}
				shown = append(shown, m.Flush()...)
				got, why := m.Result()
				if leaks(string(shown), tok[13:]) || bytes.Contains(shown, []byte("oat01")) {
					t.Fatalf("%s inside the prefix at %d: a piece of the token reached the screen: %q", tc.name, at, shown)
				}
				if tc.kept && string(got) != tok || !tc.kept && (got != nil || why != "cut") {
					t.Fatalf("%s inside the prefix at %d: kept=%v reason=%q", tc.name, at, got != nil, why)
				}
				if !bytes.Contains(shown, []byte("after")) || !bytes.Contains(shown, []byte(claudePlanMask)) {
					t.Fatalf("%s at %d: the words around the token were lost: %q", tc.name, at, shown)
				}
			}
		}
	}
}

func TestMaskerShowsABitOfTextThatOnlyLooksLikeThePrefixExactlyAsItCame(t *testing.T) {
	for _, in := range []string{"sk-an\x1b[31mgry", "sk\x1b[Kate", "sk-ant-o\x1b]0;t\x07x", "ask-\x1b[0m"} {
		m := newTokenMasker(300)
		out := append(m.Write([]byte(in)), m.Flush()...)
		if string(out) != in {
			t.Errorf("%q came out as %q", in, out)
		}
	}
}
