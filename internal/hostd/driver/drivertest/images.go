package drivertest

import (
	"bytes"
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
	t.Run("Resolve", func(t *testing.T) {
		cfg, err := im.Resolve(ctx, h.Image)
		mustNot(t, "resolving", err)
		if _, digest, _ := strings.Cut(h.Image, "@"); cfg.Digest != digest {
			t.Errorf("%s resolved to digest %q", h.Image, cfg.Digest)
		}
		_, err = im.Resolve(ctx, h.MissingImage)
		is(t, "resolving what the registry lacks", err, driver.ErrNotFound)
		ref, err := build(t, driver.BuildSpec{}, map[string]string{"Dockerfile": "FROM " + h.Image + "\nUSER 4242\n"})
		mustNot(t, "building an image of another user", err)
		cfg, err = im.Resolve(ctx, ref)
		mustNot(t, "resolving what was built", err)
		if _, digest, _ := strings.Cut(ref, "@"); cfg.Digest != digest || cfg.User != "4242" {
			t.Errorf("%s resolved to %+v, want its digest and user 4242", ref, cfg)
		}
	})
}
