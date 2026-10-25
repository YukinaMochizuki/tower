//go:build testbed

// Package e2e runs tower end to end on the testbed with hostd's real
// drivers: the tower-apiserver, hostd and tower binaries built here, hostd
// on ZFS, Docker, Cloud Hypervisor, BuildKit, the testbed's registry and
// git (docs/runbook/testbed.md). TestTheDemo is docs/runbook/demo.md's
// scenario on a real host: a box built from its own Dockerfile, its
// repository checked out, used, streamed to, kept by a process,
// idle-stopped and woken, its hostd restarted, its project deleted and
// the host left as it was. TestAK3sMachine converges workstream's k3s
// blueprint on a Cloud Hypervisor VM through tower, and reaches its API
// from a box of the same workstream by its alias.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

// slow bounds what waits on a real host: images built, VMs booted,
// blueprints converged.
const slow = 15 * time.Minute

var env testbed.Env

func TestMain(m *testing.M) {
	var err error
	env, err = testbed.New("e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Sweep(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// binaries builds tower-apiserver, hostd (with its real drivers) and the
// CLI into dir.
func binaries(t *testing.T, dir string) {
	t.Helper()
	for _, b := range []string{"tower-apiserver", "hostd", "tower"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b), "./cmd/"+b)
		cmd.Dir = testbed.Repo()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", b, err, out)
		}
	}
}

// proc is a binary running until the test ends, its stderr kept.
type proc struct {
	t    *testing.T
	name string
	cmd  *exec.Cmd
	mu   sync.Mutex
	log  bytes.Buffer
	done chan struct{}
}

func start(t *testing.T, name string, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, name: filepath.Base(name), cmd: exec.Command(name, args...), done: make(chan struct{})}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(p.done)
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 64<<10), 1<<20)
		for s.Scan() {
			p.mu.Lock()
			p.log.WriteString(s.Text() + "\n")
			p.mu.Unlock()
		}
		_ = p.cmd.Wait()
	}()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("%s's log:\n%s", p.name, p.logged())
		}
	})
	return p
}

func (p *proc) logged() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.String()
}

// find waits until the log has a line matching re, and returns its first
// group.
func (p *proc) find(re string) string {
	p.t.Helper()
	r := regexp.MustCompile(re)
	var got string
	testutil.EventuallyWithin(p.t, time.Minute, p.name+" logging "+re, func() (bool, string) {
		m := r.FindStringSubmatch(p.logged())
		if m == nil {
			return false, ""
		}
		got = m[1]
		return true, ""
	})
	return got
}

