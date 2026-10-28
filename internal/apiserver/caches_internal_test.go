package apiserver

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
)

// TestABoxsCachesAreInItsWorkspace: every cache variable points under
// CacheDir, but one the devcontainer's containerEnv or the image's own ENV
// sets, which is left as it is.
func TestABoxsCachesAreInItsWorkspace(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"GOCACHE":               "/workspace/.ws-cache/go-build",
		"GOMODCACHE":            "/workspace/.ws-cache/go-mod",
		"npm_config_cache":      "/workspace/.ws-cache/npm",
		"npm_config_store_dir":  "/workspace/.ws-cache/pnpm-store",
		"PUPPETEER_CACHE_DIR":   "/workspace/.ws-cache/puppeteer",
		"PIP_CACHE_DIR":         "/workspace/.ws-cache/pip",
		"UV_CACHE_DIR":          "/workspace/.ws-cache/uv",
		"BUN_INSTALL_CACHE_DIR": "/workspace/.ws-cache/bun",
		"MISE_DATA_DIR":         "/workspace/.ws-cache/mise",
	}
	if got := boxEnv(nil, nil); !maps.Equal(got, want) {
		t.Errorf("a box of neither: %v, want %v", got, want)
	}

	containerEnv := map[string]string{"GOMODCACHE": "/go/pkg/mod", "GOFLAGS": "-mod=mod"}
	got := boxEnv(containerEnv, []string{"PATH=/usr/bin", "GOCACHE=/cache/go", "UV_CACHE_DIR", "MISE_DATA_DIR="})
	want["GOMODCACHE"], want["GOFLAGS"] = "/go/pkg/mod", "-mod=mod"
	for _, k := range []string{"GOCACHE", "UV_CACHE_DIR", "MISE_DATA_DIR"} {
		delete(want, k)
	}
	if !maps.Equal(got, want) {
		t.Errorf("a box whose containerEnv and image set some: %v, want %v", got, want)
	}
	if containerEnv["GOCACHE"] != "" || len(containerEnv) != 2 {
		t.Errorf("the containerEnv given was changed: %v", containerEnv)
	}

	// An XDG base directory places mise's and puppeteer's tools; npm reads
	// its variables in any case.
	got = boxEnv(map[string]string{"XDG_CACHE_HOME": "/cache"}, []string{"XDG_DATA_HOME=/data", "NPM_CONFIG_CACHE=/npm"})
	for _, k := range []string{"MISE_DATA_DIR", "PUPPETEER_CACHE_DIR", "npm_config_cache"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s set to %q, which the image or the devcontainer places", k, v)
		}
	}
	if got["npm_config_store_dir"] == "" || got["GOCACHE"] == "" {
		t.Errorf("a box whose image places some caches: %v", got)
	}
}

// TestAToolDirectoryTheImageFilledIsKept: a tool directory under the
// caches is made a link to the image's own at its default location when
// that holds anything, and left alone otherwise: when the image has none,
// when it is there already, when the box's environment places it
// elsewhere.
func TestAToolDirectoryTheImageFilledIsKept(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, home string, env ...string) string {
		t.Helper()
		cache := filepath.Join(home, "ws-cache")
		c := adoptToolDirsUnder(cache)
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return cache
	}
	fill := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("filled", func(t *testing.T) {
		// A path with a space is one path.
		home := filepath.Join(t.TempDir(), "a home")
		fill(t, filepath.Join(home, ".local/share/mise"))
		fill(t, filepath.Join(home, "xdg-cache/puppeteer"))
		cache := filepath.Join(home, "ws-cache")
		run(t, home, "MISE_DATA_DIR="+cache+"/mise", "PUPPETEER_CACHE_DIR="+cache+"/puppeteer", "XDG_CACHE_HOME="+home+"/xdg-cache")
		for link, want := range map[string]string{"mise": home + "/.local/share/mise", "puppeteer": home + "/xdg-cache/puppeteer"} {
			if got, err := os.Readlink(filepath.Join(cache, link)); err != nil || got != want {
				t.Errorf("%s: %q, %v; want a link to %s", link, got, err, want)
			}
		}
	})
	t.Run("left alone", func(t *testing.T) {
		home := t.TempDir()
		fill(t, filepath.Join(home, ".cache/puppeteer"))
		if err := os.MkdirAll(filepath.Join(home, ".local/share/mise"), 0o755); err != nil {
			t.Fatal(err)
		}
		cache := filepath.Join(home, "ws-cache")
		fill(t, filepath.Join(cache, "puppeteer"))
		run(t, home, "MISE_DATA_DIR="+cache+"/mise", "PUPPETEER_CACHE_DIR="+cache+"/puppeteer")
		if _, err := os.Lstat(filepath.Join(cache, "mise")); !os.IsNotExist(err) {
			t.Errorf("mise's directory made of an image that has none: %v", err)
		}
		if fi, err := os.Lstat(filepath.Join(cache, "puppeteer")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("puppeteer's directory, there already, was replaced: %v, %v", fi, err)
		}
		other := t.TempDir()
		fill(t, filepath.Join(other, ".local/share/mise"))
		run(t, other, "MISE_DATA_DIR=/elsewhere/mise")
		if _, err := os.Lstat(filepath.Join(other, "ws-cache")); !os.IsNotExist(err) {
			t.Errorf("a tool directory the box's environment does not place under the caches was linked: %v", err)
		}
	})
}

