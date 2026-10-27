//go:build hangar

// Package hangar runs the provider contract (internal/provider/
// providertest) against tower's hangar provider over a real hangar: one
// project of an endpoint the test is pointed at, which a hangar testbed or
// a host of its own serves (docs/runbook/testbed.md). The fake runs the
// same contract in CI. It runs from anywhere that reaches the endpoint,
// as any user: `make e2e-hangar`, with
//
//   - TOWER_HANGAR_ENDPOINT, the API's URL, and TOWER_HANGAR_PROJECT;
//   - TOWER_HANGAR_TOKEN_FILE, a file holding the project's service token;
//   - TOWER_HANGAR_IMAGE, a box image by digest that carries
//     test/testbed/guestbin in /usr/local/bin, and busybox;
//   - TOWER_HANGAR_PULL_SECRET_FILE, if the image's registry needs one, a
//     Docker config hangar pulls it with;
//   - for the VMs, which are skipped without it: TOWER_HANGAR_BASE_IMAGE,
//     one of the hangar's catalogue Images, which tower's base image
//     ubuntu-24.04 maps to; its VMs must reach its package repositories,
//     as a machine's builder installs ansible-core from them;
//   - for the mirrors and builds, which are skipped without them:
//     TOWER_HANGAR_SYSTEM_IMAGE, the system image hangar pulls;
//     TOWER_HANGAR_REGISTRY, tower's registry as the system container pushes
//     to it, with TOWER_HANGAR_REGISTRY_PLAIN_HTTP=1 for one served over
//     plain HTTP and TOWER_HANGAR_PUSH_LOGIN_FILE holding its user:password;
//     TOWER_HANGAR_REGISTRY_HOST, the name hangar pulls it by, if another;
//     TOWER_HANGAR_REMOTE, a repository the system container fetches and
//     pushes to, whose main branch has a Dockerfile at docker/Dockerfile
//     building from TOWER_HANGAR_IMAGE.
//
// What it makes it deletes as it ends; a network outlives it until the
// next run's sweep.
package hangar

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/streams"
	"github.com/YukinaMochizuki/tower/internal/system"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// config is the run's, from the environment.
type config struct {
	endpoint, project, token, image string
	pull                            []byte
	system, registry, registryHost  string
	plainHTTP                       bool
	push                            system.Login
	remote                          string
	baseImage                       string
}

