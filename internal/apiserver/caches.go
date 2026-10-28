package apiserver

import (
	"context"
	"maps"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// CacheDir is where a box's toolchain caches live: in its /workspace, out
// of git's sight. See docs/decisions/devcontainers.md.
const CacheDir = "/workspace/.ws-cache"

// cacheEnv are the cache variables a box's environment points under
// CacheDir, each to its directory; it follows workstream's list
// (workstream: src/box/build-cache.ts), with mise's tools added.
var cacheEnv = map[string]string{
	"GOCACHE":               "go-build",
	"GOMODCACHE":            "go-mod",
	"npm_config_cache":      "npm",
	"npm_config_store_dir":  "pnpm-store",
	"PUPPETEER_CACHE_DIR":   "puppeteer",
	"PIP_CACHE_DIR":         "pip",
	"UV_CACHE_DIR":          "uv",
	"BUN_INSTALL_CACHE_DIR": "bun",
	"MISE_DATA_DIR":         "mise",
}

// impliedBy are the variables that set a cache variable's default
// location: one set leaves it alone too.
var impliedBy = map[string]string{"MISE_DATA_DIR": "XDG_DATA_HOME", "PUPPETEER_CACHE_DIR": "XDG_CACHE_HOME"}

// boxEnv is the environment of a box's container, or a warm golden's
// builder: its devcontainer's containerEnv, and each cache variable that
// neither it nor the image's own ENV places.
func boxEnv(containerEnv map[string]string, imageEnv []string) map[string]string {
	set := map[string]bool{}
	for _, kv := range imageEnv {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = true
	}
	for name := range containerEnv {
		set[name] = true
	}
	placed := func(name string) bool {
		if set[name] || set[impliedBy[name]] {
			return true
		}
		// npm reads its variables whatever their case.
		if strings.HasPrefix(name, "npm_config_") {
			for s := range set {
				if strings.EqualFold(s, name) {
					return true
				}
			}
		}
		return false
	}
	env := maps.Clone(containerEnv)
	if env == nil {
		env = map[string]string{}
	}
	for name, dir := range cacheEnv {
		if !placed(name) {
			env[name] = CacheDir + "/" + dir
		}
	}
	return env
}

// imageBoxEnv is boxEnv for a container of image, its ENV read from its
// registry through p.
func imageBoxEnv(ctx context.Context, p provider.Provider, image string, containerEnv map[string]string) (map[string]string, error) {
	cfg, err := p.ResolveImage(ctx, image)
	if err != nil {
		return nil, err
	}
	return boxEnv(containerEnv, cfg.Env), nil
}

// excludeCaches has git ignore CacheDir in the checkout at /workspace, as
// a line of its .git/info/exclude, written once.
var excludeCaches = []string{"sh", "-c", `f=.git/info/exclude; mkdir -p .git/info && { grep -qxF /.ws-cache/ "$f" 2>/dev/null || echo /.ws-cache/ >> "$f"; }`}

// adoptToolDirs keeps the tools an image installed at mise's or
// puppeteer's default location, which the box's environment would hide.
// Run as the image's user, in each box and builder before anything
// installs (docs/decisions/devcontainers.md).
var adoptToolDirs = adoptToolDirsUnder(CacheDir)

// adoptToolDirsUnder makes a tool directory under cache that is not there
// yet a link to the default one, when that holds anything.
func adoptToolDirsUnder(cache string) []string {
	return []string{"sh", "-c", "c='" + cache + "'" + adoptToolDirsScript}
}

// adoptToolDirsScript runs adopt for each tool directory: the variable
// naming it, then its default place, each path quoted whole.
const adoptToolDirsScript = `
adopt() {
  eval "dir=\${$1:-}"
  case "$dir" in "$c"/*) ;; *) return 0 ;; esac
  if [ -e "$dir" ] || [ -L "$dir" ]; then return 0; fi
  if [ -d "$2" ] && [ -n "$(ls -A "$2" 2>/dev/null)" ]; then
    mkdir -p "$(dirname "$dir")" && ln -s "$2" "$dir"
  fi
}
adopt MISE_DATA_DIR "${XDG_DATA_HOME:-$HOME/.local/share}/mise" &&
adopt PUPPETEER_CACHE_DIR "${XDG_CACHE_HOME:-$HOME/.cache}/puppeteer"`
