package devcontainer

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The cases of workstream's test/box-addons.test.ts, and the layer's part
// that is tower's.

func TestTheAddonsAreWorkstreams(t *testing.T) {
	if got := Addons(); !slices.Equal(got, []string{"browser", "desktop"}) {
		t.Errorf("tower has the add-ons %v", got)
	}
	for _, id := range Addons() {
		a, err := loadAddon(id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(a.Files["install.sh"]), "set -euo pipefail") {
			t.Errorf("%s's install.sh is not workstream's", id)
		}
	}
	browser, _ := loadAddon("browser")
	if !slices.Equal(browser.Options, []kv{{"playwrightVersion", "1.63.0"}, {"nodeVersion", "24.21.0"}}) ||
		!slices.Equal(browser.Env, []kv{{"CHROME_PATH", "/usr/local/bin/chromium"}}) {
		t.Errorf("the browser's options %v and containerEnv %v", browser.Options, browser.Env)
	}
	if _, err := loadAddon("gpu"); !errors.Is(err, ErrUnresolvable) {
		t.Errorf("an add-on tower lacks: %v", err)
	}
	if optionEnvName("playwrightVersion") != "PLAYWRIGHTVERSION" || optionEnvName("x-y.z") != "X_Y_Z" {
		t.Error("an option's variable is not the Feature spec's")
	}
}

func TestTheLayersDockerfile(t *testing.T) {
	browser := addon{ID: "browser", Options: []kv{{"playwrightVersion", "1.2.3"}}}
	desktop := addon{ID: "desktop", Env: []kv{{"DISPLAY", ":99"}}}
	df := layer{Base: "mcr.microsoft.com/devcontainers/typescript-node:22", Addons: []addon{browser, desktop}, RemoteUser: "node", BaseUser: "node"}.dockerfile()
	lines := strings.Split(strings.TrimSpace(df), "\n")
	if lines[0] != "FROM mcr.microsoft.com/devcontainers/typescript-node:22" || lines[1] != "USER root" {
		t.Errorf("it starts %q", lines[:2])
	}
	// tower checks apt as the layer's first step, with workstream's words.
	if !strings.Contains(lines[2], "box add-ons (browser, desktop) need an apt-based image (Debian or Ubuntu)") {
		t.Errorf("no check of apt first: %q", lines[2])
	}
	if strings.Index(df, "COPY browser/") > strings.Index(df, "COPY desktop/") {
		t.Error("the add-ons are not installed in the order asked")
	}
	if !strings.Contains(df, "PLAYWRIGHTVERSION='1.2.3' _REMOTE_USER='node' _CONTAINER_USER=root") || !strings.Contains(df, `ENV DISPLAY=":99"`) {
		t.Errorf("an add-on's options or containerEnv:\n%s", df)
	}
	// The remote user is the image's user at the end, the base's restored
	// before it.
	if lines[len(lines)-1] != "USER node" {
		t.Errorf("it ends %q", lines[len(lines)-1])
	}
	plain := layer{Base: "b", Addons: []addon{browser}}.dockerfile()
	if last := strings.Split(strings.TrimSpace(plain), "\n"); last[len(last)-1] != "USER root" {
		t.Errorf("a base without a user ends %q", last[len(last)-1])
	}
	// containerEnv, which workstream gives each container as it is
	// written, is taken as written; no add-ons, no USER root.
	env := layer{Base: "b", Env: map[string]string{"A": "$HOME/x", "B": `q"`}, RemoteUser: "vscode"}.dockerfile()
	if env != "FROM b\nENV A=\"\\$HOME/x\"\nENV B=\"q\\\"\"\nUSER vscode\n" {
		t.Errorf("a devcontainer's layer:\n%s", env)
	}
	if got := (layer{Base: "b"}).dockerfile(); got != "FROM b\n" {
		t.Errorf("a layer of nothing:\n%s", got)
	}
}

func TestAnAddonsVersionIsItsFiles(t *testing.T) {
	a := addon{ID: "browser", Files: map[string][]byte{"install.sh": []byte("echo")}}
	b := addon{ID: "browser", Files: map[string][]byte{"install.sh": []byte("echo changed")}}
	if a.version() == b.version() || !strings.HasPrefix(a.version(), "browser@") {
		t.Errorf("versions %q and %q", a.version(), b.version())
	}
}
