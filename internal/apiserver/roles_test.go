package apiserver_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// call makes one call of verb on kind r, naming the object name (in
// project "web" for a project-scoped kind), as c.
func call(ctx context.Context, c clients, r registry.Resource, verb authz.Verb, name string) error {
	key := client.Key{Name: name}
	if r.ProjectScoped() {
		key.Project = "web"
	}
	switch verb {
	case authz.Get:
		_, err := c.generic.Get(ctx, r, key)
		return err
	case authz.List:
		_, err := c.generic.List(ctx, r, client.ListOptions{Project: key.Project})
		return err
	case authz.Watch:
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		opened := errors.New("opened")
		err := c.generic.Watch(ctx, r, client.ListOptions{Project: key.Project}, 0, func(client.Event) error { return opened })
		if errors.Is(err, opened) {
			return nil
		}
		return err
	case authz.Apply:
		_, err := c.generic.Apply(ctx, r, sample(r, name))
		return err
	case authz.Delete:
		_, err := c.generic.Delete(ctx, r, key)
		return err
	case authz.UpdateStatus:
		_, err := c.generic.UpdateStatus(ctx, r, key, emptyStatus(r), nil)
		return err
	}
	panic("no call for " + string(verb))
}

// emptyStatus is a status of kind r holding nothing.
func emptyStatus(r registry.Resource) proto.Message {
	return r.Message.New().Get(r.Message.Descriptor().Fields().ByName("status")).Message().New().Interface()
}

// sample is an object of kind r, as fixtures makes it, so an apply of it
// allowed changes nothing.
func sample(r registry.Resource, name string) proto.Message {
	meta := &metav1.ObjectMeta{Name: name}
	if r.ProjectScoped() {
		meta.Project = "web"
	}
	switch r.Kind {
	case registry.Project.Kind:
		return project(name)
	case registry.Secret.Kind:
		return secret("web", name, map[string]string{"A": "x"})
	case registry.ServiceToken.Kind:
		return serviceToken(name, iamv1alpha1.Role_ROLE_WORKSTREAM, "")
	case registry.Target.Kind:
		return hostdTarget(name)
	case registry.Placement.Kind:
		return &targetsv1alpha1.Placement{Metadata: meta, Spec: &targetsv1alpha1.PlacementSpec{Target: "t1"}}
	case registry.Repository.Kind:
		return repository(name)
	case registry.Box.Kind:
		return box(name, "repo")
	case registry.Machine.Kind:
		return machine(name)
	case registry.Golden.Kind:
		return &envv1alpha1.Golden{Metadata: meta, Spec: &envv1alpha1.GoldenSpec{
			Target: "t1", InputHash: "h-" + name, Inputs: &envv1alpha1.GoldenSpec_BoxImage{BoxImage: &envv1alpha1.BoxImageInputs{}},
		}}
	case registry.Instance.Kind:
		return &envv1alpha1.Instance{Metadata: meta, Spec: &envv1alpha1.InstanceSpec{Target: "t1", Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER}}
	}
	panic("no sample of " + r.Kind)
}

// fixtures makes, as the system, one object of every kind, of target t1
// where an object has a target, and another Golden and Instance of target
// t2: gd2 and in2.
func (e *env) fixtures() {
	e.t.Helper()
	ctx := e.t.Context()
	sys := e.asSystem().generic
	for _, m := range []struct {
		r   registry.Resource
		obj proto.Message
	}{
		{registry.Project, project("web")},
		{registry.Target, hostdTarget("t1")},
		{registry.Target, hostdTarget("t2")},
		{registry.Placement, sample(registry.Placement, "web")},
		{registry.Secret, sample(registry.Secret, "s")},
		{registry.Repository, sample(registry.Repository, "repo")},
		{registry.Box, sample(registry.Box, "bx")},
		{registry.Machine, sample(registry.Machine, "mc")},
		{registry.Golden, sample(registry.Golden, "gd")},
		{registry.Instance, sample(registry.Instance, "in")},
	} {
		if _, err := sys.Apply(ctx, m.r, m.obj); err != nil {
			e.t.Fatalf("making %s: %v", m.r.Kind, err)
		}
	}
	gd2 := sample(registry.Golden, "gd2").(*envv1alpha1.Golden)
	gd2.Spec.Target = "t2"
	in2 := sample(registry.Instance, "in2").(*envv1alpha1.Instance)
	in2.Spec.Target = "t2"
	if _, err := sys.Apply(ctx, registry.Golden, gd2); err != nil {
		e.t.Fatal(err)
	}
	if _, err := sys.Apply(ctx, registry.Instance, in2); err != nil {
		e.t.Fatal(err)
	}
}

