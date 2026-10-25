package devcontainer

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// What workstream reads of a devcontainer (workstream:
// src/box/devcontainer.ts), and how; this file is a port of it, kept to
// its behavior.

// workspace is the box's work directory, which ${containerWorkspaceFolder}
// and ${localWorkspaceFolder} resolve to.
const workspace = "/workspace"

// configCandidates are where a devcontainer is looked for, in order.
var configCandidates = []string{".devcontainer/devcontainer.json", ".devcontainer.json"}

// miseFiles declare a mise toolchain, in the order workstream reads them.
var miseFiles = []string{"mise.toml", ".mise.toml", ".config/mise/config.toml", ".tool-versions", ".devcontainer/mise.toml"}

// config is the subset of devcontainer.json a box is made from.
type config struct {
	// Path is the file it was read from, Dir its directory ("" for the
	// root).
	Path, Dir string
	Image     string
	// Dockerfile and Context are build.dockerfile and build.context, as
	// written: relative to Dir.
	Dockerfile, Context string
	// Env is containerEnv, its variables resolved.
	Env map[string]string
	// OnCreate and PostCreate are its commands, as one shell line each.
	OnCreate, PostCreate string
	// User is remoteUser, else containerUser; "" runs as the image does.
	User string
	// Features are the features it names, which tower installs none of,
	// as workstream does not.
	Features []string
}

// envName is a variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// trailingCommas is workstream's: it is applied to the whole text, inside
// strings too.
var trailingCommas = regexp.MustCompile(`,(\s*[}\]])`)

