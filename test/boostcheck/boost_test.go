//go:build boostcheck

// Package boostcheck runs the checks of a boost (docs/decisions/boost.md)
// against a real tower whose target is a hangar project, as its issue's
// verification asks: a boosted `nproc; free -g` shows the boost's size, the
// box lists it, and the boost is deleted once idle; and, under a budget
// below one boost, a second box's boosted start is queued and runs once
// the first boost is gone. It runs from anywhere that reaches tower:
//
//	go test -tags boostcheck -count=1 -v -timeout 60m ./test/boostcheck/
//
// with
//
//   - TOWER_URL, tower's API, such as https://tower.example.com;
//   - TOWER_TOKEN_FILE, a file holding a ROLE_WORKSTREAM token's secret,
//     and TOWER_ADMIN_TOKEN_FILE a ROLE_ADMIN one's, which sets the
//     target's boost settings for the run and puts them back after;
//   - TOWER_PROJECT and TOWER_TARGET, the project and its hangar target;
//   - TOWER_BOX_A and TOWER_BOX_B, two READY boxes of the project, of
//     two workstreams;
//   - TOWER_BOOST_IDLE, the boost_idle set for the run, 2m unless given;
//   - for the sync's checks (Boost 2/4): TOWER_SYNC_FILE, a tracked file
//     of box A's repository, relative to /workspace, which they edit and
//     put back (README.md unless given); TOWER_SYNC_CMD, what the first
//     boosted job runs after `git diff --stat`, such as `go test ./pkg`
//     (none unless given); and TOWER_GEN_CMD, a boosted job that rewrites
//     tracked files, such as `make gen` (unless given, one that appends
//     to TOWER_SYNC_FILE on the boost).
//
// The measure's check (docs/decisions/process-usage.md),
// TestANonRootBoxsProcessIsMeasuredAsItsUser, needs none of the above but
// TOWER_URL, TOWER_TOKEN_FILE and TOWER_PROJECT, and TOWER_BOX_NONROOT, a
// READY box of the project whose image runs as a user other than root,
// such as a devcontainer's remoteUser; it changes no target, and writes
// one file of its own under the box's /tmp, which it removes.
//
// Each check logs what it saw, the boost's output among it.
package boostcheck

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// bearer adds a token's secret to every request.
type bearer struct{ secret string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(r)
}

type run struct {
	t               *testing.T
	envs            environmentsv1alpha1connect.EnvironmentServiceClient
	ws, admin       *client.Client
	project, target string
	boxA, boxB      string
	idle            time.Duration
	settingsBefore  *targetsv1alpha1.TargetSettings
}

