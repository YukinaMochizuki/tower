package apiserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A box or machine that joins a tailnet (docs/decisions/tailnet.md). The
// key is opened from its auth_secret for the call that hands it on alone:
// a box's to its provider (provider.JoinTailnet, or provider.TailnetKeys
// for one that joins as it makes the instance), a machine's on the stdin
// of the command that joins it. It enters no object, status, event or log.

const (
	// TailnetJoined is the condition of a box or machine with spec.tailnet,
	// provider.InstanceTailnetJoined's reasons. It never holds either back
	// from READY.
	TailnetJoined = provider.InstanceTailnetJoined
	// tailnetAnnotation is how a machine's join went, a tailnetStatus as
	// JSON; tailnetAppliedAnnotation the Tailnet last applied to it, as
	// JSON, "" for none (machineTailnet).
	tailnetAnnotation        = registry.Domain + "/tailnet"
	tailnetAppliedAnnotation = registry.Domain + "/tailnet-applied"
)

// tailscaleVersion and tailscaleSHA256 are the tailscale a machine that
// joins a tailnet runs when its image has none, deploy/hostd/versions.env's
// TOWER_TAILSCALE_VERSION and TOWER_TAILSCALE_SHA256 (its tests hold them
// equal).
const (
	tailscaleVersion = "1.104.2"
	tailscaleSHA256  = "512f613ed7d04fc7282f7dc5bd76aa105aae28168fbb9357eb749ebcbc29f0ae"
)

// tailnetStatus is how a join stands, as a machine's annotation keeps it.
type tailnetStatus struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// secretVersion says, without opening it, whether a Secret holds its
// tailnet key: a version of its content, changing when the content does,
// or why it holds none, for TailnetJoined's NoKey.
func (s *Server) secretVersion(ctx context.Context, project, name string) (version, missing string, err error) {
	sec, ok, err := getTyped[*secretsv1alpha1.Secret](ctx, s, registry.Secret, resource.Key{Project: project, Name: name})
	if err != nil {
		return "", "", err
	}
	if !ok || sec.GetMetadata().GetDeleteTime() != nil {
		return "", fmt.Sprintf("its auth_secret %q is not there", name), nil
	}
	if keys := sec.GetStatus().GetKeys(); len(keys) > 0 && !slices.Contains(keys, provider.TailnetKeyName) {
		return "", fmt.Sprintf("its auth_secret %q holds no %q", name, provider.TailnetKeyName), nil
	}
	if h := sec.GetStatus().GetContentHmac(); h != "" {
		return h, "", nil
	}
	return fmt.Sprint(sec.GetMetadata().GetResourceVersion()), "", nil
}