func load(t *testing.T) config {
	t.Helper()
	c := config{endpoint: os.Getenv("TOWER_HANGAR_ENDPOINT"), project: os.Getenv("TOWER_HANGAR_PROJECT"), image: os.Getenv("TOWER_HANGAR_IMAGE")}
	if c.endpoint == "" || c.project == "" || c.image == "" || os.Getenv("TOWER_HANGAR_TOKEN_FILE") == "" {
		t.Fatal("TOWER_HANGAR_ENDPOINT, TOWER_HANGAR_PROJECT, TOWER_HANGAR_TOKEN_FILE and TOWER_HANGAR_IMAGE are required (see the package's comment)")
	}
	token, err := os.ReadFile(os.Getenv("TOWER_HANGAR_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	c.token = strings.TrimSpace(string(token))
	if f := os.Getenv("TOWER_HANGAR_PULL_SECRET_FILE"); f != "" {
		if c.pull, err = os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	c.system, c.registry, c.registryHost = os.Getenv("TOWER_HANGAR_SYSTEM_IMAGE"), os.Getenv("TOWER_HANGAR_REGISTRY"), os.Getenv("TOWER_HANGAR_REGISTRY_HOST")
	c.plainHTTP, c.remote = os.Getenv("TOWER_HANGAR_REGISTRY_PLAIN_HTTP") == "1", os.Getenv("TOWER_HANGAR_REMOTE")
	c.baseImage = os.Getenv("TOWER_HANGAR_BASE_IMAGE")
	if f := os.Getenv("TOWER_HANGAR_PUSH_LOGIN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		user, pass, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
		c.push = system.Login{Username: user, Password: pass}
	}
	return c
}

// signer signs the tickets of the world's streams.
var signer = func() *ticket.Signer {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return ticket.NewSigner(key)
}()

func world(t *testing.T) *hangartest.World {
	t.Helper()
	c := load(t)
	path, err := hangartest.BuildGuest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := hangartest.Options{
		API: hangar.NewConnect(c.endpoint, c.project, c.token, nil), Project: c.project, Guest: guest, PullSecret: c.pull, Image: c.image,
		Configure: func(cfg *hangar.Config) {
			cfg.SweepGrace = time.Minute
			cfg.TicketKey = signer.PublicKey()
			cfg.SystemImage, cfg.Registry, cfg.RegistryHost, cfg.RegistryPlainHTTP, cfg.PushLogin = c.system, c.registry, c.registryHost, c.plainHTTP, c.push
		},
		// A pull and a boot on a real hangar.
		Wait: 5 * time.Minute,
	}
	if c.baseImage != "" {
		opts.BaseImage, opts.BaseImages = baseImage, map[string]string{baseImage: c.baseImage}
	}
	if c.system != "" && c.remote != "" {
		opts.Remote, opts.Branch, opts.RemoteDockerfile = c.remote, "main", "docker/Dockerfile"
	}
	return hangartest.New(t, opts)
}

// baseImage is tower's name for the base image the run's VMs boot.
const baseImage = "ubuntu-24.04"

// TestConformance holds the hangar provider, over a real hangar, to the
// provider contract.
func TestConformance(t *testing.T) {
	w := world(t)
	providertest.Run(t, w.Harness())
}

// TestAStreamReachesAnInstanceThroughHangar serves a page in an instance
// and reads it through the direct stream tower serves, on a ticket,
// relayed to hangar's port forward.
func TestAStreamReachesAnInstanceThroughHangar(t *testing.T) {
	c := load(t)
	w := world(t)
	in, _ := w.Harness().Start(t, "forward", &envv1alpha1.InstanceSpec{
		Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: c.image, Network: "net-a", Alias: "forward",
	})
	ctx := t.Context()
	if err := w.Target.WriteFile(ctx, in, "/tmp/www/index.html", 0o644, 0, strings.NewReader("hello from hangar\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Target.StartProcess(ctx, provider.ProcessRequest{Instance: in, Command: []string{"busybox", "httpd", "-f", "-p", "8080", "-h", "/tmp/www"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.Target.Streams())
	defer srv.Close()
	where, err := w.Target.Ticket(ctx, in, 8080)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(where.URL)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := signer.Issue(ticket.Claims{Target: hangartest.Target, Project: in.Project, Instance: in.Name, Port: 8080, Expires: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyWithin(t, time.Minute, "the page through the stream", func() (bool, string) {
		conn, err := streams.Dial(ctx, srv.URL+u.Path, tk)
		if err != nil {
			return false, err.Error()
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n"); err != nil {
			return false, err.Error()
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false, err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b) == "hello from hangar\n", fmt.Sprint(resp.Status, " ", string(b))
	})
}

// TestAMachineOnHangar converges a machine on the hangar target through
// tower, as workstream asks for one: its golden builder, a VM booted from
// the catalogue image, gets ansible-core and runs the blueprint's golden
// phase, is captured, and the machine is a VM cloned from it, configured.
// It runs as root, is called machine, and it and a box of its workstream
// reach each other by their aliases.
func TestAMachineOnHangar(t *testing.T) {
	c := load(t)
	if c.baseImage == "" {
		t.Skip("TOWER_HANGAR_BASE_IMAGE names no catalogue image")
	}
	w := world(t)
	ctx := t.Context()
	admin := w.Env.As(apiservertest.Admin)
	if _, err := admin.Apply(ctx, registry.Placement, &targetsv1alpha1.Placement{
		Metadata: &metav1.ObjectMeta{Name: apiservertest.Project}, Spec: &targetsv1alpha1.PlacementSpec{Target: hangartest.Target},
	}); err != nil {
		t.Fatal(err)
	}
	const playbook = `- hosts: all
  tasks:
    - name: the golden phase
      ansible.builtin.copy: {dest: /etc/tower-l3-golden, content: "converged\n"}
    - name: the configure phase
      ansible.builtin.copy: {dest: /etc/tower-l3-configured, content: "configured\n"}
      tags: [configure]
`
	if _, err := w.Env.AsSystem().Apply(ctx, registry.Machine, &envv1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "m1"},
		Spec: &envv1alpha1.MachineSpec{
			Workstream: "ws1",
			Blueprint:  &envv1alpha1.Blueprint{Name: "l3", Content: playbook, BaseImage: baseImage, NetAlias: "m1"},
			Resources:  &envv1alpha1.Resources{Cpu: "2", Memory: "2Gi", Disk: "10Gi"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Deleted however the test ends, before the world's cleanup, so its
	// controllers make no instance of it again.
	t.Cleanup(func() {
		_, _ = w.Env.AsSystem().Delete(context.Background(), registry.Machine, client.Key{Project: apiservertest.Project, Name: "m1"})
	})
	var m *envv1alpha1.Machine
	testutil.EventuallyWithin(t, 30*time.Minute, "the machine READY", func() (bool, string) {
		o, err := w.Env.AsSystem().Get(ctx, registry.Machine, client.Key{Project: apiservertest.Project, Name: "m1"})
		if err != nil {
			t.Fatal(err)
		}
		m = o.(*envv1alpha1.Machine)
		if m.GetStatus().GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED {
			t.Fatalf("the machine failed: %v", m.GetStatus())
		}
		return m.GetStatus().GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_READY, m.GetStatus().String()
	})
	in := provider.Instance{Project: apiservertest.Project, Name: m.GetStatus().GetInstance()}
	run := func(ref provider.Instance, command ...string) string {
		t.Helper()
		var out strings.Builder
		code, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: ref, Command: command, Stdout: &out, Stderr: &out})
		if err != nil || code != 0 {
			t.Errorf("%v in %s: exit %d, %v: %s", command, ref.Name, code, err, out.String())
		}
		return out.String()
	}
	for cmd, want := range map[string]string{
		"id -u": "0", "hostname": "machine", "cat /etc/tower-l3-golden": "converged", "cat /etc/tower-l3-configured": "configured",
	} {
		if got := strings.TrimSpace(run(in, "sh", "-c", cmd)); got != want {
			t.Errorf("%s in the machine: %q, want %q", cmd, got, want)
		}
	}
	run(in, "ansible-playbook", "--version")
	golden, err := w.Env.AsSystem().Get(ctx, registry.Golden, client.Key{Project: apiservertest.Project, Name: m.GetStatus().GetGolden()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the machine's golden %s is hangar CustomImage %s", m.GetStatus().GetGolden(), golden.(*envv1alpha1.Golden).GetStatus().GetArtifact())

	// A direct stream on a ticket reaches a port of the VM: its sshd's
	// banner.
	srv := httptest.NewServer(w.Target.Streams())
	defer srv.Close()
	where, err := w.Target.Ticket(ctx, in, 22)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(where.URL)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := signer.Issue(ticket.Claims{Target: hangartest.Target, Project: in.Project, Instance: in.Name, Port: 22, Expires: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := streams.Dial(ctx, srv.URL+u.Path, tk)
	if err != nil {
		t.Fatal(err)
	}
	banner, err := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if err != nil || !strings.HasPrefix(banner, "SSH-2.0-") {
		t.Errorf("the VM's port 22 through a stream: %q, %v", banner, err)
	}

	box, _ := w.Harness().Start(t, "box", &envv1alpha1.InstanceSpec{
		Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: c.image, Network: "ws1", Alias: "box",
	})
	testutil.EventuallyWithin(t, 2*time.Minute, "each reaching the other by its alias", func() (bool, string) {
		var a, b strings.Builder
		ca, erra := w.Target.Exec(ctx, provider.ExecRequest{Instance: box, Command: []string{"getent", "hosts", "m1"}, Stdout: &a, Stderr: &a})
		cb, errb := w.Target.Exec(ctx, provider.ExecRequest{Instance: in, Command: []string{"getent", "hosts", "box"}, Stdout: &b, Stderr: &b})
		return erra == nil && errb == nil && ca == 0 && cb == 0, fmt.Sprintf("box: %d %v %q; machine: %d %v %q", ca, erra, a.String(), cb, errb, b.String())
	})
}

// TestAGoldenWithoutTheSSHDDropInGetsIt: a VM cloned from a golden whose
// disk lacks the sshd drop-in tower's user_data writes, as one captured
// before tower wrote it, has it again, as its user_data runs at each boot:
// its sshd takes the sessions tower opens, many operations at once on one
// connection.
func TestAGoldenWithoutTheSSHDDropInGetsIt(t *testing.T) {
	c := load(t)
	if c.baseImage == "" {
		t.Skip("TOWER_HANGAR_BASE_IMAGE names no catalogue image")
	}
	w := world(t)
	h := w.Harness()
	ctx := t.Context()
	vm := func(alias string) *envv1alpha1.InstanceSpec {
		return &envv1alpha1.InstanceSpec{
			Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: baseImage, Network: "net-old", Alias: alias,
			Resources: &envv1alpha1.Resources{Cpu: "1", Memory: "1Gi", Disk: "2Gi"},
		}
	}
	maxSessions := func(in provider.Instance) string {
		t.Helper()
		var out strings.Builder
		code, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: in, Command: []string{"sh", "-c", "/usr/sbin/sshd -T 2>/dev/null | grep -i '^maxsessions'"}, Stdout: &out, Stderr: &out})
		if err != nil || code != 0 {
			t.Fatalf("sshd -T: exit %d, %v: %s", code, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	src, _ := h.Start(t, "old-builder", vm("old-builder"))
	if got := maxSessions(src); got != "maxsessions 64" {
		t.Errorf("a VM tower made: %q, want maxsessions 64", got)
	}
	var out strings.Builder
	if code, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: src, Command: []string{"rm", "-f", vmssh.SSHDDropIn}, Stdout: &out, Stderr: &out}); err != nil || code != 0 {
		t.Fatalf("removing the drop-in: %d, %v: %s", code, err, out.String())
	}
	h.Stop(t, src)
	art, err := w.Target.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	h.Delete(t, src)
	spec := vm("old-clone")
	spec.DiskGolden = h.Golden(t, art)
	clone, _ := h.Start(t, "old-clone", spec)
	defer h.Delete(t, clone)
	if got := maxSessions(clone); got != "maxsessions 64" {
		t.Errorf("a clone of a golden without the drop-in: %q, want maxsessions 64", got)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: clone, Command: []string{"sleep", "2"}})
			if err == nil && code != 0 {
				err = fmt.Errorf("exit %d", code)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("an operation of %d at once in the clone: %v", n, err)
		}
	}
	if err := w.Target.DeleteArtifact(ctx, art.Name); err != nil {
		t.Error(err)
	}
}
