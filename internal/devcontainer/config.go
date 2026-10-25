package devcontainer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
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
			var parts []string
			for dec.More() {
				if _, err := dec.Token(); err != nil { // the name
					return "", err
				}
				p, err := commandOf(dec)
				if err != nil {
					return "", err
				}
				if p != "" {
					parts = append(parts, p)
				}
			}
			_, err := dec.Token()
			return strings.Join(parts, " && "), err
		}
	}
	return "", nil
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
		return x.String()
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
	return c, nil
}