// openTailnetKey opens the key a tailnet's auth_secret holds, for the
// caller's one use; a Secret not there, or without the key, is
// provider.ErrNotFound. Its error never quotes the value.
func (s *Server) openTailnetKey(ctx context.Context, project, secret string) (string, error) {
	data, err := s.OpenSecret(s.system(ctx), resource.Key{Project: project, Name: secret})
	if isNotFound(err) {
		return "", fmt.Errorf("its auth_secret %q is not there: %w", secret, provider.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("its auth_secret %q cannot be opened: %s", secret, messageOf(err))
	}
	key := strings.TrimSpace(string(data[provider.TailnetKeyName]))
	if key == "" {
		return "", fmt.Errorf("its auth_secret %q holds no %q: %w", secret, provider.TailnetKeyName, provider.ErrNotFound)
	}
	return key, nil
}

// boxTailnet sets a box's TailnetJoined from its running instance's, and,
// while its provider says it has not joined for want of a key (NoKey), a
// key refused (KeyRejected) or a failure (Failed), gives it the key, in a
// job. Each give that is not followed by a join is a failure of the step,
// tried again once its backoff passes; a change of the Secret's content is
// a step of its own, given at once. It holds nothing back: the box goes on
// whatever its join does.
func (s *Server) boxTailnet(ctx context.Context, key controller.Key, box *envv1alpha1.Box, in *envv1alpha1.Instance, p provider.Provider, bs *boxState) error {
	tn := box.GetSpec().GetTailnet()
	if tn == nil {
		return nil
	}
	ic := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceTailnetJoined)
	switch {
	case ic == nil || ic.GetObservedGeneration() < in.GetMetadata().GetGeneration():
		bs.cond(TailnetJoined, false, provider.TailnetStarting, "its target has not said how its join went as its tailnet is now")
		return nil
	case !keyWanted(ic.GetReason()):
		bs.cond(TailnetJoined, ic.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE, ic.GetReason(), ic.GetMessage())
		return nil
	}
	version, missing, err := s.secretVersion(ctx, key.Project, tn.GetAuthSecret())
	if err != nil {
		return err
	}
	if missing != "" {
		bs.cond(TailnetJoined, false, provider.TailnetNoKey, missing)
		return nil
	}
	givenKey := tailnetGivenKey(tn.GetAuthSecret(), version, in)
	given := givenOf(in)
	if next, pending := s.givenPending(given, givenKey); pending {
		reason, msg := ic.GetReason(), ic.GetMessage()+"; "
		if reason == provider.TailnetNoKey {
			// Given, and its target has not taken it in yet.
			reason, msg = provider.TailnetStarting, ""
		}
		bs.cond(TailnetJoined, false, reason, msg+givenMessage(given, next))
		return nil
	}
	// The step counts a key that could not be given at all; one given is
	// the annotation's to space out (tailnetGiven).
	job, step := "tailnet/"+key.String(), boxStep(key, box, in, "tailnet")
	g, err := s.stepGate(ctx, job, step)
	if err != nil {
		return err
	}
	// A target that did not answer is said, and tried again later.
	waiting := ""
	if g.Waiting != nil {
		waiting = g.Waiting.Error()
	}
	switch {
	case g.Running:
		bs.cond(TailnetJoined, false, provider.TailnetStarting, "its key is being given to its instance")
		return nil
	case waiting != "":
		bs.cond(TailnetJoined, false, ic.GetReason(), ic.GetMessage()+"; its key waits for its target: "+waiting)
		return nil
	case g.Failed != "":
		// Given again once the step's backoff passes; the box goes on.
		bs.cond(TailnetJoined, false, ic.GetReason(), ic.GetMessage()+"; its key could not be given: "+g.Failed)
		return nil
	}
	bs.cond(TailnetJoined, false, provider.TailnetStarting, "its key is being given to its instance")
	ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	project, secret := key.Project, tn.GetAuthSecret()
	s.startJob(bs.st.GetTarget(), job, s.boxCtrl, key, func(ctx context.Context) error {
		// A reconcile that read the instance before an earlier job marked
		// it may start this one: the mark, read again, says it is given.
		fresh, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: ref.Project, Name: ref.Name})
		if err != nil || !ok {
			return err
		}
		if _, pending := s.givenPending(givenOf(fresh), givenKey); pending {
			return nil
		}
		given := givenOf(fresh)
		k, err := s.openTailnetKey(ctx, project, secret)
		if err != nil {
			return err
		}
		if err := p.JoinTailnet(ctx, provider.TailnetJoin{Instance: ref, Key: k}); err != nil {
			return fmt.Errorf("giving instance %s/%s its tailnet key: %s: %w", ref.Project, ref.Name, redact(err.Error(), k), errorKind(err))
		}
		// Given: once its provider says it joined, the box says so; until
		// then, it is given again only once the wait after this give passes.
		if err := s.markGiven(ctx, ref, givenKey, given); err != nil {
			return err
		}
		return s.stepPassed(ctx, step)
	})
	return nil
}

// keyWanted says a TailnetJoined reason of an instance's is one its key is
// given (again) for.
func keyWanted(reason string) bool {
	return reason == provider.TailnetNoKey || reason == provider.TailnetKeyRejected || reason == provider.TailnetFailed
}

