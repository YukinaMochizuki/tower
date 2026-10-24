package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// syncBuffer is a log the apiserver writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type listeners struct {
	public, admin string
	log           *syncBuffer
}

// start runs the apiserver as main does, with the admin socket, where uid
// is root. opts change the flags before it starts.
func start(t *testing.T, uid uint32, opts ...func(*flags)) listeners {
	t.Helper()
	old := adminUID
	adminUID = uid
	t.Cleanup(func() { adminUID = old })
	dir := t.TempDir()
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := port.Addr().String()
	_ = port.Close()
	// Unix socket paths are short; t.TempDir can be too long for one.
	sockDir, err := os.MkdirTemp("", "tower")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	l := listeners{public: "http://" + addr, admin: filepath.Join(sockDir, "admin.sock"), log: &syncBuffer{}}
	f := flags{db: filepath.Join(dir, "tower.db"), listen: addr, adminSocket: l.admin}
	for _, o := range opts {
		o(&f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, f, slog.New(slog.NewTextHandler(l.log, nil))) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	testutil.Eventually(t, "the apiserver started", func() (bool, string) {
		if _, err := os.Stat(l.admin); err != nil {
			return false, l.log.String()
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false, l.log.String()
		}
		_ = c.Close()
		return true, ""
	})
	return l
}

func issuer(hc connect.HTTPClient, base string, header http.Header) iamv1alpha1connect.ServiceTokenIssuerServiceClient {
	return iamv1alpha1connect.NewServiceTokenIssuerServiceClient(hc, base, connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				for k, v := range header {
					req.Header()[k] = v
				}
				return next(ctx, req)
			}
		})))
}

// mint asks for an admin token named mallory, the first thing root on the
// host does, and returns its secret.
func mint(c iamv1alpha1connect.ServiceTokenIssuerServiceClient) (string, error) {
	res, err := c.CreateServiceToken(context.Background(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Name: "mallory", Role: iamv1alpha1.Role_ROLE_ADMIN,
	}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetSecret(), nil
}

// TestAdminSocketIsRootOnTheHost: root on the host makes the first admin
// token on the admin socket, which is its alone, and the token's secret then
// works on the public listener.
func TestAdminSocketIsRootOnTheHost(t *testing.T) {
	l := start(t, uint32(os.Getuid()))
	if info, err := os.Stat(l.admin); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("admin socket %v, %v; want mode 0600", info.Mode(), err)
	}
	secret, err := mint(issuer(client.UnixSocket(l.admin), client.UnixSocketURL, nil))
	if err != nil {
		t.Fatalf("root on the host, with no token: %v", err)
	}
	if got := l.log.String(); !strings.Contains(got, `msg="admin socket request" method=POST path=/tower.iam.v1alpha1.ServiceTokenIssuerService/CreateServiceToken uid=`) ||
		!strings.Contains(got, "principal=local:root@") {
		t.Errorf("the request is not in the log as root on the host:\n%s", got)
	}
	tokens := iamv1alpha1connect.NewServiceTokenServiceClient(http.DefaultClient, l.public, connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+secret)
				return next(ctx, req)
			}
		})))
	got, err := tokens.GetServiceToken(context.Background(), connect.NewRequest(&iamv1alpha1.GetServiceTokenRequest{Name: "mallory"}))
	if err != nil || got.Msg.GetServiceToken().GetSpec().GetRole() != iamv1alpha1.Role_ROLE_ADMIN {
		t.Fatalf("the admin token on the public listener: %v, %v", got, err)
	}
}

func TestAdminSocketRefusesOtherUIDs(t *testing.T) {
	l := start(t, uint32(os.Getuid())+1)
	_, err := mint(issuer(client.UnixSocket(l.admin), client.UnixSocketURL, nil))
	if connect.CodeOf(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "is not root") {
		t.Fatalf("a uid that is not root: %v; want unauthenticated", err)
	}
	if got := l.log.String(); !strings.Contains(got, `msg="admin socket request refused"`) || !strings.Contains(got, fmt.Sprintf("uid=%d", os.Getuid())) {
		t.Errorf("the refusal is not in the log with its uid:\n%s", got)
	}
}

