// Package providertest is the contract every provider is held to: one
// suite over provider.Provider and the Instance contract, run against the
// fake in CI and against a real provider where one runs. A provider passes
// it through a Harness, which makes Instances as tower's controllers would.
package providertest

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// Harness is what the suite needs of one target besides its Provider.
type Harness struct {
	Provider provider.Provider
	// Target is the target's name, which Instances name.
	Target string
	// Start makes an Instance of the spec in the harness's project, as
	// tower's controllers do, waits until its provider reports it RUNNING,
	// and returns it with its status.
	Start func(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) (provider.Instance, *envv1alpha1.InstanceStatus)
	// Stop sets the instance's run_state STOPPED and waits until its
	// provider reports it STOPPED.
	Stop func(t *testing.T, in provider.Instance)
	// Golden makes a READY Golden holding the artifact and returns its
	// name, for an Instance to clone its disk from.
	Golden func(t *testing.T, artifact provider.Artifact) string
	// Image and BaseImage are what a container and a VM boot from; an
	// empty BaseImage skips the VM, for a provider that runs none yet.
	Image, BaseImage string
	// Skip names the tests the provider does not pass, each with why, such
	// as a hangar target's mirrors.
	Skip map[string]string
	// ProcessLimits, if set, sets the provider's cap of a process's log and
	// how many finished processes an instance keeps; nil skips the test of
	// them.
	ProcessLimits func(logCap int64, finished int)
	// Remote is a repository the provider can fetch and push to, on
	// Branch; RemoteHead is the commit Branch is at.
	Remote, Branch, RemoteHead string
	// RemoteDockerfile is the path of a Dockerfile at the remote's head
	// that builds from Image with the repository's root as its context;
	// empty skips building from a mirror.
	RemoteDockerfile string
}

// Run runs the suite.
func Run(t *testing.T, h Harness) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"InstancesRun", testInstancesRun},
		{"Exec", testExec},
		{"EnvironmentIsPerCommand", testEnvironmentIsPerCommand},
		{"Files", testFiles},
		{"Copy", testCopy},
		{"WhatIsNotThere", testWhatIsNotThere},
		{"BuildImage", testBuildImage},
		{"ResolveImage", testResolveImage},
		{"ReadAndBuildAMirror", testReadAndBuildAMirror},
		{"CaptureAndClone", testCaptureAndClone},
		{"MirrorAndCheckout", testMirrorAndCheckout},
		{"PushNeedsItsLease", testPushNeedsItsLease},
		{"Ticket", testTicket},
		{"ProcessOutput", testProcessOutput},
		{"ProcessReattach", testProcessReattach},
		{"ProcessSignalAndList", testProcessSignalAndList},
		{"ProcessesDieWithTheirInstance", testProcessesDieWithTheirInstance},
		{"ProcessLimits", testProcessLimits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why, ok := h.Skip[tt.name]; ok {
				t.Skip(why)
			}
			tt.fn(t, h)
		})
	}
}

func (h Harness) container(network string) *envv1alpha1.InstanceSpec {
	return &envv1alpha1.InstanceSpec{
		Target: h.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: h.Image,
		Network: network, Alias: "conformance",
	}
}

func testInstancesRun(t *testing.T, h Harness) {
	in, st := h.Start(t, "runs", h.container("net-a"))
	if st.GetAddress() == "" || st.GetStartTime() == nil {
		t.Errorf("a running instance's status: %v, want its address and start time", st)
	}
	h.Stop(t, in)
	if _, err := h.Provider.Exec(t.Context(), provider.ExecRequest{Instance: in, Command: []string{"true"}}); err == nil {
		t.Error("a command ran in a stopped instance")
	}
	if h.BaseImage == "" {
		return
	}
	// Its resources given, as tower's controllers always give a VM's: its
	// disk must hold its base image.
	vm := &envv1alpha1.InstanceSpec{
		Target: h.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: h.BaseImage, Network: "net-a", Alias: "vm",
		Resources: &envv1alpha1.Resources{Cpu: "1", Memory: "1Gi", Disk: "2Gi"},
	}
	if _, st := h.Start(t, "vm", vm); st.GetAddress() == "" {
		t.Errorf("a running VM's status: %v, want its address", st)
	}
}