// TestAWarmBuildersWorkspaceStartsAtTheLastGoldensSize: a builder's
// /workspace starts a quarter over what the repository's last warm golden
// held, within the box disk settings, and as a box's for the first one;
// on a hostd target it is a box's.
func TestAWarmBuildersWorkspaceStartsAtTheLastGoldensSize(t *testing.T) {
	t.Parallel()
	hangar := &targetsv1alpha1.Target{Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{}}}}
	for last, want := range map[uint64]string{0: "2Gi", 1 << 30: "2Gi", 7 << 30: "9Gi", 8 << 30: "10Gi", 100 << 30: "30Gi"} {
		r, g := builderDisks(hangar, last)
		if r.GetDisk() != want || g.GetWorkspaceMax() != "30Gi" || g.GetRoot() != "2Gi" {
			t.Errorf("after a golden of %d bytes: %v, %v; want /workspace at %s", last, r, g, want)
		}
	}
	hostd := &targetsv1alpha1.Target{Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}}}
	if r, g := builderDisks(hostd, 7<<30); r.GetDisk() != "40Gi" || g != nil {
		t.Errorf("a builder on a hostd target: %v, %v", r, g)
	}
}

// TestABoxsDisksGrowOnAHangarTarget: a box's disks grow on a hangar
// target, from tower's defaults or the target's settings, its own
// resources.disk the start of /workspace; on a hostd target they do not,
// and the box's resources are tower's defaults as before.
func TestABoxsDisksGrowOnAHangarTarget(t *testing.T) {
	t.Parallel()
	hangar := func(set *targetsv1alpha1.TargetSettings) *targetsv1alpha1.Target {
		return &targetsv1alpha1.Target{Spec: &targetsv1alpha1.TargetSpec{
			Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{}}, Settings: set,
		}}
	}
	hostd := &targetsv1alpha1.Target{Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}}}
	for what, tc := range map[string]struct {
		target    *targetsv1alpha1.Target
		box       *envv1alpha1.Resources
		resources *envv1alpha1.Resources
		growth    *envv1alpha1.DiskGrowth
	}{
		"hostd": {
			target: hostd, resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "40Gi"},
		},
		"hostd, the box's own disk": {
			target: hostd, box: &envv1alpha1.Resources{Disk: "8Gi"}, resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "8Gi"},
		},
		"hangar, tower's defaults": {
			target: hangar(nil), resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "2Gi"},
			growth: &envv1alpha1.DiskGrowth{WorkspaceMax: "30Gi", Root: "2Gi", RootMax: "30Gi", Threshold: "80%", Step: "2x"},
		},
		"hangar, the target's settings": {
			target:    hangar(&targetsv1alpha1.TargetSettings{BoxWorkspaceDisk: "3Gi", BoxWorkspaceDiskMax: "60Gi", BoxRootDisk: "8Gi", BoxRootDiskMax: "16Gi"}),
			box:       &envv1alpha1.Resources{Cpu: "4"},
			resources: &envv1alpha1.Resources{Cpu: "4", Memory: "4Gi", Disk: "3Gi"},
			growth:    &envv1alpha1.DiskGrowth{WorkspaceMax: "60Gi", Root: "8Gi", RootMax: "16Gi", Threshold: "80%", Step: "2x"},
		},
		"hangar, the box's own disk": {
			target: hangar(nil), box: &envv1alpha1.Resources{Disk: "50Gi"}, resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "50Gi"},
			growth: &envv1alpha1.DiskGrowth{WorkspaceMax: "30Gi", Root: "2Gi", RootMax: "30Gi", Threshold: "80%", Step: "2x"},
		},
	} {
		r, g := boxDisks(tc.target, tc.box)
		if !proto.Equal(r, tc.resources) || !proto.Equal(g, tc.growth) {
			t.Errorf("%s: resources %v, growth %v; want %v, %v", what, r, g, tc.resources, tc.growth)
		}
	}
}
