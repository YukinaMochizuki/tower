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
//   - TOWER_BOOST_IDLE, the boost_idle set for the run, 2m unless given.
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
	req := &envv1alpha1.StartProcessRequest{
		Environment: r.env(box), Command: &envv1alpha1.StartProcessRequest_Shell{Shell: line},
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