// fixtureOf is the object of each kind fixtures makes, which sample
// matches.
var fixtureOf = map[string]string{
	registry.Project.Kind: "web", registry.Secret.Kind: "s", registry.ServiceToken.Kind: workstream,
	registry.Target.Kind: "t1", registry.Placement.Kind: "web", registry.Repository.Kind: "repo",
	registry.Box.Kind: "bx", registry.Machine.Kind: "mc", registry.Golden.Kind: "gd", registry.Instance.Kind: "in",
}

// TestEveryRolesVerbs: every caller may use exactly the verbs its role
// lists on each kind, and is refused every other, on an object that exists
// and on one that does not alike; a refusal says only what the request
// named. A hostd token is held to its own target, object by object: see
// TestHostdTouchesOnlyItsOwnTarget.
func TestEveryRolesVerbs(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.fixtures()
	adminSecret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("admin2", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()

	writes := []authz.Verb{authz.Get, authz.List, authz.Watch, authz.Apply, authz.Delete}
	reads := []authz.Verb{authz.Get, authz.List, authz.Watch}
	every := map[string][]authz.Verb{}
	for _, r := range registry.All {
		every[r.Kind] = writes
	}
	every[registry.Golden.Kind], every[registry.Instance.Kind] = reads, reads
	for _, tc := range []struct {
		name    string
		c       clients
		allowed map[string][]authz.Verb
	}{
		{"admin, by its secret", e.withToken(adminSecret), every},
		{"admin, by the dev header", e.as(admin), every},
		{"root on the host", e.local(), every},
		{"workstream", e.as(workstream), map[string][]authz.Verb{
			registry.Project.Kind: writes, registry.Secret.Kind: writes,
			registry.Repository.Kind: writes, registry.Box.Kind: writes, registry.Machine.Kind: writes,
			registry.Target.Kind: reads, registry.Placement.Kind: reads,
			registry.Golden.Kind: reads, registry.Instance.Kind: reads,
		}},
		{"a token that is not there", e.as("nobody"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range registry.All {
				for _, verb := range authz.Verbs {
					checkVerb(t, ctx, tc.c, r, verb, slices.Contains(tc.allowed[r.Kind], verb), fixtureOf[r.Kind], "absent")
				}
			}
		})
	}
}

// checkVerb calls verb on r for each name and checks it is allowed or
// refused alike for each, a refusal saying only what was asked.
func checkVerb(t *testing.T, ctx context.Context, c clients, r registry.Resource, verb authz.Verb, allowed bool, names ...string) {
	t.Helper()
	var msgs []string
	for _, name := range names {
		if verb == authz.Delete && allowed && name != "absent" {
			continue // deleting what the others use is not the point
		}
		if verb == authz.Apply && name == "absent" && allowed {
			continue // an allowed apply would make it
		}
		err := call(ctx, c, r, verb, name)
		code := connect.CodeOf(err)
		if allowed && (code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated) {
			t.Errorf("%s %s %q: %v, want allowed", verb, r.Plural, name, err)
		}
		if !allowed {
			if code != connect.CodePermissionDenied {
				t.Errorf("%s %s %q: %v, want permission denied", verb, r.Plural, name, err)
				continue
			}
			msgs = append(msgs, messageWithout(err, name))
			if !strings.Contains(err.Error(), string(verb)) || !strings.Contains(err.Error(), r.Plural) {
				t.Errorf("%s %s %q: the refusal %q does not say what was asked", verb, r.Plural, name, err)
			}
		}
	}
	for _, m := range msgs[min(1, len(msgs)):] {
		if m != msgs[0] {
			t.Errorf("%s %s: the refusals tell one object from another: %q", verb, r.Plural, msgs)
			break
		}
	}
}

// checkHidden checks that verb on each named object answers NotFound, in
// words that do not tell one from another.
func checkHidden(t *testing.T, ctx context.Context, c clients, r registry.Resource, verb authz.Verb, names ...string) {
	t.Helper()
	var msgs []string
	for _, name := range names {
		err := call(ctx, c, r, verb, name)
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s %s %q: %v, want not found", verb, r.Plural, name, err)
			continue
		}
		msgs = append(msgs, messageWithout(err, name))
	}
	for _, m := range msgs[min(1, len(msgs)):] {
		if m != msgs[0] {
			t.Errorf("%s %s: the answers tell one object from another: %q", verb, r.Plural, msgs)
			break
		}
	}
}

