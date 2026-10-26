// Package cli is the tower command. It is built for agents as much as for
// people: it never prompts, every command can print JSON, the exit code
// says what went wrong (the Exit constants), and a secret never comes from
// the command line. Its structure and conventions are hangar's
// internal/cli (copied in part at hangar 5c1ca49), with no contexts or
// sign-in: the server and the token come from flags or the environment.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitConflict = 4
	ExitTimeout  = 5
)

// The environment the CLI reads.
const (
	// EnvServer is the API's URL, or unix:// and the admin socket's path
	// for root on tower's host.
	EnvServer = "TOWER_SERVER"
	// EnvToken is a service token's secret.
	EnvToken = "TOWER_TOKEN"
	// EnvTokenFile names a file holding it.
	EnvTokenFile = "TOWER_TOKEN_FILE"
	// EnvProject is the project of project-scoped kinds.
	EnvProject = "TOWER_PROJECT"
)

// IO is what a run reads and writes.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
	// Env looks up environment variables.
	Env func(string) string
}

// Options replace the outside world in tests; zero values use the real one.
type Options struct {
	HTTPClient *http.Client
	Now        func() time.Time
}

// Run runs the command line and returns the exit code.
func Run(ctx context.Context, args []string, stdio IO, opts Options) int {
	a := &app{io: stdio, env: stdio.Env, opts: opts}
	if a.env == nil {
		a.env = func(string) string { return "" }
	}
	if a.opts.HTTPClient == nil {
		a.opts.HTTPClient = http.DefaultClient
	}
	if a.opts.Now == nil {
		a.opts.Now = time.Now
	}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdio.In)
	root.SetOut(stdio.Out)
	root.SetErr(stdio.Err)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	// Cobra's own error for a command line that names no command, which
	// it gives no type to tell it by.
	if strings.HasPrefix(err.Error(), "unknown command ") {
		err = &exitError{code: ExitUsage, err: fmt.Errorf("%w\nrun tower --help for the commands", err)}
	}
	_, _ = fmt.Fprintln(stdio.Err, "error:", err)
	return exitCode(err)
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, args ...any) error {
	return &exitError{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return ExitNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return ExitConflict
	case connect.CodeDeadlineExceeded:
		return ExitTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ExitTimeout
	}
	return ExitError
}

type app struct {
	io   IO
	env  func(string) string
	opts Options

	server, tokenFile, project string
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "tower",
		Short: "Manage tower's resources",
		Long: `Manage tower's resources.

The server is --server or $` + EnvServer + `: the API's URL, or unix:// and the
admin socket's path for root on tower's host. The token is read from
--token-file, $` + EnvTokenFile + ` or $` + EnvToken + `, never from the command line;
the admin socket needs none.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.server, "server", "", "the API's URL, or unix:///path/admin.sock; $"+EnvServer+" unless set")
	root.PersistentFlags().StringVar(&a.tokenFile, "token-file", "", "file holding a service token's secret, mode 0600; $"+EnvTokenFile+" unless set")
	root.PersistentFlags().StringVarP(&a.project, "project", "p", "", "project for project-scoped kinds; $"+EnvProject+" unless set")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &exitError{code: ExitUsage, err: err} })
	root.AddCommand(a.applyCmd(), a.getCmd(), a.deleteCmd(), a.waitCmd(), a.tokenCmd(), a.secretCmd(), a.retryCmd(), a.targetCmd())
	return root
}

// endpoint returns the HTTP client and base URL that reach the server.
func (a *app) endpoint() (connect.HTTPClient, string, error) {
	server := a.server
	if server == "" {
		server = a.env(EnvServer)
	}
	if server == "" {
		return nil, "", usageErr("no server: pass --server or set %s", EnvServer)
	}
	if path, ok := strings.CutPrefix(server, "unix://"); ok {
		return client.UnixSocket(path), client.UnixSocketURL, nil
	}
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		return nil, "", usageErr("server %q is neither http(s):// nor unix://", server)
	}
	secret, err := a.secret()
	if err != nil {
		return nil, "", err
	}
	hc := a.opts.HTTPClient
	if secret != "" {
		hc = &http.Client{Transport: bearer{secret: secret, next: transport(hc)}, Timeout: hc.Timeout}
	}
	return hc, strings.TrimSuffix(server, "/"), nil
}

func transport(hc *http.Client) http.RoundTripper {
	if hc.Transport != nil {
		return hc.Transport
	}
	return http.DefaultTransport
}

// secret is the token's secret: from --token-file, $TOWER_TOKEN_FILE or
// $TOWER_TOKEN, "" with none.
func (a *app) secret() (string, error) {
	path := a.tokenFile
	if path == "" {
		path = a.env(EnvTokenFile)
	}
	if path == "" {
		return strings.TrimSpace(a.env(EnvToken)), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if who, fix, err := exposure(f); err != nil {
		return "", err
	} else if who != "" {
		return "", usageErr("%s holds a secret and is %s; run %s", path, who, fix)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

type bearer struct {
	secret string
	next   http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return b.next.RoundTrip(r)
}

// client returns an API client for the server.
func (a *app) client() (*client.Client, error) {
	hc, base, err := a.endpoint()
	if err != nil {
		return nil, err
	}
	return client.New(hc, base), nil
}

// projectFor returns the project to use for r, if r is project-scoped.
func (a *app) projectFor(r registry.Resource) (string, error) {
	if !r.ProjectScoped() {
		if a.project != "" {
			return "", usageErr("%s are not in projects; drop --project", r.Plural)
		}
		return "", nil
	}
	p := a.project
	if p == "" {
		p = a.env(EnvProject)
	}
	if p == "" {
		return "", usageErr("%s are project-scoped; pass --project or set %s", r.Plural, EnvProject)
	}
	return p, nil
}

func findKind(name string) (registry.Resource, error) {
	r, ok := registry.Find(name)
	if !ok {
		return registry.Resource{}, unknownKind(name)
	}
	return r, nil
}

// args checks the number of positional arguments, as a usage error.
func args(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min || (max >= 0 && len(args) > max) {
			return usageErr("usage: %s", cmd.UseLine())
		}
		return nil
	}
}