func setup(t *testing.T) *run {
	need := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Skipf("%s is not set: see the package's comment", name)
		}
		return v
	}
	secret := func(name string) string {
		b, err := os.ReadFile(need(name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	url := need("TOWER_URL")
	wsHC := &http.Client{Transport: bearer{secret("TOWER_TOKEN_FILE")}}
	adminHC := &http.Client{Transport: bearer{secret("TOWER_ADMIN_TOKEN_FILE")}}
	r := &run{
		t: t, envs: environmentsv1alpha1connect.NewEnvironmentServiceClient(wsHC, url),
		ws: client.New(wsHC, url), admin: client.New(adminHC, url),
		project: need("TOWER_PROJECT"), target: need("TOWER_TARGET"),
		boxA: need("TOWER_BOX_A"), boxB: need("TOWER_BOX_B"), idle: 2 * time.Minute,
	}
	if v := os.Getenv("TOWER_BOOST_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		r.idle = d
	}
	r.settingsBefore = &targetsv1alpha1.TargetSettings{}
	if set := r.getTarget().GetSpec().GetSettings(); set != nil {
		r.settingsBefore = proto.Clone(set).(*targetsv1alpha1.TargetSettings)
	}
	t.Cleanup(func() {
		r.setSettings(func(s *targetsv1alpha1.TargetSettings) {
			proto.Reset(s)
			proto.Merge(s, r.settingsBefore)
		})
	})
	return r
}

func (r *run) getTarget() *targetsv1alpha1.Target {
	o, err := r.admin.Get(context.Background(), registry.Target, client.Key{Name: r.target})
	if err != nil {
		r.t.Fatal(err)
	}
	return o.(*targetsv1alpha1.Target)
}

// setSettings changes the target's settings as change says.
func (r *run) setSettings(change func(*targetsv1alpha1.TargetSettings)) {
	r.t.Helper()
	for range 5 {
		tg := proto.Clone(r.getTarget()).(*targetsv1alpha1.Target)
		if tg.Spec.Settings == nil {
			tg.Spec.Settings = &targetsv1alpha1.TargetSettings{}
		}
		change(tg.Spec.Settings)
		_, err := r.admin.Apply(context.Background(), registry.Target, tg)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			r.t.Fatal(err)
		}
		return
	}
	r.t.Fatal("the target kept changing under the settings' apply")
}

func (r *run) env(box string) *envv1alpha1.Environment {
	return &envv1alpha1.Environment{Project: r.project, Kind: &envv1alpha1.Environment_Box{Box: box}}
}

// start asks for a boosted shell line in box: it returns the start's
// first answer, then, if wait, asks again with its ticket every 10
// seconds until it starts, logging each pending answer.
func (r *run) start(box, line string, wait bool) *envv1alpha1.StartProcessResponse {
	r.t.Helper()
	// In the checkout, as workstream starts every job: a process given no
	// dir runs in its image's WORKDIR, on a box and its boost alike.
	req := &envv1alpha1.StartProcessRequest{
		Environment: r.env(box), Command: &envv1alpha1.StartProcessRequest_Shell{Shell: line},
		Dir:   "/workspace",
		Boost: &envv1alpha1.BoostIntent{Size: &envv1alpha1.BoostIntent_On{On: true}},
	}
	// Paced as a client of a real tower asks again, not as
	// testutil.EventuallyWithin polls (every 50ms, for up to 30 minutes).
	for deadline := time.Now().Add(30 * time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Second) { //nolint:forbidigo // pacing a real tower's re-asks, which wait for a real boost
		res, err := r.envs.StartProcess(context.Background(), connect.NewRequest(req))
		if err != nil {
			r.t.Fatalf("a boosted start in %s: %v", box, err)
		}
		if res.Msg.GetProcess() != "" || !wait {
			return res.Msg
		}
		p := res.Msg.GetPending()
		r.t.Logf("%s: pending %s, %s (position %d, about %s)", box, p.GetReason(), p.GetMessage(), p.GetPosition(), p.GetEstimatedWait().AsDuration())
		req.Pending = p.GetTicket()
	}
	r.t.Fatalf("a boosted start in %s did not start in 30 minutes", box)
	return nil
}

// output follows the process to its end and returns what it wrote.
func (r *run) output(box, id string) (string, *envv1alpha1.ProcessExit) {
	r.t.Helper()
	stream, err := r.envs.AttachProcess(context.Background(), connect.NewRequest(&envv1alpha1.AttachProcessRequest{Environment: r.env(box), Process: id, Follow: true}))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var out strings.Builder
	var exit *envv1alpha1.ProcessExit
	for stream.Receive() {
		switch m := stream.Msg().GetResponse().(type) {
		case *envv1alpha1.AttachProcessResponse_Output:
			out.Write(m.Output.GetData())
		case *envv1alpha1.AttachProcessResponse_End:
			exit = m.End.GetExit()
		}
	}
	if err := stream.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out.String(), exit
}

// boostGone waits until box has no boost, within within.
func (r *run) boostGone(box string, within time.Duration) {
	r.t.Helper()
	testutil.EventuallyWithin(r.t, within, box+"'s boost deleted", func() (bool, string) {
		_, err := r.ws.Get(context.Background(), registry.Boost, client.Key{Project: r.project, Name: box})
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
}

// file is a box's file, read through tower, and whether it is there: an
// empty file is not a missing one.
func (r *run) file(box, p string) (string, bool) {
	r.t.Helper()
	stream, err := r.envs.ReadFile(context.Background(), connect.NewRequest(&envv1alpha1.ReadFileRequest{Environment: r.env(box), Path: "/workspace/" + p}))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var out strings.Builder
	for stream.Receive() {
		out.Write(stream.Msg().GetData())
	}
	if connect.CodeOf(stream.Err()) == connect.CodeNotFound {
		return "", false
	}
	if err := stream.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out.String(), true
}

// text is a box's file, "" for one not there.
func (r *run) text(box, p string) string {
	r.t.Helper()
	s, _ := r.file(box, p)
	return s
}

// setFile writes a box's file through tower, and puts it back as it was
// once the test ends.
func (r *run) setFile(box, p, text string) {
	r.t.Helper()
	before, existed := r.file(box, p)
	write := func(text string) error {
		_, err := r.envs.WriteFile(context.Background(), connect.NewRequest(&envv1alpha1.WriteFileRequest{Environment: r.env(box), Path: "/workspace/" + p, Data: []byte(text)}))
		return err
	}
	if err := write(text); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		if !existed {
			_, err := r.envs.Exec(context.Background(), connect.NewRequest(&envv1alpha1.ExecRequest{
				Environment: r.env(box), Command: &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"rm", "-f", "/workspace/" + p}}},
			}))
			if err != nil {
				r.t.Errorf("removing %s from %s: %v", p, box, err)
			}
			return
		}
		if err := write(before); err != nil {
			r.t.Errorf("putting %s back in %s: %v", p, box, err)
		}
	})
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestABoostSeesTheBoxsEdits: an edit in the box, not committed, is on
// its boost when a boosted job runs: `git diff --stat` there shows it.
func TestABoostSeesTheBoxsEdits(t *testing.T) {
	r := setup(t)
	file := envOr("TOWER_SYNC_FILE", "README.md")
	marker := "boostcheck " + strconv.FormatInt(time.Now().UnixNano(), 36)
	r.setFile(r.boxA, file, r.text(r.boxA, file)+marker+"\n")
	line := "git diff --stat && grep -c '" + marker + "' " + file
	if cmd := os.Getenv("TOWER_SYNC_CMD"); cmd != "" {
		line += " && " + cmd
	}
	res := r.start(r.boxA, line, true)
	b := res.GetBoost()
	t.Logf("synced in %s: %d files, %d deleted, %d bytes, whole tree %v", b.GetSync().AsDuration(), b.GetSyncFiles(), b.GetSyncDeleted(), b.GetSyncBytes(), b.GetSyncFull())
	out, exit := r.output(r.boxA, res.GetProcess())
	t.Logf("its output:\n%s", out)
	if exit.GetCode() != 0 || exit.GetLost() {
		t.Errorf("it exited %v", exit)
	}
	if !strings.Contains(out, file) || !strings.Contains(out, "\n1\n") {
		t.Errorf("the boost's git diff --stat does not show the box's edit of %s", file)
	}
}

