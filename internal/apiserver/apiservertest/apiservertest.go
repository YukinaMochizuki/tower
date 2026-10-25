// Package apiservertest runs a real apiserver for tests outside the
// apiserver package: a SQLite store in a temporary directory, the public API
// taking service tokens' secrets and, as in dev auth, the dev header, a
// listener that serves everyone as the system, and the admin socket,
// accepting this process as root on the host.
package apiservertest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller/controllertest"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/store/sqlite"
)

const (
	// Host is the host root on the admin socket is root of.
	Host = "n1"
	// Admin is an admin token, which Start makes; As(Admin) calls as it.
	Admin = "admin"
	// Project is what Setup creates.
	Project = "web"
)

// Env is a running apiserver.
type Env struct {
	t *testing.T
	// URL is the public API, which takes service tokens' secrets and
	// trusts auth.DevPrincipalHeader.
	URL string
	// SystemURL serves every caller as the System principal, as the
	// apiserver's own controllers act; tests use it to write what only the
	// system may, such as a status.
	SystemURL string
	// AdminSocket is the admin socket, where this process is root on Host.
	AdminSocket string
	Store       store.Store
	// Credentials is the store's credentials, which the apiserver keeps
	// service tokens' secrets in.
	Credentials store.Credentials
	// Server is the apiserver, for what tests call on it directly, such as
	// OpenSecret.
	Server *apiserver.Server
}

// WaitSettled waits until the apiserver's garbage collector, and the
// controllers others reports, have finished reacting to every write so far,
// and returns the keys still waiting on something outside them (see
// controllertest.WaitSettled). Controllers following the API see how far
// they have come by its bookmarks, which Start sends often unless
// configure says otherwise.
func (e *Env) WaitSettled(others ...controllertest.Statuses) []controllertest.Waiting {
	e.t.Helper()
	return controllertest.WaitSettled(e.t, e.Store.Revision, append([]controllertest.Statuses{e.Server.ControllerStatus}, others...)...)
}

// Start runs an apiserver until the test ends, with a key-encryption key
// for secrets and the admin token Admin. configure may adjust its config;
// the store, the credentials and the KEK are already set.
func Start(t *testing.T, configure func(*apiserver.Config)) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "tower.db"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := apiserver.Config{Store: st, Credentials: st, Secrets: keys, BookmarkInterval: 10 * time.Millisecond, Resolver: devcontainer.Fake{}}
	if configure != nil {
		configure(&cfg)
	}
	srv := apiserver.New(cfg)
	if err := srv.PrepareSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	tokens, ok := srv.ServiceTokens()
	if !ok {
		t.Fatal("the apiserver keeps no credentials")
	}
	public := httptest.NewServer(srv.Handler(auth.Chain{tokens, auth.Dev{}}))
	system := httptest.NewServer(srv.Handler(auth.Chain{asSystem{}}))
	// Unix socket paths are short; t.TempDir can be too long for one.
	dir, err := os.MkdirTemp("", "tower")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	admin := &http.Server{
		Handler:     srv.Handler(auth.Chain{auth.LocalRoot{Host: Host, UID: uint32(os.Getuid())}}),
		ConnContext: auth.AdminConnContext,
	}
	go func() { _ = admin.Serve(ln) }()
	t.Cleanup(func() {
		public.Close()
		system.Close()
		_ = admin.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("apiserver: %v", err)
		}
		_ = st.Close()
		_ = os.RemoveAll(dir)
	})
	e := &Env{t: t, URL: public.URL, SystemURL: system.URL, AdminSocket: sock, Store: st, Credentials: st, Server: srv}
	e.CreateToken(Admin, iamv1alpha1.Role_ROLE_ADMIN, "")
	return e
}

// CreateToken makes a service token of the role, and of the target for
// ROLE_HOSTD, as root on the host does through the admin socket, and
// returns its secret.
func (e *Env) CreateToken(name string, role iamv1alpha1.Role, target string) string {
	e.t.Helper()
	res, err := e.LocalIssuer().CreateServiceToken(context.Background(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Name: name, Role: role, Target: target,
	}))
	if err != nil {
		e.t.Fatalf("creating service token %s: %v", name, err)
	}
	return res.Msg.GetSecret()
}

// AsToken makes a service token of the role, as CreateToken does, and
// returns a client that calls with its secret.
func (e *Env) AsToken(name string, role iamv1alpha1.Role, target string, opts ...connect.ClientOption) *client.Client {
	e.t.Helper()
	return e.WithSecret(e.CreateToken(name, role, target), opts...)
}

// WithSecret returns a client that calls the public API with a service
// token's secret.
func (e *Env) WithSecret(secret string, opts ...connect.ClientOption) *client.Client {
	return client.New(Bearer(secret), e.URL, opts...)
}

// Issuer returns a ServiceTokenIssuerService client that calls with the
// secret.
func (e *Env) Issuer(secret string) iamv1alpha1connect.ServiceTokenIssuerServiceClient {
	return iamv1alpha1connect.NewServiceTokenIssuerServiceClient(Bearer(secret), e.URL)
}

// AsLocal returns a client acting as root on the host, over the admin
// socket.
func (e *Env) AsLocal() *client.Client {
	return client.New(client.UnixSocket(e.AdminSocket), client.UnixSocketURL)
}

// LocalIssuer returns a ServiceTokenIssuerService client acting as root on
// the host.
func (e *Env) LocalIssuer() iamv1alpha1connect.ServiceTokenIssuerServiceClient {
	return iamv1alpha1connect.NewServiceTokenIssuerServiceClient(client.UnixSocket(e.AdminSocket), client.UnixSocketURL)
}

// AsSystem returns a client acting as the System principal.
func (e *Env) AsSystem() *client.Client {
	return client.New(http.DefaultClient, e.SystemURL)
}

type asSystem struct{}

func (asSystem) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.SystemPrincipal, true, nil
}

// As returns a client acting as the service token of the name, through the
// dev header: it holds the role the token's ServiceToken gives it, and no
// secret is needed.
func (e *Env) As(name string, opts ...connect.ClientOption) *client.Client {
	return client.New(DevToken(name), e.URL, opts...)
}

// DevToken is an HTTP client that calls the public API as the service token
// of the name, through the dev header.
func DevToken(name string) *http.Client {
	return &http.Client{Transport: header{auth.DevPrincipalHeader, auth.Principal{Type: auth.ServiceToken, ID: name}.String()}}
}

// Bearer is an HTTP client that calls with a service token's secret.
func Bearer(secret string) *http.Client {
	return &http.Client{Transport: header{"Authorization", "Bearer " + secret}}
}

type header struct{ name, value string }

func (h header) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

// Setup creates Project as Admin.
func (e *Env) Setup() {
	e.t.Helper()
	if _, err := e.As(Admin).Apply(context.Background(), registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: Project},
	}); err != nil {
		e.t.Fatalf("creating the project: %v", err)
	}
}