func (p *proc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// cli runs the tower CLI.
type cli struct {
	t   *testing.T
	bin string
	env []string
}

func (c cli) run(stdin string, args ...string) (string, error) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = append(os.Environ(), c.env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("tower %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out), nil
}

func (c cli) must(stdin string, args ...string) string {
	c.t.Helper()
	out, err := c.run(stdin, args...)
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// world is tower and hostd on the testbed, and the CLI as an admin and as
// workstream.
type world struct {
	t                    *testing.T
	dir, bin, url, state string
	admin                cli
	envs                 environmentsv1alpha1connect.EnvironmentServiceClient
	runHostd             func() *proc
	host                 *proc
}

// newWorld starts tower-apiserver, makes the tokens, and readies hostd
// with its real drivers over the package's dataset, which runHostd starts
// once its Target is there.
func newWorld(t *testing.T) *world {
	w := &world{t: t, bin: t.TempDir()}
	binaries(t, w.bin)
	w.dir = filepath.Join(env.Dir, "world-"+strings.ToLower(t.Name()))
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(w.dir, "secrets-kek")
	if err := os.WriteFile(kek, []byte(secrets.NewKEK("e2e").String()), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(w.dir, "admin.sock")
	tower := start(t, filepath.Join(w.bin, "tower-apiserver"), "--dev", "--listen", "127.0.0.1:0",
		"--db", filepath.Join(w.dir, "tower.db"), "--admin-socket", sock, "--secrets-kek", kek)
	w.url = "http://" + tower.find(`serving the public API" address=(\S+)`)
	tower.find(`(serving the admin socket)`)
	root := cli{t: t, bin: filepath.Join(w.bin, "tower"), env: []string{"TOWER_SERVER=unix://" + sock}}
	root.must("", "token", "create", "admin", "--role", "admin", "--secret-file", filepath.Join(w.dir, "admin.token"))
	w.admin = cli{t: t, bin: root.bin, env: []string{"TOWER_SERVER=" + w.url, "TOWER_TOKEN_FILE=" + filepath.Join(w.dir, "admin.token")}}
	w.admin.must("", "token", "create", "hostd-main", "--role", "hostd", "--target", "main", "--secret-file", filepath.Join(w.dir, "hostd.token"))
	workstream := strings.TrimSpace(w.admin.must("", "token", "create", "workstream", "--role", "workstream"))
	w.envs = environmentsv1alpha1connect.NewEnvironmentServiceClient(&http.Client{Transport: bearer(workstream)}, w.url)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		// What tower held when the test failed.
		for _, kind := range []string{"box", "machine", "golden", "instance", "target"} {
			args := []string{"get", kind, "-o", "yaml"}
			if kind != "target" {
				args = append(args, "-p", "web")
			}
			out, err := w.admin.run("", args...)
			t.Logf("%s:\n%s%v", kind, out, err)
		}
	})

	reg, err := testbed.ReadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(w.dir, "registry.password")
	if err := os.WriteFile(pw, []byte(reg.Password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.state = env.StateDir()
	w.runHostd = func() *proc {
		h := start(t, filepath.Join(w.bin, "hostd"), "--tower", w.url, "--target", "main",
			"--token-file", filepath.Join(w.dir, "hostd.token"), "--ticket-public-key", filepath.Join(w.dir, "ticket-key.pub"),
			"--state-dir", w.state, "--direct-listen", "127.0.0.1:0",
			"--report-every", "2s", "--sweep-every", "2s", "--sweep-grace", "30s",
			"--zfs-dataset", env.Dataset, "--guest-binary", env.GuestBinary(),
			"--cloud-hypervisor", testbed.CloudHypervisor, "--vm-firmware", testbed.Firmware, "--base-images", testbed.BaseImages,
			"--registry", reg.Addr, "--registry-user", reg.User, "--registry-password-file", pw)
		h.find(`(serving direct streams)`)
		return h
	}
	return w
}

// remote is the repository the boxes check out: a repository's URL must
// be an https one, so a public one, read anonymously.
const remote, remoteBranch = "https://github.com/octocat/Hello-World.git", "master"

// manifests are what the scenario applies: the target, its placement, a
// repository, and a box of it built from its own Dockerfile, whose image
// carries test/testbed/guestbin, an install-dependencies that does
// nothing (the fake resolver's warm step), and an echo on 8080. The box
// is written as JSON, which is YAML too.
func manifests(t *testing.T) string {
	t.Helper()
	return `apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec:
  hostd: {}
  # The testbed's pool is 40 GB: a warm budget of tower's default would put
  # the target under disk pressure, which makes nothing new.
  settings: {idleStopAfter: 2s, warmBudget: 1Gi}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Placement
metadata: {name: web}
spec: {target: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: api}
spec: {url: "` + remote + `", defaultBranch: ` + remoteBranch + `}
---
` + boxManifest(t, "b1") + "\n"
}

// boxManifest is a box of workstream ws1, of the repository api, built
// from its own Dockerfile.
func boxManifest(t *testing.T, name string) string {
	t.Helper()
	files := map[string][]byte{"bin/install-dependencies": []byte("#!/bin/sh\nexit 0\n")}
	dir := filepath.Join(testbed.Repo(), "test/testbed/guestbin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files["bin/"+e.Name()] = b
	}
	box := &envv1alpha1.Box{
		ApiVersion: "environments.tower.hangarlab.cloud/v1alpha1", Kind: "Box",
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &envv1alpha1.BoxSpec{
			Workstream: "ws1", Checkouts: []*envv1alpha1.Checkout{{Repository: "api"}},
			ImageBuild: &envv1alpha1.ImageBuild{
				Dockerfile: "FROM " + testbed.BusyBox + "\nCOPY --chmod=0755 bin/ /usr/local/bin/\n" +
					"CMD [\"nc\", \"-lk\", \"-p\", \"8080\", \"-e\", \"/bin/cat\"]\n",
				Files: files,
			},
		},
	}
	boxJSON, err := protojson.Marshal(box)
	if err != nil {
		t.Fatal(err)
	}
	return string(boxJSON)
}

// execIn runs a command in an environment through EnvironmentService, as
// workstream does, and returns its stdout and exit code.
func execIn(ctx context.Context, envs environmentsv1alpha1connect.EnvironmentServiceClient, e *envv1alpha1.Environment, args ...string) (string, int32, error) {
	stream, err := envs.Exec(ctx, connect.NewRequest(&envv1alpha1.ExecRequest{
		Environment: e, Command: &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: args}},
	}))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = stream.Close() }()
	var out bytes.Buffer
	for stream.Receive() {
		switch r := stream.Msg().GetResponse().(type) {
		case *envv1alpha1.ExecResponse_Stdout:
			out.Write(r.Stdout)
		case *envv1alpha1.ExecResponse_Stderr:
			out.Write(r.Stderr)
		case *envv1alpha1.ExecResponse_Exit:
			return out.String(), r.Exit.GetCode(), nil
		}
	}
	if err := stream.Err(); err != nil {
		return out.String(), 0, err
	}
	return out.String(), 0, errors.New("the exec ended with no exit")
}

func boxEnv(name string) *envv1alpha1.Environment {
	return &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Box{Box: name}}
}

