// ks cruise verify: check a Cruise receipt's Ed25519 signature offline
// against the public key list KeepState publishes. Nothing is sent anywhere
// but one read of the key list (and none at all with --keys-file or
// --public-key), no sign-in and no service key are needed, and no model is
// called.
//
// The signature is over the receipt's canonical bytes: what Python's
// json.dumps(record without hmac_sha256 and sig, sort_keys=True,
// separators=(",", ":")) writes, with its default ensure_ascii. Go's
// encoding/json differs from that in ways that change the bytes (it escapes
// < > & and U+2028/U+2029, writes UTF-8 where Python writes \uXXXX, spells
// numbers its own way and cannot hold a lone surrogate), so this file carries
// a small reader and writer that reproduce Python's: the reader keeps the
// distinction between an integer and a float and the exact code units of a
// string, and the writer spells every value as Python does. The shared
// vectors (testdata/receipt-verify) hold it to the service's own verifier.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const receiptKeysURL = "https://ctl.keepstate.ai/.well-known/keepstate-receipt-keys.json"

// ---------------------------------------------------------------------
// A reader for JSON the way Python's json.loads reads it
// ---------------------------------------------------------------------

// pyInt is a JSON integer: Python keeps any number written without a
// fraction or exponent as an arbitrary-size int, and writes it back digit for
// digit.
type pyInt struct{ digits string }

type pyReader struct {
	b []byte
	i int
}

// parsePyJSON reads one JSON document as Python's json.loads would, and
// refuses what it refuses (a byte order mark, a trailing comma, a raw control
// character in a string, leading zeros, invalid UTF-8). A string keeps lone
// surrogates (\ud800 on its own) as the three bytes a surrogate takes in
// UTF-8, which Go's own strings would have replaced.
func parsePyJSON(b []byte) (any, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("not valid UTF-8")
	}
	r := &pyReader{b: b}
	r.ws()
	v, err := r.value(0)
	if err != nil {
		return nil, err
	}
	r.ws()
	if r.i != len(r.b) {
		return nil, fmt.Errorf("extra data at byte %d", r.i)
	}
	return v, nil
}

func (r *pyReader) ws() {
	for r.i < len(r.b) && (r.b[r.i] == ' ' || r.b[r.i] == '\t' || r.b[r.i] == '\n' || r.b[r.i] == '\r') {
		r.i++
	}
}

func (r *pyReader) fail(what string) error { return fmt.Errorf("%s at byte %d", what, r.i) }

func (r *pyReader) value(depth int) (any, error) {
	if depth > 900 {
		return nil, r.fail("nested too deeply")
	}
	if r.i >= len(r.b) {
		return nil, r.fail("unexpected end")
	}
	switch c := r.b[r.i]; {
	case c == '{':
		return r.object(depth)
	case c == '[':
		return r.array(depth)
	case c == '"':
		return r.str()
	case c == '-' || (c >= '0' && c <= '9'):
		if strings.HasPrefix(string(r.b[r.i:min(len(r.b), r.i+9)]), "-Infinity") {
			r.i += 9
			return math.Inf(-1), nil
		}
		return r.number()
	}
	for lit, val := range map[string]any{"true": true, "false": false, "null": nil, "NaN": math.NaN(), "Infinity": math.Inf(1)} {
		if strings.HasPrefix(string(r.b[r.i:min(len(r.b), r.i+len(lit))]), lit) {
			r.i += len(lit)
			return val, nil
		}
	}
	return nil, r.fail("unexpected character")
}

func (r *pyReader) object(depth int) (any, error) {
	r.i++
	m := map[string]any{}
	r.ws()
	if r.i < len(r.b) && r.b[r.i] == '}' {
		r.i++
		return m, nil
	}
	for {
		r.ws()
		if r.i >= len(r.b) || r.b[r.i] != '"' {
			return nil, r.fail("expected a string key")
		}
		k, err := r.str()
		if err != nil {
			return nil, err
		}
		r.ws()
		if r.i >= len(r.b) || r.b[r.i] != ':' {
			return nil, r.fail("expected ':'")
		}
		r.i++
		r.ws()
		v, err := r.value(depth + 1)
		if err != nil {
			return nil, err
		}
		m[k.(string)] = v // a repeated key: the last one wins, as in Python
		r.ws()
		if r.i >= len(r.b) {
			return nil, r.fail("unexpected end")
		}
		switch r.b[r.i] {
		case ',':
			r.i++
		case '}':
			r.i++
			return m, nil
		default:
			return nil, r.fail("expected ',' or '}'")
		}
	}
}

