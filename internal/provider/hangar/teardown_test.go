package hangar_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
)

// labelled lists what the fake holds labelled with target, as kind/name.
func labelled(t *testing.T, f *fake.Fake, target string) []string {
	t.Helper()
	ctx, sel := t.Context(), hangar.LabelTarget+"="+target
	var out []string
	add := func(kind string, metas []*hangarmeta.ObjectMeta, err error) {
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range metas {
			out = append(out, kind+"/"+m.GetName())
		}
	}
	cts, err := f.Containers().List(ctx, sel)
	add("container", metas(cts), err)
	disks, err := f.Disks().List(ctx, sel)
	add("disk", metas(disks), err)
	nets, err := f.Networks().List(ctx, sel)
	add("network", metas(nets), err)
	imgs, err := f.CustomImages().List(ctx, sel)
	add("customimage", metas(imgs), err)
	secrets, err := f.Secrets().List(ctx, sel)
	add("secret", metas(secrets), err)
	neps, err := f.NetworkEndpoints().List(ctx, sel)
	add("networkendpoint", metas(neps), err)
	svcs, err := f.EndpointServices().List(ctx, sel)
	add("endpointservice", metas(svcs), err)
	sgs, err := f.SecurityGroups().List(ctx, sel)
	add("securitygroup", metas(sgs), err)
	slices.Sort(out)
	return out
}

func metas[T interface{ GetMetadata() *hangarmeta.ObjectMeta }](objs []T) []*hangarmeta.ObjectMeta {
	out := make([]*hangarmeta.ObjectMeta, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetMetadata())
	}
	return out
}

// outOfOrder says what hangar holds being deleted before what holds it
// went: a disk a container still mounts, a network a container is still
// on, a security group an interface still names, a registry login while
// a container of the target is still there.
func outOfOrder(t *testing.T, f *fake.Fake, target string) []string {
	t.Helper()
	ctx := t.Context()
	cts, err := f.Containers().List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	disks, err := f.Disks().List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		for _, ct := range cts {
			for _, v := range ct.GetSpec().GetVolumes() {
				if d.GetMetadata().GetDeleteTime() != nil && v.GetSource() == d.GetMetadata().GetName() {
					bad = append(bad, fmt.Sprintf("disk %s deleted while container %s mounts it", v.GetSource(), ct.GetMetadata().GetName()))
				}
			}
		}
	}
	nets, err := f.Networks().List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		for _, ct := range cts {
			for _, i := range ct.GetSpec().GetInterfaces() {
				if n.GetMetadata().GetDeleteTime() != nil && i.GetNetwork() == n.GetMetadata().GetName() {
					bad = append(bad, fmt.Sprintf("network %s deleted while container %s is on it", i.GetNetwork(), ct.GetMetadata().GetName()))
				}
			}
		}
	}
	sgs, err := f.SecurityGroups().List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sg := range sgs {
		for _, ct := range cts {
			for _, i := range ct.GetSpec().GetInterfaces() {
				if sg.GetMetadata().GetDeleteTime() != nil && slices.Contains(i.GetSecurityGroups(), sg.GetMetadata().GetName()) {
					bad = append(bad, fmt.Sprintf("security group %s deleted while container %s names it", sg.GetMetadata().GetName(), ct.GetMetadata().GetName()))
				}
			}
		}
	}
	secrets, err := f.Secrets().List(ctx, hangar.LabelTarget+"="+target)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		for _, ct := range cts {
			if s.GetMetadata().GetDeleteTime() != nil && ct.GetMetadata().GetLabels()[hangar.LabelTarget] == target {
				bad = append(bad, fmt.Sprintf("secret %s deleted while container %s is there", s.GetMetadata().GetName(), ct.GetMetadata().GetName()))
			}
		}
	}
	return bad
}

