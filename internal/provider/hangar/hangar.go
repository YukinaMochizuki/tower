// Package hangar is tower's provider of a hangar target: one project of a
// hangar endpoint, which tower uses as an ordinary client with that
// project's service token (docs/decisions/layers.md). A Target makes the
// target's Instances real there, as hostd does on its host: it follows
// them through tower's API, converges the project to them, reads hangar
// back and reports what it found, and a sweep removes what it made that
// nothing accounts for any more. It decides nothing.
//
// Each workstream network of a tower project is a hangar Network of its
// own, which hangar keeps apart from every other. A container Instance is
// a hangar Container on it with a Disk at /workspace; a VM Instance a
// VirtualMachine on it booting from a Disk of its own (vm.go). Either is
// named on the network by the Instance's alias. What runs in it runs
// through tower-guest (internal/guest), which the provider writes into a
// container over hangar's exec, and into a VM over its own SSH as root
// through hangar's port forward, and drives there as hostd does in its
// guests, so processes outlive the exec that starts them. Direct streams
// are relayed by tower to hangar's port forward
// (docs/decisions/traffic.md).
package hangar

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/streams"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// Defaults of a Config.
const (
	DefaultReportEvery = 30 * time.Second
	DefaultSweepEvery  = time.Minute
	// DefaultSweepGrace is how old what nothing accounts for must be
	// before the sweep removes it: an object made as its Instance was
	// listed.
	DefaultSweepGrace = 10 * time.Minute
	// DefaultMirrorOps is how many of a target's mirror operations run at
	// once: each holds an exec in the system container, and the rest wait,
	// so a project of many repositories leaves the connections hangar's
	// front door allows to the target's boxes and VMs as well.
	DefaultMirrorOps = 8
)

// Labels the provider sets on what it makes in hangar. It touches only
// what carries its target's LabelTarget. Their prefix is not tower's API
// domain: hangar keeps every key under hangarlab.cloud to itself.
const (
	LabelTarget   = "tower/target"
	LabelUID      = "tower/instance-uid"
	LabelProject  = "tower/project"
	LabelInstance = "tower/instance"
	LabelNetwork  = "tower/network"
)

// Config is a target's.
type Config struct {
	// Target is the Target the provider provides.
	Target string
	API    API
	// Client acts on tower's API for the target: it reads its Instances and
	// Goldens and writes their status and the Target's.
	Client *client.Client
	// PullSecret is the registry login, in the Docker config format, hangar
	// pulls tower's images with; nil pulls them anonymously.
	PullSecret []byte
	// Guest is tower-guest, statically linked, which the provider writes
	// into each machine.
	Guest []byte
	// SSHKey is the target's key for its VMs' root, an ed25519 key in
	// OpenSSH's PEM: cloud-init gives root its public half. nil runs no VM.
	SSHKey []byte
	// BaseImages maps tower's base images, such as ubuntu-24.04, to the
	// hangar Images VMs of them boot from, such as ubuntu-24-04.
	BaseImages map[string]string
	// ReadyTimeout bounds how long an operation in a VM just started waits
	// for its ssh; zero is DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// KeepAliveEvery is how often a VM's sshd is asked whether it answers,
	// KeepAliveTimeout how long it has to; a connection to one that does
	// not is closed. Zero is each one's default.
	KeepAliveEvery, KeepAliveTimeout time.Duration
	// TicketKey is tower's public key, which direct streams' tickets are
	// verified with; nil refuses every stream.
	TicketKey ed25519.PublicKey
	// StreamURL is where clients reach Streams, such as
	// https://tower.example/streams/hangar/t1/; empty refuses tickets.
	// StreamPrefix is its path, which Streams serves.
	StreamURL, StreamPrefix string
	// SystemImage is the system container's image, by digest
	// (internal/system), in Registry by its name there, which hangar pulls
	// by RegistryHost; empty makes none, and the target keeps no mirror and
	// builds no image. SystemDisk, SystemCPU and SystemMemory size it;
	// empty is each one's default.
	SystemImage                         string
	SystemDisk, SystemCPU, SystemMemory string
	// Registry is tower's registry as the system container pushes to it,
	// host[:port], over the tailnet; RegistryHost, the name hangar pulls
	// from it by, which the images the target builds are named by. Empty
	// is Registry.
	Registry, RegistryHost string
	// RegistryPlainHTTP says Registry is served over plain HTTP, as one on
	// the tailnet alone may be, whose traffic the tailnet encrypts.
	RegistryPlainHTTP bool
	// PushLogin is the registry login the system container pushes with.
	PushLogin system.Login
	// TailnetAuthKey joins the system container to the tailnet, once: its
	// state is kept on its disk.
	TailnetAuthKey string
	// ReportEvery is how often the Target's status is reported, the system
	// container checked and every Instance read back; SweepEvery how often
	// the sweep runs, SweepGrace its grace. Zero is each one's default.
	ReportEvery, SweepEvery, SweepGrace time.Duration
	// MirrorOps caps the target's mirror operations running at once:
	// fetches, reads, checkouts, origins and pushes. Those that are not
	// interactive (provider.Interactive) take at most three quarters of
	// them, so a person's push or fetch never waits behind a reconcile's.
	// Zero is DefaultMirrorOps.
	MirrorOps int
	// Teardown starts the target in teardown, as Teardown puts it: for a
	// Target being deleted.
	Teardown bool
	Clock    clockwork.Clock
	Log      *slog.Logger
}