// TestASecondJobSendsOnlyWhatChanged: after another edit, a second
// boosted job's sync sends that file alone.
func TestASecondJobSendsOnlyWhatChanged(t *testing.T) {
	r := setup(t)
	r.output(r.boxA, r.start(r.boxA, "true", true).GetProcess())
	r.setFile(r.boxA, "boostcheck.tmp", "only this\n")
	res := r.start(r.boxA, "cat boostcheck.tmp", true)
	b := res.GetBoost()
	t.Logf("the second sync: %s, %d files, %d deleted, %d bytes", b.GetSync().AsDuration(), b.GetSyncFiles(), b.GetSyncDeleted(), b.GetSyncBytes())
	if b.GetSyncFull() || b.GetSyncFiles() != 1 || b.GetSyncDeleted() != 0 {
		t.Errorf("the second sync sent %v, want the one file", b)
	}
	if out, _ := r.output(r.boxA, res.GetProcess()); out != "only this\n" {
		t.Errorf("the second job saw %q", out)
	}
}

// TestAGenJobsChangesComeBack: a boosted job that rewrites a tracked file
// leaves the box's as it wrote it, and its exit says what came back.
func TestAGenJobsChangesComeBack(t *testing.T) {
	r := setup(t)
	file := envOr("TOWER_SYNC_FILE", "README.md")
	gen := envOr("TOWER_GEN_CMD", "echo boostcheck-generated >> "+file)
	// Put back as it is once the test ends.
	r.setFile(r.boxA, file, r.text(r.boxA, file))
	res := r.start(r.boxA, gen, true)
	_, exit := r.output(r.boxA, res.GetProcess())
	back := exit.GetBoost().GetBack()
	t.Logf("%q: %d files back, %d deleted, %d bytes in %s; moved %v; conflicts %v; over the cap %v; error %q",
		gen, back.GetFiles(), back.GetDeleted(), back.GetBytes(), back.GetTook().AsDuration(), back.GetMoved(), back.GetConflicts(), back.GetOverCap(), back.GetError())
	if exit.GetCode() != 0 || back.GetTotal() == 0 || back.GetError() != "" {
		t.Fatalf("it exited %v, its write-back %v, want what it changed back", exit, back)
	}
	if os.Getenv("TOWER_GEN_CMD") == "" && !strings.Contains(r.text(r.boxA, file), "boostcheck-generated") {
		t.Errorf("%s, which it rewrote on the boost, is not the job's in the box", file)
	}
}

