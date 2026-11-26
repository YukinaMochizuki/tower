package apiserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// imageGoldensOf is the names of the project's box-image goldens made of
// the base digest with add-ons whose names, in order, are addons
// ("browser" for "browser@<hash>").
func (w *world) imageGoldensOf(digest string, addons ...string) []string {
	w.t.Helper()
	res, err := w.asSystem().generic.List(w.t.Context(), registry.Golden, client.ListOptions{Project: "web"})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, o := range res.Items {
		g := o.(*envv1alpha1.Golden)
		in := g.GetSpec().GetBoxImage()
		if in == nil || in.GetBaseImageDigest() != digest || g.GetMetadata().GetDeleteTime() != nil || len(in.GetAddons()) != len(addons) {
			continue
		}
		match := true
		for i, a := range in.GetAddons() {
			name, _, _ := strings.Cut(a, "@")
			match = match && name == addons[i]
		}
		if match {
			out = append(out, g.GetMetadata().GetName())
		}
	}
	return out
}

// TestAnAddonImageIsMadeAheadOfItsBoxes: once a repository's boxes asked
// for an add-on set, the image of that set is made again when the
// repository's devcontainer image changes, before any box asks for it, once
// the head has settled, so a burst of commits builds once; the next box of
// the set is made of it, and a base tag that moves under the same
// devcontainer counts too. A set last asked for longer than warm_window
// ago is not made.
func TestAnAddonImageIsMadeAheadOfItsBoxes(t *testing.T) {
	t.Parallel()
	w := newWorld(t, realResolver())
	url := "https://git.example.com/api.git"
	devcontainer := func(tag string) map[string]string {
		return map[string]string{".devcontainer/devcontainer.json": `{"image": "registry.example.com/base:` + tag + `", "remoteUser": "vscode"}`}
	}
	for _, tag := range []string{"1", "2", "3", "4"} {
		w.fake.SetImage("registry.example.com/base:"+tag, provider.ImageConfig{Digest: "sha256:b" + tag, User: "root"})
	}
	w.fake.SetRemote(url, "main", devcontainer("1"), "c1")
	browser := func(b *envv1alpha1.Box) { b.Spec.Addons = []string{"browser"} }
	first := w.readyBox("dev", browser)
	if got := w.imageGoldensOf("sha256:b1", "browser"); len(got) != 1 || got[0] != first.GetStatus().GetImageGolden() {
		t.Fatalf("the browser images of base:1 %v, want the box's %s", got, first.GetStatus().GetImageGolden())
	}

	// The devcontainer moves to base:2, then, within warm_settle, to
	// base:3: only base:3's browser image is made, once it has settled, with
	// no box asking for it.
	w.fake.SetRemote(url, "main", devcontainer("2"), "c1", "c2")
	w.advanceFetching(apiserver.MirrorFetchEvery)
	w.fake.SetRemote(url, "main", devcontainer("3"), "c1", "c2", "c3")
	w.advanceFetching(apiserver.MirrorFetchEvery)
	if got := w.imageGoldensOf("sha256:b3", "browser"); len(got) != 0 {
		t.Fatalf("base:3's browser image was made before its head settled: %v", got)
	}
	w.advance(apiserver.DefaultWarmSettle + time.Minute)
	var ahead string
	testutil.Eventually(t, "the browser image of base:3, made ahead", func() (bool, string) {
		got := w.imageGoldensOf("sha256:b3", "browser")
		if len(got) == 1 {
			ahead = got[0]
		}
		return len(got) == 1, fmt.Sprint(got)
	})
	if got := w.imageGoldensOf("sha256:b2", "browser"); len(got) != 0 {
		t.Errorf("base:2, the head for less than warm_settle, had its browser image made: %v", got)
	}
	if b := w.readyBox("next", browser); b.GetStatus().GetImageGolden() != ahead {
		t.Errorf("the next browser box is made of %q, want the image made ahead, %q", b.GetStatus().GetImageGolden(), ahead)
	}

	// A base tag moved under an unchanged devcontainer is found once
	// warm_settle has passed since the last look.
	w.fake.SetImage("registry.example.com/base:3", provider.ImageConfig{Digest: "sha256:b3-moved", User: "root"})
	w.advance(apiserver.DefaultWarmSettle + time.Minute)
	testutil.Eventually(t, "the browser image of the moved base:3, made ahead", func() (bool, string) {
		got := w.imageGoldensOf("sha256:b3-moved", "browser")
		return len(got) == 1, fmt.Sprint(got)
	})

	// Once no box asked for it within warm_window, it is no longer made.
	for _, name := range []string{"dev", "next"} {
		if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	w.advance(apiserver.DefaultWarmWindow + time.Hour)
	w.fake.SetRemote(url, "main", devcontainer("4"), "c1", "c2", "c3", "c4")
	w.advanceFetching(apiserver.MirrorFetchEvery)
	w.advance(apiserver.DefaultWarmSettle + time.Minute)
	if got := w.imageGoldensOf("sha256:b4", "browser"); len(got) != 0 {
		t.Errorf("the browser image of base:4 was made though no box asked for it within warm_window: %v", got)
	}
}
