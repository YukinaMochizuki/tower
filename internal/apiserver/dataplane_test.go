package apiserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

func boxEnv(name string) *envv1alpha1.Environment {
	return &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Box{Box: name}}
}

func machineEnv(name string) *envv1alpha1.Environment {
	return &envv1alpha1.Environment{Project: "web", Kind: &envv1alpha1.Environment_Machine{Machine: name}}
}

func argv(args ...string) *envv1alpha1.ExecRequest_Argv {
	return &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: args}}
}

// ran is what an Exec answered: its output in order, each line as
// "out:line" or "err:line", and how it ended.
type ran struct {
	lines []string
	exit  *envv1alpha1.ExecExit
}

// exec runs a command through EnvironmentService.
func exec(ctx context.Context, envs environmentsv1alpha1connect.EnvironmentServiceClient, req *envv1alpha1.ExecRequest) (ran, error) {
	stream, err := envs.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		return ran{}, err
	}
	defer func() { _ = stream.Close() }()
	var r ran
	for stream.Receive() {
		switch m := stream.Msg().GetResponse().(type) {
		case *envv1alpha1.ExecResponse_Stdout:
			for line := range strings.Lines(string(m.Stdout)) {
				r.lines = append(r.lines, "out:"+strings.TrimSuffix(line, "\n"))
			}
		case *envv1alpha1.ExecResponse_Stderr:
			for line := range strings.Lines(string(m.Stderr)) {
				r.lines = append(r.lines, "err:"+strings.TrimSuffix(line, "\n"))
			}
		case *envv1alpha1.ExecResponse_Exit:
			r.exit = m.Exit
		}
	}
	return r, stream.Err()
}

// readFile reads through EnvironmentService, returning what it read and
// whether it reached the file's end.
func readFile(ctx context.Context, envs environmentsv1alpha1connect.EnvironmentServiceClient, req *envv1alpha1.ReadFileRequest) (string, bool, error) {
	stream, err := envs.ReadFile(ctx, connect.NewRequest(req))
	if err != nil {
		return "", false, err
	}
	defer func() { _ = stream.Close() }()
	var b strings.Builder
	eof := false
	for stream.Receive() {
		b.Write(stream.Msg().GetData())
		if end := stream.Msg().GetEnd(); end != nil {
			eof = end.GetEof()
		}
	}
	return b.String(), eof, stream.Err()
}

func writeFile(ctx context.Context, envs environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment, path, data string, offset int64) error {
	_, err := envs.WriteFile(ctx, connect.NewRequest(&envv1alpha1.WriteFileRequest{Environment: env, Path: path, Data: []byte(data), Offset: offset}))
	return err
}

