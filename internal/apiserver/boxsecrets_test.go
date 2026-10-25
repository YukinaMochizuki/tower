package apiserver_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// TestSecretFilesAreWrittenAtEachStart: a box's secret files are written
// into its instance, with their modes, before any command runs there, and
// again at each start of it, over what was there; their values reach no
// object, event, log line or database file.
func TestSecretFilesAreWrittenAtEachStart(t *testing.T) {
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const hosts, key = "canary-hosts-Rb41", "canary-key-Jm09"
	w.mustApply(registry.Secret, secret("web", "gh", map[string]string{"hosts": hosts}))
	w.mustApply(registry.Secret, secret("web", "deploy", map[string]string{"key": key}))
	const hostsPath, keyPath = "/root/.config/gh/hosts.yml", "/etc/deploy/key"
	// Every command in the box finds its files there already.
	var mu sync.Mutex
	var early []string
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		files := w.fake.Files(req.Instance)
		if files[hostsPath] != hosts || files[keyPath] != key {
			mu.Lock()
			early = append(early, req.Command[0])
			mu.Unlock()
		}
		return 0, false
	})
	b := w.readyBox("dev", func(b *envv1alpha1.Box) {
		b.Spec.SecretFiles = []*envv1alpha1.SecretFile{
			{Secret: "gh", Key: "hosts", Path: hostsPath},
			{Secret: "deploy", Key: "key", Path: keyPath, Mode: 0o640},
		}
	})
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	if m, ok := w.fake.Mode(ref, hostsPath); !ok || m != 0o600 {
		t.Errorf("%s was written with mode %o, %v; want 0600", hostsPath, m, ok)
	}
	if m, ok := w.fake.Mode(ref, keyPath); !ok || m != 0o640 {
		t.Errorf("%s was written with mode %o, %v; want 0640", keyPath, m, ok)
	}
	envs := w.as(workstream).envs
	if _, err := exec(t.Context(), envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("true")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(early) > 0 {
		t.Errorf("commands ran before the secret files were written: %v", early)
	}
	mu.Unlock()

	// Changed in the box, then the box stopped and started: written again.
	if err := writeFile(t.Context(), envs, boxEnv("dev"), keyPath, "changed", 0); err != nil {
		t.Fatal(err)
	}
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	stopped := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	stopped.Spec.RunState = envv1alpha1.RunState_RUN_STATE_RUNNING
	w.mustApply(registry.Box, stopped)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if got := w.fake.Files(ref)[keyPath]; got != key {
		t.Errorf("after a start, %s holds %q; want its secret's value again", keyPath, got)
	}
	w.waitSettled()
	w.nowhere(hosts, &logs)
	w.nowhere(key, &logs)
}

// TestASecretFileThatCannotBeWrittenFailsTheBox: a secret file whose value
// is gone fails the box, saying which secret and key, never a value.
func TestASecretFileThatCannotBeWrittenFailsTheBox(t *testing.T) {
	w := newWorld(t)
	const value = "canary-refused-Ty62"
	w.mustApply(registry.Secret, secret("web", "gh", map[string]string{"hosts": value}))
	if _, ok := w.getRepo("api"); !ok {
		w.mustApply(registry.Repository, repository("api"))
	}
	// The key leaves the secret after the box names it, while its image
	// is held building: it cannot be opened when the file is due.
	release := w.fake.HoldBuilds()
	b := box("dev", "api")
	b.Spec.SecretFiles = []*envv1alpha1.SecretFile{{Secret: "gh", Key: "hosts", Path: "/etc/hosts.yml"}}
	w.mustApply(registry.Box, b)
	w.mustApply(registry.Secret, secret("web", "gh", map[string]string{"other": value}))
	release()
	failed := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	msg := conditions.Find(failed.GetStatus().GetConditions(), apiserver.BoxReady).GetMessage()
	if !strings.Contains(msg, `"gh"`) || !strings.Contains(msg, `"hosts"`) || strings.Contains(msg, value) {
		t.Errorf("the box failed saying %q; want the secret and its key named, no value", msg)
	}
}

// TestABoxOfItsOwnImageBuild: a box with an image_build has its image built
// from its Dockerfile and files, with its add-ons, as a golden of those
// inputs that boxes of the same build share and another build does not.
func TestABoxOfItsOwnImageBuild(t *testing.T) {
	w := newWorld(t)
	build := &envv1alpha1.ImageBuild{
		Dockerfile: "FROM debian:12\nCOPY etc/app.conf /etc/app.conf\n",
		Files:      map[string][]byte{"etc/app.conf": []byte("a=1\n")},
	}
	own := func(name string, b *envv1alpha1.ImageBuild) *envv1alpha1.Box {
		x := box(name)
		x.Spec.ImageBuild, x.Spec.Addons = b, []string{"browser"}
		w.mustApply(registry.Box, x)
		return w.waitBox(name, envv1alpha1.BoxPhase_BOX_PHASE_READY)
	}
	one := own("one", build)
	g, _ := w.golden(one.GetStatus().GetImageGolden())
	inputs := g.GetSpec().GetBoxImage()
	if inputs.GetBuildHash() == "" || inputs.GetDevcontainerHash() != "" || inputs.GetBaseImageDigest() != "" || !slices.Equal(inputs.GetAddons(), []string{"browser@1"}) {
		t.Errorf("the image's inputs: %v", inputs)
	}
	ctxFiles := tarFiles(t, w.fake.BuildContext(g.GetStatus().GetArtifact()))
	if ctxFiles["etc/app.conf"] != "a=1\n" || !bytes.HasPrefix([]byte(ctxFiles["Dockerfile"]), []byte(build.GetDockerfile())) {
		t.Errorf("the image was built from %q", ctxFiles)
	}
	if two := own("two", build); two.GetStatus().GetImageGolden() != one.GetStatus().GetImageGolden() {
		t.Errorf("two boxes of one build have images %q and %q", one.GetStatus().GetImageGolden(), two.GetStatus().GetImageGolden())
	}
	other := &envv1alpha1.ImageBuild{Dockerfile: build.GetDockerfile(), Files: map[string][]byte{"etc/app.conf": []byte("a=2\n")}}
	if three := own("three", other); three.GetStatus().GetImageGolden() == one.GetStatus().GetImageGolden() {
		t.Error("a build of another file shares the image")
	}
}

func tarFiles(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.FileInfo().Mode()&fs.ModeType != 0 {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(data)
	}
}