func (r *pyReader) array(depth int) (any, error) {
	r.i++
	l := []any{}
	r.ws()
	if r.i < len(r.b) && r.b[r.i] == ']' {
		r.i++
		return l, nil
	}
	for {
		r.ws()
		v, err := r.value(depth + 1)
		if err != nil {
			return nil, err
		}
		l = append(l, v)
		r.ws()
		if r.i >= len(r.b) {
			return nil, r.fail("unexpected end")
		}
		switch r.b[r.i] {
		case ',':
			r.i++
		case ']':
			r.i++
			return l, nil
		default:
			return nil, r.fail("expected ',' or ']'")
		}
	}
}

var pyNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?`)

func (r *pyReader) number() (any, error) {
	m := pyNumber.Find(r.b[r.i:])
	if m == nil {
		return nil, r.fail("not a number")
	}
	r.i += len(m)
	s := string(m)
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, r.fail("not a number")
		}
		return f, nil
	}
	digits := strings.TrimPrefix(s, "-")
	if len(digits) > 4300 { // Python's limit on integer text
		return nil, r.fail("integer with more than 4300 digits")
	}
	if digits == "0" {
		return pyInt{"0"}, nil // -0 is the integer 0
	}
	return pyInt{s}, nil
}

func (r *pyReader) str() (any, error) {
	r.i++ // the opening quote
	var out []byte
	for {
		if r.i >= len(r.b) {
			return nil, r.fail("unterminated string")
		}
		c := r.b[r.i]
		switch {
		case c == '"':
			r.i++
			return string(out), nil
		case c < 0x20:
			return nil, r.fail("control character in a string")
		case c != '\\':
			out = append(out, c)
			r.i++
			continue
		}
		r.i++
		if r.i >= len(r.b) {
			return nil, r.fail("unterminated string")
		}
		e := r.b[r.i]
		r.i++
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, 8)
		case 'f':
			out = append(out, 12)
		case 'n':
			out = append(out, 10)
		case 'r':
			out = append(out, 13)
		case 't':
			out = append(out, 9)
		case 'u':
			cp, ok := r.hex4()
			if !ok {
				return nil, r.fail("bad \\u escape")
			}
			if cp >= 0xD800 && cp < 0xDC00 && r.i+1 < len(r.b) && r.b[r.i] == '\\' && r.b[r.i+1] == 'u' {
				save := r.i
				r.i += 2
				if lo, ok := r.hex4(); ok && lo >= 0xDC00 && lo < 0xE000 {
					cp = 0x10000 + (cp-0xD800)<<10 + (lo - 0xDC00)
				} else {
					r.i = save
				}
			}
			out = appendWTF8(out, cp)
		default:
			return nil, r.fail("bad escape")
		}
	}
}

func (r *pyReader) hex4() (rune, bool) {
	if r.i+4 > len(r.b) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(r.b[r.i:r.i+4]), 16, 32)
	if err != nil || strings.ContainsAny(string(r.b[r.i:r.i+4]), "+-_ x") {
		return 0, false
	}
	r.i += 4
	return rune(n), true
}

// appendWTF8 is utf8.AppendRune, except that a surrogate is kept as its own
// three bytes instead of becoming U+FFFD.
func appendWTF8(b []byte, cp rune) []byte {
	if cp >= 0xD800 && cp < 0xE000 {
		return append(b, 0xE0|byte(cp>>12), 0x80|byte(cp>>6)&0x3F, 0x80|byte(cp)&0x3F)
	}
	return utf8.AppendRune(b, cp)
}

// ---------------------------------------------------------------------
// A writer for JSON the way Python's json.dumps(sort_keys=True,
// separators=(",", ":")) writes it, ensure_ascii on
// ---------------------------------------------------------------------

func pyCanonical(v any) ([]byte, error) {
	var b []byte
	return pyWrite(b, v)
}

func pyWrite(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		if x {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case pyInt:
		return append(b, x.digits...), nil
	case float64:
		return append(b, pyFloat(x)...), nil
	case string:
		return pyString(b, x), nil
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = pyWrite(b, e); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// byte order of UTF-8 (surrogates included) is code point order,
		// which is how Python sorts str keys
		sort.Strings(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = pyString(b, k)
			b = append(b, ':')
			var err error
			if b, err = pyWrite(b, x[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	return nil, fmt.Errorf("a value of type %T has no canonical form", v)
}

// pyFloat spells a float as Python's repr does: the shortest digits that
// read back the same, fixed notation for exponents from -5 to 15 and
// scientific ("1e+16", "1.5e-07") outside it, ".0" on a whole number, and
// NaN, Infinity and -Infinity as json.dumps writes them.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	mant, exps, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(exps)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1
	if decpt <= -4 || decpt > 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		e := decpt - 1
		es := "+"
		if e < 0 {
			es, e = "-", -e
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, e)
	}
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	}
	return sign + digits[:decpt] + "." + digits[decpt:]
}

// pyString writes a string with ensure_ascii: every character outside space
// to tilde is \uXXXX (lowercase hex, a pair for a character above U+FFFF),
// and the quote, the backslash and \b \f \n \r \t keep their short forms.
func pyString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			i++
			switch {
			case c == '"':
				b = append(b, '\\', '"')
			case c == '\\':
				b = append(b, '\\', '\\')
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c == 8:
				b = append(b, '\\', 'b')
			case c == 12:
				b = append(b, '\\', 'f')
			case c >= 0x20 && c <= 0x7e:
				b = append(b, c)
			default:
				b = append(b, fmt.Sprintf("\\u%04x", c)...)
			}
			continue
		}
		var cp rune
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 { // a lone surrogate (WTF-8)
			cp = rune(c&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
			i += 3
		} else {
			var n int
			cp, n = utf8.DecodeRuneInString(s[i:])
			i += n
		}
		if cp >= 0x10000 {
			c := cp - 0x10000
			b = append(b, fmt.Sprintf("\\u%04x\\u%04x", 0xD800+(c>>10), 0xDC00+(c&0x3FF))...)
		} else {
			b = append(b, fmt.Sprintf("\\u%04x", cp)...)
		}
	}
	return append(b, '"')
}

// receiptSignedBytes is the byte string the signature covers: the receipt
// without hmac_sha256 and sig, in canonical form.
func receiptSignedBytes(rec map[string]any) ([]byte, error) {
	own := make(map[string]any, len(rec))
	for k, v := range rec {
		if k != "hmac_sha256" && k != "sig" {
			own[k] = v
		}
	}
	return pyCanonical(own)
}

// ---------------------------------------------------------------------
// The check: the same answers as the service's scripts/verify-receipt.py
// ---------------------------------------------------------------------

func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case pyInt:
		return x.digits != "0"
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case pyInt:
		return x.digits
	case float64:
		return strings.NewReplacer("NaN", "nan", "Infinity", "inf").Replace(pyFloat(x))
	}
	b, _ := pyCanonical(v)
	return string(b)
}

func pyEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case pyInt:
		y, ok := b.(pyInt)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	}
	return false
}

var strictBase64 = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

func decodeBase64(s string) ([]byte, error) {
	if !strictBase64.MatchString(s) {
		return nil, errors.New("not base64")
	}
	return base64.StdEncoding.DecodeString(s)
}

// decodeReceiptKey reads a public key written as 64 hex characters or as
// base64.
func decodeReceiptKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return decodeBase64(s)
}

var keyTime = regexp.MustCompile(`^([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})T([0-9]{1,2}):([0-9]{1,2}):([0-9]{1,2})Z$`)

// keyUnix reads a key list's RFC 3339 UTC time (YYYY-MM-DDTHH:MM:SSZ).
func keyUnix(v any) (int64, error) {
	s, _ := v.(string)
	m := keyTime.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("the key list has a time that is not like 2026-01-31T00:00:00Z: %s", pyStr(v))
	}
	n := make([]int, 7)
	for i := 1; i <= 6; i++ {
		n[i], _ = strconv.Atoi(m[i])
	}
	t := time.Date(n[1], time.Month(n[2]), n[3], n[4], n[5], n[6], 0, time.UTC)
	if n[2] < 1 || n[2] > 12 || n[4] > 23 || n[5] > 59 || n[6] > 61 || int(t.Month()) != n[2] || t.Day() != n[3] {
		return 0, fmt.Errorf("the key list has a time that is not like 2026-01-31T00:00:00Z: %s", pyStr(v))
	}
	return t.Unix(), nil
}

// lessThan and greaterThan compare a receipt's time (an integer or a float)
// with a key's boundary the way Python compares them.
func receiptTime(v any) (isNum bool, less func(int64) bool, greater func(int64) bool) {
	switch x := v.(type) {
	case pyInt:
		n, _ := new(big.Int).SetString(x.digits, 10)
		return true,
			func(u int64) bool { return n.Cmp(big.NewInt(u)) < 0 },
			func(u int64) bool { return n.Cmp(big.NewInt(u)) > 0 }
	case float64:
		return true, func(u int64) bool { return x < float64(u) }, func(u int64) bool { return x > float64(u) }
	}
	return false, nil, nil
}

// checkReceipt is verify-receipt.py's check: (exit code, words). keys is the
// "keys" list of the published document. Exit 0 verified; 1 not verified;
// 3 no Ed25519 signature (service-verifiable only).
func checkReceipt(rec map[string]any, keys []any, trustBeforeRevocation bool) (int, string) {
	if alg, _ := rec["sig_alg"].(string); alg != "ed25519" || !pyTruthy(rec["sig"]) {
		return 3, "no Ed25519 signature: service-verifiable only (an old receipt, or one made before the key was placed)"
	}
	kid := rec["key_id"]
	var key map[string]any
	for _, k := range keys {
		if km, ok := k.(map[string]any); ok && pyEqual(km["key_id"], kid) {
			key = km
			break
		}
	}
	if key == nil {
		return 1, fmt.Sprintf("key_id %s is not in the key list", pyStr(kid))
	}
	pubText, ok1 := key["public_key"].(string)
	sigText, ok2 := rec["sig"].(string)
	var pub, sig []byte
	var err1, err2 error
	if ok1 {
		pub, err1 = decodeReceiptKey(pubText)
	}
	if ok2 {
		sig, err2 = decodeBase64(sigText)
	}
	if !ok1 || !ok2 || err1 != nil || err2 != nil {
		return 1, "the key or the signature is not valid base64"
	}
	sum := sha256.Sum256(pub)
	if kidText, _ := kid.(string); len(pub) != 32 || hex.EncodeToString(sum[:]) != kidText {
		return 1, "the listed key is not 32 bytes or does not match its key_id"
	}
	signed, err := receiptSignedBytes(rec)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(pub), signed, sig) {
		return 1, "the signature does not verify: the receipt was altered or is signed under another key"
	}
	tv, present := rec["ts_unix"]
	if !present {
		tv = rec["measured_at"]
	}
	isNum, less, greater := receiptTime(tv)
	revoked := key["revoked_at"]
	if isNum {
		if pyTruthy(key["valid_from"]) {
			u, err := keyUnix(key["valid_from"])
			if err != nil {
				return 1, err.Error()
			}
			if less(u) {
				return 1, "the receipt is older than its key's valid_from"
			}
		}
		if pyTruthy(key["valid_to"]) {
			u, err := keyUnix(key["valid_to"])
			if err != nil {
				return 1, err.Error()
			}
			if greater(u) {
				return 1, "the receipt is newer than its key's valid_to"
			}
		}
		if pyTruthy(revoked) {
			before := false
			if trustBeforeRevocation {
				u, err := keyUnix(revoked)
				if err != nil {
					return 1, err.Error()
				}
				before = less(u)
			}
			if !before {
				return 1, fmt.Sprintf("the key was revoked at %s", pyStr(revoked))
			}
		}
	} else if pyTruthy(revoked) {
		return 1, fmt.Sprintf("the key was revoked at %s and the receipt has no time to compare", pyStr(revoked))
	}
	return 0, "verified: Ed25519 under key " + string([]byte(pyStr(kid))[:min(16, len(pyStr(kid)))])
}

// parseKeyList reads the published key document and returns its "keys".
func parseKeyList(b []byte) ([]any, error) {
	v, err := parsePyJSON(b)
	if err != nil {
		return nil, err
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the key list is not a JSON object")
	}
	keys, present := doc["keys"]
	if !present {
		return []any{}, nil
	}
	l, ok := keys.([]any)
	if !ok {
		return nil, errors.New("the key list's keys are not a list")
	}
	return l, nil
}

// ---------------------------------------------------------------------
// The verb
// ---------------------------------------------------------------------

func receiptInputError(err error) {
	die(&cliError{Code: exitUsage, Kind: "unreadable_input", Message: "cannot read the inputs: " + err.Error(), WorkStarted: workNo})
}

func fetchReceiptKeys(raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http or https address", raw)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(raw)
	if err != nil {
		return nil, fmt.Errorf("the key list could not be fetched: %v", errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the key list answered HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("the key list is larger than 1 MiB")
	}
	return b, nil
}

func cruiseVerify(inv *Invocation) {
	sources := 0
	for _, f := range []string{"keys-url", "keys-file", "public-key"} {
		if inv.Set(f) {
			sources++
		}
	}
	if sources > 1 {
		die(&UsageError{Cmd: inv.Cmd, Message: "name one place for the keys: --keys-url, --keys-file or --public-key"})
	}
	raw, err := os.ReadFile(inv.Arg(0))
	if err != nil {
		receiptInputError(err)
	}
	v, err := parsePyJSON(raw)
	if err != nil {
		receiptInputError(fmt.Errorf("%s: %v", inv.Arg(0), err))
	}
	rec, ok := v.(map[string]any)
	if !ok {
		receiptInputError(fmt.Errorf("%s: the receipt is not a JSON object", inv.Arg(0)))
	}
	var keys []any
	switch {
	case inv.Set("public-key"):
		pub, err := decodeReceiptKey(inv.Str("public-key"))
		if err != nil || len(pub) != 32 {
			receiptInputError(errors.New("--public-key is not 32 bytes in base64 or hex"))
		}
		sum := sha256.Sum256(pub)
		keys = []any{map[string]any{"key_id": hex.EncodeToString(sum[:]), "public_key": base64.StdEncoding.EncodeToString(pub)}}
	case inv.Set("keys-file"):
		b, err := os.ReadFile(inv.Str("keys-file"))
		if err == nil {
			keys, err = parseKeyList(b)
		}
		if err != nil {
			receiptInputError(err)
		}
	default:
		u := receiptKeysURL
		if inv.Set("keys-url") {
			u = inv.Str("keys-url")
		}
		b, err := fetchReceiptKeys(u)
		if err == nil {
			keys, err = parseKeyList(b)
		}
		if err != nil {
			receiptInputError(err)
		}
	}
	code, words := checkReceipt(rec, keys, inv.Bool("trust-before-revocation"))
	words = controlChars.ReplaceAllString(words, "")
	data := map[string]any{"verified": code == 0, "exit": code, "message": words}
	if code == 0 {
		if kid, _ := rec["key_id"].(string); kid != "" {
			data["key_id"] = kid
		}
	}
	if code == 3 {
		data["service_verifiable_only"] = true
	}
	if out.json {
		emit(data, func() {})
	} else if code == 0 {
		fmt.Println("ks cruise verify: " + words)
	} else {
		fmt.Fprintln(os.Stderr, "ks cruise verify: "+words)
	}
	os.Exit(code)
}