// tailnetGivenAnnotation is the last give of an instance's tailnet key
// (or, for a machine, the last join tried with it) that did not end in a
// join yet: a tailnetGiven as JSON. One value per instance, gone with it.
const tailnetGivenAnnotation = registry.Domain + "/tailnet-given"

// tailnetGiven is what tailnetGivenAnnotation holds: which key was given
// (tailnetGivenKey), when, and how many times in a row.
type tailnetGiven struct {
	Key   string    `json:"key"`
	At    time.Time `json:"at"`
	Times int       `json:"times"`
}

// tailnetGivenKey names a give of one content of a tailnet's Secret to one
// run of an instance at one generation: one of another content, run or
// spec is given at once.
func tailnetGivenKey(secret, version string, in *envv1alpha1.Instance) string {
	sum := sha256.Sum256([]byte(secret + "/" + version))
	return fmt.Sprintf("%s@%s#%d", hex.EncodeToString(sum[:6]), runOf(in), in.GetMetadata().GetGeneration())
}

func givenOf(in *envv1alpha1.Instance) tailnetGiven {
	var g tailnetGiven
	_ = json.Unmarshal([]byte(in.GetMetadata().GetAnnotations()[tailnetGivenAnnotation]), &g)
	return g
}

// givenPending says the key of givenKey was given and is not to be given
// again before next: StepRetryFirst after the first give, doubled after
// each give in a row, up to StepRetryMax, as a failed step waits.
func (s *Server) givenPending(g tailnetGiven, givenKey string) (next time.Time, pending bool) {
	if g.Key != givenKey || g.Times == 0 {
		return time.Time{}, false
	}
	wait := StepRetryFirst
	for i := 1; i < g.Times; i++ {
		if wait *= 2; wait >= StepRetryMax {
			wait = StepRetryMax
			break
		}
	}
	next = g.At.Add(wait)
	return next, s.clock.Now().Before(next)
}

// givenMessage says a give pending: when it was, and when the key is
// given again unless it joins.
func givenMessage(g tailnetGiven, next time.Time) string {
	return fmt.Sprintf("its key was given at %s; it is given again at %s unless it joins", g.At.UTC().Format(time.RFC3339), next.UTC().Format(time.RFC3339))
}

