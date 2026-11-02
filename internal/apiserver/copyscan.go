package apiserver

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// minScannableSecret is the shortest secret value a copy can be scanned
// for: a shorter one matches by chance, and workstream's own scan holds
// the same floor.
const minScannableSecret = 8

// A piece of a secret (a line, a field) is a weak needle only if it looks
// like a credential rather than structure: minPiece bytes at least of a
// token's characters, and either letters of both cases and digits with
// minPieceEntropy bits a byte, hex digits with letters and digits, a
// UUID, or lower case and digits interleaved as a random token's are,
// with no dot or slash. Then a kubeconfig's "apiVersion: v1" or
// "server: https://<id>.gr7.eu-west-1.eks.amazonaws.com", a path, a name
// such as "kubelet-client-2024-01-01" or a .netrc's "machine github.com"
// never count.
const (
	minPiece        = 16
	minLowerPiece   = 20
	minPieceEntropy = 3.0
	// minLowerEntropy, and a letter-digit switch every lowerSwitchEvery
	// bytes, for a token of lower case and digits.
	minLowerEntropy  = 3.5
	lowerSwitchEvery = 8
)

// maxFlatHold bounds the bytes a scan holds back past its longest
// pattern, for the newlines a wrapped encoding of one may carry.
const maxFlatHold = 64 << 10

