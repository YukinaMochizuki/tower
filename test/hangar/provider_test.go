//go:build hangar

// Package hangar runs the provider contract (internal/provider/
// providertest) against tower's hangar provider over a real hangar: one
// project of an endpoint the test is pointed at, which a hangar testbed or
// a host of its own serves (docs/runbook/testbed.md). The fake runs the
// same contract in CI. It runs from anywhere that reaches the endpoint,
// as any user: `make e2e-hangar`, with
//
//   - TOWER_HANGAR_ENDPOINT, the API's URL, and TOWER_HANGAR_PROJECT;
//   - TOWER_HANGAR_TOKEN_FILE, a file holding the project's service token;
//   - TOWER_HANGAR_IMAGE, a box image by digest that carries
//     test/testbed/guestbin in /usr/local/bin, and busybox;
//   - TOWER_HANGAR_PULL_SECRET_FILE, if the image's registry needs one, a
//     Docker config hangar pulls it with;
//   - for the mirrors and builds, which are skipped without them:
//     TOWER_HANGAR_SYSTEM_IMAGE, the system image hangar pulls;
//     TOWER_HANGAR_REGISTRY, tower's registry as the system container pushes
//     to it, with TOWER_HANGAR_REGISTRY_PLAIN_HTTP=1 for one served over
//     plain HTTP and TOWER_HANGAR_PUSH_LOGIN_FILE holding its user:password;
//     TOWER_HANGAR_REGISTRY_HOST, the name hangar pulls it by, if another;
//     TOWER_HANGAR_REMOTE, a repository the system container fetches and
//     pushes to, whose main branch has a Dockerfile at docker/Dockerfile
//     building from TOWER_HANGAR_IMAGE.
//
// What it makes it deletes as it ends; a network outlives it until the
// next run's sweep.
package hangar

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/streams"
	"github.com/YukinaMochizuki/tower/internal/system"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// config is the run's, from the environment.
type config struct {
	endpoint, project, token, image string
	pull                            []byte
	system, registry, registryHost  string
	plainHTTP                       bool
	push                            system.Login
	remote                          string
}

func load(t *testing.T) config {
	t.Helper()
	c := config{endpoint: os.Getenv("TOWER_HANGAR_ENDPOINT"), project: os.Getenv("TOWER_HANGAR_PROJECT"), image: os.Getenv("TOWER_HANGAR_IMAGE")}
	if c.endpoint == "" || c.project == "" || c.image == "" || os.Getenv("TOWER_HANGAR_TOKEN_FILE") == "" {
		t.Fatal("TOWER_HANGAR_ENDPOINT, TOWER_HANGAR_PROJECT, TOWER_HANGAR_TOKEN_FILE and TOWER_HANGAR_IMAGE are required (see the package's comment)")
	}
	token, err := os.ReadFile(os.Getenv("TOWER_HANGAR_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	c.token = strings.TrimSpace(string(token))
	if f := os.Getenv("TOWER_HANGAR_PULL_SECRET_FILE"); f != "" {
		if c.pull, err = os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	c.system, c.registry, c.registryHost = os.Getenv("TOWER_HANGAR_SYSTEM_IMAGE"), os.Getenv("TOWER_HANGAR_REGISTRY"), os.Getenv("TOWER_HANGAR_REGISTRY_HOST")
	c.plainHTTP, c.remote = os.Getenv("TOWER_HANGAR_REGISTRY_PLAIN_HTTP") == "1", os.Getenv("TOWER_HANGAR_REMOTE")
	if f := os.Getenv("TOWER_HANGAR_PUSH_LOGIN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		user, pass, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
		c.push = system.Login{Username: user, Password: pass}
	}
	return c
}

// signer signs the tickets of the world's streams.
var signer = func() *ticket.Signer {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return ticket.NewSigner(key)
}()

func world(t *testing.T) *hangartest.World {
	t.Helper()
	c := load(t)
	path, err := hangartest.BuildGuest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := hangartest.Options{
		API: hangar.NewConnect(c.endpoint, c.project, c.token, nil), Project: c.project, Guest: guest, PullSecret: c.pull, Image: c.image,
		Configure: func(cfg *hangar.Config) {
			cfg.SweepGrace = time.Minute
			cfg.TicketKey = signer.PublicKey()
			cfg.SystemImage, cfg.Registry, cfg.RegistryHost, cfg.RegistryPlainHTTP, cfg.PushLogin = c.system, c.registry, c.registryHost, c.plainHTTP, c.push
		},
		// A pull and a boot on a real hangar.
		Wait: 5 * time.Minute,
	}
	if c.system != "" && c.remote != "" {
		opts.Remote, opts.Branch, opts.RemoteDockerfile = c.remote, "main", "docker/Dockerfile"
	}
	return hangartest.New(t, opts)
}

// TestConformance holds the hangar provider, over a real hangar, to the
// provider contract.
func TestConformance(t *testing.T) {
	w := world(t)
	providertest.Run(t, w.Harness())
}

// TestAStreamReachesAnInstanceThroughHangar serves a page in an instance
// and reads it through the direct stream tower serves, on a ticket,
// relayed to hangar's port forward.
func TestAStreamReachesAnInstanceThroughHangar(t *testing.T) {
	c := load(t)
	w := world(t)
	in, _ := w.Harness().Start(t, "forward", &envv1alpha1.InstanceSpec{
		Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: c.image, Network: "net-a", Alias: "forward",
	})
	ctx := t.Context()
	if err := w.Target.WriteFile(ctx, in, "/tmp/www/index.html", 0o644, 0, strings.NewReader("hello from hangar\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Target.StartProcess(ctx, provider.ProcessRequest{Instance: in, Command: []string{"busybox", "httpd", "-f", "-p", "8080", "-h", "/tmp/www"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.Target.Streams())
	defer srv.Close()
	where, err := w.Target.Ticket(ctx, in, 8080)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(where.URL)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := signer.Issue(ticket.Claims{Target: hangartest.Target, Project: in.Project, Instance: in.Name, Port: 8080, Expires: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyWithin(t, time.Minute, "the page through the stream", func() (bool, string) {
		conn, err := streams.Dial(ctx, srv.URL+u.Path, tk)
		if err != nil {
			return false, err.Error()
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n"); err != nil {
			return false, err.Error()
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false, err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b) == "hello from hangar\n", fmt.Sprint(resp.Status, " ", string(b))
	})
}
