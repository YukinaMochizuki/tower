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
// token's characters and minPieceEntropy bits a byte, and either letters
// of both cases and digits, a long hex string, a UUID, or lower case and
// digits interleaved as a random token's are, with no dot or slash. Then
// a kubeconfig's "apiVersion: v1" or "server: https://kube1.example.com",
// a path, a name such as "kubelet-client-2024-01-01" or a .netrc's
// "machine github.com" never count.
const (
	minPiece        = 16
	minHexPiece     = 32
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
// those too short to scan for. Strong: each value whole and trimmed, and
// a PEM block's base64 body. Weak: each credential-shaped line or field
// of a value with more than one. Each raw, in hex, in base64 and
// base64url at each byte alignment, URL-encoded and JSON-escaped; a form
// both strong and weak is strong.
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
		trimmed := bytes.TrimSpace(s.value)
		if len(trimmed) < minScannableSecret {
			tooShort = append(tooShort, s.name)
			continue
		}
		scannable = append(scannable, s)
		add(s.name, s.value, true)
		add(s.name, trimmed, true)
		for _, v := range unescaped(s.value) {
			add(s.name, pemBody(v), true)
		}
	}
	for _, s := range scannable {
		for _, v := range unescaped(s.value) {
			for _, p := range pieces(v) {
				add(s.name, p, false)
			}
		}
	}
	return patterns, tooShort
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

// pieces are the credential-shaped lines and fields of a value made of
// more than one, none for a value of one.
func pieces(v []byte) [][]byte {
	fields := bytes.FieldsFunc(v, isPieceSeparator)
	if len(fields) < 2 {
		return nil
	}
	var out [][]byte
	for line := range bytes.Lines(v) {
		if l := bytes.TrimSpace(line); credentialShaped(l) {
			out = append(out, l)
		}
	}
	for _, f := range fields {
		if credentialShaped(f) {
			out = append(out, f)
		}
	}
	return out
}

func isPieceSeparator(r rune) bool {
	return strings.ContainsRune(" \t\r\n\"'`:,;=@&?<>()[]{}", r)
}

// credentialShaped reports whether p looks like a credential: a token, a
// key's line, a long hex string; not a word, a host, a path or a number.
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
	case upper && lower && digit, len(p) >= minHexPiece && digit && hexShape.Match(p), uuidShape.Match(p):
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
