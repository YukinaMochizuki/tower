package apiserver_test

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
	"google.golang.org/protobuf/proto"

	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1/targetsv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// readyGolden makes a READY golden of the project on target, whose
// artifact is art, held by its artifact finalizer as tower's are.
func readyGolden(t *testing.T, sys *client.Client, project, name, target, art string) {
	t.Helper()
	if _, err := sys.Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name, Finalizers: []string{apiserver.GoldenArtifactFinalizer}},
		Spec: &envv1alpha1.GoldenSpec{Target: target, InputHash: name,
			Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.UpdateStatus(t.Context(), registry.Golden, client.Key{Project: project, Name: name}, &envv1alpha1.GoldenStatus{
		Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: art,
	}, nil); err != nil {
		t.Fatal(err)
	}
}

// getTarget reads a target, nil once it is gone.
func getTarget(t *testing.T, c *client.Client, name string) *targetsv1alpha1.Target {
	t.Helper()
	o, err := c.Get(t.Context(), registry.Target, client.Key{Name: name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return o.(*targetsv1alpha1.Target)
}

// targets is the TargetActionService of the service token of the name.
func targets(e *apiservertest.Env, name string) targetsv1alpha1connect.TargetActionServiceClient {
	return targetsv1alpha1connect.NewTargetActionServiceClient(withHeader(auth.DevPrincipalHeader, "serviceToken:"+name), e.URL)
}

// TestATargetKeepsItsTeardownFinalizer: a Target is made with the teardown
// finalizer, which a user's apply cannot drop; one without it, as made
// before tower set it, is given it.
func TestATargetKeepsItsTeardownFinalizer(t *testing.T) {
	t.Parallel()
	env := apiservertest.Start(t, func(c *apiserver.Config) {
		c.Providers = func(string) (provider.Provider, error) { return nil, errNoProviderHere }
	})
	admin, sys := env.As(apiservertest.Admin), env.AsSystem()
	tg := hostdTarget("t1")
	tg.Metadata.Finalizers = []string{"example.com/mine"}
	// What each apply wrote, before the target controller could see it.
	res, err := admin.Apply(t.Context(), registry.Target, tg)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Object.(*targetsv1alpha1.Target).GetMetadata().GetFinalizers(); !slices.Equal(got, []string{apiserver.TargetTeardownFinalizer}) {
		t.Errorf("a target made by an admin has finalizers %v", got)
	}
	if res, err = admin.Apply(t.Context(), registry.Target, hostdTarget("t1")); err != nil {
		t.Fatal(err)
	}
	if got := res.Object.(*targetsv1alpha1.Target).GetMetadata().GetFinalizers(); !slices.Equal(got, []string{apiserver.TargetTeardownFinalizer}) {
		t.Errorf("after an apply without finalizers, the target has %v", got)
	}

	// As a target stored before tower set the finalizer; the controller
	// may write its status meanwhile.
	testutil.Eventually(t, "the target stored without the finalizer", func() (bool, string) {
		old := getTarget(t, sys, "t1")
		old.Metadata.Finalizers = nil
		_, err := sys.Apply(t.Context(), registry.Target, old)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
	testutil.Eventually(t, "the target given its teardown finalizer", func() (bool, string) {
		got := getTarget(t, sys, "t1").GetMetadata().GetFinalizers()
		return slices.Equal(got, []string{apiserver.TargetTeardownFinalizer}), fmt.Sprint(got)
	})
}

// artifactDeleter is a provider whose only call is DeleteArtifact, which
// it records: all a hostd target's teardown asks of it.
type artifactDeleter struct {
	provider.Provider
	mu      sync.Mutex
	deleted []string
}

func (d *artifactDeleter) DeleteArtifact(_ context.Context, art string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, art)
	return nil
}

func (d *artifactDeleter) Deleted() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.deleted)
}

// TestDeletingAHostdTargetDeletesItsGoldens: a hostd Target being deleted
// deletes its Goldens, whose artifacts hostd deletes, and goes once none
// is left; another target's golden stays.
func TestDeletingAHostdTargetDeletesItsGoldens(t *testing.T) {
	t.Parallel()
	d := &artifactDeleter{}
	env := apiservertest.Start(t, func(c *apiserver.Config) {
		c.Providers = func(target string) (provider.Provider, error) {
			if target == "t1" {
				return d, nil
			}
			return nil, errNoProviderHere
		}
	})
	env.Setup()
	admin, sys := env.As(apiservertest.Admin), env.AsSystem()
	for _, name := range []string{"t1", "t2"} {
		if _, err := admin.Apply(t.Context(), registry.Target, hostdTarget(name)); err != nil {
			t.Fatal(err)
		}
	}
	readyGolden(t, sys, apiservertest.Project, "g1", "t1", "art-1")
	readyGolden(t, sys, apiservertest.Project, "g2", "t2", "art-2")
	if _, err := admin.Delete(t.Context(), registry.Target, client.Key{Name: "t1"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the target gone", func() (bool, string) {
		tg := getTarget(t, sys, "t1")
		return tg == nil, fmt.Sprint(tg.GetStatus())
	})
	if got := d.Deleted(); !slices.Equal(got, []string{"art-1"}) {
		t.Errorf("hostd deleted artifacts %v", got)
	}
	if _, err := sys.Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g1"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the target's golden: %v", err)
	}
	if _, err := sys.Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g2"}); err != nil {
		t.Errorf("another target's golden: %v", err)
	}
}

