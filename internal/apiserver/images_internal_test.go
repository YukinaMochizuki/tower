package apiserver

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	"github.com/jonboulle/clockwork"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
)

// TestAnImageIsBuiltFromTheBaseItsGoldenNames: the base image a golden's
// inputs name by digest is what its build is FROM, though its tag moved
// to another since the golden was made.
func TestAnImageIsBuiltFromTheBaseItsGoldenNames(t *testing.T) {
	s := &Server{cfg: Config{Resolver: devcontainer.Real{}}}
	p := fake.New("t1", clockwork.NewRealClock())
	p.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:moved"})
	g := &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "img-1"},
		Spec:     &envv1alpha1.GoldenSpec{Inputs: &envv1alpha1.GoldenSpec_BoxImage{BoxImage: &envv1alpha1.BoxImageInputs{BaseImageDigest: "sha256:made"}}},
	}
	ref, err := s.resolveAndBuild(t.Context(), p, g, devcontainer.Request{Target: "t1", Image: "ubuntu:24.04"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(p.BuildContext(ref)))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "Dockerfile" {
			df, _ := io.ReadAll(tr)
			if string(df) != "FROM ubuntu:24.04@sha256:made\n" {
				t.Errorf("built with\n%s", df)
			}
			return
		}
	}
}