// Target is one hangar target. Make it with New and run it with Run.
type Target struct {
	cfg   Config
	api   API
	c     *client.Client
	clock clockwork.Clock
	log   *slog.Logger
	ctrl  *controller.Controller
	// mu keeps reconciles, which share it, apart from sweeps, so a sweep
	// never removes what a reconcile is making.
	mu    sync.RWMutex
	guest guestBinary
	// key is the target's key for its VMs' root, nil without one.
	key     *vmssh.Key
	vms     vmConns
	limits  processLimits
	procs   processCache
	streams *streams.Server
	sys     systemState
	// ep is the system container's endpoint (endpoint.go); sgMu keeps its
	// security group's writes apart.
	ep   endpointState
	sgMu sync.Mutex
	// pullWritten says the pull secret is in hangar, as of this run.
	pullMu      sync.Mutex
	pullWritten bool
	// tearingDown says the target is in teardown (Teardown).
	tearingDown atomic.Bool
	// mirrorOps holds a token for each mirror operation running
	// (MirrorOps), and backgroundOps for each of those not interactive.
	mirrorOps, backgroundOps chan struct{}
}

var _ provider.Provider = (*Target)(nil)

// New returns a target of cfg.
func New(cfg Config) *Target {
	cfg.ReportEvery = cmp.Or(cfg.ReportEvery, DefaultReportEvery)
	cfg.SweepEvery = cmp.Or(cfg.SweepEvery, DefaultSweepEvery)
	cfg.SweepGrace = cmp.Or(cfg.SweepGrace, DefaultSweepGrace)
	cfg.RegistryHost = cmp.Or(cfg.RegistryHost, cfg.Registry)
	cfg.ReadyTimeout = cmp.Or(cfg.ReadyTimeout, DefaultReadyTimeout)
	cfg.KeepAliveEvery = cmp.Or(cfg.KeepAliveEvery, DefaultKeepAliveEvery)
	cfg.KeepAliveTimeout = cmp.Or(cfg.KeepAliveTimeout, DefaultKeepAliveTimeout)
	if cfg.MirrorOps <= 0 {
		cfg.MirrorOps = DefaultMirrorOps
	}
	// A quarter of the operations, one at least once there are two, are
	// kept for the interactive.
	kept := cfg.MirrorOps / 4
	if kept == 0 && cfg.MirrorOps > 1 {
		kept = 1
	}
	t := &Target{
		cfg: cfg, api: cfg.API, c: cfg.Client, clock: cfg.Clock, log: cfg.Log,
		mirrorOps: make(chan struct{}, cfg.MirrorOps), backgroundOps: make(chan struct{}, cfg.MirrorOps-kept),
	}
	if t.clock == nil {
		t.clock = clockwork.NewRealClock()
	}
	if t.log == nil {
		t.log = slog.New(slog.DiscardHandler)
	}
	t.log = t.log.With("target", cfg.Target)
	if len(cfg.SSHKey) > 0 {
		if k, err := vmssh.ParseKey(cfg.SSHKey, "tower-hangar-"+cfg.Target); err != nil {
			t.log.Error("the target's ssh key is not one; it runs no VM", "error", err)
		} else {
			t.key = &k
		}
	}
	t.tearingDown.Store(cfg.Teardown)
	t.ep.made, t.ep.refused, t.ep.probed = map[string]bool{}, map[string]refusal{}, map[string]refusal{}
	t.guest = newGuestBinary(cfg.Guest)
	t.ctrl = t.newController()
	t.streams = t.newStreams()
	return t
}

