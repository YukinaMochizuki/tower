package system

import "testing"

// TestTheBuilderIsWithoutDocker: tower-system's image driver is made
// without Docker and holds nothing, so buildkitd pushes each build, and
// the registry is plain HTTP as the container's configuration says.
func TestTheBuilderIsWithoutDocker(t *testing.T) {
	for _, plain := range []bool{true, false} {
		c := builderConfig(Config{Root: "/var/lib/tower", Registry: "registry.example", RegistryPlainHTTP: plain}, Header{})
		if !c.NoDocker || c.Docker != nil || c.UnpushedDir != "" || c.PlainHTTP != plain || c.Registry != "registry.example" {
			t.Errorf("plain HTTP %v: the builder's config %+v", plain, c)
		}
	}
}