var (
	tokenShape = regexp.MustCompile(`^[A-Za-z0-9+/=_.-]+$`)
	hexShape   = regexp.MustCompile(`^[0-9A-Fa-f]+$`)
	uuidShape  = regexp.MustCompile(`^[0-9A-Fa-f]{8}-([0-9A-Fa-f]{4}-){3}[0-9A-Fa-f]{12}$`)
	// hostShape is a DNS name: labels, the last a top-level domain's.
	hostShape = regexp.MustCompile(`^([A-Za-z0-9-]{1,63}\.)+[A-Za-z]{2,24}$`)
	// envLine is a line of an env file, KEY=value.
	envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=\s*(.*?)\s*$`)
	// secretKey is a key whose name says its value is a secret.
	secretKey = regexp.MustCompile(`(?i)PASS|PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|AUTH|PRIVATE|APIKEY|API_KEY`)
	// jsonUnescape is a JSON string's escapes a PEM key in a JSON file, as
	// a service account's private_key, is written with.
	jsonUnescape = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\/`, "/", `\"`, `"`, `\\`, `\`)
)

// namedSecret is one secret value given to an environment, and what a
// refusal calls it: "$NAME" for an environment variable, a path for a
// file. The value lives in memory for one call.
type namedSecret struct {
	name  string
	value []byte
}

// scanPattern is one form a secret value may take in what is copied.
// A strong one is the value itself; a weak one a piece of it, which a
// human must judge (docs/decisions/copy-out-scan.md).
type scanPattern struct {
	name, form string
	b          []byte
	strong     bool
}

// secretPatterns is every form scanned for of secrets, and the names of
// those too short to scan for. Strong needles are each value whole and
// trimmed and the credentials it holds as fields (strongFields); weak ones
// its credential-shaped lines and pieces. A JSON value counts each string
// it holds, unescaped, as a value too. Each needle is looked for raw, in
// hex, in base64 and base64url at each byte alignment, URL-encoded and
// JSON-escaped; a form both strong and weak is strong.
func secretPatterns(secrets []namedSecret) (patterns []scanPattern, tooShort []string) {
	seen := map[string]bool{}
	add := func(name string, n []byte, strong bool) {
		if len(n) < minScannableSecret {
			return
		}
		for _, f := range forms(n) {
			if len(f.b) < minScannableSecret || seen[string(f.b)] {
				continue
			}
			seen[string(f.b)] = true
			patterns = append(patterns, scanPattern{name: name, form: f.form, b: f.b, strong: strong})
		}
	}
	var scannable []namedSecret
	for _, s := range secrets {
		if len(bytes.TrimSpace(s.value)) < minScannableSecret {
			tooShort = append(tooShort, s.name)
			continue
		}
		scannable = append(scannable, s)
		for _, n := range strongNeedles(s.value) {
			add(s.name, n, true)
		}
	}
	for _, s := range scannable {
		values := append(unescaped(s.value), jsonStrings(s.value)...)
		for _, v := range values {
			for _, p := range pieces(v) {
				add(s.name, p, false)
			}
		}
	}
	return patterns, tooShort
}

// strongNeedles are what of a value is a credential itself: the value
// whole and trimmed, a PEM block's base64 body, and its fields
// (strongFields); in a JSON value, each string it holds that is a PEM
// block or a token (tokenLike), as the program reading it gets it.
func strongNeedles(v []byte) [][]byte {
	out := [][]byte{v, bytes.TrimSpace(v)}
	for _, u := range unescaped(v) {
		out = append(out, pemBody(u))
		out = append(out, strongFields(u)...)
	}
	for _, s := range jsonStrings(v) {
		switch {
		case bytes.Contains(s, []byte("-----BEGIN ")):
			out = append(out, s, bytes.TrimSpace(s), pemBody(s))
		case tokenLike(s):
			out = append(out, s)
		}
	}
	return out
}

// tokenLike reports whether a field's value is a credential by its shape
// alone: what a weak piece must be, and not a DNS name nor a JWT's header.
func tokenLike(p []byte) bool {
	return credentialShaped(p) && !hostShape.Match(p) && !joseHeader(p)
}

// strongFields are the credentials a value holds as fields: a .netrc's
// password, and the value of each KEY=value line whose key's name says
// secret, whatever they look like; a .netrc's login and any other value
// only if it is tokenLike. So "AWS_REGION=us-east-1" or "login
// ci-bot-2024" is none, which would refuse every copy naming them.
func strongFields(v []byte) [][]byte {
	var out [][]byte
	field := func(b []byte, always bool) {
		if always || tokenLike(b) {
			out = append(out, b)
		}
	}
	words := strings.Fields(string(v))
	for i, w := range words[:max(len(words)-1, 0)] {
		switch w {
		case "password":
			field([]byte(words[i+1]), true)
		case "login":
			field([]byte(words[i+1]), false)
		}
	}
	for line := range strings.Lines(string(v)) {
		if m := envLine.FindStringSubmatch(line); m != nil {
			field([]byte(strings.Trim(m[2], `"'`)), secretKey.MatchString(m[1]))
		}
	}
	return out
}