func machineEnv(name string) *envv1alpha1.Environment {
	return &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Machine{Machine: name}}
}

func (w *world) box(name string) *envv1alpha1.Box {
	w.t.Helper()
	var b envv1alpha1.Box
	if err := protojson.Unmarshal([]byte(w.admin.must("", "get", "box", name, "-p", "web", "-o", "json")), &b); err != nil {
		w.t.Fatal(err)
	}
	return &b
}

// hostEmpty reports whether the host holds nothing of the target's: no
// container or VM of hostd's, no disk or golden in its dataset.
func hostEmpty() (bool, string) {
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=tower.hangarlab.cloud/hostd=main").Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return false, "guests: " + s
	}
	out, _ = exec.Command("zfs", "list", "-H", "-o", "name", "-r", env.Dataset+"/disks", env.Dataset+"/goldens").Output()
	var left []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != env.Dataset+"/disks" && l != env.Dataset+"/goldens" && l != "" {
			left = append(left, l)
		}
	}
	return len(left) == 0, strings.Join(left, " ")
}

// TestTheDemo is docs/runbook/demo.md's scenario on a real host.
func TestTheDemo(t *testing.T) {
	w := newWorld(t)
	w.admin.must(manifests(t), "apply", "-f", "-", "-p", "web")
	w.host = w.runHostd()
	w.admin.must("", "wait", "box", "b1", "-p", "web", "--for", "condition=Ready", "--timeout", "10m")
	ctx := t.Context()

	// Its repository is checked out, its own image runs, and a command
	// through tower prints its output.
	if out, code, err := execIn(ctx, w.envs, boxEnv("b1"), "cat", "/workspace/README"); err != nil || code != 0 || !strings.Contains(out, "Hello World") {
		t.Fatalf("the checkout's README: %q, exit %d, %v", out, code, err)
	}
	if out, code, err := execIn(ctx, w.envs, boxEnv("b1"), "emit", "out:hello from b1"); err != nil || code != 0 || out != "hello from b1\n" {
		t.Fatalf("exec: %q, exit %d, %v", out, code, err)
	}

	// A direct stream on a ticket reaches the box's port.
	tk, err := w.envs.IssueTicket(ctx, connect.NewRequest(&envv1alpha1.IssueTicketRequest{Environment: boxEnv("b1"), Port: 8080}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := hostd.DialStream(ctx, tk.Msg.GetUrl(), tk.Msg.GetTicket())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping\n" {
		t.Errorf("the stream answered %q, %v", echo, err)
	}
	_ = conn.Close()

	// A process outlives its call, and keeps the box from idling; once it
	// is signalled the box idles and is stopped, and a command wakes it.
	started, err := w.envs.StartProcess(ctx, connect.NewRequest(&envv1alpha1.StartProcessRequest{
		Environment: boxEnv("b1"), Name: "server",
		Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"serve", "listening", "on", "8081"}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := started.Msg.GetProcess()
	testutil.Unchanged(t, "the box's phase while its process runs", 70*time.Second, func() (string, error) {
		return w.box("b1").GetStatus().GetPhase().String(), nil
	})
	if _, err := w.envs.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{
		Environment: boxEnv("b1"), Process: id, Signal: envv1alpha1.Signal_SIGNAL_TERM,
	})); err != nil {
		t.Fatal(err)
	}
	uid := w.box("b1").GetMetadata().GetUid()
	testutil.EventuallyWithin(t, slow, "the box idle-stopped", func() (bool, string) {
		b := w.box("b1")
		return b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_STOPPED, b.GetStatus().String()
	})
	if out, code, err := execIn(ctx, w.envs, boxEnv("b1"), "echo", "awake"); err != nil || code != 0 || out != "awake\n" {
		t.Fatalf("exec in an idle-stopped box: %q, exit %d, %v", out, code, err)
	}
	if b := w.box("b1"); b.GetMetadata().GetUid() != uid {
		t.Errorf("the box woken is another: %v", b)
	}
	list, err := w.envs.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("b1")}))
	if err != nil || len(list.Msg.GetProcesses()) != 1 || list.Msg.GetProcesses()[0].GetRunning() {
		t.Errorf("the box's processes after its stop: %v, %v", list.Msg.GetProcesses(), err)
	}

	// A hostd started again finds the host as it was.
	w.host.stop()
	w.host = w.runHostd()
	testutil.EventuallyWithin(t, slow, "exec once hostd is back", func() (bool, string) {
		out, _, err := execIn(ctx, w.envs, boxEnv("b1"), "echo", "back")
		return err == nil && out == "back\n", fmt.Sprint(out, err)
	})

	// Deleting the project deletes everything of it on the host.
	w.admin.must("", "delete", "project", "web")
	testutil.EventuallyWithin(t, slow, "the host empty", hostEmpty)
}

