package apiserver_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1/secretsv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// openWatch watches secrets in project, everywhere when it is empty, with
// c, and returns the events after the opening bookmark, and, once the watch
// ends, what it ended with.
func openWatch(t *testing.T, c secretsv1alpha1connect.SecretServiceClient, project string) (events <-chan *secretsv1alpha1.WatchSecretsResponse, ended <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := c.WatchSecrets(ctx, connect.NewRequest(&secretsv1alpha1.WatchSecretsRequest{Project: project}))
	if err != nil {
		t.Fatal(err)
	}
	// The opening bookmark: the watch is held from here on.
	if !stream.Receive() {
		t.Fatalf("the watch did not open: %v", stream.Err())
	}
	ev := make(chan *secretsv1alpha1.WatchSecretsResponse, 100)
	end := make(chan error, 1)
	go func() {
		for stream.Receive() {
			ev <- stream.Msg()
		}
		end <- stream.Err()
	}()
	return ev, end
}

// endsWith waits for a stream to end, a recheck after its revoke, and checks
// that it ended with code: a refusal its client can tell from a dropped
// connection.
func endsWith(t *testing.T, ended <-chan error, code connect.Code, what string) {
	t.Helper()
	var err error
	testutil.Eventually(t, what+": the stream ended", func() bool {
		select {
		case err = <-ended:
			return true
		default:
			return false
		}
	})
	wantCode(t, err, code, what)
}

// receivesAdded waits for a watch to tell of the secret name being added:
// it is still open.
func receivesAdded(t *testing.T, events <-chan *secretsv1alpha1.WatchSecretsResponse, ended <-chan error, name, what string) {
	t.Helper()
	testutil.Eventually(t, what+": an event of "+name, func() bool {
		for {
			select {
			case ev := <-events:
				if ev.GetType().String() == "EVENT_TYPE_ADDED" && ev.GetSecret().GetMetadata().GetName() == name {
					return true
				}
			case err := <-ended:
				t.Fatalf("%s ended: %v", what, err)
			default:
				return false
			}
		}
	})
}

// TestAWatchEndsWithItsServiceToken: a watch opened with a service token's
// secret ends, at the next recheck, once the token is deleted or its secret
// rotated; one whose caller stays authenticated, as through the dev header,
// ends once the token it names is gone and its role with it.
func TestAWatchEndsWithItsServiceToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	a := e.as(admin)
	deployer := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	ci := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("ci", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("named", iamv1alpha1.Role_ROLE_WORKSTREAM, "")))
	_, deployerEnded := openWatch(t, e.withToken(deployer).secrets, "web")
	ciEvents, ciEnded := openWatch(t, e.withToken(ci).secrets, "web")
	_, namedEnded := openWatch(t, e.as("named").secrets, "")

	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("deployer")))
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("named")))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, deployerEnded, connect.CodeUnauthenticated, "a watch whose token was deleted")
	endsWith(t, namedEnded, connect.CodePermissionDenied, "a watch by the name of a token deleted")
	must[secretsv1alpha1.ApplySecretResponse](t)(a.secrets.ApplySecret(ctx, applySecret(secret("web", "after-delete", map[string]string{"A": "x"}))))
	receivesAdded(t, ciEvents, ciEnded, "after-delete", "the other token's watch")

	must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("ci")))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, ciEnded, connect.CodeUnauthenticated, "a watch whose token was rotated")
}

// filler is a megabyte of random text.
func filler() string {
	b := make([]byte, 3<<18)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// TestARevokedStreamEndsThoughItsClientStoppedReading: a watch blocked
// writing to a client that reads nothing, and so in no read or write that
// would see its end, still ends at the recheck that refuses its caller.
func TestARevokedStreamEndsThoughItsClientStoppedReading(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	for _, how := range []string{"secret", "name"} {
		t.Run(how, func(t *testing.T) {
			secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("reader", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
			c := e.withToken(secret)
			if how == "name" {
				c = e.as("reader")
			}
			sctx, cancel := context.WithCancel(ctx)
			defer cancel()
			stream, err := c.projects.WatchProjects(sctx, connect.NewRequest(&tenancyv1alpha1.WatchProjectsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if !stream.Receive() {
				t.Fatalf("the watch did not open: %v", stream.Err())
			}
			// From here on the client reads nothing. Events of a megabyte
			// each, which no compression shrinks, fill what the connection
			// buffers, so the apiserver blocks writing the next one.
			for range 48 {
				p := project("big-" + how)
				p.Metadata.Annotations = map[string]string{"filler": filler()}
				must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, applyProject(p)))
			}
			if n := e.watches.Load(); n != 1 {
				t.Fatalf("%d watches open before the revoke, want 1", n)
			}
			must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("reader")))
			e.clock.Advance(auth.RecheckInterval)
			// Its writes are cut off EndGrace after the refusal.
			testutil.EventuallyWithin(t, auth.EndGrace+testutil.Wait, "the watch ended on the apiserver", func() bool { return e.watches.Load() == 0 })
			e.waitSettled()
		})
	}
}
