//go:build e2e

// Package e2e runs tower end to end, hermetically: the tower-apiserver,
// hostd and tower binaries, built here, with hostd on a fake host (the
// hostdfake build tag). It is what make e2e runs; docs/runbook/demo.md
// is the same scenario by hand.
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
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/fake"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// slow bounds what waits on the apiserver's own timers, which run on its
// real clock: idleness is checked once a minute.
const slow = 3 * time.Minute

// manifests are what the scenario applies, as an administrator and
// workstream would.
const manifests = `apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec:
  hostd: {}
  settings: {idleStopAfter: 2s}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Placement
metadata: {name: web}
spec: {target: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: api}
spec: {url: "https://git.example.com/api.git", defaultBranch: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Box
metadata: {name: b1}
spec:
  workstream: ws1
  checkouts: [{repository: api}]
`

// binaries builds the three binaries into dir; hostd on the fake host.
func binaries(t *testing.T, dir string) {
	t.Helper()
	for _, b := range []struct{ name, tags string }{{"tower-apiserver", ""}, {"hostd", "hostdfake"}, {"tower", ""}} {
		args := []string{"build", "-o", filepath.Join(dir, b.name)}
		if b.tags != "" {
			args = append(args, "-tags", b.tags)
		}
		cmd := exec.Command("go", append(args, "./cmd/"+b.name)...)
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", b.name, err, out)
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
	testutil.Eventually(p.t, p.name+" logging "+re, func() (bool, string) {
		m := r.FindStringSubmatch(p.logged())
		if m == nil {
			return false, p.logged()
		}
		got = m[1]
		return true, ""
	})
	return got
}

// stop stops the process as an operator would, and waits for it.
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

// box reads the box through the CLI.
func (c cli) box() *envv1alpha1.Box {
	c.t.Helper()
	var b envv1alpha1.Box
	if err := protojson.Unmarshal([]byte(c.must("", "get", "box", "b1", "-p", "web", "-o", "json")), &b); err != nil {
		c.t.Fatal(err)
	}
	return &b
}

func (c cli) target() *targetsv1alpha1.Target {
	c.t.Helper()
	var tg targetsv1alpha1.Target
	if err := protojson.Unmarshal([]byte(c.must("", "get", "target", "main", "-o", "json")), &tg); err != nil {
		c.t.Fatal(err)
	}
	return &tg
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// exec runs a command in the box through EnvironmentService, as
// workstream does, and returns its stdout and exit code.
func execIn(ctx context.Context, env environmentsv1alpha1connect.EnvironmentServiceClient, args ...string) (string, int32, error) {
	stream, err := env.Exec(ctx, connect.NewRequest(&envv1alpha1.ExecRequest{
		Environment: &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Box{Box: "b1"}},
		Command:     &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: args}},
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
		case *envv1alpha1.ExecResponse_Exit:
			return out.String(), r.Exit.GetCode(), nil
		}
	}
	if err := stream.Err(); err != nil {
		return out.String(), 0, err
	}
	return out.String(), 0, errors.New("the exec ended with no exit")
}

