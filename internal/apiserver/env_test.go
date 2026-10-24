package apiserver_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1/secretsv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller/controllertest"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store/sqlite"
)

// The service tokens newEnv makes, one of each role.
const (
	admin      = "admin"
	workstream = "workstream"
	hostd      = "hostd"
)

// env is a running apiserver on a fake clock: the public API taking
// service tokens' secrets and the dev header, and the admin socket, where
// this process is root on the host.
type env struct {
	t     *testing.T
	srv   *apiserver.Server
	store *sqlite.Store
	clock *clockwork.FakeClock
	url   string
	// systemURL serves every caller as the system.
	systemURL string
	sock      string
	dbDir     string
}

type envOption func(*apiserver.Config)

// newEnv starts an apiserver, with a KEK and the store's credentials, and
// makes an admin, a workstream and a hostd token by apply, as the system.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dbDir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dbDir, "tower.db"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewFakeClock()
	cfg := apiserver.Config{Store: st, Clock: clock, Secrets: keys, Credentials: st}
	for _, o := range opts {
		o(&cfg)
	}
	srv := apiserver.New(cfg)
	if err := srv.PrepareSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	chain := auth.Chain{auth.Dev{}}
	if tokens, ok := srv.ServiceTokens(); ok {
		chain = append(auth.Chain{tokens}, chain...)
	}
	public := httptest.NewServer(srv.Handler(chain))
	system := httptest.NewServer(srv.Handler(auth.Chain{asSystem{}}))
	// Unix socket paths are short; t.TempDir can be too long for one.
	sockDir, err := os.MkdirTemp("", "tower")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir, "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	adminSock := &http.Server{
		Handler:     srv.Handler(auth.Chain{auth.LocalRoot{Host: "n1", UID: uint32(os.Getuid())}}),
		ConnContext: auth.AdminConnContext,
	}
	go func() { _ = adminSock.Serve(ln) }()
	t.Cleanup(func() {
		public.Close()
		system.Close()
		_ = adminSock.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server Run: %v", err)
		}
		_ = st.Close()
		_ = os.RemoveAll(sockDir)
	})
	e := &env{t: t, srv: srv, store: st, clock: clock, url: public.URL, systemURL: system.URL, sock: sock, dbDir: dbDir}
	for name, role := range map[string]iamv1alpha1.Role{admin: iamv1alpha1.Role_ROLE_ADMIN, workstream: iamv1alpha1.Role_ROLE_WORKSTREAM, hostd: iamv1alpha1.Role_ROLE_HOSTD} {
		target := ""
		if role == iamv1alpha1.Role_ROLE_HOSTD {
			target = "h1"
		}
		if _, err := e.asSystem().tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: serviceToken(name, role, target)})); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// waitSettled waits until the server's garbage collector has finished
// reacting to every write so far (see controllertest.WaitSettled).
func (e *env) waitSettled() []controllertest.Waiting {
	e.t.Helper()
	return controllertest.WaitSettled(e.t, e.store.Revision, e.srv.ControllerStatus)
}

// clients are typed clients of every service, calling as one caller.
type clients struct {
	projects tenancyv1alpha1connect.ProjectServiceClient
	secrets  secretsv1alpha1connect.SecretServiceClient
	tokens   iamv1alpha1connect.ServiceTokenServiceClient
	issuer   iamv1alpha1connect.ServiceTokenIssuerServiceClient
	generic  *client.Client
}

func newClients(hc connect.HTTPClient, base string) clients {
	return clients{
		projects: tenancyv1alpha1connect.NewProjectServiceClient(hc, base),
		secrets:  secretsv1alpha1connect.NewSecretServiceClient(hc, base),
		tokens:   iamv1alpha1connect.NewServiceTokenServiceClient(hc, base),
		issuer:   iamv1alpha1connect.NewServiceTokenIssuerServiceClient(hc, base),
		generic:  client.New(hc, base),
	}
}

// as calls as the service token of the name through the dev header, as
// its ServiceToken's role allows; "" calls with no credentials.
func (e *env) as(name string) clients {
	if name == "" {
		return newClients(http.DefaultClient, e.url)
	}
	return newClients(withHeader(auth.DevPrincipalHeader, "serviceToken:"+name), e.url)
}

// withToken calls with a service token's secret.
func (e *env) withToken(secret string) clients {
	return newClients(withHeader("Authorization", "Bearer "+secret), e.url)
}

// local calls as root on the host, over the admin socket.
func (e *env) local() clients {
	return newClients(client.UnixSocket(e.sock), client.UnixSocketURL)
}

// asSystem calls as the apiserver itself does.
func (e *env) asSystem() clients { return newClients(http.DefaultClient, e.systemURL) }

type asSystem struct{}

func (asSystem) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.SystemPrincipal, true, nil
}

func withHeader(name, value string) *http.Client {
	return &http.Client{Transport: header{name, value}}
}

type header struct{ name, value string }

func (h header) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

// authenticates reports whether a secret authenticates, whatever its role
// then allows: an unauthenticated call is refused before authorization.
func (e *env) authenticates(secret string) (bool, error) {
	_, err := e.withToken(secret).projects.ListProjects(context.Background(), connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{}))
	switch {
	case err == nil, connect.CodeOf(err) == connect.CodePermissionDenied:
		return true, nil
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		return false, nil
	}
	return false, err
}

// setup makes project "web" as the admin token.
func (e *env) setup() {
	e.t.Helper()
	if _, err := e.as(admin).projects.ApplyProject(context.Background(), connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: project("web")})); err != nil {
		e.t.Fatalf("creating project web: %v", err)
	}
}

func project(name string) *tenancyv1alpha1.Project {
	return &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: name}}
}

func serviceToken(name string, role iamv1alpha1.Role, target string) *iamv1alpha1.ServiceToken {
	return &iamv1alpha1.ServiceToken{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &iamv1alpha1.ServiceTokenSpec{Role: role, Target: target}}
}

// secret is an opaque secret in the project, holding data.
func secret(projectName, name string, data map[string]string) *secretsv1alpha1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: projectName, Name: name},
		Spec:     &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: d},
	}
}

func applySecret(s *secretsv1alpha1.Secret) *connect.Request[secretsv1alpha1.ApplySecretRequest] {
	return connect.NewRequest(&secretsv1alpha1.ApplySecretRequest{Secret: s})
}

func getSecret(projectName, name string) *connect.Request[secretsv1alpha1.GetSecretRequest] {
	return connect.NewRequest(&secretsv1alpha1.GetSecretRequest{Project: projectName, Name: name})
}

func createToken(name string, role iamv1alpha1.Role, target string) *connect.Request[iamv1alpha1.CreateServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{Name: name, Role: role, Target: target})
}

func rotateToken(name string) *connect.Request[iamv1alpha1.RotateServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{Name: name})
}

func getToken(name string) *connect.Request[iamv1alpha1.GetServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.GetServiceTokenRequest{Name: name})
}

func deleteToken(name string) *connect.Request[iamv1alpha1.DeleteServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Name: name})
}

// must returns a call's message, failing the test on its error.
func must[T any](t *testing.T) func(*connect.Response[T], error) *T {
	return func(res *connect.Response[T], err error) *T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
}

func wantCode(t *testing.T, err error, want connect.Code, what string) {
	t.Helper()
	if got := connect.CodeOf(err); err == nil || got != want {
		t.Errorf("%s: got %v (%v), want %v", what, got, err, want)
	}
}