// markGiven records a give of the key of givenKey to an instance, after
// prior, the one recorded before.
func (s *Server) markGiven(ctx context.Context, ref provider.Instance, givenKey string, prior tailnetGiven) error {
	g := tailnetGiven{Key: givenKey, At: s.clock.Now(), Times: 1}
	if prior.Key == givenKey {
		g.Times = prior.Times + 1
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return s.annotateInstance(ctx, ref.Project, ref.Name, tailnetGivenAnnotation, string(b))
}

// errorKind is the provider error err is, for a message that cannot wrap
// err itself: ErrFailed for none.
func errorKind(err error) error {
	for _, kind := range []error{provider.ErrUnavailable, provider.ErrNotFound, provider.ErrNoSpace, provider.ErrFailed} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	return provider.ErrFailed
}

// syncInstanceTailnet gives a box's instance the tailnet its box asks for
// now, which its provider applies to it as it runs. It returns the
// instance as written.
func (s *Server) syncInstanceTailnet(ctx context.Context, in *envv1alpha1.Instance, want *envv1alpha1.Tailnet) (*envv1alpha1.Instance, error) {
	if proto.Equal(in.GetSpec().GetTailnet(), want) {
		return in, nil
	}
	updated := proto.Clone(in).(*envv1alpha1.Instance)
	updated.Spec.Tailnet = nil
	if want != nil {
		updated.Spec.Tailnet = proto.Clone(want).(*envv1alpha1.Tailnet)
	}
	res, err := s.backend(registry.Instance).Apply(s.system(ctx), updated)
	if err != nil {
		return nil, err
	}
	return res.Object.(*envv1alpha1.Instance), nil
}

// dropTailnetJoined takes TailnetJoined out of conditions: it goes with
// the spec's tailnet.
func dropTailnetJoined(conditions []*metav1.Condition) []*metav1.Condition {
	return slices.DeleteFunc(slices.Clone(conditions), func(c *metav1.Condition) bool { return c.GetType() == TailnetJoined })
}

// redact is msg without secret in it.
func redact(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// TailnetKeys opens a tailnet's key for a provider that joins an instance
// as it makes it (provider.TailnetKeys).
func (s *Server) TailnetKeys(ctx context.Context, project, secret string) (string, error) {
	return s.openTailnetKey(ctx, project, secret)
}

// machineTailnetCond sets a machine's TailnetJoined from what its
// configure phase, or a change since, recorded on its instance; none for
// a machine whose spec joins no tailnet.
func (s *Server) machineTailnetCond(m *envv1alpha1.Machine, in *envv1alpha1.Instance, st *envv1alpha1.MachineStatus) {
	if m.GetSpec().GetTailnet() == nil {
		st.Conditions = dropTailnetJoined(st.GetConditions())
		return
	}
	var ts tailnetStatus
	a := in.GetMetadata().GetAnnotations()[tailnetAnnotation]
	if a == "" || json.Unmarshal([]byte(a), &ts) != nil || ts.Reason == "" {
		s.machineCond(st, TailnetJoined, false, provider.TailnetStarting, "it joins before its configure phase")
		return
	}
	s.machineCond(st, TailnetJoined, ts.Reason == provider.TailnetJoined, ts.Reason, ts.Message)
}

// appliedTailnet is the tailnet last applied to a machine's instance, nil
// for none.
func appliedTailnet(in *envv1alpha1.Instance) *envv1alpha1.Tailnet {
	a := in.GetMetadata().GetAnnotations()[tailnetAppliedAnnotation]
	if a == "" {
		return nil
	}
	t := &envv1alpha1.Tailnet{}
	if protojson.Unmarshal([]byte(a), t) != nil {
		return nil
	}
	return t
}

// markTailnet records on a machine's instance the tailnet applied to it
// and how its join went, nil and none for one it left.
func (s *Server) markTailnet(ctx context.Context, project, name string, applied *envv1alpha1.Tailnet, ts *tailnetStatus) error {
	status, tn := "", ""
	if ts != nil {
		b, err := json.Marshal(ts)
		if err != nil {
			return err
		}
		status = string(b)
	}
	if applied != nil {
		b, err := protojson.Marshal(applied)
		if err != nil {
			return err
		}
		tn = string(b)
	}
	if err := s.annotateInstance(ctx, project, name, tailnetAnnotation, status); err != nil {
		return err
	}
	return s.annotateInstance(ctx, project, name, tailnetAppliedAnnotation, tn)
}

// machineTailnet applies a change of a configured machine's tailnet to
// its running VM, in a job, as root over its SSH: added, it joins
// (joinMachineTailnet); removed, it logs out and its node's state goes
// (leaveVMTailnet); a new hostname or accept_routes is set on its node
// (setVMTailnet). One that has not joined for want of a key, a key refused
// or a failure joins again: at once for a Secret changed, else once the
// step's backoff passes; a join that failed on the way (a download, a
// tailscaled not ready) is a failure of the step, tried again so. Nothing
// of it holds the machine back.
func (s *Server) machineTailnet(ctx context.Context, key controller.Key, m *envv1alpha1.Machine, in *envv1alpha1.Instance, p provider.Provider, st *envv1alpha1.MachineStatus) error {
	want, have := m.GetSpec().GetTailnet(), appliedTailnet(in)
	var prior tailnetStatus
	_ = json.Unmarshal([]byte(in.GetMetadata().GetAnnotations()[tailnetAnnotation]), &prior)
	join := func(ctx context.Context, ref provider.Instance) (*tailnetStatus, error) {
		ts, err := s.joinMachineTailnet(ctx, p, ref, key.Project, want)
		return &ts, err
	}
	var act func(ctx context.Context, ref provider.Instance) (*tailnetStatus, error)
	switch {
	case want == nil && have == nil:
		return nil
	case want == nil:
		act = func(ctx context.Context, ref provider.Instance) (*tailnetStatus, error) {
			return nil, runVM(ctx, p, ref, leaveVMTailnet, vmTailnetState, vmUnits)
		}
	case have == nil:
		act = join
	case want.GetHostname() != have.GetHostname() || want.GetAcceptRoutes() != have.GetAcceptRoutes():
		act = func(ctx context.Context, ref provider.Instance) (*tailnetStatus, error) {
			routes := "false"
			if want.GetAcceptRoutes() {
				routes = "true"
			}
			if err := runVM(ctx, p, ref, setVMTailnet, want.GetHostname(), routes); err != nil {
				return nil, err
			}
			// How its join stands is as it was, or joined now.
			ts := prior
			now, err := vmTailnetStatus(ctx, p, ref)
			if err != nil {
				return nil, err
			}
			if now.Reason == provider.TailnetJoined || ts.Reason == "" {
				ts = now
			}
			return &ts, nil
		}
	default:
		if !keyWanted(prior.Reason) {
			return nil
		}
		version, missing, err := s.secretVersion(ctx, key.Project, want.GetAuthSecret())
		if err != nil || missing != "" {
			return err
		}
		// A join tried with this key is tried again once its wait passes;
		// one with another key at once.
		given, retryKey := givenOf(in), tailnetGivenKey(want.GetAuthSecret(), version, in)
		if next, pending := s.givenPending(given, retryKey); pending {
			s.machineCond(st, TailnetJoined, false, prior.Reason, prior.Message+"; "+givenMessage(given, next))
			return nil
		}
		act = func(ctx context.Context, ref provider.Instance) (*tailnetStatus, error) {
			// A reconcile that read the instance before an earlier job
			// marked it may start this one: the mark, read again, says so.
			fresh, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: ref.Project, Name: ref.Name})
			if err != nil || !ok {
				return nil, err
			}
			if _, pending := s.givenPending(givenOf(fresh), retryKey); pending {
				return &prior, nil
			}
			return join(ctx, ref)
		}
	}
	// The step counts a change that could not be applied at all; a join
	// tried and not joined is the annotation's to space out (tailnetGiven).
	job, step := "tailnet/"+key.String(), machineStep(key, m, in, "tailnet")
	g, err := s.stepGate(ctx, job, step)
	if err != nil {
		return err
	}
	waiting := ""
	if g.Waiting != nil {
		waiting = g.Waiting.Error()
	}
	// What it says while it waits: why it did not join, if it said so.
	reason, said := provider.TailnetFailed, "its tailnet could not be applied"
	if keyWanted(prior.Reason) {
		reason, said = prior.Reason, prior.Message
	}
	switch {
	case g.Running:
	case waiting != "":
		if want != nil {
			s.machineCond(st, TailnetJoined, false, provider.TailnetStarting, "its tailnet waits for its target: "+waiting)
		}
	case g.Failed != "":
		// Tried again once the step's backoff passes; the machine goes on.
		if want != nil {
			s.machineCond(st, TailnetJoined, false, reason, said+"; "+g.Failed)
		}
	default:
		if want != nil {
			s.machineCond(st, TailnetJoined, false, provider.TailnetStarting, "its tailnet is being applied")
		}
		ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
		s.startJob(st.GetTarget(), job, s.machineCtrl, key, func(ctx context.Context) error {
			ts, err := act(ctx, ref)
			if err != nil {
				return err
			}
			if err := s.markTailnet(ctx, ref.Project, ref.Name, want, ts); err != nil {
				return err
			}
			if err := s.markTried(ctx, ref, want, ts); err != nil {
				return err
			}
			return s.stepPassed(ctx, step)
		})
	}
	return nil
}

