package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
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
		var err error
		switch r.Kind {
		case registry.Project.Kind:
			_, err = c.projects.UpdateProjectStatus(ctx, connect.NewRequest(&tenancyv1alpha1.UpdateProjectStatusRequest{Name: name, Status: &tenancyv1alpha1.ProjectStatus{}}))
		case registry.Secret.Kind:
			_, err = c.secrets.UpdateSecretStatus(ctx, connect.NewRequest(&secretsv1alpha1.UpdateSecretStatusRequest{Project: "web", Name: name, Status: &secretsv1alpha1.SecretStatus{}}))
		case registry.ServiceToken.Kind:
			_, err = c.tokens.UpdateServiceTokenStatus(ctx, connect.NewRequest(&iamv1alpha1.UpdateServiceTokenStatusRequest{Name: name, Status: &iamv1alpha1.ServiceTokenStatus{}}))
		default:
			panic("no status call for " + r.Kind)
		}
		return err
	}
	panic("no call for " + string(verb))
}

// sample is an object of kind r, as the fixtures of TestEveryRolesVerbs
// have it, so an apply of it allowed changes nothing.
func sample(r registry.Resource, name string) proto.Message {
	switch r.Kind {
	case registry.Project.Kind:
		return project(name)
	case registry.Secret.Kind:
		return secret("web", name, map[string]string{"A": "x"})
	case registry.ServiceToken.Kind:
		return serviceToken(name, iamv1alpha1.Role_ROLE_WORKSTREAM, "")
	}
	panic("no sample of " + r.Kind)
}

// TestEveryRolesVerbs: every caller may use exactly the verbs its role
// lists on each kind, and is refused every other, on an object that exists
// and on one that does not alike; a refusal says only what the request
// named. A hostd token is authenticated, and refused everything.
func TestEveryRolesVerbs(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	must[secretsv1alpha1.ApplySecretResponse](t)(e.as(admin).secrets.ApplySecret(ctx, applySecret(secret("web", "s", map[string]string{"A": "x"}))))
	adminSecret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("admin2", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()
	hostdSecret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("hostd2", iamv1alpha1.Role_ROLE_HOSTD, "h1"))).GetSecret()
	if ok, err := e.authenticates(hostdSecret); !ok || err != nil {
		t.Fatalf("a hostd token's secret does not authenticate: %v", err)
	}
	// The existing object of each kind, which sample matches.
	existing := map[string]string{registry.Project.Kind: "web", registry.Secret.Kind: "s", registry.ServiceToken.Kind: workstream}

	writes := []authz.Verb{authz.Get, authz.List, authz.Watch, authz.Apply, authz.Delete}
	every := map[string][]authz.Verb{registry.Project.Kind: writes, registry.Secret.Kind: writes, registry.ServiceToken.Kind: writes}
	for _, tc := range []struct {
		name    string
		c       clients
		allowed map[string][]authz.Verb
	}{
		{"admin, by its secret", e.withToken(adminSecret), every},
		{"admin, by the dev header", e.as(admin), every},
		{"root on the host", e.local(), every},
		{"workstream", e.as(workstream), map[string][]authz.Verb{registry.Project.Kind: writes, registry.Secret.Kind: writes}},
		{"hostd, by its secret", e.withToken(hostdSecret), nil},
		{"hostd, by the dev header", e.as(hostd), nil},
		{"a token that is not there", e.as("nobody"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range registry.All {
				for _, verb := range authz.Verbs {
					allowed := slices.Contains(tc.allowed[r.Kind], verb)
					var msgs []string
					for _, name := range []string{existing[r.Kind], "absent"} {
						if verb == authz.Delete && name != "absent" {
							continue // deleting what the others use is not the point
						}
						if verb == authz.Apply && name == "absent" && allowed {
							continue // an allowed apply would make it
						}
						err := call(ctx, tc.c, r, verb, name)
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
					if len(msgs) == 2 && msgs[0] != msgs[1] {
						t.Errorf("%s %s: the refusals tell an existing object from an absent one: %q, %q", verb, r.Plural, msgs[0], msgs[1])
					}
				}
			}
		})
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