// jsonStrings are the strings a JSON value holds, its keys' values at any
// depth, unescaped; none for a value that is not JSON.
func jsonStrings(v []byte) [][]byte {
	var doc any
	if err := json.Unmarshal(v, &doc); err != nil {
		return nil
	}
	var out [][]byte
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			out = append(out, []byte(x))
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// unescaped is v, and v with its JSON escapes undone when it has a
// line break escaped: a JSON file holding a PEM key or a config.
func unescaped(v []byte) [][]byte {
	if !bytes.Contains(v, []byte(`\n`)) {
		return [][]byte{v}
	}
	return [][]byte{v, []byte(jsonUnescape.Replace(string(v)))}
}

// pemBody is the base64 of a PEM block in v, its lines joined, nil for
// none.
func pemBody(v []byte) []byte {
	var body []byte
	inside := false
	for line := range bytes.Lines(v) {
		line = bytes.TrimSpace(line)
		switch {
		// Inside JSON a marker is not at its line's start.
		case bytes.Contains(line, []byte("-----BEGIN ")):
			inside = true
		case bytes.Contains(line, []byte("-----END ")):
			inside = false
		case inside && !bytes.Contains(line, []byte(":")):
			body = append(body, line...)
		}
	}
	return body
}

// pieces are the credential-shaped lines and pieces of v: each line, and
// each field split on what separates the parts of a URL, a path or a
// config line. A DNS name is no piece, nor any of its labels, and a JWT's
// header, which every token of its algorithm shares, is none either.
func pieces(v []byte) [][]byte {
	var out [][]byte
	for line := range bytes.Lines(v) {
		if l := bytes.TrimSpace(line); credentialShaped(l) && !hostShape.Match(l) {
			out = append(out, l)
		}
	}
	for _, seg := range bytes.FieldsFunc(v, isPieceSeparator) {
		if hostShape.Match(seg) {
			continue
		}
		for _, p := range bytes.Split(seg, []byte(".")) {
			if credentialShaped(p) && !joseHeader(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// isPieceSeparator splits a value into segments: a DNS name stays whole,
// to be told apart, and is split on its dots after.
func isPieceSeparator(r rune) bool {
	return strings.ContainsRune(" \t\r\n\"'`:/,;=@&?<>()[]{}", r)
}

// joseHeader reports whether p is a JWT's header: base64url of a JSON
// object naming its algorithm.
func joseHeader(p []byte) bool {
	if !bytes.HasPrefix(p, []byte("eyJ")) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(string(p), "="))
	if err != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(b, &h) != nil {
		return false
	}
	_, ok := h["alg"]
	return ok
}

// credentialShaped reports whether p looks like a credential: a token, a
// key's line, a hex key, a UUID; not a word, a host, a path or a number.
// Entropy is judged per alphabet: hex has 16 symbols, so a hex key is
// told by its mix of letters and digits rather than its bits a byte.
func credentialShaped(p []byte) bool {
	if len(p) < minPiece || !tokenShape.Match(p) {
		return false
	}
	upper, lower, digit := false, false, false
	for _, c := range p {
		upper = upper || 'A' <= c && c <= 'Z'
		lower = lower || 'a' <= c && c <= 'z'
		digit = digit || '0' <= c && c <= '9'
	}
	switch {
	case hexShape.Match(p):
		return digit && (upper || lower)
	case uuidShape.Match(p):
		return true
	case upper && lower && digit:
		return entropy(p) >= minPieceEntropy
	case !upper && lower && digit && len(p) >= minLowerPiece && !bytes.ContainsAny(p, "./"):
		return letterDigitSwitches(p)*lowerSwitchEvery >= len(p) && entropy(p) >= minLowerEntropy
	}
	return false
}

// letterDigitSwitches counts where a letter follows a digit or a digit a
// letter: often in a random token, seldom in a name or a date.
func letterDigitSwitches(p []byte) int {
	isDigit := func(c byte) bool { return '0' <= c && c <= '9' }
	isLetter := func(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
	n := 0
	for i := 1; i < len(p); i++ {
		a, b := p[i-1], p[i]
		if isDigit(a) && isLetter(b) || isLetter(a) && isDigit(b) {
			n++
		}
	}
	return n
}

// entropy is p's Shannon entropy in bits a byte.
func entropy(p []byte) float64 {
	var counts [256]int
	for _, c := range p {
		counts[c]++
	}
	h := 0.0
	for _, n := range counts {
		if n > 0 {
			f := float64(n) / float64(len(p))
			h -= f * math.Log2(f)
		}
	}
	return h
}

// forms are the ways n may be written into what is copied.
func forms(n []byte) []scanPattern {
	var out []scanPattern
	add := func(form string, b []byte) { out = append(out, scanPattern{form: form, b: b}) }
	add("raw", n)
	h := hex.EncodeToString(n)
	add("hex", []byte(h))
	add("hex", []byte(strings.ToUpper(h)))
	// In a longer encoded stream n starts at one of three byte alignments.
	// Of each, only the characters n's own bytes alone make are kept: the
	// leading ones mix in the bytes before it, the trailing the bytes after.
	for k := range 3 {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, k), n...))
		full := (k + len(n)) / 3 * 4
		skip := [3]int{0, 2, 3}[k]
		if full > skip {
			core := enc[skip:full]
			add("base64", []byte(core))
			add("base64url", []byte(strings.NewReplacer("+", "-", "/", "_").Replace(core)))
		}
	}
	u := uriComponent(n)
	add("url", []byte(u))
	add("url", []byte(lowerEscapes(u)))
	add("url", []byte(url.QueryEscape(string(n))))
	// Every byte but an unreserved one escaped (Python's quote(safe=""),
	// curl), and Python's default quote, which keeps slashes.
	for _, safe := range []string{"", "/"} {
		q := percentEncode(n, safe)
		add("url", []byte(q))
		add("url", []byte(lowerEscapes(q)))
	}
	add("url", []byte(url.PathEscape(string(n))))
	// As a password, and a user, in a URL's userinfo.
	add("url", []byte(strings.TrimPrefix(url.UserPassword("u", string(n)).String(), "u:")))
	add("url", []byte(url.User(string(n)).String()))
	if utf8.Valid(n) {
		j := jsonEscape(string(n))
		add("json", []byte(j))
		add("json", []byte(strings.ReplaceAll(j, "/", `\/`)))
		if m, err := json.Marshal(string(n)); err == nil {
			add("json", m[1:len(m)-1])
		}
		add("json", []byte(unicodeEscape(string(n), "%04x")))
		add("json", []byte(unicodeEscape(string(n), "%04X")))
	}
	return out
}

// percentEncode is b with every byte escaped but the unreserved ones of
// RFC 3986 and those in safe.
func percentEncode(b []byte, safe string) string {
	var sb strings.Builder
	for _, c := range b {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.~"+safe, c) >= 0 {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// uriComponent is b as JavaScript's encodeURIComponent writes it.
func uriComponent(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

var percentEscape = regexp.MustCompile(`%[0-9A-F]{2}`)

func lowerEscapes(s string) string {
	return percentEscape.ReplaceAllStringFunc(s, strings.ToLower)
}

// jsonEscape is s inside a JSON string as JavaScript's JSON.stringify
// writes it.
func jsonEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	return sb.String()
}

// unicodeEscape is s with every character but a letter or a digit of
// ASCII written as \u and its UTF-16 code units in format.
func unicodeEscape(s, format string) string {
	var sb strings.Builder
	for _, r := range s {
		if r < utf8.RuneSelf && ('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			sb.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&sb, `\u`+format, u)
		}
	}
	return sb.String()
}

// errRefused stops a copy's stream once its scan refused it.
var errRefused = errors.New("copy refused")

// scanStop is why a scan stopped a stream.
type scanStop struct {
	// pattern is the strong form of a secret found, nil for a stream past
	// max.
	pattern *scanPattern
	max     int64
}

// scanner is a writer passing what is written on to out once no secret
// pattern can be in it. It stops the stream at the first strong pattern
// found or past max bytes. At the first weak one it passes nothing more
// on but reads on, so a strong one later still decides. It holds back the
// longest pattern's length less one, so a pattern split across writes is
// found before a byte of it passes; a second scan over the stream with
// its line breaks removed finds an encoding wrapped across lines.
type scanner struct {
	m    *matcher
	keep int
	max  int64
	out  io.Writer
	// n is the bytes written so far.
	n int64
	// held is what is not passed on yet; flat the stream's last keep
	// bytes with its line breaks removed.
	held, flat []byte
	stop       *scanStop
	// suspect is the first weak pattern found.
	suspect *scanPattern
}

func newScanner(patterns []scanPattern, limit int64, out io.Writer) *scanner {
	keep := 0
	for _, p := range patterns {
		keep = max(keep, len(p.b)-1)
	}
	return &scanner{m: newMatcher(patterns), keep: keep, max: limit, out: out}
}

func (s *scanner) Write(p []byte) (int, error) {
	if s.stop != nil {
		return 0, errRefused
	}
	// What crosses max is scanned up to it first: a secret found there
	// decides over the size.
	over := s.max > 0 && s.n+int64(len(p)) > s.max
	if over {
		p = p[:s.max-s.n]
	}
	s.n += int64(len(p))
	s.held = append(s.held, p...)
	s.flat = append(s.flat, withoutLineBreaks(p)...)
	strong, weak := s.m.find(s.held, s.suspect == nil)
	if strong == nil {
		var flatWeak *scanPattern
		strong, flatWeak = s.m.find(s.flat, s.suspect == nil && weak == nil)
		weak = cmp.Or(weak, flatWeak)
	}
	if strong != nil {
		s.stop = &scanStop{pattern: strong}
		return 0, errRefused
	}
	if s.suspect == nil && weak != nil {
		s.suspect = weak
	}
	if over {
		s.stop = &scanStop{max: s.max}
		return 0, errRefused
	}
	if len(s.flat) > s.keep {
		s.flat = append(s.flat[:0], s.flat[len(s.flat)-s.keep:]...)
	}
	// What a pattern not yet whole may still start in stays: the last keep
	// bytes, and the last keep but line breaks.
	cut := len(s.held) - s.keep
	for i, other := len(s.held), 0; i > 0 && other < s.keep; {
		i--
		if c := s.held[i]; c != '\n' && c != '\r' {
			other++
		}
		cut = min(cut, i)
	}
	cut = max(cut, len(s.held)-s.keep-maxFlatHold, 0)
	if cut > 0 {
		if s.suspect == nil {
			if _, err := s.out.Write(s.held[:cut]); err != nil {
				return 0, err
			}
		}
		s.held = append(s.held[:0], s.held[cut:]...)
	}
	return len(p), nil
}

// Flush passes on what is held, once the stream ended.
func (s *scanner) Flush() error {
	if s.stop != nil || s.suspect != nil {
		return errRefused
	}
	_, err := s.out.Write(s.held)
	s.held = nil
	return err
}

// matcher finds patterns, every one at least 8 bytes, by their first 8
// bytes: a bitset of their hashes passes over most positions at one test,
// and an index gives the patterns to compare at the rest.
type matcher struct {
	patterns []scanPattern
	bits     []uint64
	index    map[uint64][]int
}

const matcherBits = 20

func newMatcher(patterns []scanPattern) *matcher {
	m := &matcher{patterns: patterns, bits: make([]uint64, 1<<matcherBits/64), index: map[uint64][]int{}}
	for i, p := range patterns {
		w := binary.LittleEndian.Uint64(p.b)
		h := prefixHash(w)
		m.bits[h/64] |= 1 << (h % 64)
		m.index[w] = append(m.index[w], i)
	}
	return m
}

func prefixHash(w uint64) uint64 { return (w * 0x9E3779B97F4A7C15) >> (64 - matcherBits) }

// find is a strong pattern in b, else, if weak is asked for, the first
// weak one.
func (m *matcher) find(b []byte, weak bool) (strong, firstWeak *scanPattern) {
	for i := 0; i+8 <= len(b); i++ {
		w := binary.LittleEndian.Uint64(b[i:])
		if h := prefixHash(w); m.bits[h/64]&(1<<(h%64)) == 0 {
			continue
		}
		for _, j := range m.index[w] {
			p := &m.patterns[j]
			if !bytes.HasPrefix(b[i:], p.b) {
				continue
			}
			if p.strong {
				return p, nil
			}
			if weak && firstWeak == nil {
				firstWeak = p
			}
		}
	}
	return nil, firstWeak
}

func withoutLineBreaks(b []byte) []byte {
	if bytes.IndexByte(b, '\n') < 0 && bytes.IndexByte(b, '\r') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\n' && c != '\r' {
			out = append(out, c)
		}
	}
	return out
}