// markTried records a machine's join tried with its key that did not end
// in a join (markGiven), so the same key is tried again only once its wait
// passes; one tried with no key in the Secret is none.
func (s *Server) markTried(ctx context.Context, ref provider.Instance, tn *envv1alpha1.Tailnet, ts *tailnetStatus) error {
	if tn == nil || ts == nil || !keyWanted(ts.Reason) {
		return nil
	}
	version, missing, err := s.secretVersion(ctx, ref.Project, tn.GetAuthSecret())
	if err != nil || missing != "" {
		return err
	}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: ref.Project, Name: ref.Name})
	if err != nil || !ok {
		return err
	}
	givenKey := tailnetGivenKey(tn.GetAuthSecret(), version, in)
	if _, pending := s.givenPending(givenOf(in), givenKey); pending {
		// Marked by the try that just ran, or one before it.
		return nil
	}
	return s.markGiven(ctx, ref, givenKey, givenOf(in))
}

// runVM runs a script in a VM as root, a failure quoting its output. As
// root, not as the blueprint's run_as user, whom an exec in a machine runs
// as otherwise: the scripts install tailscale, write systemd units and
// restart tailscaled.
func runVM(ctx context.Context, p provider.Provider, ref provider.Instance, script string, args ...string) error {
	var out bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: append([]string{"sh", "-c", script, "tower-tailnet"}, args...), AsRoot: true, Stdout: &out, Stderr: &out})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exited %d: %s: %w", code, tail(out.String()), provider.ErrFailed)
	}
	return nil
}