// TestAGoldenOfATargetGoneCanBeDeleted: a golden whose Target does not
// exist, as one left before targets were torn down, goes when deleted,
// though no one can delete its artifact.
func TestAGoldenOfATargetGoneCanBeDeleted(t *testing.T) {
	t.Parallel()
	env := apiservertest.Start(t, func(c *apiserver.Config) { c.Providers = nil })
	env.Setup()
	sys := env.AsSystem()
	readyGolden(t, sys, apiservertest.Project, "g1", "gone", "art-1")
	if _, err := sys.Delete(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g1"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the golden gone", func() (bool, string) {
		o, err := sys.Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g1"})
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(o, err)
	})
}

// hangarWorld is an apiserver running a hangar target's provider over a
// fake hangar, as tower runs one: target h1, its credential the Secret
// hangar-token of the reserved project. A token "revoked" is refused by
// a hangar of its own, as a real one refuses a token it revoked.
type hangarWorld struct {
	env       *apiservertest.Env
	f         *fake.Fake
	mu        sync.Mutex
	lastToken string
}

func newHangarWorld(t *testing.T) *hangarWorld {
	t.Helper()
	return newHangarWorldWith(t, "good")
}

// newHangarWorldWith is newHangarWorld whose credential is token from the
// start; the system container is waited for only with "good".
func newHangarWorldWith(t *testing.T, token string) *hangarWorld {
	t.Helper()
	w := &hangarWorld{f: fake.New("", t.TempDir(), clockwork.NewRealClock())}
	t.Cleanup(w.f.Close)
	revoked := fake.New("", t.TempDir(), clockwork.NewRealClock())
	revoked.SetDown(connect.NewError(connect.CodeUnauthenticated, errors.New("the token is revoked")))
	w.env = apiservertest.Start(t, func(c *apiserver.Config) {
		c.Providers = nil
		c.SystemImage = "registry.tower.internal/tower/system@sha256:01"
		c.HangarAPI = func(_, _, token string) hangar.API {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.lastToken = token
			if token == "revoked" {
				return revoked
			}
			return w.f
		}
	})
	w.env.Setup()
	admin := w.env.As(apiservertest.Admin)
	if _, err := admin.Apply(t.Context(), registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "system"}}); err != nil {
		t.Fatal(err)
	}
	w.setToken(t, token)
	if _, err := admin.Apply(t.Context(), registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "h1"},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example", Project: fake.Project, CredentialSecret: "hangar-token",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if token != "good" {
		return w
	}
	testutil.Eventually(t, "the system container made", func() (bool, string) {
		return slices.ContainsFunc(w.f.ContainerNames(), func(n string) bool { return strings.HasPrefix(n, "tower-sys-") }), fmt.Sprint(w.f.ContainerNames())
	})
	return w
}