// Run converges the target, reports and sweeps until ctx is done, then
// closes every connection to its VMs: a provider made again, as when its
// target's secrets change, makes its own.
func (t *Target) Run(ctx context.Context) error {
	defer t.vms.closeAll()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return t.ctrl.Run(ctx) })
	g.Go(func() error { t.every(ctx, t.cfg.ReportEvery, "report", t.Report); return nil })
	g.Go(func() error { t.every(ctx, t.cfg.ReportEvery, "system check", t.EnsureSystem); return nil })
	g.Go(func() error { t.every(ctx, t.cfg.SweepEvery, "sweep", t.Sweep); return nil })
	return g.Wait()
}

// every runs fn now and then every d until ctx is done.
func (t *Target) every(ctx context.Context, d time.Duration, what string, fn func(context.Context) error) {
	tick := t.clock.NewTicker(d)
	defer tick.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			t.log.Error(what+" failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.Chan():
		}
	}
}

// Status is what the target's controller is doing, for WaitSettled.
func (t *Target) Status() []controller.Status { return []controller.Status{t.ctrl.Status()} }

// Names of what the provider makes in hangar. An instance's are after its
// uid, so a later Instance of the same name never takes an earlier one's.
// hangar names are DNS labels, unique in the project, which other targets
// on the same project share: a network's carries a hash of what it is.
func objectName(uid string) string { return "t-" + uid }

func networkName(target, project, network string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + project + "\x00" + network))
	readable := label(project + "-" + network)
	const most = 63 - len("w--") - 12
	if len(readable) > most {
		readable = strings.TrimRight(readable[:most], "-")
	}
	return "w-" + readable + "-" + hex.EncodeToString(sum[:])[:12]
}

func pullSecretName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "tower-pull-" + hex.EncodeToString(sum[:])[:12]
}

// label is s with what a DNS label cannot hold made "-", lower case.
func label(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-")
}

// machineOf is the Machine a Container runs as, as hangar names it: its
// name with "ct-" before it (hangar: internal/apiserver/controllers.go,
// machineName), which every name the provider gives keeps within a DNS
// label. The machine read is checked to be the container's own.
func machineOf(container string) string { return "ct-" + container }

// selector is the hangar label selector of what this target made.
func (t *Target) selector(extra ...string) string {
	return strings.Join(append([]string{LabelTarget + "=" + t.cfg.Target}, extra...), ",")
}

// mine reports whether what carries labels is this target's.
func (t *Target) mine(labels map[string]string) bool { return labels[LabelTarget] == t.cfg.Target }

// providerError is what an error of hangar's is to tower.
func providerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrNotFound) || errors.Is(err, provider.ErrUnavailable) || errors.Is(err, provider.ErrFailed) ||
		errors.Is(err, provider.ErrNoSpace) || errors.Is(err, provider.ErrLeaseLost) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %w", err, provider.ErrNotFound)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		// Not hangar's answer: hangar was not reached, or its answer cut
		// short.
		return fmt.Errorf("%w: %w", err, provider.ErrUnavailable)
	}
	switch ce.Code() {
	case connect.CodeNotFound:
		return fmt.Errorf("%w: %w", err, provider.ErrNotFound)
	case connect.CodeResourceExhausted:
		return fmt.Errorf("%w: %w", err, provider.ErrNoSpace)
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeUnknown, connect.CodeInternal, connect.CodeCanceled:
		return fmt.Errorf("%w: %w", err, provider.ErrUnavailable)
	}
	return fmt.Errorf("%w: %w", err, provider.ErrFailed)
}
