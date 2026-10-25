package devcontainer

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// The cases of workstream's test/devcontainer.test.ts, and what tower
// reads that it does not test.

func TestStripJSONC(t *testing.T) {
	var j struct {
		Image string `json:"image"`
		Build struct {
			Dockerfile string `json:"dockerfile"`
		} `json:"build"`
		URL  string `json:"url"`
		Glob string `json:"glob"`
	}
	in := `{
      // a line comment
      "image": "python:3.12", /* inline */
      "build": { "dockerfile": "Dockerfile", }, // trailing comma
      "url": "https://example.com/x", "glob": "a/*.ts",
    }`
	if err := json.Unmarshal([]byte(stripJSONC(in)), &j); err != nil {
		t.Fatal(err)
	}
	if j.Image != "python:3.12" || j.Build.Dockerfile != "Dockerfile" {
		t.Errorf("comments and trailing commas: %+v", j)
	}
	if j.URL != "https://example.com/x" || j.Glob != "a/*.ts" {
		t.Errorf("// and /* in a string were taken for comments: %+v", j)
	}
}

func TestParseConfig(t *testing.T) {
	t.Run("an image, its variables, its commands", func(t *testing.T) {
		c, err := parseConfig(".devcontainer/devcontainer.json", []byte(`{
			"image": "ardge/base:1",
			"containerEnv": {"WORKDIR": "${containerWorkspaceFolder}", "HOME_HINT": "${localEnv:HOME}", "N": 3},
			"onCreateCommand": ["bash", "setup.sh"],
			"postCreateCommand": "make deps",
			"remoteUser": "vscode", "containerUser": "other"
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.Image != "ardge/base:1" || c.Dir != ".devcontainer" || c.User != "vscode" {
			t.Errorf("read %+v", c)
		}
		// ${localEnv:...} is the apiserver's environment, which is not a
		// box's: it resolves to nothing. A value not a string is left out.
		if want := map[string]string{"WORKDIR": "/workspace", "HOME_HINT": ""}; !maps.Equal(c.Env, want) {
			t.Errorf("containerEnv %v, want %v", c.Env, want)
		}
		if c.OnCreate != "bash setup.sh" || c.PostCreate != "make deps" {
			t.Errorf("commands %q and %q", c.OnCreate, c.PostCreate)
		}
	})
	t.Run("a build, at the root, its features named", func(t *testing.T) {
		c, err := parseConfig(".devcontainer.json", []byte(`{"build": {"dockerfile": "Dockerfile", "context": ".."},
			"features": {"ghcr.io/devcontainers/features/docker-in-docker:2": {}}, "containerUser": "node"}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.Image != "" || c.Dockerfile != "Dockerfile" || c.Context != ".." || c.Dir != "" || c.User != "node" {
			t.Errorf("read %+v", c)
		}
		if !slices.Equal(c.Features, []string{"ghcr.io/devcontainers/features/docker-in-docker:2"}) {
			t.Errorf("features %v", c.Features)
		}
	})
	t.Run("named commands, in the order written", func(t *testing.T) {
		c, err := parseConfig(".devcontainer.json", []byte(`{"image": "x", "postCreateCommand": {"z": "first", "a": ["second", 2], "m": {"n": "third"}, "skip": 1}}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.PostCreate != "first && second 2 && third" {
			t.Errorf("postCreateCommand %q", c.PostCreate)
		}
	})
	if _, err := parseConfig(".devcontainer.json", []byte(`{"image": `)); err == nil {
		t.Error("a devcontainer that does not parse parsed")
	}
	t.Run("values as written", func(t *testing.T) {
		c, err := parseConfig(".devcontainer.json", []byte(`{"image": "x", "containerEnv": {"MULTI": "one\ntwo", "CTRL": "a\u0001b", "DOLLAR": "$HOME"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"MULTI": "one\ntwo", "CTRL": "a\x01b", "DOLLAR": "$HOME"}; !maps.Equal(c.Env, want) {
			t.Errorf("containerEnv %q, want %q", c.Env, want)
		}
	})
	for what, raw := range map[string]string{
		"a variable that is no name": `{"image": "x", "containerEnv": {"A B": "1"}}`,
		"a value holding NUL":        `{"image": "x", "containerEnv": {"A": "a\u0000b"}}`,
		"a user that is no name":     `{"image": "x", "remoteUser": "node\nRUN id"}`,
	} {
		if _, err := parseConfig(".devcontainer.json", []byte(raw)); err == nil {
			t.Errorf("a devcontainer of %s parsed", what)
		}
	}
}