// setToken writes the target's credential, and waits for the provider
// made with it.
func (w *hangarWorld) setToken(t *testing.T, token string) {
	t.Helper()
	if _, err := w.env.As(apiservertest.Admin).Apply(t.Context(), registry.Secret, secret("system", "hangar-token", map[string]string{"token": token})); err != nil {
		t.Fatal(err)
	}
	if getTarget(t, w.env.AsSystem(), "h1") == nil {
		return
	}
	testutil.Eventually(t, "the provider made with token "+token, func() (bool, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lastToken == token, w.lastToken
	})
}

// labelled lists what the fake hangar holds labelled with target h1.
func (w *hangarWorld) labelled(t *testing.T) []string {
	t.Helper()
	ctx, sel := t.Context(), hangar.LabelTarget+"=h1"
	var out []string
	cts, err := w.f.Containers().List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range cts {
		out = append(out, "container/"+o.GetMetadata().GetName())
	}
	disks, err := w.f.Disks().List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range disks {
		out = append(out, "disk/"+o.GetMetadata().GetName())
	}
	nets, err := w.f.Networks().List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range nets {
		out = append(out, "network/"+o.GetMetadata().GetName())
	}
	imgs, err := w.f.CustomImages().List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range imgs {
		out = append(out, "customimage/"+o.GetMetadata().GetName())
	}
	secrets, err := w.f.Secrets().List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range secrets {
		out = append(out, "secret/"+o.GetMetadata().GetName())
	}
	slices.Sort(out)
	return out
}