// TestABuiltExecutableRunsInTheBox: a boosted job builds an executable in
// a path the box ignores; once it ends, a plain exec in the box, as
// bash_box runs one, runs it.
func TestABuiltExecutableRunsInTheBox(t *testing.T) {
	r := setup(t)
	exec := func(args ...string) (string, int32) {
		t.Helper()
		stream, err := r.envs.Exec(context.Background(), connect.NewRequest(&envv1alpha1.ExecRequest{
			Environment: r.env(r.boxA), Command: &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: args}}, Dir: "/workspace",
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stream.Close() }()
		var out strings.Builder
		code := int32(-1)
		for stream.Receive() {
			switch m := stream.Msg().GetResponse().(type) {
			case *envv1alpha1.ExecResponse_Stdout:
				out.Write(m.Stdout)
			case *envv1alpha1.ExecResponse_Stderr:
				out.Write(m.Stderr)
			case *envv1alpha1.ExecResponse_Exit:
				code = m.Exit.GetCode()
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		return out.String(), code
	}
	// Ignored in the box, by its own exclude file.
	if out, code := exec("sh", "-c", "echo /boostcheck-out/ >> .git/info/exclude"); code != 0 {
		t.Fatalf("ignoring boostcheck-out in the box: %d %s", code, out)
	}
	t.Cleanup(func() {
		exec("sh", "-c", "grep -vx /boostcheck-out/ .git/info/exclude > .git/info/exclude.boostcheck; mv .git/info/exclude.boostcheck .git/info/exclude; rm -rf boostcheck-out")
	})
	if out, _ := exec("git", "check-ignore", "boostcheck-out/app"); strings.TrimSpace(out) != "boostcheck-out/app" {
		t.Fatalf("boostcheck-out/app is not ignored in the box: %q", out)
	}
	res := r.start(r.boxA, `mkdir -p boostcheck-out && printf '#!/bin/sh\necho boostcheck-built on %s\n' "$(nproc)" > boostcheck-out/app && chmod +x boostcheck-out/app`, true)
	_, exit := r.output(r.boxA, res.GetProcess())
	t.Logf("its write-back: %v", exit.GetBoost().GetBack())
	if exit.GetCode() != 0 {
		t.Fatalf("it exited %v", exit)
	}
	out, code := exec("./boostcheck-out/app")
	t.Logf("run in the box: %q", out)
	if code != 0 || !strings.HasPrefix(out, "boostcheck-built on ") {
		t.Errorf("the job's executable run in the box: exit %d, %q", code, out)
	}
}

// TestABoostShowsItsSizeAndGoesWhenIdle: a boosted `nproc; free -g` runs
// on a boost of the target's boost_resources, the box lists it as run on
// a boost, and the boost is deleted once boost_idle passes.
func TestABoostShowsItsSizeAndGoesWhenIdle(t *testing.T) {
	r := setup(t)
	r.setSettings(func(s *targetsv1alpha1.TargetSettings) { s.BoostIdle = durationpb.New(r.idle) })
	res := r.start(r.boxA, "nproc; free -g", true)
	size := res.GetRanOn().GetResources()
	t.Logf("started %s on a boost of %s vCPU and %s", res.GetProcess(), size.GetCpu(), size.GetMemory())
	out, exit := r.output(r.boxA, res.GetProcess())
	t.Logf("its output:\n%s", out)
	if exit.GetCode() != 0 {
		t.Errorf("it exited %v", exit)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != size.GetCpu() {
		t.Errorf("nproc said %q, the boost has %s vCPU", lines[0], size.GetCpu())
	}
	memory, _ := quantity.Bytes(size.GetMemory())
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > 1 && f[0] == "Mem:" {
			gib, _ := strconv.Atoi(f[1])
			if want := int(memory >> 30); gib < want-1 || gib > want {
				t.Errorf("free -g said %d GiB in all, the boost has %s", gib, size.GetMemory())
			}
		}
	}
	list, err := r.envs.ListProcesses(context.Background(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: r.env(r.boxA)}))
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, p := range list.Msg.GetProcesses() {
		listed = listed || (p.GetProcess() == res.GetProcess() && p.GetRanOn().GetKind() == envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST)
	}
	if !listed {
		t.Errorf("the box does not list %s as run on a boost: %v", res.GetProcess(), list.Msg.GetProcesses())
	}
	r.boostGone(r.boxA, r.idle+3*time.Minute)
	t.Logf("the boost was deleted after boost_idle (%s)", r.idle)
}

// TestASecondBoostWaitsForTheBudget: with boost_budget below one boost,
// a second box's boosted start answers queued at once, and runs once the
// first box's boost is gone.
func TestASecondBoostWaitsForTheBudget(t *testing.T) {
	r := setup(t)
	r.setSettings(func(s *targetsv1alpha1.TargetSettings) {
		s.BoostIdle = durationpb.New(r.idle)
		s.BoostBudget = &targetsv1alpha1.BoostSize{Cpu: "1", Memory: "1Gi"}
	})
	first := r.start(r.boxA, "sleep 30", true)
	t.Logf("%s runs %s on a boost", r.boxA, first.GetProcess())
	asked := time.Now()
	second := r.start(r.boxB, "nproc", false)
	if p := second.GetPending(); p.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Fatalf("%s's boosted start beside %s's boost answered %v, want queued", r.boxB, r.boxA, second)
	}
	if took := time.Since(asked); took > 10*time.Second {
		t.Errorf("the queued answer took %s, not at once", took)
	}
	t.Logf("%s queued: %s, about %s", r.boxB, second.GetPending().GetMessage(), second.GetPending().GetEstimatedWait().AsDuration())
	res := r.start(r.boxB, "nproc", true)
	if _, err := r.ws.Get(context.Background(), registry.Boost, client.Key{Project: r.project, Name: r.boxA}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("%s's boost started while %s's is still there (%v)", r.boxB, r.boxA, err)
	}
	out, _ := r.output(r.boxB, res.GetProcess())
	t.Logf("%s's boost, %s after it asked: nproc %s", r.boxB, time.Since(asked).Round(time.Second), strings.TrimSpace(out))
}

