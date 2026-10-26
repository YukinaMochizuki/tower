package hangar

import "testing"

func TestRenamed(t *testing.T) {
	for _, c := range []struct{ ref, from, to, want string }{
		{"tower.ts.net:5000/web/box@sha256:01", "tower.ts.net:5000", "registry.example.com", "registry.example.com/web/box@sha256:01"},
		{"docker.io/library/debian:12", "tower.ts.net:5000", "registry.example.com", "docker.io/library/debian:12"},
		{"tower.ts.net:5000x/web", "tower.ts.net:5000", "registry.example.com", "tower.ts.net:5000x/web"},
		{"tower.ts.net:5000/web", "tower.ts.net:5000", "", "tower.ts.net:5000/web"},
		{"tower.ts.net:5000/web", "tower.ts.net:5000", "tower.ts.net:5000", "tower.ts.net:5000/web"},
	} {
		if got := renamed(c.ref, c.from, c.to); got != c.want {
			t.Errorf("renamed(%q, %q, %q) = %q, want %q", c.ref, c.from, c.to, got, c.want)
		}
	}
}