// golden makes a golden image of h1's in the fake hangar, captured from a
// disk of its, and its READY Golden.
func (w *hangarWorld) golden(t *testing.T, name string) {
	t.Helper()
	labels := map[string]string{hangar.LabelTarget: "h1"}
	if _, err := w.f.Disks().Apply(t.Context(), &hangarstorage.Disk{
		Metadata: &hangarmeta.ObjectMeta{Name: "t-" + name, Labels: labels}, Spec: &hangarstorage.DiskSpec{Size: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.CustomImages().Apply(t.Context(), &hangarstorage.CustomImage{
		Metadata: &hangarmeta.ObjectMeta{Name: "g-" + name, Labels: labels},
		Spec: &hangarstorage.CustomImageSpec{Source: &hangarstorage.CustomImageSource{
			Source: &hangarstorage.CustomImageSource_Disk{Disk: &hangarstorage.CustomImageDiskSource{Name: "t-" + name}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	readyGolden(t, w.env.AsSystem(), apiservertest.Project, name, "h1", "g-"+name)
}

// TestDeletingAHangarTargetTearsItDown: a hangar Target being deleted
// deletes its Goldens, then everything its provider made in hangar, the
// system container's included, and goes once hangar holds none of it.
// While hangar refuses its token, its status says so and what is left;
// once its credential is written again, the teardown goes on.
func TestDeletingAHangarTargetTearsItDown(t *testing.T) {
	t.Parallel()
	w := newHangarWorld(t)
	sys := w.env.AsSystem()
	w.golden(t, "g1")
	before := w.labelled(t)
	for _, want := range []string{"customimage/g-g1", "disk/t-g1"} {
		if !slices.Contains(before, want) {
			t.Fatalf("before the delete hangar holds %v", before)
		}
	}
	for _, kind := range []string{"container/tower-sys-", "disk/tower-sys-", "network/tower-sys-"} {
		if !slices.ContainsFunc(before, func(s string) bool { return strings.HasPrefix(s, kind) }) {
			t.Fatalf("before the delete hangar holds %v, no %s", before, kind)
		}
	}

	w.setToken(t, "revoked")
	if _, err := w.env.As(apiservertest.Admin).Delete(t.Context(), registry.Target, client.Key{Name: "h1"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the target saying hangar refuses it, and what is left", func() (bool, string) {
		st := getTarget(t, sys, "h1").GetStatus()
		return slices.Equal(st.GetRemaining(), []string{"golden web/g1"}) && strings.Contains(st.GetLastError(), "the token is revoked"), st.String()
	})
	if _, err := sys.Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g1"}); err != nil {
		t.Fatalf("the golden while hangar refuses the token: %v", err)
	}
	if got := w.labelled(t); !slices.Equal(got, before) {
		t.Fatalf("while hangar refuses the token, what it holds went from %v to %v", before, got)
	}

	w.setToken(t, "good")
	testutil.Eventually(t, "the target gone", func() (bool, string) {
		tg := getTarget(t, sys, "h1")
		return tg == nil, fmt.Sprint(tg.GetStatus())
	})
	if got := w.labelled(t); len(got) > 0 {
		t.Errorf("the target went, leaving %v in hangar", got)
	}
	if _, err := sys.Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "g1"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the target's golden: %v", err)
	}
}

// TestAbandonTarget: abandoning a target is refused to anyone but an
// admin, and for a target not being deleted; a target whose teardown
// waits on hangar goes, and the answer says what it last found left.
func TestAbandonTarget(t *testing.T) {
	t.Parallel()
	w := newHangarWorld(t)
	sys := w.env.AsSystem()
	ctx := t.Context()
	w.env.CreateToken("ws", iamv1alpha1.Role_ROLE_WORKSTREAM, "")
	w.env.CreateToken("hostd-h1", iamv1alpha1.Role_ROLE_HOSTD, "h1")
	req := connect.NewRequest(&targetsv1alpha1.AbandonTargetRequest{Name: "h1"})
	if _, err := targets(w.env, apiservertest.Admin).AbandonTarget(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("abandoning a target not being deleted: %v", err)
	}

	keySecret := client.Key{Project: "system", Name: "hangar-ssh-h1"}
	if _, err := w.env.As(apiservertest.Admin).Get(ctx, registry.Secret, keySecret); err != nil {
		t.Fatalf("the target's ssh key: %v", err)
	}

	// hangar keeps what is deleted, being deleted.
	w.f.HoldDeletes()
	defer w.f.ReleaseDeletes()
	if _, err := w.env.As(apiservertest.Admin).Delete(ctx, registry.Target, client.Key{Name: "h1"}); err != nil {
		t.Fatal(err)
	}
	var remaining []string
	testutil.Eventually(t, "the target saying what is left", func() (bool, string) {
		st := getTarget(t, sys, "h1").GetStatus()
		remaining = st.GetRemaining()
		return slices.ContainsFunc(remaining, func(s string) bool { return strings.HasPrefix(s, "container/tower-sys-") }), st.String()
	})
	for _, who := range []string{"ws", "hostd-h1"} {
		if _, err := targets(w.env, who).AbandonTarget(ctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s abandoning the target: %v", who, err)
		}
	}
	if tg := getTarget(t, sys, "h1"); tg == nil {
		t.Fatal("the target went after abandons that were refused")
	}
	res, err := targets(w.env, apiservertest.Admin).AbandonTarget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetUnconfirmed(); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "container/tower-sys-") }) || !res.Msg.GetUnconfirmedKnown() {
		t.Errorf("the abandon said %v (known %t) was not confirmed; the status said %v", got, res.Msg.GetUnconfirmedKnown(), remaining)
	}
	if tg := getTarget(t, sys, "h1"); tg != nil {
		t.Errorf("the target abandoned is still there: %v", proto.Clone(tg))
	}
	// Its ssh key goes with it, as the abandon answers.
	if _, err := w.env.As(apiservertest.Admin).Get(ctx, registry.Secret, keySecret); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the abandoned target's ssh key: %v, want it gone", err)
	}
}

// TestATargetRefusedFromTheStartLeavesWhatIsLeftUnknown: a hangar target
// that refuses its token from the teardown's first pass says nothing is
// known of what is left on it, not that nothing is, and so does its
// abandon.
func TestATargetRefusedFromTheStartLeavesWhatIsLeftUnknown(t *testing.T) {
	t.Parallel()
	w := newHangarWorldWith(t, "revoked")
	sys := w.env.AsSystem()
	ctx := t.Context()
	if _, err := w.env.As(apiservertest.Admin).Delete(ctx, registry.Target, client.Key{Name: "h1"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the target saying hangar refuses it", func() (bool, string) {
		st := getTarget(t, sys, "h1").GetStatus()
		return strings.Contains(st.GetLastError(), "the token is revoked"), st.String()
	})
	if st := getTarget(t, sys, "h1").GetStatus(); st.GetRemainingKnown() || len(st.GetRemaining()) > 0 {
		t.Errorf("a target never listed says %v is left, known %t", st.GetRemaining(), st.GetRemainingKnown())
	}
	res, err := targets(w.env, apiservertest.Admin).AbandonTarget(ctx, connect.NewRequest(&targetsv1alpha1.AbandonTargetRequest{Name: "h1"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetUnconfirmedKnown() || len(res.Msg.GetUnconfirmed()) > 0 {
		t.Errorf("the abandon of a target never listed said %v, known %t", res.Msg.GetUnconfirmed(), res.Msg.GetUnconfirmedKnown())
	}
}

// TestAProviderStartedForADeletingTargetMakesNoSystemContainer: the
// provider of a Target already being deleted is made in teardown, and
// never makes its system container, though no teardown pass of its has
// run: one held at a golden whose image is another target's, which the
// provider refuses to delete.
func TestAProviderStartedForADeletingTargetMakesNoSystemContainer(t *testing.T) {
	t.Parallel()
	// No provider yet: its credential holds no token.
	w := newHangarWorldWith(t, "")
	ctx := t.Context()
	theirs := map[string]string{hangar.LabelTarget: "other"}
	if _, err := w.f.Disks().Apply(ctx, &hangarstorage.Disk{
		Metadata: &hangarmeta.ObjectMeta{Name: "t-theirs", Labels: theirs}, Spec: &hangarstorage.DiskSpec{Size: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.CustomImages().Apply(ctx, &hangarstorage.CustomImage{
		Metadata: &hangarmeta.ObjectMeta{Name: "g-theirs", Labels: theirs},
		Spec: &hangarstorage.CustomImageSpec{Source: &hangarstorage.CustomImageSource{
			Source: &hangarstorage.CustomImageSource_Disk{Disk: &hangarstorage.CustomImageDiskSource{Name: "t-theirs"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	readyGolden(t, w.env.AsSystem(), apiservertest.Project, "g1", "h1", "g-theirs")
	if _, err := w.env.As(apiservertest.Admin).Delete(ctx, registry.Target, client.Key{Name: "h1"}); err != nil {
		t.Fatal(err)
	}
	w.setToken(t, "good")
	var p *hangar.Target
	testutil.Eventually(t, "the provider made with the token", func() (bool, string) {
		got, err := w.env.Server.ProviderFor("h1")
		p, _ = got.(*hangar.Target)
		return err == nil && p != nil, fmt.Sprint(err)
	})
	// As its own loop does, at a time the test knows. tower-system would
	// not answer in this fake: what counts is what it makes.
	if err := p.EnsureSystem(ctx); err != nil {
		t.Logf("EnsureSystem: %v", err)
	}
	if got := w.labelled(t); len(got) > 0 {
		t.Errorf("the provider of a target being deleted made %v", got)
	}
}