// processSteps runs a process in box b1 as workstream does.
func processSteps(t *testing.T, env environmentsv1alpha1connect.EnvironmentServiceClient, url, adminToken string) {
	t.Helper()
	ctx := t.Context()
	box := &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Box{Box: "b1"}}
	started, err := env.StartProcess(ctx, connect.NewRequest(&envv1alpha1.StartProcessRequest{
		Environment: box, Name: "server",
		Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"serve", "listening", "on", "8080"}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := started.Msg.GetProcess()

	// A follow reads what it wrote while it runs; the call then ends, so
	// what keeps the box in use below is the process alone.
	attachCtx, endAttach := context.WithTimeout(ctx, 30*time.Second)
	stream, err := env.AttachProcess(attachCtx, connect.NewRequest(&envv1alpha1.AttachProcessRequest{Environment: box, Process: id, Follow: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("attaching to the process: %v", stream.Err())
	}
	if got := string(stream.Msg().GetOutput().GetData()); got != "listening on 8080\n" {
		t.Errorf("the process's output: %q", got)
	}
	endAttach()
	_ = stream.Close()

	secret, err := os.ReadFile(adminToken)
	if err != nil {
		t.Fatal(err)
	}
	api := client.New(&http.Client{Transport: bearer(strings.TrimSpace(string(secret)))}, url)
	read := func() (*envv1alpha1.Box, error) {
		o, err := api.Get(ctx, registry.Box, client.Key{Project: "web", Name: "b1"})
		if err != nil {
			return nil, err
		}
		return o.(*envv1alpha1.Box), nil
	}
	// Idleness is checked once a minute, and the box is idle after 2s: a
	// window past one check shows the process keeps it running.
	testutil.Unchanged(t, "the box's phase while its process runs", 70*time.Second, func() (string, error) {
		b, err := read()
		return b.GetStatus().GetPhase().String(), err
	})
	if b, _ := read(); b.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("the box with a process running: %v", b.GetStatus())
	}

	if _, err := env.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{
		Environment: box, Process: id, Signal: envv1alpha1.Signal_SIGNAL_TERM,
	})); err != nil {
		t.Fatal(err)
	}
	list, err := env.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: box}))
	if err != nil {
		t.Fatal(err)
	}
	ps := list.Msg.GetProcesses()
	if len(ps) != 1 || ps[0].GetProcess() != id || ps[0].GetRunning() || ps[0].GetName() != "server" ||
		ps[0].GetExit().GetSignal() != envv1alpha1.Signal_SIGNAL_TERM || ps[0].GetExit().GetCode() != 143 {
		t.Errorf("the processes once it was signalled: %v", ps)
	}
}