// TestAK3sMachine: workstream's k3s blueprint (k3s.yml, as workstream has
// it, untagged) converges on a Cloud Hypervisor VM through tower: its
// golden phase in a builder VM booted from ubuntu-24.04, captured, and a
// machine cloned from it, configured; a box of the same workstream
// reaches its API by its alias; a process runs in the machine.
func TestAK3sMachine(t *testing.T) {
	w := newWorld(t)
	playbook, err := os.ReadFile("k3s.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := &envv1alpha1.Machine{
		ApiVersion: "environments.tower.hangarlab.cloud/v1alpha1", Kind: "Machine",
		Metadata: &metav1.ObjectMeta{Name: "k3s"},
		Spec: &envv1alpha1.MachineSpec{
			Workstream: "ws1",
			Blueprint: &envv1alpha1.Blueprint{
				Name: "k3s", Content: string(playbook), BaseImage: "ubuntu-24.04", ExposePorts: []int32{6443}, NetAlias: "k3s",
			},
			Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "20Gi"},
		},
	}
	mJSON, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	w.admin.must(manifests(t)+"---\n"+string(mJSON)+"\n", "apply", "-f", "-", "-p", "web")
	w.host = w.runHostd()
	w.admin.must("", "wait", "machine", "k3s", "-p", "web", "--for", "condition=Ready", "--timeout", "30m")
	// The box has idled while the machine converged: a command wakes it.
	ctx := t.Context()

	// The machine runs k3s, its node named by the one hostname. A machine
	// is ready once configured; k3s, started again in the clone, answers a
	// moment later (workstream's own driver waits on a blueprint's
	// readiness check, which tower's Blueprint does not carry).
	testutil.EventuallyWithin(t, 5*time.Minute, "k3s answering in the machine", func() (bool, string) {
		out, code, err := execIn(ctx, w.envs, machineEnv("k3s"), "k3s", "kubectl", "get", "nodes", "-o", "name")
		return err == nil && code == 0 && strings.TrimSpace(out) == "node/machine", fmt.Sprint(out, code, err)
	})
	// A box of its workstream reaches its API by its alias.
	testutil.EventuallyWithin(t, 5*time.Minute, "the box reaching k3s:6443", func() (bool, string) {
		out, code, err := execIn(ctx, w.envs, boxEnv("b1"), "nc", "-z", "-w", "5", "k3s", "6443")
		return err == nil && code == 0, fmt.Sprint(out, code, err)
	})
	// And the machine reaches the box by its.
	if out, code, err := execIn(ctx, w.envs, machineEnv("k3s"), "sh", "-c", "getent hosts b1 && echo ping | nc -q 2 b1 8080"); err != nil || code != 0 || !strings.Contains(out, "ping") {
		t.Errorf("the machine reaching the box b1:8080: %q, exit %d, %v", out, code, err)
	}
	// A process runs in the machine, apart from the call.
	started, err := w.envs.StartProcess(ctx, connect.NewRequest(&envv1alpha1.StartProcessRequest{
		Environment: machineEnv("k3s"), Name: "readyz",
		// Until its API is ready, which it says once.
		Command: &envv1alpha1.StartProcessRequest_Shell{Shell: "until k3s kubectl get --raw /readyz >/dev/null 2>&1; do sleep 2; done; k3s kubectl get --raw /readyz"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	attachCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stream, err := w.envs.AttachProcess(attachCtx, connect.NewRequest(&envv1alpha1.AttachProcessRequest{Environment: machineEnv("k3s"), Process: started.Msg.GetProcess(), Follow: true}))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var end *envv1alpha1.ProcessEnd
	for stream.Receive() {
		if o := stream.Msg().GetOutput(); o != nil {
			out.Write(o.GetData())
		}
		if e := stream.Msg().GetEnd(); e != nil {
			end = e
		}
	}
	_ = stream.Close()
	if strings.TrimSpace(out.String()) != "ok" || end.GetExit().GetCode() != 0 {
		t.Errorf("the process in the machine wrote %q, ended %v, %v", out.String(), end, stream.Err())
	}

	w.admin.must("", "delete", "project", "web")
	testutil.EventuallyWithin(t, slow, "the host empty", hostEmpty)
}
