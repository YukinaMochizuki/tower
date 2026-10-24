package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

func applyProject(p *tenancyv1alpha1.Project) *connect.Request[tenancyv1alpha1.ApplyProjectRequest] {
	return connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: p})
}

func getProject(name string) *connect.Request[tenancyv1alpha1.GetProjectRequest] {
	return connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: name})
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)

	created := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(project("web"))))
	if !created.GetCreated() {
		t.Error("first apply of a project did not report created")
	}
	meta := created.GetProject().GetMetadata()
	if meta.GetUid() == "" || meta.GetGeneration() != 1 || meta.GetResourceVersion() == 0 || meta.GetCreateTime() == nil {
		t.Errorf("system fields not set on create: %v", meta)
	}
	if got := created.GetProject().GetApiVersion(); got != "tenancy.tower.hangarlab.cloud/v1alpha1" {
		t.Errorf("apiVersion %q", got)
	}

	p := project("web")
	p.Spec = &tenancyv1alpha1.ProjectSpec{DisplayName: "Web"}
	first := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p)))
	got := must[tenancyv1alpha1.GetProjectResponse](t)(a.projects.GetProject(ctx, getProject("web")))
	if !proto.Equal(got.GetProject(), first.GetProject()) {
		t.Errorf("Get returned %v, Apply returned %v", got.GetProject(), first.GetProject())
	}
	if first.GetProject().GetMetadata().GetGeneration() != 2 || first.GetProject().GetMetadata().GetUid() != meta.GetUid() {
		t.Errorf("after a spec change: %v", first.GetProject().GetMetadata())
	}

	// Applying the same object again changes nothing.
	again := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p)))
	if again.GetCreated() || again.GetProject().GetMetadata().GetResourceVersion() != first.GetProject().GetMetadata().GetResourceVersion() {
		t.Errorf("a no-op apply wrote: %v", again.GetProject().GetMetadata())
	}

	// A label change is a write but not a new generation; a spec change is.
	p.Metadata.Labels = map[string]string{"tier": "front"}
	labelled := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p)))
	if lm := labelled.GetProject().GetMetadata(); lm.GetGeneration() != 2 || lm.GetResourceVersion() == first.GetProject().GetMetadata().GetResourceVersion() {
		t.Errorf("a label change: %v", lm)
	}
	// Apply replaces the whole spec: one that leaves the display name out
	// clears it.
	p.Spec = nil
	cleared := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p)))
	if cleared.GetProject().GetSpec().GetDisplayName() != "" || cleared.GetProject().GetMetadata().GetGeneration() != 3 {
		t.Errorf("an apply without the field: %v", cleared.GetProject())
	}

	list := must[tenancyv1alpha1.ListProjectsResponse](t)(a.projects.ListProjects(ctx, connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{})))
	if len(list.GetItems()) != 1 || list.GetRevision() < cleared.GetProject().GetMetadata().GetResourceVersion() {
		t.Errorf("List returned %d items at revision %d", len(list.GetItems()), list.GetRevision())
	}
	selected := must[tenancyv1alpha1.ListProjectsResponse](t)(a.projects.ListProjects(ctx, connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{LabelSelector: "tier=back"})))
	if len(selected.GetItems()) != 0 {
		t.Errorf("label selector tier=back matched %d items", len(selected.GetItems()))
	}

	// A project goes once garbage collection has seen to what it owns.
	must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "web"})))
	testutil.Eventually(t, "Get after Delete says not_found", func() (bool, string) {
		_, err := a.projects.GetProject(ctx, getProject("web"))
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
}

func TestApplyRules(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	a := e.as(admin)

	s := secret("web", "api", map[string]string{"A": "x"})
	s.Metadata.Labels = map[string]string{"tower.hangarlab.cloud/managed": "yes"}
	_, err := a.secrets.ApplySecret(ctx, applySecret(s))
	wantCode(t, err, connect.CodeInvalidArgument, "a token setting a reserved label")
	s.Metadata.Labels = map[string]string{"x.tower.hangarlab.cloud/managed": "yes"}
	_, err = a.secrets.ApplySecret(ctx, applySecret(s))
	wantCode(t, err, connect.CodeInvalidArgument, "a token setting a label under a reserved subdomain")

	_, err = a.secrets.ApplySecret(ctx, applySecret(secret("web", "Bad_Name", map[string]string{"A": "x"})))
	wantCode(t, err, connect.CodeInvalidArgument, "an invalid name")
	_, err = a.secrets.ApplySecret(ctx, applySecret(secret("", "api", map[string]string{"A": "x"})))
	wantCode(t, err, connect.CodeInvalidArgument, "a project-scoped object without a project")
	global := project("web2")
	global.Metadata.Project = "web"
	_, err = a.projects.ApplyProject(ctx, applyProject(global))
	wantCode(t, err, connect.CodeInvalidArgument, "a global object in a project")
	wrongKind := secret("web", "api", map[string]string{"A": "x"})
	wrongKind.Kind = "Project"
	_, err = a.secrets.ApplySecret(ctx, applySecret(wrongKind))
	wantCode(t, err, connect.CodeInvalidArgument, "a secret claiming another kind")

	// Fields the system owns are ignored on input; a secret's status is
	// what its sealing says, at its generation.
	sneaky := secret("web", "api", map[string]string{"A": "x"})
	sneaky.Metadata.Finalizers = []string{"user/finalizer"}
	sneaky.Metadata.OwnerReferences = []*metav1.OwnerReference{{ApiVersion: "x/v1", Kind: "X", Name: "x", Uid: "x"}}
	sneaky.Metadata.Uid = "forged"
	sneaky.Metadata.Generation = 7
	sneaky.Status = &secretsv1alpha1.SecretStatus{ObservedGeneration: 99}
	stored := must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(sneaky))).GetSecret()
	sm := stored.GetMetadata()
	if slices.Contains(sm.GetFinalizers(), "user/finalizer") || sm.GetUid() == "forged" || sm.GetGeneration() != 1 ||
		stored.GetStatus().GetObservedGeneration() != 1 || len(sm.GetOwnerReferences()) != 1 || sm.GetOwnerReferences()[0].GetKind() != "Project" {
		t.Errorf("system-owned fields taken from input: %v / %v", sm, stored.GetStatus())
	}

	// Optimistic concurrency.
	stale := secret("web", "api", map[string]string{"A": "y"})
	stale.Metadata.ResourceVersion = sm.GetResourceVersion() - 1
	_, err = a.secrets.ApplySecret(ctx, applySecret(stale))
	wantCode(t, err, connect.CodeAborted, "an apply with a stale resource version")
	fresh := secret("web", "api", map[string]string{"A": "y"})
	fresh.Metadata.ResourceVersion = sm.GetResourceVersion()
	must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(fresh)))
	missing := secret("web", "missing", map[string]string{"A": "y"})
	missing.Metadata.ResourceVersion = sm.GetResourceVersion()
	_, err = a.secrets.ApplySecret(ctx, applySecret(missing))
	wantCode(t, err, connect.CodeNotFound, "an apply at a resource version of an object that is not there")
	rv := sm.GetResourceVersion()
	_, err = a.secrets.DeleteSecret(ctx, connect.NewRequest(&secretsv1alpha1.DeleteSecretRequest{Project: "web", Name: "api", ExpectedResourceVersion: &rv}))
	wantCode(t, err, connect.CodeAborted, "a delete with a stale resource version")

	// Immutable fields.
	regAuth := secret("web", "api", nil)
	regAuth.Spec.Type = secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH
	regAuth.Spec.Data = map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"ghcr.io":{"identitytoken":"t"}}}`)}
	_, err = a.secrets.ApplySecret(ctx, applySecret(regAuth))
	wantCode(t, err, connect.CodeInvalidArgument, "changing a secret's type")
	_, err = a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: serviceToken(workstream, iamv1alpha1.Role_ROLE_ADMIN, "")}))
	wantCode(t, err, connect.CodeInvalidArgument, "changing a token's role")
	_, err = a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: serviceToken(hostd, iamv1alpha1.Role_ROLE_HOSTD, "h2")}))
	wantCode(t, err, connect.CodeInvalidArgument, "changing a hostd token's target")
}

// TestCreateOnlyApplyNeverReplaces: a create-only apply refuses an object
// that exists with AlreadyExists, changing nothing, and takes no resource
// version.
func TestCreateOnlyApplyNeverReplaces(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	created := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: project("web"), CreateOnly: true})))
	if !created.GetCreated() {
		t.Error("a create-only apply of a new name did not create")
	}
	other := project("web")
	other.Spec = &tenancyv1alpha1.ProjectSpec{DisplayName: "someone else's"}
	_, err := a.projects.ApplyProject(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: other, CreateOnly: true}))
	wantCode(t, err, connect.CodeAlreadyExists, "a create-only apply of a name taken")
	got := must[tenancyv1alpha1.GetProjectResponse](t)(a.projects.GetProject(ctx, getProject("web")))
	if got.GetProject().GetMetadata().GetResourceVersion() != created.GetProject().GetMetadata().GetResourceVersion() {
		t.Errorf("a refused create-only apply wrote: %v", got.GetProject())
	}
	versioned := project("other")
	versioned.Metadata.ResourceVersion = 1
	_, err = a.projects.ApplyProject(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: versioned, CreateOnly: true}))
	wantCode(t, err, connect.CodeInvalidArgument, "a create-only apply with a resource version")
}

// TestCreateOnlyDeniesBeforeSayingAnObjectExists: a caller who may not
// write a kind is refused for that, by name, whether the object exists or
// not, before anything says the name is taken.
func TestCreateOnlyDeniesBeforeSayingAnObjectExists(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	w := e.as(workstream)
	var msgs []string
	for _, name := range []string{admin, "absent"} {
		_, err := w.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
			ServiceToken: serviceToken(name, iamv1alpha1.Role_ROLE_WORKSTREAM, ""), CreateOnly: true,
		}))
		wantCode(t, err, connect.CodePermissionDenied, "a workstream token creating a token named "+name)
		msgs = append(msgs, messageWithout(err, name))
	}
	if msgs[0] != msgs[1] {
		t.Errorf("the refusals tell an existing name from an absent one: %q, %q", msgs[0], msgs[1])
	}
}

// messageWithout is err's message with name taken out, to compare two
// refusals that differ only by the name each request named.
func messageWithout(err error, name string) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return fmt.Sprint(err)
	}
	return strings.ReplaceAll(ce.Message(), fmt.Sprintf("%q", name), "NAME")
}

func TestWatch(t *testing.T) {
	e := newEnv(t, func(c *apiserver.Config) { c.BookmarkInterval = 30 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setup()
	a := e.as(admin)

	list := must[secretsv1alpha1.ListSecretsResponse](t)(a.secrets.ListSecrets(ctx, connect.NewRequest(&secretsv1alpha1.ListSecretsRequest{Project: "web"})))
	// Opened in the background: the call returns only once the response
	// begins, which the opening bookmark is for.
	events := make(chan *secretsv1alpha1.WatchSecretsResponse, 16)
	ended := make(chan error, 1)
	go func() {
		defer close(events)
		stream, err := a.secrets.WatchSecrets(ctx, connect.NewRequest(&secretsv1alpha1.WatchSecretsRequest{Project: "web", SinceRevision: list.GetRevision()}))
		if err != nil {
			ended <- err
			return
		}
		for stream.Receive() {
			events <- stream.Msg()
		}
		ended <- stream.Err()
	}()
	next := func() *secretsv1alpha1.WatchSecretsResponse {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch ended: %v", <-ended)
			}
			return ev
		case <-time.After(testutil.Wait):
			t.Fatal("no watch event")
		}
		return nil
	}

	if ev := next(); ev.GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK || ev.GetRevision() != list.GetRevision() {
		t.Fatalf("want an opening bookmark at %d, got %v at %d", list.GetRevision(), ev.GetType(), ev.GetRevision())
	}
	must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(secret("web", "api", map[string]string{"A": "1"}))))
	must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(secret("web", "api", map[string]string{"A": "2"}))))
	must[secretsv1alpha1.DeleteSecretResponse](t)(a.secrets.DeleteSecret(ctx, connect.NewRequest(&secretsv1alpha1.DeleteSecretRequest{Project: "web", Name: "api"})))
	for _, want := range []metav1.EventType{
		metav1.EventType_EVENT_TYPE_ADDED, metav1.EventType_EVENT_TYPE_MODIFIED, metav1.EventType_EVENT_TYPE_DELETED,
	} {
		ev := next()
		if ev.GetType() != want || ev.GetSecret().GetMetadata().GetName() != "api" {
			t.Fatalf("got %v %v, want %v", ev.GetType(), ev.GetSecret().GetMetadata().GetName(), want)
		}
	}
	last, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An idle watch sends a bookmark on the clock, at the revision it has
	// reached. Three tickers run: compaction, the garbage collection sweep,
	// and this watch.
	if err := e.clock.BlockUntilContext(ctx, 3); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30 * time.Second)
	if ev := next(); ev.GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK || ev.GetRevision() != last || ev.GetSecret() != nil {
		t.Errorf("want a bookmark at %d, got %v at %d", last, ev.GetType(), ev.GetRevision())
	}
}

func TestWatchReportsItsDeadline(t *testing.T) {
	e := newEnv(t)
	e.setup()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	stream, err := e.as(admin).secrets.WatchSecrets(ctx, connect.NewRequest(&secretsv1alpha1.WatchSecretsRequest{Project: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeDeadlineExceeded, "a watch that ran out of time")
}

func TestWatchFromCompactedRevision(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	rev, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	stream, err := e.as(admin).secrets.WatchSecrets(ctx, connect.NewRequest(&secretsv1alpha1.WatchSecretsRequest{Project: "web", SinceRevision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if stream.Msg().GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK {
			t.Fatalf("unexpected event %v", stream.Msg())
		}
	}
	wantCode(t, stream.Err(), connect.CodeOutOfRange, "watching from a compacted revision")
}

// TestCompactionKeepsTheRetention: the periodic compaction drops history
// beyond EventRetention, so a watch from before it is OUT_OF_RANGE, and one
// from within it is not.
func TestCompactionKeepsTheRetention(t *testing.T) {
	e := newEnv(t, func(c *apiserver.Config) { c.EventRetention = 2 })
	ctx := t.Context()
	e.setup()
	for i := range 3 {
		must[secretsv1alpha1.ApplySecretResponse](t)(e.as(admin).secrets.ApplySecret(ctx, applySecret(secret("web", fmt.Sprintf("s%d", i), map[string]string{"A": "x"}))))
	}
	rev, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	watchFrom := func(since int64) error {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		stream, err := e.as(admin).secrets.WatchSecrets(ctx, connect.NewRequest(&secretsv1alpha1.WatchSecretsRequest{Project: "web", SinceRevision: since}))
		if err != nil {
			return err
		}
		for stream.Receive() {
		}
		return stream.Err()
	}
	testutil.Eventually(t, "history compacted", func() (bool, string) {
		e.clock.Advance(10 * time.Minute) // the compaction interval
		err := watchFrom(1)
		return connect.CodeOf(err) == connect.CodeOutOfRange, fmt.Sprint(err)
	})
	if err := watchFrom(rev - 1); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("a watch within the retention: %v, want it to run until its deadline", err)
	}
}

// TestProjectDeletionDeletesItsContents: everything in a project is owned
// by it; deleting the project deletes its contents first, then the
// project.
func TestProjectDeletionDeletesItsContents(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	a := e.as(admin)
	p := must[tenancyv1alpha1.GetProjectResponse](t)(a.projects.GetProject(ctx, getProject("web"))).GetProject()
	if !slices.Contains(p.GetMetadata().GetFinalizers(), "tower.hangarlab.cloud/dependents") {
		t.Errorf("a project without the dependents finalizer: %v", p.GetMetadata().GetFinalizers())
	}
	s := must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(secret("web", "db", map[string]string{"A": "x"})))).GetSecret()
	refs := s.GetMetadata().GetOwnerReferences()
	if len(refs) != 1 || refs[0].GetKind() != "Project" || refs[0].GetName() != "web" || refs[0].GetUid() != p.GetMetadata().GetUid() {
		t.Errorf("a secret's owners: %v, want project web", refs)
	}
	// Held back by a finalizer of its own, the secret keeps the project
	// being deleted.
	held := secret("web", "held", map[string]string{"A": "x"})
	held.Metadata.Finalizers = []string{"tower.hangarlab.cloud/test"}
	must[secretsv1alpha1.ApplySecretResponse](t)(e.asSystem().secrets.ApplySecret(ctx, applySecret(held)))

	must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "web"})))
	testutil.Eventually(t, "the secret collected", func() (bool, string) {
		_, err := a.secrets.GetSecret(ctx, getSecret("web", "db"))
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
	e.waitSettled()
	got, err := a.projects.GetProject(ctx, getProject("web"))
	if err != nil || got.Msg.GetProject().GetMetadata().GetDeleteTime() == nil {
		t.Fatalf("the project while a secret in it stays: %v, %v", got, err)
	}
	// Nothing new goes into a project being deleted.
	_, err = a.secrets.ApplySecret(ctx, applySecret(secret("web", "late", map[string]string{"A": "x"})))
	wantCode(t, err, connect.CodeFailedPrecondition, "a secret in a project being deleted")

	if err := e.srv.RemoveFinalizer(ctx, registry.Secret, resource.Key{Project: "web", Name: "held"}, "tower.hangarlab.cloud/test"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the project goes once nothing is left in it", func() (bool, string) {
		_, err := a.projects.GetProject(ctx, getProject("web"))
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
	if objs, _, err := e.store.List(ctx, store.ListOptions{Kind: "Secret"}); err != nil || len(objs) != 0 {
		t.Errorf("secrets left after their project: %d, %v", len(objs), err)
	}
}

// TestAnApplyKeepsTheSystemsFinalizers: a plain apply by anyone but the
// system, which carries no finalizers, or others, keeps those the project
// was made with, so deleting it afterwards still deletes its contents
// first.
func TestAnApplyKeepsTheSystemsFinalizers(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	a := e.as(admin)
	must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(secret("web", "db", map[string]string{"A": "x"}))))
	for _, finalizers := range [][]string{nil, {"user/finalizer"}} {
		p := project("web")
		p.Metadata.Finalizers = finalizers
		p.Spec = &tenancyv1alpha1.ProjectSpec{DisplayName: fmt.Sprint(finalizers)}
		applied := must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p))).GetProject()
		stored := must[tenancyv1alpha1.GetProjectResponse](t)(a.projects.GetProject(ctx, getProject("web"))).GetProject()
		for _, got := range [][]string{applied.GetMetadata().GetFinalizers(), stored.GetMetadata().GetFinalizers()} {
			if !slices.Equal(got, []string{"tower.hangarlab.cloud/dependents"}) {
				t.Errorf("finalizers after an apply carrying %v: %v, want the dependents finalizer alone", finalizers, got)
			}
		}
	}
	must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "web"})))
	testutil.Eventually(t, "the project gone", func() (bool, string) {
		_, err := a.projects.GetProject(ctx, getProject("web"))
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
	if objs, _, err := e.store.List(ctx, store.ListOptions{Kind: "Secret"}); err != nil || len(objs) != 0 {
		t.Errorf("secrets left after their project: %d, %v", len(objs), err)
	}
}

// TestNothingGoesIntoAProjectThatIsNotThere: a project-scoped object in a
// project that does not exist is refused, whoever writes it.
func TestNothingGoesIntoAProjectThatIsNotThere(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	for who, c := range map[string]clients{"admin": e.as(admin), "root on the host": e.local(), "the system": e.asSystem()} {
		_, err := c.secrets.ApplySecret(ctx, applySecret(secret("ghost", "s", map[string]string{"A": "x"})))
		wantCode(t, err, connect.CodeFailedPrecondition, who+" writing a secret into a project that is not there")
	}
}

// TestStatusIsTheSystems: status is a subresource only the system writes;
// an apply never changes it.
func TestStatusIsTheSystems(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	status := &tenancyv1alpha1.UpdateProjectStatusRequest{Name: "web", Status: &tenancyv1alpha1.ProjectStatus{ObservedGeneration: 1}}
	for who, c := range map[string]clients{"admin": e.as(admin), "root on the host": e.local(), "workstream": e.as(workstream), "hostd": e.as(hostd)} {
		_, err := c.projects.UpdateProjectStatus(ctx, connect.NewRequest(status))
		wantCode(t, err, connect.CodePermissionDenied, who+" writing status")
	}
	written := must[tenancyv1alpha1.UpdateProjectStatusResponse](t)(e.asSystem().projects.UpdateProjectStatus(ctx, connect.NewRequest(status))).GetProject()
	if written.GetStatus().GetObservedGeneration() != 1 {
		t.Fatalf("the system's status: %v", written.GetStatus())
	}
	p := project("web")
	p.Status = &tenancyv1alpha1.ProjectStatus{ObservedGeneration: 9}
	p.Spec = &tenancyv1alpha1.ProjectSpec{DisplayName: "Web"}
	applied := must[tenancyv1alpha1.ApplyProjectResponse](t)(e.as(admin).projects.ApplyProject(ctx, applyProject(p))).GetProject()
	if applied.GetStatus().GetObservedGeneration() != 1 {
		t.Errorf("an apply wrote status: %v", applied.GetStatus())
	}
}
