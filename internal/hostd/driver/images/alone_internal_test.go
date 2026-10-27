package images

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// TestWithoutDockerBuildKitPushesTheBuild: a driver without Docker has
// BuildKit export each build as an image it pushes to the registry, over
// plain HTTP as the configuration says, under a tag of the build's own,
// and nothing to a layout of its own.
func TestWithoutDockerBuildKitPushesTheBuild(t *testing.T) {
	for _, plain := range []bool{true, false} {
		d, err := New(Config{Registry: "registry.example:5000", NoDocker: true, PlainHTTP: plain, ScratchDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		ex, err := d.pushExports("registry.example:5000/web/box")
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].Type != client.ExporterImage || ex[0].OutputDir != "" {
			t.Fatalf("plain HTTP %v: the exports %+v, want the image alone", plain, ex)
		}
		want := map[string]string{"push": "true", "registry.insecure": map[bool]string{true: "true", false: "false"}[plain]}
		for k, v := range want {
			if ex[0].Attrs[k] != v {
				t.Errorf("plain HTTP %v: the export's %s is %q, want %q", plain, k, ex[0].Attrs[k], v)
			}
		}
		if tag, ok := strings.CutPrefix(ex[0].Attrs["name"], "registry.example:5000/web/box:build-"); !ok || len(tag) < 16 {
			t.Errorf("plain HTTP %v: the export's name %q, want the repository with a build's own tag", plain, ex[0].Attrs["name"])
		}
	}
}

// TestWithoutDockerEachBuildIsPushedUnderATagOfItsOwn: Build without
// Docker solves with BuildKit pushing the image as it exports it, and
// answers the digest BuildKit pushed; two builds of one repository, as a
// devcontainer's base and its layer are, are pushed under two tags, so
// neither leaves the other untagged.
func TestWithoutDockerEachBuildIsPushedUnderATagOfItsOwn(t *testing.T) {
	root := t.TempDir()
	d, err := New(Config{Registry: "registry.example:5000", NoDocker: true, PlainHTTP: true, ScratchDir: filepath.Join(root, "scratch"),
		BuildKit: "unix://" + filepath.Join(root, "none.sock")})
	if err != nil {
		t.Fatal(err)
	}
	var solved []client.SolveOpt
	d.solve = func(_ context.Context, _ *client.Client, opt client.SolveOpt, status chan *client.SolveStatus) (*client.SolveResponse, error) {
		close(status)
		solved = append(solved, opt)
		return &client.SolveResponse{ExporterResponse: map[string]string{exptypes.ExporterImageDigestKey: "sha256:" + strings.Repeat("a", 64)}}, nil
	}
	for range 2 {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: 13})
		_, _ = tw.Write([]byte("FROM scratch\n"))
		_ = tw.Close()
		ref, err := d.Build(t.Context(), driver.BuildSpec{Repository: "web/box", Context: &b})
		if err != nil || ref != "registry.example:5000/web/box@sha256:"+strings.Repeat("a", 64) {
			t.Fatalf("the build answered %q, %v", ref, err)
		}
	}
	tags := map[string]bool{}
	for _, opt := range solved {
		if len(opt.Exports) != 1 || opt.Exports[0].Type != client.ExporterImage || opt.Exports[0].Attrs["push"] != "true" || len(opt.OCIStores) != 0 {
			t.Fatalf("the build solved with the exports %+v and OCI stores %v, want BuildKit to push the image alone", opt.Exports, opt.OCIStores)
		}
		tags[opt.Exports[0].Attrs["name"]] = true
	}
	if len(solved) != 2 || len(tags) != 2 {
		t.Errorf("two builds pushed as %v", tags)
	}
}

// TestADriverWithoutDockerHoldsNothing: a driver without Docker is made
// with no Docker client and no directory to hold images in, holds
// nothing, and what needs Docker says it has none rather than failing
// otherwise; a driver neither says it is without Docker nor has it is not
// made.
func TestADriverWithoutDockerHoldsNothing(t *testing.T) {
	ctx := t.Context()
	d, err := New(Config{Registry: "registry.example", NoDocker: true, ScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := d.Unpushed(ctx); err != nil || len(list) != 0 {
		t.Errorf("unpushed: %v, %v", list, err)
	}
	if err := d.Push(ctx, "registry.example/web/box@sha256:00"); err != nil {
		t.Errorf("a push of what it built, which BuildKit pushed: %v", err)
	}
	for name, err := range map[string]error{
		"Pull":   d.Pull(ctx, "registry.example/web/box@sha256:00"),
		"Remove": d.Remove(ctx, "registry.example/web/box@sha256:00"),
		"Local": func() error {
			_, err := d.Local(ctx, "registry.example/web/box@sha256:00")
			return err
		}(),
		"List": func() error {
			_, err := d.List(ctx)
			return err
		}(),
		"DiskUsage": func() error {
			_, err := d.DiskUsage(ctx)
			return err
		}(),
	} {
		if !errors.Is(err, ErrNoDocker) {
			t.Errorf("%s: %v, want ErrNoDocker", name, err)
		}
	}
	if _, err := New(Config{Registry: "registry.example", NoDocker: true, ScratchDir: t.TempDir(), UnpushedDir: t.TempDir()}); err == nil {
		t.Error("a driver without Docker made with a directory to hold images in")
	}
	if _, err := New(Config{Registry: "registry.example", ScratchDir: t.TempDir(), UnpushedDir: t.TempDir()}); err == nil {
		t.Error("a driver made with no Docker client, not saying it has none")
	}
}
