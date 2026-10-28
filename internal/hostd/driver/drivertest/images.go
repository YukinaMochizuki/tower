package drivertest

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// RunImages runs the image driver's suite.
func RunImages(t *testing.T, h Harness) {
	im, ctx := h.Drivers.Images, t.Context()
	t.Run("Pull", func(t *testing.T) {
		mustNot(t, "pulling", im.Pull(ctx, h.Image))
		mustNot(t, "pulling again", im.Pull(ctx, h.Image))
		is(t, "pulling what the registry lacks", im.Pull(ctx, h.MissingImage), driver.ErrNotFound)
	})
	build := func(t *testing.T, spec driver.BuildSpec, files map[string]string) (string, error) {
		t.Helper()
		var ctxTar bytes.Buffer
		_, err := ctxTar.ReadFrom(tarOf(t, files))
		mustNot(t, "making a context", err)
		spec.Repository, spec.Context = "drivertest/"+name(t, ""), &ctxTar
		return im.Build(ctx, spec)
	}
	t.Run("Build", func(t *testing.T) {
		built := func(dockerfile string) string {
			t.Helper()
			ref, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": dockerfile})
			mustNot(t, "building", err)
			if !strings.Contains(ref, "@sha256:") {
				t.Errorf("image %q is no reference by digest", ref)
			}
			return ref
		}
		one, again, other := built("FROM "+h.Image+"\n"), built("FROM "+h.Image+"\n"), built("FROM "+h.Image+"\nRUN true\n")
		if one != again {
			t.Errorf("one context built %q, then %q", one, again)
		}
		if one == other {
			t.Errorf("two contexts built one image %q", one)
		}
		mustNot(t, "pulling what was built", im.Pull(ctx, one))
		if _, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "NOT A DOCKERFILE\n"}); err == nil {
			t.Error("a Dockerfile that does not build built")
		}
	})
	t.Run("BuildADirectoryWithADockerfileApart", func(t *testing.T) {
		// The context is ctx alone: what the Dockerfile copies is in it,
		// and the Dockerfile is beside it, as a devcontainer's often is.
		files := map[string]string{
			"ctx/hello":        "hello\n",
			"docker/Build.df":  "FROM " + h.Image + "\nCOPY hello /hello\n",
			"docker/Escape.df": "FROM " + h.Image + "\n",
		}
		ref, err := build(t, driver.BuildSpec{Dir: "ctx", Dockerfile: "docker/Build.df"}, files)
		mustNot(t, "building a directory with its Dockerfile apart", err)
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("image %q is no reference by digest", ref)
		}
		for what, spec := range map[string]driver.BuildSpec{
			"a Dockerfile outside the tar": {Dockerfile: "../Escape.df"},
			"a directory outside the tar":  {Dir: "../ctx", Dockerfile: "docker/Escape.df"},
			"a directory not in the tar":   {Dir: "nowhere", Dockerfile: "docker/Escape.df"},
		} {
			if _, err := build(t, spec, files); err == nil {
				t.Errorf("%s built", what)
			}
		}
	})
	t.Run("ListAndRemove", func(t *testing.T) {
		pulled := func(name string) string {
			t.Helper()
			ref, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "FROM " + h.Image + "\nLABEL drivertest=" + name + "\n"})
			mustNot(t, "building "+name, err)
			mustNot(t, "pulling "+name, im.Pull(ctx, ref))
			return ref
		}
		listed := func(ref string) bool {
			t.Helper()
			list, err := im.List(ctx)
			mustNot(t, "listing images", err)
			for _, i := range list {
				if slices.Contains(i.Refs, ref) {
					return true
				}
			}
			return false
		}
		loose, used := pulled("loose"), pulled("used")
		if !listed(loose) || !listed(used) {
			t.Fatalf("images pulled are not listed")
		}
		if n, err := im.DiskUsage(ctx); err != nil || n == 0 {
			t.Errorf("the images hold %d bytes, %v", n, err)
		}
		mustNot(t, "removing an image", im.Remove(ctx, loose))
		if listed(loose) {
			t.Error("an image removed is listed")
		}
		mustNot(t, "removing it again", im.Remove(ctx, loose))
		// One a container is made of stays.
		c := h.containers()
		c.Create = func(t *testing.T, spec driver.GuestSpec) error {
			return h.Drivers.Containers.Create(t.Context(), driver.ContainerSpec{GuestSpec: spec, Image: h.Local(t, used)})
		}
		_ = c.guestWithDisk(t, h, "image-user")
		is(t, "removing an image a container is made of", im.Remove(ctx, used), driver.ErrBusy)
		if !listed(used) {
			t.Error("an image a container is made of went")
		}
	})
	// What is built is the host's at once, before it is pushed: a
	// container is made of it, a build is FROM it, it is resolved, and a
	// pull of it is none; it is left to push until it is pushed, which
	// puts the digest it was built as in the registry.
	t.Run("ABuildIsTheHostsBeforeItIsPushed", func(t *testing.T) {
		left := func(ref string) bool {
			t.Helper()
			list, err := im.Unpushed(ctx)
			mustNot(t, "listing what is left to push", err)
			return slices.Contains(list, ref)
		}
		base, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "FROM " + h.Image + "\nLABEL drivertest=" + name(t, "base") + "\nUSER 4242\n"})
		mustNot(t, "building", err)
		if !left(base) {
			t.Errorf("%s, built, is not left to push", base)
		}
		mustNot(t, "pulling what was built", im.Pull(ctx, base))
		cfg, err := im.Resolve(ctx, base)
		if _, digest, _ := strings.Cut(base, "@"); err != nil || cfg.Digest != digest || cfg.User != "4242" {
			t.Errorf("%s, not pushed, resolved to %+v, %v; want its digest and user 4242", base, cfg, err)
		}
		layer, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "FROM " + base + "\nLABEL drivertest-layer=" + name(t, "layer") + "\n"})
		mustNot(t, "building FROM an image not pushed", err)
		if cfg, err := im.Resolve(ctx, layer); err != nil || cfg.User != "4242" {
			t.Errorf("%s, FROM %s, resolved to %+v, %v; want user 4242", layer, base, cfg, err)
		}
		c := h.containers()
		c.Create = func(t *testing.T, spec driver.GuestSpec) error {
			return h.Drivers.Containers.Create(t.Context(), driver.ContainerSpec{GuestSpec: spec, Image: h.Local(t, layer)})
		}
		g := c.guestWithDisk(t, h, "built")
		if got, err := h.Drivers.Containers.Inspect(ctx, g); err != nil || !got.Running {
			t.Errorf("a container of %s, not pushed: %+v, %v", layer, got, err)
		}
		list, err := im.List(ctx)
		mustNot(t, "listing images", err)
		for _, ref := range []string{base, layer} {
			if !slices.ContainsFunc(list, func(i driver.HostImage) bool { return slices.Contains(i.Refs, ref) }) {
				t.Errorf("%s, built, is not listed: %+v", ref, list)
			}
		}
		for _, ref := range []string{base, layer} {
			mustNot(t, "pushing "+ref, im.Push(ctx, ref))
			if left(ref) {
				t.Errorf("%s, pushed, is left to push", ref)
			}
			mustNot(t, "pushing "+ref+" again", im.Push(ctx, ref))
			// Read from the registry now.
			cfg, err := im.Resolve(ctx, ref)
			if _, digest, _ := strings.Cut(ref, "@"); err != nil || cfg.Digest != digest {
				t.Errorf("%s, pushed, resolved to %+v, %v; want its digest", ref, cfg, err)
			}
		}
	})
	t.Run("Resolve", func(t *testing.T) {
		cfg, err := im.Resolve(ctx, h.Image)
		mustNot(t, "resolving", err)
		if _, digest, _ := strings.Cut(h.Image, "@"); cfg.Digest != digest {
			t.Errorf("%s resolved to digest %q", h.Image, cfg.Digest)
		}
		_, err = im.Resolve(ctx, h.MissingImage)
		is(t, "resolving what the registry lacks", err, driver.ErrNotFound)
		ref, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "FROM " + h.Image + "\nUSER 4242\nENV DRIVERTEST_CACHE=/cache\n"})
		mustNot(t, "building an image of another user", err)
		cfg, err = im.Resolve(ctx, ref)
		mustNot(t, "resolving what was built", err)
		if _, digest, _ := strings.Cut(ref, "@"); cfg.Digest != digest || cfg.User != "4242" || !slices.Contains(cfg.Env, "DRIVERTEST_CACHE=/cache") {
			t.Errorf("%s resolved to %+v, want its digest, user 4242 and DRIVERTEST_CACHE=/cache in its environment", ref, cfg)
		}
	})
}
