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
	t.Run("Build", func(t *testing.T) {
		build := func(dockerfile string) string {
			t.Helper()
			var ctxTar bytes.Buffer
			_, err := ctxTar.ReadFrom(tarOf(t, map[string]string{"Dockerfile": dockerfile}))
			mustNot(t, "making a context", err)
			ref, err := im.Build(ctx, "drivertest/"+name(t, ""), &ctxTar)
			mustNot(t, "building", err)
			if !strings.Contains(ref, "@sha256:") {
				t.Errorf("image %q is no reference by digest", ref)
			}
			return ref
		}
		one, again, other := build("FROM "+h.Image+"\n"), build("FROM "+h.Image+"\n"), build("FROM "+h.Image+"\nRUN true\n")
		if one != again {
			t.Errorf("one context built %q, then %q", one, again)
		}
		if one == other {
			t.Errorf("two contexts built one image %q", one)
		}
		mustNot(t, "pulling what was built", im.Pull(ctx, one))
		if _, err := im.Build(ctx, "drivertest/"+name(t, "bad"), tarOf(t, map[string]string{"Dockerfile": "NOT A DOCKERFILE\n"})); err == nil {
			t.Error("a Dockerfile that does not build built")
		}
	})
}