// tail is the end of a command's output, as a message quotes it.
func tail(out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 512 {
		out = out[len(out)-512:]
	}
	return out
}

// vmTailnetStatus reads how a VM's node stands from tailscale status.
func vmTailnetStatus(ctx context.Context, p provider.Provider, ref provider.Instance) (tailnetStatus, error) {
	var out bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"sh", "-c", vmBackendState, "tower-tailnet"}, AsRoot: true, Stdout: &out, Stderr: &out})
	if err != nil {
		return tailnetStatus{}, err
	}
	state := strings.TrimSpace(out.String())
	switch {
	case code != 0:
		return tailnetStatus{Reason: provider.TailnetFailed, Message: "tailscale status failed: " + state}, nil
	case state == "Running":
		return tailnetStatus{Reason: provider.TailnetJoined, Message: "joined"}, nil
	case state == "NeedsLogin" || state == "NoState" || state == "":
		return tailnetStatus{Reason: provider.TailnetNoKey, Message: "its node has no state"}, nil
	}
	return tailnetStatus{Reason: provider.TailnetFailed, Message: "tailscaled says it is " + state}, nil
}

// Where tower keeps a VM's node, as its scripts are given them: its state,
// and the systemd units tailscaled runs as.
const (
	vmTailnetState = "/var/lib/tailscale"
	vmUnits        = "/etc/systemd/system"
)

// vmBackendState prints the BackendState tailscale status says.
const vmBackendState = `tailscale status --json --peers=false </dev/null 2>/dev/null | sed -n 's/^[[:space:]]*"BackendState":[[:space:]]*"\([A-Za-z]*\)".*/\1/p' | head -n1`

// leaveVMTailnet takes a VM off its tailnet: sh -c leaveVMTailnet
// tower-tailnet STATE UNITS. Its node logs out, which the tailnet forgets,
// tailscaled stops and is disabled, and its state and tower's DNS split go,
// so a later join is a new node.
const leaveVMTailnet = `state=$1 units=$2
tailscale logout </dev/null >/dev/null 2>&1 || true
systemctl disable --now tailscaled.service </dev/null >/dev/null 2>&1 || true
rm -rf "$state" "$units/tailscaled.service.d/tower-dns.conf"
systemctl daemon-reload </dev/null >/dev/null 2>&1 || true
echo left`