// TestOnlyWorkstreamUsesEnvironments: every EnvironmentService call is
// refused to an admin, root on the host, hostd and the unauthenticated,
// alike whether what it names exists; workstream's goes through.
func TestOnlyWorkstreamUsesEnvironments(t *testing.T) {
	w := newWorld(t)
	w.readyBox("dev")
	ctx := t.Context()
	calls := map[string]func(environmentsv1alpha1connect.EnvironmentServiceClient, *envv1alpha1.Environment) error{
		"Exec": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := exec(ctx, c, &envv1alpha1.ExecRequest{Environment: env, Command: argv("true")})
			return err
		},
		"ReadFile": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, _, err := readFile(ctx, c, &envv1alpha1.ReadFileRequest{Environment: env, Path: "/workspace/go.mod"})
			return err
		},
		"WriteFile": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			return writeFile(ctx, c, env, "/tmp/x", "x", 0)
		},
		"CopyFile": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{From: env, FromPath: "/a", To: env, ToPath: "/b"}))
			return err
		},
		"FetchOrigin": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.FetchOrigin(ctx, connect.NewRequest(&envv1alpha1.FetchOriginRequest{Environment: env}))
			return err
		},
		"Push": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.Push(ctx, connect.NewRequest(&envv1alpha1.PushRequest{Environment: env, Repository: "api", Branch: "main"}))
			return err
		},
		"StartProcess": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.StartProcess(ctx, connect.NewRequest(&envv1alpha1.StartProcessRequest{Environment: env, Command: &envv1alpha1.StartProcessRequest_Shell{Shell: "true"}}))
			return err
		},
		"AttachProcess": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, _, err := attachProcess(ctx, c, &envv1alpha1.AttachProcessRequest{Environment: env, Process: "p1"})
			return err
		},
		"SignalProcess": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{Environment: env, Process: "p1", Signal: envv1alpha1.Signal_SIGNAL_TERM}))
			return err
		},
		"ListProcesses": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: env}))
			return err
		},
		"IssueTicket": func(c environmentsv1alpha1connect.EnvironmentServiceClient, env *envv1alpha1.Environment) error {
			_, err := c.IssueTicket(ctx, connect.NewRequest(&envv1alpha1.IssueTicketRequest{Environment: env, Port: 8080}))
			return err
		},
	}
	callers := map[string]struct {
		c    clients
		code connect.Code
	}{
		"an admin":          {w.as(admin), connect.CodePermissionDenied},
		"hostd":             {w.as(hostd), connect.CodePermissionDenied},
		"root on the host":  {w.local(), connect.CodePermissionDenied},
		"no one":            {w.as(""), connect.CodeUnauthenticated},
		"a token now gone":  {w.as("deleted"), connect.CodePermissionDenied},
		"workstream's role": {w.as(workstream), 0},
	}
	for who, caller := range callers {
		for _, call := range slices.Sorted(maps.Keys(calls)) {
			fn := calls[call]
			there, absent := fn(caller.c.envs, boxEnv("dev")), fn(caller.c.envs, boxEnv("absent"))
			if caller.code == 0 {
				if connect.CodeOf(there) == connect.CodePermissionDenied || connect.CodeOf(there) == connect.CodeUnauthenticated {
					t.Errorf("%s as %s: %v", call, who, there)
				}
				continue
			}
			wantCode(t, there, caller.code, call+" as "+who+" on a box that is there")
			wantCode(t, absent, caller.code, call+" as "+who+" on a box that is not")
			if strings.ReplaceAll(messageOfErr(absent), "absent", "dev") != messageOfErr(there) || strings.Contains(messageOfErr(there), "dev") {
				t.Errorf("%s as %s tells a box there (%q) from one not (%q)", call, who, messageOfErr(there), messageOfErr(absent))
			}
		}
	}
	_, err := exec(ctx, w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("absent"), Command: argv("true")})
	wantCode(t, err, connect.CodeNotFound, "workstream's exec in a box that is not there")
}

func messageOfErr(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return fmt.Sprint(err)
}

