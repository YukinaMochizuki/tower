package devcontainer

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The add-ons are workstream's, copied (docs/decisions/box-addons.md), and
// the layer is built as workstream builds it (workstream:
// src/box/addon-image.ts); this file is a port of that, kept to its
// behavior.

//go:embed addons
var addonFiles embed.FS

// addon is one add-on: a devcontainer Feature.
type addon struct {
	ID string
	// Options are its options' defaults, by the option's name, in the
	// order its devcontainer-feature.json writes them.
	Options []kv
	// Env is its containerEnv, baked in as ENV, in the same order.
	Env []kv
	// Files are every file of it, by name.
	Files map[string][]byte
}

// kv is a name and its value.
type kv struct{ K, V string }

// objectKeys is the names of a JSON object, in the order it writes them,
// as JavaScript's Object.entries gives them.
func objectKeys(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("not an object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// Addons are the add-ons tower has, by name.
func Addons() []string {
	entries, _ := fs.ReadDir(addonFiles, "addons")
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// loadAddon reads one add-on's Feature and files.
func loadAddon(id string) (addon, error) {
	if !slices.Contains(Addons(), id) {
		return addon{}, fmt.Errorf("tower has no add-on %q; it has %s: %w", id, strings.Join(Addons(), ", "), ErrUnresolvable)
	}
	dir := path.Join("addons", id)
	entries, err := fs.ReadDir(addonFiles, dir)
	if err != nil {
		return addon{}, err
	}
	a := addon{ID: id, Files: map[string][]byte{}}
	for _, e := range entries {
		if a.Files[e.Name()], err = fs.ReadFile(addonFiles, path.Join(dir, e.Name())); err != nil {
			return addon{}, err
		}
	}
	var feature struct {
		ID      string `json:"id"`
		Options map[string]struct {
			Default any `json:"default"`
		} `json:"options"`
		ContainerEnv map[string]string `json:"containerEnv"`
	}
	var order struct {
		Options      json.RawMessage `json:"options"`
		ContainerEnv json.RawMessage `json:"containerEnv"`
	}
	if err := json.Unmarshal(a.Files["devcontainer-feature.json"], &feature); err != nil {
		return addon{}, fmt.Errorf("add-on %s: %w", id, err)
	}
	_ = json.Unmarshal(a.Files["devcontainer-feature.json"], &order)
	optionKeys, err := objectKeys(order.Options)
	if err != nil {
		return addon{}, fmt.Errorf("add-on %s: options: %w", id, err)
	}
	envKeys, err := objectKeys(order.ContainerEnv)
	if err != nil {
		return addon{}, fmt.Errorf("add-on %s: containerEnv: %w", id, err)
	}
	if feature.ID != id {
		return addon{}, fmt.Errorf("add-on %s: devcontainer-feature.json declares id %q", id, feature.ID)
	}
	if a.Files["install.sh"] == nil {
		return addon{}, fmt.Errorf("add-on %s: no install.sh", id)
	}
	for _, k := range optionKeys {
		v := ""
		if d := feature.Options[k].Default; d != nil {
			v = fmt.Sprint(d)
		}
		a.Options = append(a.Options, kv{k, v})
	}
	for _, k := range envKeys {
		a.Env = append(a.Env, kv{k, feature.ContainerEnv[k]})
	}
	return a, nil
}

// version is an add-on with a hash of its files, such as "browser@3f2a...".
func (a addon) version() string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(a.Files)) {
		_, _ = fmt.Fprintf(h, "%d:%s%d:", len(name), name, len(a.Files[name]))
		_, _ = h.Write(a.Files[name])
	}
	return a.ID + "@" + hex.EncodeToString(h.Sum(nil))[:16]
}

// nonWord is what the Feature spec turns into _ in an option's variable.
var nonWord = regexp.MustCompile(`[^\w]`)

// optionEnvName is an option's variable: upper-cased, each non-word
// character an _.
func optionEnvName(option string) string {
	return strings.ToUpper(nonWord.ReplaceAllString(option, "_"))
}

// shq quotes a value for sh.
func shq(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// jsonString is JSON.stringify of a string, which is how workstream writes
// an ENV value.
func jsonString(v string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// layer is what a box's image layer is built from.
type layer struct {
	// Base is the image it is FROM, by reference; BaseUser the user it runs
	// as.
	Base, BaseUser string
	Addons         []addon
	// RemoteUser is who the add-ons install for and, when set, the user
	// the image runs as.
	RemoteUser string
	// Env is the devcontainer's containerEnv.
	Env map[string]string
}

// dockerfile is the layer's Dockerfile. The add-ons' part is workstream's,
// line for line, but for its check of apt, which workstream runs in a
// container of the base before building and tower runs as the layer's
// first step, so a base without apt fails with the same sentence before
// any installer. After them come the devcontainer's containerEnv, which
// workstream gives each container (docker run -e) and tower bakes in, its
// $ escaped so it is taken as written, and its remoteUser, which workstream
// runs every command as (docker exec -u) and tower makes the image's user.
func (l layer) dockerfile() string {
	lines := []string{"FROM " + l.Base}
	if len(l.Addons) > 0 {
		user := l.RemoteUser
		if user == "" {
			user = "root"
		}
		ids := make([]string, len(l.Addons))
		for i, a := range l.Addons {
			ids[i] = a.ID
		}
		lines = append(lines, "USER root",
			fmt.Sprintf("RUN command -v apt-get >/dev/null 2>&1 || { echo %s >&2; exit 1; }",
				shq(fmt.Sprintf("box add-ons (%s) need an apt-based image (Debian or Ubuntu); %s has no apt-get", strings.Join(ids, ", "), l.Base))))
		for _, a := range l.Addons {
			var vars []string
			for _, o := range a.Options {
				vars = append(vars, optionEnvName(o.K)+"="+shq(o.V))
			}
			vars = append(vars, "_REMOTE_USER="+shq(user), "_CONTAINER_USER=root", fmt.Sprintf(`_REMOTE_USER_HOME="$(getent passwd %s | cut -d: -f6)"`, shq(user)))
			lines = append(lines,
				fmt.Sprintf("COPY %s/ /tmp/ws-addons/%s/", a.ID, a.ID),
				fmt.Sprintf("RUN cd /tmp/ws-addons/%s && %s bash ./install.sh && rm -rf /tmp/ws-addons/%s", a.ID, strings.Join(vars, " "), a.ID))
			for _, e := range a.Env {
				lines = append(lines, fmt.Sprintf("ENV %s=%s", e.K, jsonString(e.V)))
			}
		}
		base := l.BaseUser
		if base == "" {
			base = "root"
		}
		lines = append(lines, "USER "+base)
	}
	for _, k := range slices.Sorted(maps.Keys(l.Env)) {
		lines = append(lines, fmt.Sprintf("ENV %s=%s", k, strings.ReplaceAll(jsonString(l.Env[k]), "$", `\$`)))
	}
	if l.RemoteUser != "" {
		lines = append(lines, "USER "+l.RemoteUser)
	}
	return strings.Join(lines, "\n") + "\n"
}

// context is the layer's build context: its Dockerfile and each add-on's
// files under its name.
func (l layer) context() ([]byte, error) {
	files := map[string]file{"Dockerfile": {data: []byte(l.dockerfile()), mode: 0o644}}
	for _, a := range l.Addons {
		for name, data := range a.Files {
			mode := int64(0o644)
			if strings.HasSuffix(name, ".sh") {
				mode = 0o755
			}
			files[a.ID+"/"+name] = file{data: data, mode: mode}
		}
	}
	return tarOf(files)
}