// nonRootSetup is the measure's check's run, of the workstream's token
// alone: no admin token, no boxes A and B, no target settings changed or
// put back. It returns the box TOWER_BOX_NONROOT names.
func nonRootSetup(t *testing.T) (*run, string) {
	need := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Skipf("%s is not set: see the package's comment", name)
		}
		return v
	}
	box := need("TOWER_BOX_NONROOT")
	url, project := need("TOWER_URL"), need("TOWER_PROJECT")
	b, err := os.ReadFile(need("TOWER_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: bearer{strings.TrimSpace(string(b))}}
	return &run{t: t, envs: environmentsv1alpha1connect.NewEnvironmentServiceClient(hc, url), project: project}, box
}

// execOut runs argv in box through tower's exec, as the box's user, and
// returns its stdout, failing the test unless it exits 0.
func (r *run) execOut(box string, argv ...string) string {
	r.t.Helper()
	stream, err := r.envs.Exec(context.Background(), connect.NewRequest(&envv1alpha1.ExecRequest{
		Environment: r.env(box), Command: &envv1alpha1.ExecRequest_Argv{Argv: &envv1alpha1.Argv{Args: argv}},
	}))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var out, errOut strings.Builder
	var exit *envv1alpha1.ExecExit
	for stream.Receive() {
		switch m := stream.Msg().GetResponse().(type) {
		case *envv1alpha1.ExecResponse_Stdout:
			out.Write(m.Stdout)
		case *envv1alpha1.ExecResponse_Stderr:
			errOut.Write(m.Stderr)
		case *envv1alpha1.ExecResponse_Exit:
			exit = m.Exit
		}
	}
	if err := stream.Err(); err != nil {
		r.t.Fatal(err)
	}
	if exit.GetCode() != 0 {
		r.t.Fatalf("%v in %s exited %d: %s", argv, box, exit.GetCode(), errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// TestANonRootBoxsProcessIsMeasuredAsItsUser: a process started in a box
// whose image runs as a user other than root runs as that user, as an
// exec there does (`id -un`, `id -u`), what it writes is that user's, and
// its exit reports its usage: tower measured it.
func TestANonRootBoxsProcessIsMeasuredAsItsUser(t *testing.T) {
	r, box := nonRootSetup(t)
	user, uid := r.execOut(box, "id", "-un"), r.execOut(box, "id", "-u")
	if uid == "0" {
		t.Fatalf("%s runs as root: TOWER_BOX_NONROOT names a box whose image runs as another user", box)
	}
	// Under /tmp: the box's own work tree is left as it is.
	file := "/tmp/.boostcheck-measured-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { r.execOut(box, "rm", "-f", file) })
	res, err := r.envs.StartProcess(context.Background(), connect.NewRequest(&envv1alpha1.StartProcessRequest{
		Environment: r.env(box), Dir: "/tmp",
		Command: &envv1alpha1.StartProcessRequest_Shell{Shell: "id -un; id -u; echo measured > " + file + " && stat -c %U " + file},
	}))
	if err != nil {
		t.Fatal(err)
	}
	out, exit := r.output(box, res.Msg.GetProcess())
	t.Logf("%s: the exec runs as %s (%s); the process wrote %q and ended %v", box, user, uid, out, exit)
	if got := strings.Fields(out); len(got) != 3 || got[0] != user || got[1] != uid || got[2] != user {
		t.Errorf("the process said %q: want it run as %s (%s), its file %s's", out, user, uid, user)
	}
	if exit.GetCode() != 0 || exit.MemoryPeakBytes == nil || exit.GetWall() == nil {
		t.Errorf("the process ended %v: want it measured, its memory peak and wall reported", exit)
	}
}
