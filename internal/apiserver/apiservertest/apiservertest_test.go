package apiservertest_test

import (
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// TestEveryCallerTheHelpersMake: each helper calls as whom it says, with
// what that caller may do.
func TestEveryCallerTheHelpersMake(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	ctx := t.Context()
	project := client.Key{Name: apiservertest.Project}
	for what, c := range map[string]*client.Client{
		"the admin token":             env.As(apiservertest.Admin),
		"root on the host":            env.AsLocal(),
		"the system":                  env.AsSystem(),
		"a workstream token's secret": env.AsToken("ws", iamv1alpha1.Role_ROLE_WORKSTREAM, ""),
	} {
		if _, err := c.Get(ctx, registry.Project, project); err != nil {
			t.Errorf("%s reading a project: %v", what, err)
		}
	}
	hostd := env.AsToken("agent", iamv1alpha1.Role_ROLE_HOSTD, "h1")
	if _, err := hostd.Get(ctx, registry.Project, project); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a hostd token reading a project: %v, want permission denied", err)
	}
	if _, err := env.As("ws").Get(ctx, registry.ServiceToken, client.Key{Name: apiservertest.Admin}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a workstream token, by the dev header, reading a token: %v, want permission denied", err)
	}
	secret := env.CreateToken("issuer", iamv1alpha1.Role_ROLE_ADMIN, "")
	if _, err := env.Issuer(secret).RotateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{Name: "ws"})); err != nil {
		t.Errorf("an admin token's secret rotating a token: %v", err)
	}
	if _, err := env.LocalIssuer().RotateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{Name: "ws"})); err != nil {
		t.Errorf("root on the host rotating a token: %v", err)
	}
	if waiting := env.WaitSettled(); len(waiting) != 0 {
		t.Errorf("waiting: %v", waiting)
	}
}