// TestHostdTouchesOnlyItsOwnTarget: a hostd token reads the Instances and
// Goldens of its target and its Target, writes the status of those
// Instances and of its Target, and nothing else: another target's objects,
// and ones that do not exist, are not found alike, so hostd reads a
// refusal for any other reason apart from an object gone; a list or a
// watch shows only its own. A hostd token is authenticated, and its secret works.
func TestHostdTouchesOnlyItsOwnTarget(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.fixtures()
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("agent", iamv1alpha1.Role_ROLE_HOSTD, "t1"))).GetSecret()
	if ok, err := e.authenticates(secret); !ok || err != nil {
		t.Fatalf("a hostd token's secret does not authenticate: %v", err)
	}
	own := map[string][]authz.Verb{
		registry.Instance.Kind: {authz.Get, authz.UpdateStatus},
		registry.Golden.Kind:   {authz.Get},
		registry.Target.Kind:   {authz.Get, authz.UpdateStatus},
	}
	others := map[string]string{registry.Instance.Kind: "in2", registry.Golden.Kind: "gd2", registry.Target.Kind: "t2"}
	for how, c := range map[string]clients{"by its secret": e.withToken(secret), "by the dev header": e.as("agent")} {
		t.Run(how, func(t *testing.T) {
			for _, r := range registry.All {
				for _, verb := range authz.Verbs {
					if verb == authz.List || verb == authz.Watch {
						continue // below: allowed, and filtered
					}
					if slices.Contains(own[r.Kind], verb) {
						checkVerb(t, ctx, c, r, verb, true, fixtureOf[r.Kind])
						checkHidden(t, ctx, c, r, verb, others[r.Kind], "absent")
						continue
					}
					checkVerb(t, ctx, c, r, verb, false, fixtureOf[r.Kind], "absent")
				}
			}
			for _, r := range []registry.Resource{registry.Instance, registry.Golden} {
				for _, project := range []string{"web", ""} {
					res, err := c.generic.List(ctx, r, client.ListOptions{Project: project})
					if err != nil {
						t.Fatalf("listing %s in %q: %v", r.Plural, project, err)
					}
					var names []string
					for _, o := range res.Items {
						names = append(names, object.Meta(o).GetName())
					}
					if !slices.Equal(names, []string{fixtureOf[r.Kind]}) {
						t.Errorf("listing %s in %q shows %v, want only its own target's %s", r.Plural, project, names, fixtureOf[r.Kind])
					}
				}
			}
			for _, r := range []registry.Resource{registry.Target, registry.Placement, registry.Box} {
				checkVerb(t, ctx, c, r, authz.List, false, "absent")
				checkVerb(t, ctx, c, r, authz.Watch, false, "absent")
			}
		})
	}
	// A watch shows its own target's objects alone.
	rev, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var seen []string
	done := errors.New("done")
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		for _, name := range []string{"in3", "in4"} {
			in := sample(registry.Instance, name).(*envv1alpha1.Instance)
			if name == "in3" {
				in.Spec.Target = "t2"
			}
			if _, err := e.asSystem().generic.Apply(ctx, registry.Instance, in); err != nil {
				t.Error(err)
			}
		}
	}()
	err = e.withToken(secret).generic.Watch(wctx, registry.Instance, client.ListOptions{}, rev, func(ev client.Event) error {
		if ev.Object == nil {
			return nil
		}
		seen = append(seen, object.Meta(ev.Object).GetName())
		if object.Meta(ev.Object).GetName() == "in4" {
			return done
		}
		return nil
	})
	<-applied
	if !errors.Is(err, done) || !slices.Equal(seen, []string{"in4"}) {
		t.Errorf("a hostd watch saw %v (%v), want only in4, of its target", seen, err)
	}
}