// run runs a command and returns its exit code and output.
func run(t *testing.T, h Harness, in provider.Instance, env map[string]string, command ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := h.Provider.Exec(t.Context(), provider.ExecRequest{Instance: in, Command: command, Env: env, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("running %v: %v", command, err)
	}
	return code, out.String()
}

func testExec(t *testing.T, h Harness) {
	in, _ := h.Start(t, "exec", h.container("net-a"))
	if code, out := run(t, h, in, nil, "echo", "hello", "world"); code != 0 || out != "hello world\n" {
		t.Errorf("echo: %d %q", code, out)
	}
	if code, _ := run(t, h, in, nil, "false"); code != 1 {
		t.Errorf("false exited %d, want 1", code)
	}
	if code, _ := run(t, h, in, nil, "true"); code != 0 {
		t.Errorf("true exited %d, want 0", code)
	}
	if code, _ := run(t, h, in, nil, "exit", "3"); code != 3 {
		t.Errorf("exit 3 exited %d", code)
	}
	// stdout and stderr reach the caller in the order they were written.
	var mu sync.Mutex
	var got []string
	writer := func(stream string) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			for line := range strings.Lines(string(p)) {
				got = append(got, stream+":"+strings.TrimSuffix(line, "\n"))
			}
			return len(p), nil
		})
	}
	want := []string{"out:one", "err:two", "out:three", "err:four"}
	code, err := h.Provider.Exec(t.Context(), provider.ExecRequest{
		Instance: in, Command: append([]string{"emit"}, want...), Stdout: writer("out"), Stderr: writer("err"),
	})
	if err != nil || code != 0 {
		t.Fatalf("emit: %d, %v", code, err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the output came as %q, want %q", got, want)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func testEnvironmentIsPerCommand(t *testing.T, h Harness) {
	in, _ := h.Start(t, "env", h.container("net-a"))
	if _, out := run(t, h, in, map[string]string{"TOWER_CONFORMANCE": "given"}, "env"); !strings.Contains(out, "TOWER_CONFORMANCE=given\n") {
		t.Errorf("the environment given a command is not its own: %q", out)
	}
	if _, out := run(t, h, in, nil, "env"); strings.Contains(out, "TOWER_CONFORMANCE") {
		t.Errorf("the environment given one command reached the next: %q", out)
	}
}

func testFiles(t *testing.T, h Harness) {
	in, _ := h.Start(t, "files", h.container("net-a"))
	ctx := t.Context()
	if err := h.Provider.WriteFile(ctx, in, "/tmp/conformance/a.txt", 0o644, 0, strings.NewReader("cont")); err != nil {
		t.Fatal(err)
	}
	if err := h.Provider.WriteFile(ctx, in, "/tmp/conformance/a.txt", 0o644, 4, strings.NewReader("ents")); err != nil {
		t.Fatal(err)
	}
	if err := h.Provider.WriteFile(ctx, in, "/tmp/conformance/a.txt", 0o644, 2, strings.NewReader("xx")); !errors.Is(err, provider.ErrFailed) {
		t.Errorf("a chunk not where the file ends: %v, want ErrFailed", err)
	}
	var got bytes.Buffer
	if err := h.Provider.ReadFile(ctx, in, "/tmp/conformance/a.txt", &got); err != nil || got.String() != "contents" {
		t.Errorf("read back %q, %v", got.String(), err)
	}
	if err := h.Provider.WriteFile(ctx, in, "/tmp/conformance/a.txt", 0o644, 0, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	got.Reset()
	if err := h.Provider.ReadFile(ctx, in, "/tmp/conformance/a.txt", &got); err != nil || got.String() != "new" {
		t.Errorf("a write at offset 0 left %q, %v; want the file replaced", got.String(), err)
	}
	if code, out := run(t, h, in, nil, "cat", "/tmp/conformance/a.txt"); code != 0 || out != "new" {
		t.Errorf("the file as a command reads it: %d %q", code, out)
	}
	if err := h.Provider.ReadFile(ctx, in, "/tmp/conformance/none", io.Discard); err == nil {
		t.Error("read a file that is not there")
	}
}

func testCopy(t *testing.T, h Harness) {
	a, _ := h.Start(t, "copy-a", h.container("net-a"))
	b, _ := h.Start(t, "copy-b", h.container("net-a"))
	ctx := t.Context()
	if err := h.Provider.WriteFile(ctx, a, "/tmp/out/result.txt", 0o644, 0, strings.NewReader("built")); err != nil {
		t.Fatal(err)
	}
	if err := h.Provider.Copy(ctx, a, "/tmp/out", b, "/tmp/in"); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := h.Provider.ReadFile(ctx, b, "/tmp/in/result.txt", &got); err != nil || got.String() != "built" {
		t.Errorf("the copy: %q, %v", got.String(), err)
	}
}

func testWhatIsNotThere(t *testing.T, h Harness) {
	in, _ := h.Start(t, "present", h.container("net-a"))
	absent := provider.Instance{Project: in.Project, Name: "absent"}
	ctx := t.Context()
	if _, err := h.Provider.Exec(ctx, provider.ExecRequest{Instance: absent, Command: []string{"true"}}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("a command in an instance that is not there: %v, want ErrNotFound", err)
	}
	if err := h.Provider.ReadFile(ctx, absent, "/etc/hostname", io.Discard); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("a file of an instance that is not there: %v, want ErrNotFound", err)
	}
	if _, err := h.Provider.Capture(ctx, absent); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("capturing an instance that is not there: %v, want ErrNotFound", err)
	}
	if err := h.Provider.DeleteArtifact(ctx, "conformance/never-made"); err != nil {
		t.Errorf("deleting an artifact that is not there: %v, want no error", err)
	}
}