// TestExecStreamsOutputInOrder: a command's stdout and stderr reach the
// caller in the order written, then its exit code; a shell line runs under
// /bin/sh -c, and a command past its timeout is killed and says so.
func TestExecStreamsOutputInOrder(t *testing.T) {
	w := newWorld(t)
	b := w.readyBox("dev")
	envs := w.as(workstream).envs
	ctx := t.Context()
	want := []string{"out:one", "err:two", "out:three", "err:four"}
	r, err := exec(ctx, envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv(append([]string{"emit"}, want...)...)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.lines, want) || r.exit.GetCode() != 0 || r.exit.GetTimedOut() {
		t.Errorf("ran %v, exit %v; want %v and 0", r.lines, r.exit, want)
	}
	if r, err := exec(ctx, envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("exit", "3")}); err != nil || r.exit.GetCode() != 3 {
		t.Errorf("exit 3: %v, %v", r.exit, err)
	}
	if _, err := exec(ctx, envs, &envv1alpha1.ExecRequest{
		Environment: boxEnv("dev"), Command: &envv1alpha1.ExecRequest_Shell{Shell: "make test"}, Dir: "/workspace", Env: map[string]string{"CI": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	execs := w.fake.Execs()
	last := execs[len(execs)-1]
	if !slices.Equal(last.Command, []string{"/bin/sh", "-c", "make test"}) || last.Env["CI"] != "1" ||
		last.Instance != (provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}) {
		t.Errorf("the shell line ran as %+v", last)
	}
	r, err = exec(ctx, envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("sleep"), Timeout: durationpb.New(50 * time.Millisecond)})
	if err != nil || !r.exit.GetTimedOut() || r.exit.GetCode() != 124 {
		t.Errorf("a command past its timeout: exit %v, %v; want timed out, 124", r.exit, err)
	}
	_, err = exec(ctx, envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev")})
	wantCode(t, err, connect.CodeInvalidArgument, "an exec of no command")
}

// TestAPrivilegedBoxsSecretReachesOnlyItsCommands: tower adds the values a
// privileged box names to each command's environment, under the names it
// gives them, over what the caller gives; a key it does not name is not
// given, and the values reach no object, event, log line or database file.
func TestAPrivilegedBoxsSecretReachesOnlyItsCommands(t *testing.T) {
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const canary, npm, unnamed = "canary-gh-Wq07", "canary-npm-Lp30", "canary-unnamed-Zt88"
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": canary, "UNNAMED": unnamed}))
	w.mustApply(registry.Secret, secret("web", "registry", map[string]string{"token": npm}))
	w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}, {Secret: "registry", Key: "token", As: "NPM_TOKEN"}}
	})
	w.readyBox("plain")
	envs := w.as(workstream).envs
	for _, name := range []string{"priv", "plain"} {
		if _, err := exec(t.Context(), envs, &envv1alpha1.ExecRequest{
			Environment: boxEnv(name), Command: argv("gh", "pr", "list"), Env: map[string]string{"GH_TOKEN": "forged", "PAGER": "cat"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	execs := w.fake.Execs()
	priv, plain := execs[len(execs)-2], execs[len(execs)-1]
	if priv.Env["GH_TOKEN"] != canary || priv.Env["NPM_TOKEN"] != npm || priv.Env["PAGER"] != "cat" || len(priv.Env) != 3 {
		t.Errorf("the privileged box's command was given %v", priv.Env)
	}
	if plain.Env["GH_TOKEN"] != "forged" {
		t.Errorf("a plain box's command was given %v", plain.Env)
	}
	w.waitSettled()
	w.nowhere(canary, &logs)
	w.nowhere(npm, &logs)
}

// TestExecWakesAnIdleBox: a command in a box tower stopped for idleness
// starts it, waits until it is READY, and runs there: the same box, on the
// same instance.
func TestExecWakesAnIdleBox(t *testing.T) {
	w := newWorld(t, idleAfter(time.Hour))
	before := w.readyBox("dev")
	w.advance(61 * time.Minute)
	stopped := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	if stopped.GetStatus().GetStoppedReason() != "Idle" {
		t.Fatalf("stopped for %q", stopped.GetStatus().GetStoppedReason())
	}
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	r, err := exec(ctx, w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("echo", "awake")})
	if err != nil || !slices.Equal(r.lines, []string{"out:awake"}) {
		t.Fatalf("an exec in an idle box: %v, %v", r.lines, err)
	}
	after := w.box("dev")
	if after.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("the box is %v after the exec", after.GetStatus().GetPhase())
	}
	if after.GetMetadata().GetUid() != before.GetMetadata().GetUid() || after.GetStatus().GetInstance() != before.GetStatus().GetInstance() {
		t.Error("the exec woke another box or another instance")
	}
	// The exec counts as a use: the box is not idle again an hour on from
	// when it was ready.
	w.advance(time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box just used is %v", p)
	}
}

// TestARequestedStopIsRefused: a box or machine workstream stopped is not
// started by a call; the call says why.
func TestARequestedStopIsRefused(t *testing.T) {
	w := newWorld(t)
	b := w.readyBox("dev")
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	w.mustApply(registry.Machine, machine("k3s"))
	m := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	m.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Machine, m)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)
	for _, env := range []*envv1alpha1.Environment{boxEnv("dev"), machineEnv("k3s")} {
		_, err := exec(t.Context(), w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: env, Command: argv("true")})
		wantCode(t, err, connect.CodeFailedPrecondition, "an exec in "+env.String())
		if !strings.Contains(messageOfErr(err), "workstream's request") {
			t.Errorf("the refusal does not say why: %v", err)
		}
	}
	w.waitSettled()
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_STOPPED {
		t.Errorf("the refused call left the box %v", p)
	}
}