// setVMTailnet sets a VM node's hostname and routes as it runs: sh -c
// setVMTailnet tower-tailnet HOSTNAME true|false.
const setVMTailnet = `set -eu
tailscale set --hostname="$1" --accept-routes="$2" </dev/null`

// joinMachineTailnet joins a machine's VM to its tailnet, as root over the
// provider's SSH: tailscale, installed if the VM has none, runs as
// tailscaled.service with its state on the VM's disk, and joins with the
// key, on the command's stdin alone, only while it has no state. It
// returns how the join went, which never holds the machine back: a
// Secret without the key is NoKey; a join that failed on the way, to be
// tried again, is an error.
func (s *Server) joinMachineTailnet(ctx context.Context, p provider.Provider, ref provider.Instance, project string, tn *envv1alpha1.Tailnet) (tailnetStatus, error) {
	key, err := s.openTailnetKey(ctx, project, tn.GetAuthSecret())
	if errors.Is(err, provider.ErrNotFound) {
		return tailnetStatus{Reason: provider.TailnetNoKey, Message: strings.TrimSuffix(err.Error(), ": "+provider.ErrNotFound.Error())}, nil
	}
	if err != nil {
		return tailnetStatus{}, err
	}
	return joinVM(ctx, p, ref, key, tn)
}

// The exits of joinVMTailnet but 0, joined: tailscale up failed; the node
// has state tailscaled says is not Running; anything else on the way, a
// download or a tailscaled not ready, to be tried again.
const (
	exitTailnetUp    = 10
	exitTailnetState = 11
)

// joinVM runs joinVMTailnet in a VM as root, key on its stdin alone, and
// says how the join went: Joined; KeyRejected for a key tailscale up was
// refused; Failed for a node whose state is not Running. Any other end is
// an error, the join to be tried again. Its words never hold the key.
func joinVM(ctx context.Context, p provider.Provider, ref provider.Instance, key string, tn *envv1alpha1.Tailnet) (tailnetStatus, error) {
	routes := ""
	if tn.GetAcceptRoutes() {
		routes = "1"
	}
	var out bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{
		Instance: ref, Command: []string{"sh", "-c", joinVMTailnet, "tower-tailnet", tailscaleVersion, tailscaleSHA256, tn.GetHostname(), routes, vmUnits},
		AsRoot: true, Stdin: strings.NewReader(key + "\n"), Stdout: &out, Stderr: &out,
	})
	if err != nil {
		return tailnetStatus{}, fmt.Errorf("joining: %s: %w", redact(err.Error(), key), errorKind(err))
	}
	words := tail(redact(out.String(), key))
	switch {
	case code == 0:
		return tailnetStatus{Reason: provider.TailnetJoined, Message: lastLine(words)}, nil
	case code == exitTailnetUp && tailnetKeyRejected.MatchString(words):
		return tailnetStatus{Reason: provider.TailnetKeyRejected, Message: "the tailnet refused its key: " + words}, nil
	case code == exitTailnetState:
		return tailnetStatus{Reason: provider.TailnetFailed, Message: words}, nil
	}
	return tailnetStatus{}, fmt.Errorf("joining exited %d: %s: %w", code, words, provider.ErrFailed)
}

// lastLine is the last line of s.
func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// tailnetKeyRejected matches what tailscale up says of a key the control
// server refused: invalid, expired, or used up.
var tailnetKeyRejected = regexp.MustCompile(`(?i)invalid key|auth ?key|key (has )?expired|not valid|unable to validate`)