// TestTheDemo is the demo of docs/runbook/demo.md: a box made, used,
// streamed to, kept running by a process, idle-stopped and woken; its host
// gone and back; its project deleted, and the host empty after.
func TestTheDemo(t *testing.T) {
	bin := t.TempDir()
	binaries(t, bin)
	// Unix socket paths are short; t.TempDir can be too long for one.
	dir, err := os.MkdirTemp("", "tower-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kek := filepath.Join(dir, "secrets-kek")
	if err := os.WriteFile(kek, []byte(secrets.NewKEK("e2e").String()), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "admin.sock")
	tower := start(t, filepath.Join(bin, "tower-apiserver"), "--dev", "--listen", "127.0.0.1:0",
		"--db", filepath.Join(dir, "tower.db"), "--admin-socket", sock, "--secrets-kek", kek)
	url := "http://" + tower.find(`serving the public API" address=(\S+)`)
	tower.find(`(serving the admin socket)`)

	// The first admin token, as root on tower's host.
	root := cli{t: t, bin: filepath.Join(bin, "tower"), env: []string{"TOWER_SERVER=unix://" + sock}}
	root.must("", "token", "create", "admin", "--role", "admin", "--secret-file", filepath.Join(dir, "admin.token"))
	admin := cli{t: t, bin: root.bin, env: []string{"TOWER_SERVER=" + url, "TOWER_TOKEN_FILE=" + filepath.Join(dir, "admin.token")}}
	admin.must("", "token", "create", "hostd-main", "--role", "hostd", "--target", "main", "--secret-file", filepath.Join(dir, "hostd.token"))
	workstreamSecret := strings.TrimSpace(admin.must("", "token", "create", "workstream", "--role", "workstream"))
	admin.must(manifests, "apply", "-f", "-", "-p", "web")

	state := filepath.Join(dir, "host")
	runHostd := func() *proc {
		h := start(t, filepath.Join(bin, "hostd"), "--tower", url, "--target", "main",
			"--token-file", filepath.Join(dir, "hostd.token"), "--ticket-public-key", filepath.Join(dir, "ticket-key.pub"),
			"--state-dir", state, "--direct-listen", "127.0.0.1:0",
			"--report-every", "500ms", "--sweep-every", "500ms", "--sweep-grace", "1s")
		h.find(`(serving direct streams)`)
		return h
	}
	host := runHostd()

	// The box goes READY on the host.
	admin.must("", "wait", "box", "b1", "-p", "web", "--for", "condition=Ready", "--timeout", "2m")
	reachable := func(want bool) {
		t.Helper()
		testutil.EventuallyWithin(t, slow, fmt.Sprintf("target main Reachable=%v", want), func() (bool, string) {
			st := admin.target().GetStatus()
			return conditions.IsTrue(st.GetConditions(), "Reachable") == want, st.String()
		})
	}
	reachable(true)
	if st := admin.target().GetStatus(); !conditions.IsTrue(st.GetConditions(), hostd.TargetReporting) || st.GetCapacity().GetDiskTotal() == "" {
		t.Errorf("the target as hostd reports it: %v", st)
	}

	// A command through tower prints its output.
	env := environmentsv1alpha1connect.NewEnvironmentServiceClient(&http.Client{Transport: bearer(workstreamSecret)}, url)
	ctx := t.Context()
	if out, code, err := execIn(ctx, env, "echo", "hello", "from", "b1"); err != nil || code != 0 || out != "hello from b1\n" {
		t.Fatalf("exec: %q, exit %d, %v", out, code, err)
	}

	// A direct stream on a ticket reaches the instance's port.
	tk, err := env.IssueTicket(ctx, connect.NewRequest(&envv1alpha1.IssueTicketRequest{
		Environment: &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Box{Box: "b1"}}, Port: 8080,
	}))
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
	if _, err := hostd.DialStream(ctx, strings.Replace(tk.Msg.GetUrl(), "/8080", "/22", 1), tk.Msg.GetTicket()); err == nil {
		t.Error("a ticket for port 8080 opened port 22")
	}

	// A process in the box: its output followed while it runs, the box
	// kept from idling by it alone, then signalled, its exit listed.
	processSteps(t, env, url, filepath.Join(dir, "admin.token"))

	// Unused past its target's idle_stop_after, the box is stopped; a
	// command starts it again, and keeps its id.
	uid := admin.box().GetMetadata().GetUid()
	testutil.EventuallyWithin(t, slow, "the box idle-stopped", func() (bool, string) {
		b := admin.box()
		return b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_STOPPED, b.GetStatus().String()
	})
	if out, code, err := execIn(ctx, env, "echo", "awake"); err != nil || code != 0 || out != "awake\n" {
		t.Fatalf("exec in an idle-stopped box: %q, exit %d, %v", out, code, err)
	}
	if b := admin.box(); b.GetMetadata().GetUid() != uid || b.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("the box woken: %v", b)
	}

	// With hostd gone, the target is unreachable and a command
	// Unavailable; a hostd started again finds the host as it was.
	host.stop()
	reachable(false)
	if _, _, err := execIn(ctx, env, "true"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("exec with hostd stopped: %v, want Unavailable", err)
	}
	runHostd()
	reachable(true)
	testutil.EventuallyWithin(t, slow, "exec once hostd is back", func() (bool, string) {
		out, _, err := execIn(ctx, env, "echo", "back")
		return err == nil && out == "back\n", fmt.Sprint(out, err)
	})

	// Deleting the project deletes everything on the host.
	if s, err := fake.ReadState(filepath.Join(state, "fake-host.json")); err != nil || s.Empty() {
		t.Fatalf("the host before the delete: %+v, %v", s, err)
	}
	admin.must("", "delete", "project", "web")
	testutil.EventuallyWithin(t, slow, "the fake host empty", func() (bool, string) {
		s, err := fake.ReadState(filepath.Join(state, "fake-host.json"))
		return err == nil && s.Empty(), fmt.Sprintf("%+v %v", s, err)
	})
}