// TestWhoMayIssueTokens: only an admin token and root on the host may
// create and rotate tokens' secrets; every other caller is refused, saying
// only what it named, whether the token exists or not.
func TestWhoMayIssueTokens(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	for i, c := range []clients{e.as(admin), e.local()} {
		name := fmt.Sprintf("made-%d", i)
		must[iamv1alpha1.CreateServiceTokenResponse](t)(c.issuer.CreateServiceToken(ctx, createToken(name, iamv1alpha1.Role_ROLE_WORKSTREAM, "")))
		must[iamv1alpha1.RotateServiceTokenResponse](t)(c.issuer.RotateServiceToken(ctx, rotateToken(name)))
	}
	for who, c := range map[string]clients{"workstream": e.as(workstream), "hostd": e.as(hostd), "a token that is not there": e.as("nobody")} {
		_, err := c.issuer.CreateServiceToken(ctx, createToken("mine", iamv1alpha1.Role_ROLE_ADMIN, ""))
		wantCode(t, err, connect.CodePermissionDenied, who+" creating a token")
		var msgs []string
		for _, name := range []string{"made-0", "absent"} {
			_, err := c.issuer.RotateServiceToken(ctx, rotateToken(name))
			wantCode(t, err, connect.CodePermissionDenied, who+" rotating "+name)
			msgs = append(msgs, messageWithout(err, name))
		}
		if msgs[0] != msgs[1] {
			t.Errorf("%s: the refusals tell an existing token from an absent one: %q, %q", who, msgs[0], msgs[1])
		}
	}
	_, err := e.as("").issuer.CreateServiceToken(ctx, createToken("mine", iamv1alpha1.Role_ROLE_ADMIN, ""))
	wantCode(t, err, connect.CodeUnauthenticated, "a call without credentials")
}

// TestARoleCountsAtOnce: a token's role is read at every call, so deleting
// the token ends what it may do on its very next call, and a token of the
// same name made again holds the new one's role.
func TestARoleCountsAtOnce(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	w := e.as(workstream)
	must[tenancyv1alpha1.GetProjectResponse](t)(w.projects.GetProject(ctx, getProject("web")))
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(e.as(admin).tokens.DeleteServiceToken(ctx, deleteToken(workstream)))
	_, err := w.projects.GetProject(ctx, getProject("web"))
	wantCode(t, err, connect.CodePermissionDenied, "a deleted token, by name")
	e.waitSettled()
	must[iamv1alpha1.ApplyServiceTokenResponse](t)(e.as(admin).tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
		ServiceToken: serviceToken(workstream, iamv1alpha1.Role_ROLE_ADMIN, ""),
	})))
	must[iamv1alpha1.GetServiceTokenResponse](t)(w.tokens.GetServiceToken(ctx, getToken(admin)))
}

// TestAHostdListLearnsNothingOfOtherTargets: paging one object at a time,
// a hostd token is shown its own target's objects alone, and no page token
// names an object it was not shown, as one naming another target's object
// would tell that it exists.
func TestAHostdListLearnsNothingOfOtherTargets(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.fixtures()
	// Around "in", t1's, sort instances of t2 alone.
	for _, name := range []string{"ia", "io"} {
		in := sample(registry.Instance, name).(*envv1alpha1.Instance)
		in.Spec.Target = "t2"
		if _, err := e.asSystem().generic.Apply(ctx, registry.Instance, in); err != nil {
			t.Fatal(err)
		}
	}
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("agent", iamv1alpha1.Role_ROLE_HOSTD, "t1"))).GetSecret()
	c := e.withToken(secret)
	for _, r := range []registry.Resource{registry.Instance, registry.Golden} {
		var shown []string
		token := ""
		for range 10 {
			res, err := c.generic.List(ctx, r, client.ListOptions{Project: "web", PageSize: 1, PageToken: token})
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range res.Items {
				shown = append(shown, object.Meta(o).GetName())
			}
			if token = res.NextPageToken; token == "" {
				break
			}
			raw, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil {
				t.Fatalf("page token %q: %v", token, err)
			}
			if _, name, _ := strings.Cut(string(raw), "/"); !slices.Contains(shown, name) {
				t.Errorf("listing %s, a page token names %q, which the caller was not shown (shown %v)", r.Plural, name, shown)
			}
		}
		if !slices.Equal(shown, []string{fixtureOf[r.Kind]}) {
			t.Errorf("listing %s page by page showed %v, want only %s", r.Plural, shown, fixtureOf[r.Kind])
		}
	}
}