// buildContext is a tar holding a Dockerfile of the content.
func buildContext(t *testing.T, dockerfile string) io.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func testBuildImage(t *testing.T, h Harness) {
	ctx := t.Context()
	build := func(dockerfile string) string {
		t.Helper()
		image, err := h.Provider.BuildImage(ctx, provider.BuildRequest{Context: buildContext(t, dockerfile), Repository: "conformance/images"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("image %q is not a reference by digest", image)
		}
		return image
	}
	one, again, other := build("FROM "+h.Image+"\n"), build("FROM "+h.Image+"\n"), build("FROM "+h.Image+"\nRUN true\n")
	if one != again {
		t.Errorf("the same context built %q, then %q", one, again)
	}
	if one == other {
		t.Errorf("two contexts built one image %q", one)
	}
}

func testResolveImage(t *testing.T, h Harness) {
	ctx := t.Context()
	cfg, err := h.Provider.ResolveImage(ctx, h.Image)
	if err != nil {
		t.Fatal(err)
	}
	if _, digest, ok := strings.Cut(h.Image, "@"); ok && cfg.Digest != digest {
		t.Errorf("%s resolved to digest %q", h.Image, cfg.Digest)
	}
	// What tower builds, it resolves, with the user it runs as.
	image, err := h.Provider.BuildImage(ctx, provider.BuildRequest{Context: buildContext(t, "FROM "+h.Image+"\nUSER 4242\n"), Repository: "conformance/images"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = h.Provider.ResolveImage(ctx, image)
	if _, digest, _ := strings.Cut(image, "@"); err != nil || cfg.Digest != digest || cfg.User != "4242" {
		t.Errorf("%s resolved to %+v, %v; want its digest and user 4242", image, cfg, err)
	}
}

func testReadAndBuildAMirror(t *testing.T, h Harness) {
	ctx := t.Context()
	const mirror = "conformance/read"
	head, err := h.Provider.Fetch(ctx, provider.FetchRequest{Mirror: mirror, URL: h.Remote, Branch: h.Branch})
	if err != nil {
		t.Fatal(err)
	}
	source := provider.MirrorSource{Mirror: mirror}
	read, err := h.Provider.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: source, Paths: []string{"", "no/such/file"}})
	if err != nil {
		t.Fatal(err)
	}
	if root, ok := read.Entries[""]; read.Commit != head || !ok || !root.Dir || root.Object == "" {
		t.Errorf("read the root at %q as %+v", read.Commit, read.Entries)
	}
	if _, ok := read.Entries["no/such/file"]; ok {
		t.Error("a path not there was read")
	}
	if _, err := h.Provider.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: provider.MirrorSource{Mirror: "conformance/never-fetched"}, Paths: []string{""}}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("reading a mirror never fetched: %v, want ErrNotFound", err)
	}
	if _, err := h.Provider.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: provider.MirrorSource{Mirror: mirror, Ref: "no-such-ref"}, Paths: []string{""}}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("reading a ref the mirror lacks: %v, want ErrNotFound", err)
	}
	if _, err := h.Provider.BuildImage(ctx, provider.BuildRequest{Mirror: &provider.MirrorSource{Mirror: "conformance/never-fetched"}, Repository: "conformance/images"}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("building a mirror never fetched: %v, want ErrNotFound", err)
	}
	if _, err := h.Provider.BuildImage(ctx, provider.BuildRequest{Mirror: &source, Dockerfile: "no/such/Dockerfile", Repository: "conformance/images"}); !errors.Is(err, provider.ErrFailed) {
		t.Errorf("building a mirror with no such Dockerfile: %v, want ErrFailed", err)
	}
	if h.RemoteDockerfile == "" {
		return
	}
	read, err = h.Provider.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: source, Paths: []string{h.RemoteDockerfile}, Content: true})
	if e := read.Entries[h.RemoteDockerfile]; err != nil || e.Dir || !strings.HasPrefix(string(e.Content), "FROM ") || e.Size != int64(len(e.Content)) {
		t.Errorf("read %s as %+v, %v", h.RemoteDockerfile, e, err)
	}
	build := func() string {
		t.Helper()
		image, err := h.Provider.BuildImage(ctx, provider.BuildRequest{Mirror: &source, Dockerfile: h.RemoteDockerfile, Repository: "conformance/images"})
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	if one, again := build(), build(); !strings.Contains(one, "@sha256:") || one != again {
		t.Errorf("a mirror's work tree built %q, then %q", one, again)
	}
}