// TestFilesThroughTower: a file written in chunks, in order, reads back
// whole and by window; a chunk out of order is refused.
func TestFilesThroughTower(t *testing.T) {
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("dev"), "/tmp/notes/a.txt", "hello ", 0); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(ctx, envs, boxEnv("dev"), "/tmp/notes/a.txt", "world", 6); err != nil {
		t.Fatal(err)
	}
	wantCode(t, writeFile(ctx, envs, boxEnv("dev"), "/tmp/notes/a.txt", "!", 3), connect.CodeFailedPrecondition, "a chunk not where the file ends")
	for _, tt := range []struct {
		offset, limit int64
		want          string
		eof           bool
	}{
		{0, 0, "hello world", true},
		{6, 0, "world", true},
		{0, 5, "hello", false},
		{2, 3, "llo", false},
		{6, 5, "world", true},
		{20, 0, "", true},
	} {
		got, eof, err := readFile(ctx, envs, &envv1alpha1.ReadFileRequest{Environment: boxEnv("dev"), Path: "/tmp/notes/a.txt", Offset: tt.offset, Limit: tt.limit})
		if err != nil || got != tt.want || eof != tt.eof {
			t.Errorf("read at %d, limit %d: %q, eof %v, %v; want %q, eof %v", tt.offset, tt.limit, got, eof, err, tt.want, tt.eof)
		}
	}
	_, _, err := readFile(ctx, envs, &envv1alpha1.ReadFileRequest{Environment: boxEnv("dev"), Path: "/tmp/none"})
	wantCode(t, err, connect.CodeNotFound, "a read of a file not there")
	_, _, err = readFile(ctx, envs, &envv1alpha1.ReadFileRequest{Environment: boxEnv("dev"), Path: "/tmp/notes/a.txt", Limit: apiserver.MaxReadFile + 1})
	wantCode(t, err, connect.CodeInvalidArgument, "a read over the cap")
	big := strings.Repeat("x", apiserver.MaxWriteFile+1)
	wantCode(t, writeFile(ctx, envs, boxEnv("dev"), "/tmp/big", big, 0), connect.CodeInvalidArgument, "a chunk over the cap")
}

// TestCopyFileBetweenBoxes: a copy between two environments of one
// workstream happens on the target; one between workstreams is refused.
func TestCopyFileBetweenBoxes(t *testing.T) {
	w := newWorld(t)
	a := w.readyBox("a")
	bx := w.readyBox("b")
	w.readyBox("other", func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out/result.txt", "built", 0); err != nil {
		t.Fatal(err)
	}
	copyFile := func(to string) error {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv(to), ToPath: "/tmp/in"}))
		return err
	}
	if err := copyFile("b"); err != nil {
		t.Fatal(err)
	}
	files := w.fake.Files(provider.Instance{Project: "web", Name: bx.GetStatus().GetInstance()})
	if files["/tmp/in/result.txt"] != "built" {
		t.Errorf("the copy reached b as %v", files)
	}
	if a.GetStatus().GetTarget() != bx.GetStatus().GetTarget() {
		t.Errorf("two boxes of one workstream on targets %q and %q", a.GetStatus().GetTarget(), bx.GetStatus().GetTarget())
	}
	wantCode(t, copyFile("other"), connect.CodeInvalidArgument, "a copy to another workstream's box")
}