// stripJSONC takes // and /* */ comments out of JSONC and its trailing
// commas, as workstream's parseJsonc does: a string is quoted by " or ',
// with \ escaping the next character.
func stripJSONC(text string) string {
	var out strings.Builder
	inStr := false
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		var next byte
		if i+1 < len(text) {
			next = text[i+1]
		}
		if inStr {
			out.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(text) {
					out.WriteByte(next)
				}
				i++
			case quote:
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr, quote = true, c
			out.WriteByte(c)
			continue
		}
		if c == '/' && next == '/' {
			for i < len(text) && text[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if c == '/' && next == '*' {
			i += 2
			for i < len(text) && (text[i] != '*' || i+1 >= len(text) || text[i+1] != '/') {
				i++
			}
			i++ // the closing '/'
			continue
		}
		out.WriteByte(c)
	}
	return trailingCommas.ReplaceAllString(out.String(), "$1")
}

// localEnv is ${localEnv:NAME}, which workstream resolves from its own
// server's environment. tower resolves it to "": the apiserver's
// environment is not a box's to read.
var localEnv = regexp.MustCompile(`\$\{localEnv:([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveVars resolves the substitutions workstream does.
func resolveVars(v string) string {
	v = strings.ReplaceAll(v, "${containerWorkspaceFolder}", workspace)
	v = strings.ReplaceAll(v, "${localWorkspaceFolder}", workspace)
	return localEnv.ReplaceAllString(v, "")
}

// command is a devcontainer command as one shell line: a string as it is,
// an array's elements joined by spaces, an object's commands joined by &&
// in the order they are written. Anything else is none.
func command(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return commandOf(dec)
}

func commandOf(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	switch t := tok.(type) {
	case string:
		return t, nil
	case json.Delim:
		switch t {
		case '[':
			var parts []string
			for dec.More() {
				var v any
				if err := dec.Decode(&v); err != nil {
					return "", err
				}
				parts = append(parts, jsString(v))
			}
			_, err := dec.Token()
			return strings.Join(parts, " "), err
		case '{':
			// Object.values' order: a key that is an array index first,
			// by its number, then the rest as written; a key written twice
			// keeps its first place and its last value.
			type named struct {
				key string
				cmd string
			}
			var entries []named
			for dec.More() {
				tok, err := dec.Token() // the name
				if err != nil {
					return "", err
				}
				key, _ := tok.(string)
				p, err := commandOf(dec)
				if err != nil {
					return "", err
				}
				if i := slices.IndexFunc(entries, func(e named) bool { return e.key == key }); i >= 0 {
					entries[i].cmd = p
				} else {
					entries = append(entries, named{key, p})
				}
			}
			if _, err := dec.Token(); err != nil {
				return "", err
			}
			slices.SortStableFunc(entries, func(a, b named) int {
				ai, aok := arrayIndex(a.key)
				bi, bok := arrayIndex(b.key)
				switch {
				case aok && bok:
					return cmp.Compare(ai, bi)
				case aok:
					return -1
				case bok:
					return 1
				}
				return 0
			})
			var parts []string
			for _, e := range entries {
				if e.cmd != "" {
					parts = append(parts, e.cmd)
				}
			}
			return strings.Join(parts, " && "), nil
		}
	}
	return "", nil
}

// arrayIndex reads a key that JavaScript orders as an array index: the
// canonical text of an integer below 2^32-1.
func arrayIndex(key string) (uint64, bool) {
	n, err := strconv.ParseUint(key, 10, 32)
	if err != nil || n == 1<<32-1 || strconv.FormatUint(n, 10) != key {
		return 0, false
	}
	return n, true
}

// jsNumber is JavaScript's String of a JSON number: the shortest text
// that reads back as the same double, in exponent form from 1e21 and
// below 1e-6.
func jsNumber(n json.Number) string {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return string(n)
	}
	if f == 0 {
		return "0"
	}
	if a := math.Abs(f); a >= 1e21 || a < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		return mant + "e" + sign + strings.TrimLeft(exp[1:], "0")
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsString is JavaScript's String(v) of a JSON value, for an array of
// arguments.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case json.Number:
		return jsNumber(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// parseConfig reads a devcontainer.json found at configPath.
func parseConfig(configPath string, raw []byte) (config, error) {
	var j struct {
		Image         json.RawMessage            `json:"image"`
		Build         map[string]json.RawMessage `json:"build"`
		Features      map[string]json.RawMessage `json:"features"`
		ContainerEnv  map[string]json.RawMessage `json:"containerEnv"`
		OnCreate      json.RawMessage            `json:"onCreateCommand"`
		PostCreate    json.RawMessage            `json:"postCreateCommand"`
		RemoteUser    json.RawMessage            `json:"remoteUser"`
		ContainerUser json.RawMessage            `json:"containerUser"`
	}
	dec := json.NewDecoder(strings.NewReader(stripJSONC(string(raw))))
	if err := dec.Decode(&j); err != nil {
		return config{}, fmt.Errorf("%s does not parse: %w", configPath, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return config{}, fmt.Errorf("%s does not parse: it holds more than one value", configPath)
	}
	str := func(r json.RawMessage) string {
		var s string
		if json.Unmarshal(r, &s) != nil {
			return ""
		}
		return s
	}
	c := config{Path: configPath, Dir: path.Dir(configPath), Image: str(j.Image), Env: map[string]string{}}
	if c.Dir == "." {
		c.Dir = ""
	}
	c.Dockerfile, c.Context = str(j.Build["dockerfile"]), str(j.Build["context"])
	for k, v := range j.ContainerEnv {
		var s string
		if json.Unmarshal(v, &s) == nil {
			c.Env[k] = resolveVars(s)
		}
	}
	var err error
	if c.OnCreate, err = command(j.OnCreate); err != nil {
		return config{}, fmt.Errorf("%s: onCreateCommand: %w", configPath, err)
	}
	if c.PostCreate, err = command(j.PostCreate); err != nil {
		return config{}, fmt.Errorf("%s: postCreateCommand: %w", configPath, err)
	}
	if c.User = str(j.RemoteUser); c.User == "" {
		c.User = str(j.ContainerUser)
	}
	c.Features = slices.Sorted(maps.Keys(j.Features))
	// What reaches a container's environment and its Dockerfile is what
	// they can take: a variable's name, a value without NUL, a user's name.
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		if !envName.MatchString(k) {
			return config{}, fmt.Errorf("%s: containerEnv sets %q, which is no variable's name", configPath, k)
		}
		if strings.ContainsRune(c.Env[k], 0) {
			return config{}, fmt.Errorf("%s: containerEnv's %s holds a NUL", configPath, k)
		}
	}
	if !validUser(c.User) {
		return config{}, fmt.Errorf("%s: remoteUser %q is no user's name", configPath, c.User)
	}
	return c, nil
}