func testCaptureAndClone(t *testing.T, h Harness) {
	ctx := t.Context()
	src, _ := h.Start(t, "capture-src", h.container("net-a"))
	if err := h.Provider.WriteFile(ctx, src, "/workspace/warm.txt", 0o644, 0, strings.NewReader("installed")); err != nil {
		t.Fatal(err)
	}
	h.Stop(t, src)
	art, err := h.Provider.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if art.Name == "" || art.Size == 0 {
		t.Errorf("artifact %+v, want a name and a size", art)
	}
	spec := h.container("net-a")
	spec.DiskGolden = h.Golden(t, art)
	clone, _ := h.Start(t, "capture-clone", spec)
	var got bytes.Buffer
	if err := h.Provider.ReadFile(ctx, clone, "/workspace/warm.txt", &got); err != nil || got.String() != "installed" {
		t.Errorf("a clone of the artifact holds %q, %v", got.String(), err)
	}
	if err := h.Provider.DeleteArtifact(ctx, art.Name); err != nil {
		t.Fatal(err)
	}
	if err := h.Provider.DeleteArtifact(ctx, art.Name); err != nil {
		t.Errorf("deleting an artifact twice: %v", err)
	}
}

func testMirrorAndCheckout(t *testing.T, h Harness) {
	ctx := t.Context()
	head, err := h.Provider.Fetch(ctx, provider.FetchRequest{Mirror: "conformance/repo", URL: h.Remote, Branch: h.Branch})
	if err != nil {
		t.Fatal(err)
	}
	if head == "" || (h.RemoteHead != "" && head != h.RemoteHead) {
		t.Errorf("fetched to %q, want %q", head, h.RemoteHead)
	}
	if n, err := h.Provider.CommitsBehind(ctx, "conformance/repo", head, h.Branch); err != nil || n != 0 {
		t.Errorf("the head is %d commits behind its branch, %v", n, err)
	}
	in, _ := h.Start(t, "checkout", h.container("net-a"))
	commit, err := h.Provider.Checkout(ctx, in, "conformance/repo", "", "/workspace")
	if err != nil || commit != head {
		t.Errorf("checked out %q, %v; want %q", commit, err, head)
	}
	if _, err := h.Provider.Checkout(ctx, in, "conformance/never-fetched", "", "/repos/x"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("checking out a mirror never fetched: %v, want ErrNotFound", err)
	}
	if err := h.Provider.UpdateOrigin(ctx, in, "conformance/repo", "/workspace"); err != nil {
		t.Errorf("updating the checkout's origin: %v", err)
	}
	if err := h.Provider.UpdateOrigin(ctx, in, "conformance/never-fetched", "/workspace"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("updating an origin from a mirror never fetched: %v, want ErrNotFound", err)
	}
}

func testPushNeedsItsLease(t *testing.T, h Harness) {
	ctx := t.Context()
	head, err := h.Provider.Fetch(ctx, provider.FetchRequest{Mirror: "conformance/push", URL: h.Remote, Branch: h.Branch})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := h.Start(t, "push", h.container("net-a"))
	if _, err := h.Provider.Checkout(ctx, in, "conformance/push", "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	req := provider.PushRequest{Instance: in, Dir: "/workspace", Mirror: "conformance/push", URL: h.Remote, Branch: h.Branch, Lease: "0000000"}
	if err := h.Provider.Push(ctx, req); !errors.Is(err, provider.ErrLeaseLost) {
		t.Errorf("a push on a lease the branch is not at: %v, want ErrLeaseLost", err)
	}
	req.Lease = head
	if err := h.Provider.Push(ctx, req); err != nil {
		t.Errorf("a push on its lease: %v", err)
	}
}

func testTicket(t *testing.T, h Harness) {
	in, _ := h.Start(t, "ticket", h.container("net-a"))
	tk, err := h.Provider.Ticket(t.Context(), in, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if tk.URL == "" || (!tk.Expires.IsZero() && !tk.Expires.After(time.Now())) {
		t.Errorf("ticket %+v, want a URL, and no expiry or one to come", tk)
	}
}
