package secrets

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
)

// DockerConfigKey is the one key of a registry-auth secret's data, as
// Kubernetes names it.
const DockerConfigKey = ".dockerconfigjson"

// MaxDataSize bounds a secret's data, keys and values together.
const MaxDataSize = 64 << 10

// envName is what an opaque secret's key must be: an environment variable
// name, as its values are given to an environment by those names.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Login is one registry's login.
type Login struct {
	Username, Password string
	// IdentityToken is an OAuth refresh token, used instead of a password.
	IdentityToken string
}

// dockerConfig is the part of the Docker config format that holds logins.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// Validate checks a secret's data against its type. Its errors name keys
// and registries, never a value or any part of one.
func Validate(t secretsv1alpha1.SecretType, data map[string][]byte) error {
	size := 0
	for k, v := range data {
		size += len(k) + len(v)
	}
	if size > MaxDataSize {
		return fmt.Errorf("data is %d bytes; at most %d", size, MaxDataSize)
	}
	switch t {
	case secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH:
		if len(data) != 1 || data[DockerConfigKey] == nil {
			return fmt.Errorf("a registry-auth secret's data holds one key, %q", DockerConfigKey)
		}
		_, err := parseDockerConfig(data[DockerConfigKey])
		return err
	case secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE:
		// A key is named, as an error names keys; it is not a value.
		for _, k := range slices.Sorted(maps.Keys(data)) {
			if !envName.MatchString(k) {
				return fmt.Errorf("an opaque secret's keys are environment variable names (letters, digits and '_', not starting with a digit); %q is not", k)
			}
		}
		return nil
	case secretsv1alpha1.SecretType_SECRET_TYPE_UNSPECIFIED:
		return errors.New("type is required: registry-auth or opaque")
	}
	return errors.New("unknown type")
}

// parseDockerConfig reads a Docker config's logins by registry host. A
// login is a username and password, the base64 "auth" of the two, or an
// identity token.
func parseDockerConfig(b []byte) (map[string]Login, error) {
	var cfg dockerConfig
	// The decoder's error can quote the input, so it is not passed on.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not a Docker config: it must be a JSON object with \"auths\"", DockerConfigKey)
	}
	if len(cfg.Auths) == 0 {
		return nil, fmt.Errorf("%s holds no login under \"auths\"", DockerConfigKey)
	}
	logins := map[string]Login{}
	for _, key := range slices.Sorted(maps.Keys(cfg.Auths)) {
		host, err := RegistryHost(key)
		if err != nil {
			return nil, err
		}
		if _, dup := logins[host]; dup {
			return nil, fmt.Errorf("%s holds two logins for registry %s", DockerConfigKey, host)
		}
		a := cfg.Auths[key]
		l := Login{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			user, pass, ok := strings.Cut(string(raw), ":")
			if err != nil || !ok || user == "" {
				return nil, fmt.Errorf("the login for registry %s: \"auth\" must be base64 of username:password", host)
			}
			if l.Username != "" && l.Username != user || l.Password != "" && l.Password != pass {
				return nil, fmt.Errorf("the login for registry %s: \"auth\" and \"username\"/\"password\" disagree", host)
			}
			l.Username, l.Password = user, pass
		}
		if l.IdentityToken == "" && (l.Username == "" || l.Password == "") {
			return nil, fmt.Errorf("the login for registry %s needs a username and password, \"auth\", or \"identitytoken\"", host)
		}
		logins[host] = l
	}
	return logins, nil
}

// Registries are the hosts a valid registry-auth secret's data holds
// logins for, sorted.
func Registries(data map[string][]byte) []string {
	logins, err := parseDockerConfig(data[DockerConfigKey])
	if err != nil {
		return nil
	}
	return slices.Sorted(maps.Keys(logins))
}

// LoginFor returns the login data holds for the registry host, which
// RegistryHost has normalized.
func LoginFor(data map[string][]byte, host string) (Login, bool) {
	logins, err := parseDockerConfig(data[DockerConfigKey])
	if err != nil {
		return Login{}, false
	}
	l, ok := logins[host]
	return l, ok
}

// dockerHub are the names of Docker Hub's registry, which references name
// index.docker.io.
var dockerHub = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// RegistryHost normalizes a registry as a Docker config or a reference
// names it, to the host go-containerregistry gives: lowercase, with a port
// if any, without a scheme, and Docker Hub as index.docker.io. A Docker
// config key may end in "/v1/" or "/v2/"; any other path is refused, since
// a login is matched to a whole registry.
func RegistryHost(s string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(s))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if host, path, ok := strings.Cut(h, "/"); ok {
		if path != "" && path != "v1/" && path != "v2/" && path != "v1" && path != "v2" {
			return "", fmt.Errorf("registry %q names a path: a login is for a whole registry, named by its host", s)
		}
		h = host
	}
	if h == "" || strings.ContainsAny(h, " \t@?#") {
		return "", fmt.Errorf("registry %q is not a host", s)
	}
	if slices.Contains(dockerHub, h) {
		return "index.docker.io", nil
	}
	return h, nil
}
