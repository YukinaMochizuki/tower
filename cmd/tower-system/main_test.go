package main

import (
	"slices"
	"testing"
)

// TestTheConfigurationIsTheEnvironments: the container's environment, as
// the provider sets it (internal/provider/hangar), configures tower-system,
// TOWER_REGISTRY_PLAIN_HTTP=1 alone saying the registry is plain HTTP, and
// the image's, git's proxy, none when it names none.
func TestTheConfigurationIsTheEnvironments(t *testing.T) {
	env := map[string]string{"TOWER_REGISTRY": "100.64.0.9:5000", "TOWER_REGISTRY_PLAIN_HTTP": "1", "TAILSCALE_SOCKET": "/run/ts.sock",
		"TOWER_SYSTEM_GIT_PROXY": "http://127.0.0.1:1055"}
	c := configOf(func(k string) string { return env[k] })
	if c.Registry != "100.64.0.9:5000" || !c.RegistryPlainHTTP || c.Root != "/var/lib/tower" || c.GitProxy != "http://127.0.0.1:1055" ||
		c.BuildKit != "unix:///run/buildkit/buildkitd.sock" || !slices.Equal(c.Tailscale, []string{"tailscale", "--socket=/run/ts.sock"}) {
		t.Errorf("configured as %+v", c)
	}
	if c := configOf(func(string) string { return "" }); c.GitProxy != "" {
		t.Errorf("with no proxy named, git's is %q", c.GitProxy)
	}
	for _, v := range []string{"", "0", "true"} {
		env["TOWER_REGISTRY_PLAIN_HTTP"] = v
		if configOf(func(k string) string { return env[k] }).RegistryPlainHTTP {
			t.Errorf("TOWER_REGISTRY_PLAIN_HTTP=%q says the registry is plain HTTP", v)
		}
	}
}