// joinVMTailnet joins a VM to its tailnet, as root: sh -c joinVMTailnet
// tower-tailnet VERSION SHA256 HOSTNAME ACCEPT_ROUTES UNITS, the key on
// its stdin, which tailscale up alone reads (every other command's stdin
// is /dev/null), never in an argument or a file. tailscaled, installed at
// VERSION if the VM has none, runs as tailscaled.service, its state in
// /var/lib/tailscale; tailscale up runs only while the node has none.
// It joins with --accept-dns=false, and a drop-in of tailscaled's sends
// ts.net names alone to MagicDNS through systemd-resolved, so the VM's
// peers' aliases resolve as before; a VM without systemd-resolved running
// skips it at once, and resolves no MagicDNS name
// (docs/decisions/tailnet.md).
const joinVMTailnet = `set -eu
ver=$1 sum=$2 host=$3 routes=$4 units=$5
if ! command -v tailscaled >/dev/null 2>&1; then
	case $(uname -m) in x86_64) ;; *) echo "tower installs tailscale on amd64 alone; this VM is $(uname -m)" >&2; exit 11 ;; esac
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	url="https://pkgs.tailscale.com/stable/tailscale_${ver}_amd64.tgz"
	if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$tmp/ts.tgz" "$url" </dev/null || exit 12
	else wget -qO "$tmp/ts.tgz" "$url" </dev/null || exit 12; fi
	echo "$sum  $tmp/ts.tgz" | sha256sum -c --status || { echo "tailscale $ver does not match its sha256" >&2; exit 12; }
	tar -xzf "$tmp/ts.tgz" -C "$tmp" </dev/null
	install -m 0755 "$tmp/tailscale_${ver}_amd64/tailscale" "$tmp/tailscale_${ver}_amd64/tailscaled" /usr/local/bin/ </dev/null
fi
if ! systemctl cat tailscaled.service </dev/null >/dev/null 2>&1; then
	cat >"$units/tailscaled.service" <<EOF
[Unit]
Description=Tailscale node agent
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=$(command -v tailscaled) --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock
RuntimeDirectory=tailscale
StateDirectory=tailscale
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
fi
mkdir -p "$units/tailscaled.service.d"
cat >"$units/tailscaled.service.d/tower-dns.conf" <<'EOF'
[Service]
ExecStartPost=-/bin/sh -c 'command -v resolvectl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved || exit 0; n=0; until resolvectl dns tailscale0 100.100.100.100 2>/dev/null; do n=$$((n + 1)); [ $$n -ge 30 ] && exit 0; sleep 1; done; resolvectl domain tailscale0 "~ts.net"'
EOF
systemctl daemon-reload </dev/null
systemctl enable tailscaled.service </dev/null >/dev/null 2>&1 || exit 12
systemctl restart tailscaled.service </dev/null >/dev/null 2>&1 || exit 12
state=
i=0
while [ $i -lt 30 ]; do
	state=$(tailscale status --json --peers=false </dev/null 2>/dev/null | sed -n 's/^[[:space:]]*"BackendState":[[:space:]]*"\([A-Za-z]*\)".*/\1/p' | head -n1)
	case $state in NeedsLogin | Running | Stopped | NeedsMachineAuth) break ;; esac
	i=$((i + 1))
	sleep 1
done
case $state in
NeedsLogin) ;;
Running) echo "its node has state; it comes back as itself"; exit 0 ;;
"" | NoState | Starting) echo "tailscaled is not ready: ${state:-it does not answer}" >&2; exit 12 ;;
*) echo "its node has state, and tailscaled says it is $state"; exit 11 ;;
esac
set -- --auth-key=file:/dev/stdin --hostname="$host" --accept-dns=false --timeout=2m
[ "$routes" = 1 ] && set -- "$@" --accept-routes
tailscale up "$@" 2>&1 || exit 10
echo "joined as $host"
`

// scrubGoldenTailnet leaves a machine's golden without any tailscale node
// state, which every machine cloned from it would share: sh -c
// scrubGoldenTailnet tower-tailnet STATE. Whatever the golden phase ran,
// tower joins each machine itself, in its own configure phase.
const scrubGoldenTailnet = `state=$1
if [ -e "$state/tailscaled.state" ]; then
	systemctl stop tailscaled.service </dev/null >/dev/null 2>&1 || true
	rm -f "$state/tailscaled.state"
fi`