// TestFetchOriginAndPush: FetchOrigin brings the mirror and the box's
// origin/* to the forge; Push goes only with a credential, and forces only
// on its lease.
func TestFetchOriginAndPush(t *testing.T) {
	w := newWorld(t)
	const url = "https://git.example.com/api.git"
	w.fake.SetRemote(url, "main", map[string]string{"go.mod": "module api\n"}, "c1", "c2")
	b := w.readyBox("dev")
	in := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	envs := w.as(workstream).envs
	ctx := t.Context()
	push := func(lease string) error {
		_, err := envs.Push(ctx, connect.NewRequest(&envv1alpha1.PushRequest{Environment: boxEnv("dev"), Repository: "api", Branch: "main", Lease: lease}))
		return err
	}
	err := push("c2")
	wantCode(t, err, connect.CodeFailedPrecondition, "a push to a repository without a credential")
	if !strings.Contains(messageOfErr(err), "credential") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// Someone pushes c3 to the forge.
	w.fake.SetRemote(url, "main", map[string]string{"go.mod": "module api\n"}, "c1", "c2", "c3")
	res, err := envs.FetchOrigin(ctx, connect.NewRequest(&envv1alpha1.FetchOriginRequest{Environment: boxEnv("dev")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetCheckouts(); len(got) != 1 || got[0].GetRepository() != "api" || got[0].GetHead() != "c3" {
		t.Errorf("fetched %v", got)
	}
	files := w.fake.Files(in)
	if files["/workspace/"+fake.OriginRef] != "c3" || files["/workspace/.git/HEAD"] != "c2" {
		t.Errorf("after FetchOrigin the checkout's origin is %q and its HEAD %q; want c3 and c2 as they were",
			files["/workspace/"+fake.OriginRef], files["/workspace/.git/HEAD"])
	}
	if repo, _ := w.getRepo("api"); repo.GetStatus().GetMirrors()[0].GetHead() != "c3" {
		t.Errorf("the repository's mirror: %v", repo.GetStatus().GetMirrors())
	}

	w.mustApply(registry.Secret, secret("web", "forge-write", map[string]string{"token": "tok-write"}))
	// Applied at the version read, which the repository's controller may
	// move between.
	testutil.Eventually(t, "the repository given a credential", func() (bool, string) {
		repo, _ := w.getRepo("api")
		repo = proto.Clone(repo).(*envv1alpha1.Repository)
		repo.Spec.CredentialSecret = "forge-write"
		repo.Status = nil
		_, err := w.asSystem().generic.Apply(ctx, registry.Repository, repo)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
	wantCode(t, push("c2"), connect.CodeAborted, "a push on a lease the branch has moved past")
	if head := w.fake.RemoteHead(url); head != "c3" {
		t.Errorf("a refused push moved the branch to %q", head)
	}
	if err := push("c3"); err != nil {
		t.Fatalf("a push on its lease: %v", err)
	}
	if head := w.fake.RemoteHead(url); head != "c2" {
		t.Errorf("after the push the branch is at %q, want the box's c2", head)
	}
	for _, branch := range []string{"--force", "main:other", "refs/heads/x", ""} {
		_, err := envs.Push(ctx, connect.NewRequest(&envv1alpha1.PushRequest{Environment: boxEnv("dev"), Repository: "api", Branch: branch}))
		wantCode(t, err, connect.CodeInvalidArgument, fmt.Sprintf("a push of branch %q", branch))
	}
	_, err = envs.Push(ctx, connect.NewRequest(&envv1alpha1.PushRequest{Environment: boxEnv("dev"), Repository: "web-ui", Branch: "main"}))
	wantCode(t, err, connect.CodeInvalidArgument, "a push of a repository the box has no checkout of")
}

// ticketTTL is shorter than the fake's own bound on a URL, a minute, so
// tower's TTL is what a ticket's expiry shows.
const ticketTTL = 30 * time.Second

func withTickets(signer *ticket.Signer) worldOption {
	return worldOption{config: func(c *apiserver.Config) { c.Tickets, c.TicketTTL = signer, ticketTTL }}
}

func newSigner(t *testing.T) *ticket.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return ticket.NewSigner(key)
}

// TestATicketVerifiesForItsPortAlone: IssueTicket returns where to stream
// and a ticket that verifies with tower's public key for the box's
// instance and that port alone, until it expires.
func TestATicketVerifiesForItsPortAlone(t *testing.T) {
	signer := newSigner(t)
	w := newWorld(t, withTickets(signer))
	b := w.readyBox("dev")
	res, err := w.as(workstream).envs.IssueTicket(t.Context(), connect.NewRequest(&envv1alpha1.IssueTicketRequest{Environment: boxEnv("dev"), Port: 8080}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if m.GetUrl() == "" || m.GetTicket() == "" {
		t.Fatalf("ticket %v", m)
	}
	expires := m.GetExpireTime().AsTime()
	if want := w.clock.Now().Add(ticketTTL).Truncate(time.Second); !expires.Equal(want) {
		t.Errorf("the ticket expires at %v, want %v, its TTL on", expires, want)
	}
	want := ticket.Claims{Target: "t1", Project: "web", Instance: b.GetStatus().GetInstance(), Port: 8080}
	if _, err := ticket.Verify(signer.PublicKey(), m.GetTicket(), want, w.clock.Now()); err != nil {
		t.Errorf("the ticket does not verify: %v", err)
	}
	other := want
	other.Port = 22
	if _, err := ticket.Verify(signer.PublicKey(), m.GetTicket(), other, w.clock.Now()); !errors.Is(err, ticket.ErrNotFor) {
		t.Errorf("the ticket verified for another port: %v", err)
	}
	if _, err := ticket.Verify(signer.PublicKey(), m.GetTicket(), want, expires); !errors.Is(err, ticket.ErrExpired) {
		t.Errorf("the ticket verified at its expiry: %v", err)
	}
	// A byte of the signature changed: not its last character, whose low
	// bits base64 ignores.
	sig := strings.LastIndex(m.GetTicket(), ".") + 5
	flipped := byte('A')
	if m.GetTicket()[sig] == 'A' {
		flipped = 'B'
	}
	tampered := m.GetTicket()[:sig] + string(flipped) + m.GetTicket()[sig+1:]
	if _, err := ticket.Verify(signer.PublicKey(), tampered, want, w.clock.Now()); !errors.Is(err, ticket.ErrInvalid) {
		t.Errorf("a tampered ticket verified: %v", err)
	}
}

// TestNoTicketOnHangar: a ticket for an environment on a hangar target is
// Unimplemented, and says why.
func TestNoTicketOnHangar(t *testing.T) {
	w := newWorld(t, withTickets(newSigner(t)))
	w.mustApply(registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "hg"},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example.com", Project: "tower", CredentialSecret: "hangar",
		}}},
	})
	w.mustApply(registry.Placement, placement("web", "hg"))
	// A box made on the hangar target, as far as tower can make it with no
	// hangar provider: its target and instance chosen, and said ready.
	b := box("dev")
	b.Spec.Image = "ubuntu:24.04"
	w.mustApply(registry.Box, b)
	testutil.Eventually(t, "the box placed on hg", func() (bool, string) {
		return w.box("dev").GetStatus().GetTarget() == "hg", w.box("dev").GetStatus().String()
	})
	testutil.Eventually(t, "the box said ready", func() (bool, string) {
		b := w.box("dev")
		st := proto.Clone(b.GetStatus()).(*envv1alpha1.BoxStatus)
		st.Phase, st.Instance = envv1alpha1.BoxPhase_BOX_PHASE_READY, "box-dev"
		rv := b.GetMetadata().GetResourceVersion()
		_, err := w.asSystem().generic.UpdateStatus(t.Context(), registry.Box, client.Key{Project: "web", Name: "dev"}, st, &rv)
		return err == nil, fmt.Sprint(err)
	})
	_, err := w.as(workstream).envs.IssueTicket(t.Context(), connect.NewRequest(&envv1alpha1.IssueTicketRequest{Environment: boxEnv("dev"), Port: 8080}))
	wantCode(t, err, connect.CodeUnimplemented, "a ticket on a hangar target")
	if !strings.Contains(messageOfErr(err), "hangar") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