// TestTeardownTakesEverythingOfTheTargetInHangarsOrder: Teardown deletes
// what the target made in hangar, an instance's container, disk, network,
// network endpoint and golden image, the system container with its disk,
// network, endpoint service and security group, and the registry login,
// each once what holds it has gone, while hangar keeps what is deleted
// until it lets it go; what another target made on the same project
// stays. In teardown, its system container is not made again.
func TestTeardownTakesEverythingOfTheTargetInHangarsOrder(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) {
		c.PullSecret = []byte(`{"auths":{"registry.tower.internal":{"auth":"cmVhZDpvbmx5"}}}`)
	})
	ctx := t.Context()
	h := w.Harness()
	src, _ := h.Start(t, "src", container("ws1"))
	h.Stop(t, src)
	art, err := w.Target.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	theirs := map[string]string{hangar.LabelTarget: "other"}
	meta := func(name string) *hangarmeta.ObjectMeta { return &hangarmeta.ObjectMeta{Name: name, Labels: theirs} }
	if _, err := f.Networks().Apply(ctx, &hangarnetwork.Network{Metadata: meta("w-theirs"), Spec: &hangarnetwork.NetworkSpec{Ipv4Cidr: hangar.NetworkCIDR}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"t-theirs", "t-theirs-spare"} {
		if _, err := f.Disks().Apply(ctx, &hangarstorage.Disk{Metadata: meta(d), Spec: &hangarstorage.DiskSpec{Size: "1Gi"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Containers().Apply(ctx, &hangarcompute.Container{Metadata: meta("t-theirs"), Spec: &hangarcompute.ContainerSpec{
		Image: "registry.tower.internal/base@sha256:00", RunState: hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_STOPPED,
		Interfaces: []*hangarcompute.NetworkInterface{{Network: "w-theirs"}},
		Volumes:    []*hangarcompute.ContainerVolume{{Type: "volume", Source: "t-theirs", Target: hangar.WorkspaceDir}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CustomImages().Apply(ctx, &hangarstorage.CustomImage{Metadata: meta("g-theirs"), Spec: &hangarstorage.CustomImageSpec{
		Source: &hangarstorage.CustomImageSource{Source: &hangarstorage.CustomImageSource_Disk{Disk: &hangarstorage.CustomImageDiskSource{Name: "t-theirs-spare"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Secrets().Apply(ctx, &hangarsecrets.Secret{Metadata: meta("tower-pull-theirs"), Spec: &hangarsecrets.SecretSpec{
		Type: hangarsecrets.SecretType_SECRET_TYPE_REGISTRY_AUTH, Data: map[string][]byte{".dockerconfigjson": []byte("{}")},
	}}); err != nil {
		t.Fatal(err)
	}
	others := labelled(t, f, "other")

	sys := systemName(f)
	uid := strings.TrimPrefix(art.Name, "g-")
	before := labelled(t, f, hangartest.Target)
	for _, want := range []string{"container/" + sys, "disk/" + sys, "network/" + sys, "endpointservice/" + sys, "securitygroup/" + sys,
		"container/t-" + uid, "disk/t-" + uid, "customimage/" + art.Name} {
		if !slices.Contains(before, want) {
			t.Fatalf("before the teardown hangar holds %v, not %s", before, want)
		}
	}
	if !slices.ContainsFunc(before, func(s string) bool { return strings.HasPrefix(s, "secret/tower-pull-") }) {
		t.Fatalf("before the teardown hangar holds %v, no registry login", before)
	}
	if !slices.ContainsFunc(before, func(s string) bool { return strings.HasPrefix(s, "networkendpoint/tower-ep-") }) {
		t.Fatalf("before the teardown hangar holds %v, no network endpoint", before)
	}

	// hangar keeps what is deleted until it lets it go, which it does
	// between the passes.
	f.HoldDeletes()
	// The world's end waits for what hangar holds to go.
	t.Cleanup(f.ReleaseDeletes)
	for pass := 1; ; pass++ {
		left, err := w.Target.Teardown(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if bad := outOfOrder(t, f, hangartest.Target); len(bad) > 0 {
			t.Fatalf("pass %d: %v", pass, bad)
		}
		if got := labelled(t, f, hangartest.Target); !slices.Equal(left, got) {
			t.Fatalf("pass %d said %v is left, hangar holds %v", pass, left, got)
		}
		// An endpoint holds an address and the project's quota: it goes at
		// once, not only with its network.
		if neps, err := f.NetworkEndpoints().List(ctx, hangar.LabelTarget+"="+hangartest.Target); err != nil {
			t.Fatal(err)
		} else if pass == 1 && slices.ContainsFunc(neps, func(ne *hangarnetwork.NetworkEndpoint) bool { return ne.GetMetadata().GetDeleteTime() == nil }) {
			t.Errorf("after the first pass, a network endpoint is not being deleted")
		}
		if len(left) == 0 {
			break
		}
		if pass == 10 {
			t.Fatalf("after %d passes, %v is left", pass, left)
		}
		f.ReleaseDeletes()
		f.HoldDeletes()
	}
	f.ReleaseDeletes()

	if got := labelled(t, f, "other"); !slices.Equal(got, others) {
		t.Errorf("another target's objects were %v and are %v", others, got)
	}
	if err := w.Target.EnsureSystem(ctx); err != nil {
		t.Fatal(err)
	}
	if got := labelled(t, f, hangartest.Target); len(got) > 0 {
		t.Errorf("in teardown, the target made %v again", got)
	}
}

// lockWatch is a hangar whose applies of networks, disks and containers
// record whether the target's lock was held as each was made.
type lockWatch struct {
	*fake.Fake
	target *hangar.Target
	mu     sync.Mutex
	unheld []string
}

func (a *lockWatch) applied(what string) {
	if !a.target.Locked() {
		a.mu.Lock()
		a.unheld = append(a.unheld, what)
		a.mu.Unlock()
	}
}

func (a *lockWatch) Unheld() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.unheld)
}

func (a *lockWatch) Networks() hangar.Kind[*hangarnetwork.Network] {
	return watched[*hangarnetwork.Network]{a.Fake.Networks(), a}
}
func (a *lockWatch) Disks() hangar.Kind[*hangarstorage.Disk] {
	return watched[*hangarstorage.Disk]{a.Fake.Disks(), a}
}
func (a *lockWatch) Containers() hangar.Kind[*hangarcompute.Container] {
	return watched[*hangarcompute.Container]{a.Fake.Containers(), a}
}
func (a *lockWatch) EndpointServices() hangar.Kind[*hangarnetwork.EndpointService] {
	return watched[*hangarnetwork.EndpointService]{a.Fake.EndpointServices(), a}
}
func (a *lockWatch) SecurityGroups() hangar.Kind[*hangarnetwork.SecurityGroup] {
	return watched[*hangarnetwork.SecurityGroup]{a.Fake.SecurityGroups(), a}
}

type watched[T interface{ GetMetadata() *hangarmeta.ObjectMeta }] struct {
	hangar.Kind[T]
	a *lockWatch
}

func (k watched[T]) Apply(ctx context.Context, obj T) (T, error) {
	k.a.applied(fmt.Sprintf("%T %s", obj, obj.GetMetadata().GetName()))
	return k.Kind.Apply(ctx, obj)
}

// TestTheSystemContainerIsMadeUnderTheTargetsLock: each of the system
// container's objects is made holding the target's lock, which a teardown
// pass takes, and under which it puts the target in teardown: so no pass
// runs between EnsureSystem reading that the target is not in teardown
// and its making an object, which would outlive the teardown.
func TestTheSystemContainerIsMadeUnderTheTargetsLock(t *testing.T) {
	f := fake.New(towerGuest, t.TempDir(), clockwork.NewRealClock())
	t.Cleanup(f.Close)
	a := &lockWatch{Fake: f}
	a.target = hangar.New(hangar.Config{Target: "t1", API: a, SystemImage: "registry.tower.internal/tower/system@sha256:01"})
	// tower-system does not answer in this fake's container: what counts is
	// what was made.
	_ = a.target.EnsureSystem(t.Context())
	if got := labelled(t, f, "t1"); len(got) != 5 {
		t.Fatalf("EnsureSystem made %v", got)
	}
	if got := a.Unheld(); len(got) > 0 {
		t.Errorf("made without the target's lock: %v", got)
	}
}

// flakyAPI is a hangar whose endpoint services fail to be read, and whose
// next reads of containers fail, while set, counting the containers
// applied.
type flakyAPI struct {
	*fake.Fake
	mu        sync.Mutex
	failSvc   bool
	failGets  int
	ctApplies int
}

func (a *flakyAPI) Containers() hangar.Kind[*hangarcompute.Container] {
	return flakyContainers{a.Fake.Containers(), a}
}

func (a *flakyAPI) EndpointServices() hangar.Kind[*hangarnetwork.EndpointService] {
	return flakyServices{a.Fake.EndpointServices(), a}
}

type flakyContainers struct {
	hangar.Kind[*hangarcompute.Container]
	a *flakyAPI
}

func (k flakyContainers) Get(ctx context.Context, name string) (*hangarcompute.Container, error) {
	k.a.mu.Lock()
	fail := k.a.failGets > 0
	if fail {
		k.a.failGets--
	}
	k.a.mu.Unlock()
	if fail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("hangar did not answer"))
	}
	return k.Kind.Get(ctx, name)
}

func (k flakyContainers) Apply(ctx context.Context, ct *hangarcompute.Container) (*hangarcompute.Container, error) {
	k.a.mu.Lock()
	k.a.ctApplies++
	k.a.mu.Unlock()
	return k.Kind.Apply(ctx, ct)
}

type flakyServices struct {
	hangar.Kind[*hangarnetwork.EndpointService]
	a *flakyAPI
}

func (k flakyServices) Get(ctx context.Context, name string) (*hangarnetwork.EndpointService, error) {
	k.a.mu.Lock()
	fail := k.a.failSvc
	k.a.mu.Unlock()
	if fail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("hangar did not answer"))
	}
	return k.Kind.Get(ctx, name)
}

// TestTheSystemContainersGroupsAreNeverGuessed: when the endpoint service
// cannot be read, and neither can the system container once, the system
// container is left as it is, still in its security group, not applied
// without it, which would restart it.
func TestTheSystemContainersGroupsAreNeverGuessed(t *testing.T) {
	f := fake.New(towerGuest, t.TempDir(), clockwork.NewRealClock())
	f.System = towerSystem
	t.Cleanup(f.Close)
	a := &flakyAPI{Fake: f}
	target := hangar.New(hangar.Config{Target: "t1", API: a, SystemImage: "registry.tower.internal/tower/system@sha256:01"})
	ctx := t.Context()
	if err := target.EnsureSystem(ctx); err != nil {
		t.Fatal(err)
	}
	sys := systemName(f)
	groups := func() []string {
		ct, err := f.Containers().Get(ctx, sys)
		if err != nil {
			t.Fatal(err)
		}
		return ct.GetSpec().GetInterfaces()[0].GetSecurityGroups()
	}
	if got := groups(); !slices.Equal(got, []string{sys}) {
		t.Fatalf("the system container's groups: %v", got)
	}
	a.mu.Lock()
	a.failSvc, a.failGets, a.ctApplies = true, 1, 0
	a.mu.Unlock()
	if err := target.EnsureSystem(ctx); err == nil {
		t.Error("EnsureSystem answered no error, its container's groups not read")
	}
	a.mu.Lock()
	applies := a.ctApplies
	a.mu.Unlock()
	if applies != 0 {
		t.Errorf("the system container was applied %d times, its groups not read", applies)
	}
	if got := groups(); !slices.Equal(got, []string{sys}) {
		t.Errorf("the system container's groups after: %v", got)
	}
}
