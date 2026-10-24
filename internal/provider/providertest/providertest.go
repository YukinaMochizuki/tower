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
	"strings"
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
	// Image and BaseImage are what a container and a VM boot from.
	Image, BaseImage string
	// Remote is a repository the provider can fetch and push to, on
	// Branch; RemoteHead is the commit Branch is at.
	Remote, Branch, RemoteHead string
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
		{"CaptureAndClone", testCaptureAndClone},
		{"MirrorAndCheckout", testMirrorAndCheckout},
		{"PushNeedsItsLease", testPushNeedsItsLease},
		{"Ticket", testTicket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h) })
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
	vm := &envv1alpha1.InstanceSpec{Target: h.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: h.BaseImage, Network: "net-a", Alias: "vm"}
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
}

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
	if err := h.Provider.WriteFile(ctx, in, "/tmp/conformance/a.txt", 0o644, strings.NewReader("contents")); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := h.Provider.ReadFile(ctx, in, "/tmp/conformance/a.txt", &got); err != nil || got.String() != "contents" {
		t.Errorf("read back %q, %v", got.String(), err)
	}
	if code, out := run(t, h, in, nil, "cat", "/tmp/conformance/a.txt"); code != 0 || out != "contents" {
		t.Errorf("the file as a command reads it: %d %q", code, out)
	}
	if err := h.Provider.ReadFile(ctx, in, "/tmp/conformance/none", io.Discard); err == nil {
		t.Error("read a file that is not there")
	}
}

func testCopy(t *testing.T, h Harness) {
	a, _ := h.Start(t, "copy-a", h.container("net-a"))
	b, _ := h.Start(t, "copy-b", h.container("net-b"))
	ctx := t.Context()
	if err := h.Provider.WriteFile(ctx, a, "/tmp/out/result.txt", 0o644, strings.NewReader("built")); err != nil {
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

func testCaptureAndClone(t *testing.T, h Harness) {
	ctx := t.Context()
	src, _ := h.Start(t, "capture-src", h.container("net-a"))
	if err := h.Provider.WriteFile(ctx, src, "/workspace/warm.txt", 0o644, strings.NewReader("installed")); err != nil {
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
	if tk.URL == "" || !tk.Expires.After(time.Time{}) {
		t.Errorf("ticket %+v, want a URL and an expiry", tk)
	}
}