// TestNoOtherListenerIsRoot: whatever a caller sends, the public listener
// never acts as root on the host, even for the uid the admin socket
// accepts, in dev mode or not.
func TestNoOtherListenerIsRoot(t *testing.T) {
	for _, dev := range []bool{false, true} {
		t.Run(fmt.Sprintf("dev %t", dev), func(t *testing.T) {
			l := start(t, uint32(os.Getuid()), func(f *flags) { f.dev = dev })
			for _, tt := range []struct {
				name   string
				header http.Header
			}{
				{"no credentials", nil},
				{"as the principal's name", http.Header{auth.DevPrincipalHeader: {"local:root@n1"}}},
				{"as the system", http.Header{auth.DevPrincipalHeader: {"system:apiserver"}}},
				{"as a token that is not there", http.Header{auth.DevPrincipalHeader: {"serviceToken:mallory"}}},
				{"with a secret that is not one", http.Header{"Authorization": {"Bearer twr_nope"}}},
			} {
				t.Run(tt.name, func(t *testing.T) {
					_, err := mint(issuer(http.DefaultClient, l.public, tt.header))
					if code := connect.CodeOf(err); code != connect.CodeUnauthenticated && code != connect.CodePermissionDenied {
						t.Fatalf("minted an admin token: %v", err)
					}
				})
			}
			if strings.Contains(l.log.String(), "local:") {
				t.Errorf("a principal of type local outside the admin socket:\n%s", l.log.String())
			}
		})
	}
}

// TestDevTrustsTheHeaderOnlyInDev: the dev header names a caller only with
// --dev.
func TestDevTrustsTheHeaderOnlyInDev(t *testing.T) {
	for _, dev := range []bool{false, true} {
		l := start(t, uint32(os.Getuid()), func(f *flags) { f.dev = dev })
		if _, err := mint(issuer(client.UnixSocket(l.admin), client.UnixSocketURL, nil)); err != nil {
			t.Fatal(err)
		}
		tokens := iamv1alpha1connect.NewServiceTokenServiceClient(http.DefaultClient, l.public)
		req := connect.NewRequest(&iamv1alpha1.GetServiceTokenRequest{Name: "mallory"})
		req.Header().Set(auth.DevPrincipalHeader, "serviceToken:mallory")
		_, err := tokens.GetServiceToken(context.Background(), req)
		if dev && err != nil {
			t.Errorf("--dev: the dev header as an admin token: %v", err)
		}
		if !dev && connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("without --dev: the dev header got %v; want unauthenticated", err)
		}
	}
}

func TestHealthz(t *testing.T) {
	l := start(t, uint32(os.Getuid()))
	resp, err := http.Get(l.public + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
}

// TestDevOnlyOnALoopbackIP: --dev listens on a loopback IP, not on
// localhost, which a hosts file can point anywhere, nor anywhere else.
func TestDevOnlyOnALoopbackIP(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.1.2.3:8080", "[::1]:8080"} {
		if err := checkDev(flags{dev: true, listen: addr}); err != nil {
			t.Errorf("--dev on %s: %v", addr, err)
		}
	}
	for _, addr := range []string{"localhost:8080", "LOCALHOST:8080", "localhost.:8080", "0.0.0.0:8080", "[::]:8080", ":8080", "10.0.0.1:8080", "127.0.0.1"} {
		err := checkDev(flags{dev: true, listen: addr})
		if err == nil || !strings.Contains(err.Error(), "loopback IP") {
			t.Errorf("--dev on %s: %v", addr, err)
		}
	}
	if err := checkDev(flags{listen: "0.0.0.0:8080"}); err != nil {
		t.Errorf("without --dev on any address: %v", err)
	}
}

// TestAKEKThatDoesNotOpenStopsStartup: an apiserver given another KEK than
// the one its stored secrets were sealed under, or none, does not start.
func TestAKEKThatDoesNotOpenStopsStartup(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "tower.db")
	kek := func(id string) string {
		path := filepath.Join(dir, id)
		if err := os.WriteFile(path, []byte(secrets.NewKEK(id).String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := kek("first")
	t.Run("the first run stores a secret", func(t *testing.T) {
		l := start(t, uint32(os.Getuid()), func(f *flags) { f.db, f.secretsKEK = db, first })
		local := client.New(client.UnixSocket(l.admin), client.UnixSocketURL)
		if _, err := local.Apply(t.Context(), registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := local.Apply(t.Context(), registry.Secret, &secretsv1alpha1.Secret{
			Metadata: &metav1.ObjectMeta{Project: "web", Name: "s"},
			Spec:     &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: map[string][]byte{"A": []byte("x")}},
		}); err != nil {
			t.Fatal(err)
		}
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, path := range map[string]string{"another KEK": kek("second"), "no KEK": ""} {
		// One that starts serves until the deadline, and then ends well.
		ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
		err := run(ctx, flags{db: db, listen: "127.0.0.1:0", secretsKEK: path}, log)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: started over a secret it cannot open: %v", name, err)
		}
	}
}

// TestAdminSocketPath: an admin socket path holding a file other than a
// socket is refused, and one holding a stale socket is replaced.
func TestAdminSocketPath(t *testing.T) {
	dir, err := os.MkdirTemp("", "tower")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(file, 0o600); err == nil {
		t.Error("a regular file was replaced by the socket")
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := listenUnix(sock, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Left behind, as by a crash: the listener's file stays.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	ln, err = listenUnix(sock, 0o600)
	if err != nil {
		t.Fatalf("a stale socket: %v", err)
	}
	_ = ln.Close()
}
