package apiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hostd"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// Bounds of EnvironmentService's calls.
const (
	// MaxReadFile is the most one ReadFile reads.
	MaxReadFile = 64 << 20
	// MaxWriteFile is the most one WriteFile writes.
	MaxWriteFile = 4 << 20
	// DefaultTicketTTL is how long a ticket lasts unless Config says.
	DefaultTicketTTL = 5 * time.Minute
	// maxEnvironmentMessage bounds a request: a WriteFile's chunk and the
	// rest of its message.
	maxEnvironmentMessage = MaxWriteFile + 64<<10
	// outputChunk is the most a response carries of a command's output or
	// a file.
	outputChunk = 256 << 10
	// timedOutCode is a command's exit code once killed past its timeout,
	// as timeout(1) exits.
	timedOutCode = 124
)

// environmentService serves EnvironmentService: workstream's data plane
// into its environments, through tower (docs/decisions/traffic.md).
type environmentService struct{ s *Server }

// environment is an environment a call names, found and running: its
// instance on its target, and what a call there needs.
type environment struct {
	key        resource.Key
	kind       string
	workstream string
	target     *targetsv1alpha1.Target
	ref        provider.Instance
	// p is the target's provider, or perr why there is none.
	p    provider.Provider
	perr error
	box  *envv1alpha1.Box
	// secretEnv is a privileged box's Secret's values, which every command
	// in it is given.
	secretEnv map[string]string
	// release ends the call's use of the environment.
	release func()
}

func (e *environment) String() string {
	return fmt.Sprintf("%s %q in project %q", e.kind, e.key.Name, e.key.Project)
}

// begin refuses whoever may not use environments, saying nothing of what
// the request names, and checks the environment's name.
func (h environmentService) begin(ctx context.Context, env *envv1alpha1.Environment) (authz.Caller, error) {
	c, err := h.s.begin(ctx)
	if err != nil {
		return c, err
	}
	if !authz.MayUseEnvironments(c) {
		return c, denied("%s may not use environments: only ROLE_WORKSTREAM does", c)
	}
	return c, checkEnvironment(env)
}

// held registers, for a stream, the decision it was let in under.
func (h environmentService) held(ctx context.Context) {
	auth.Recheck(ctx, func(ctx context.Context) error {
		c, err := h.s.begin(ctx)
		if err != nil {
			return err
		}
		if !authz.MayUseEnvironments(c) {
			return denied("%s may no longer use environments", c)
		}
		return nil
	})
}

func checkEnvironment(env *envv1alpha1.Environment) error {
	if err := ValidName(env.GetProject()); err != nil {
		return invalid("environment.project: %v", err)
	}
	name := env.GetBox() + env.GetMachine()
	if name == "" {
		return invalid("environment names a box or a machine")
	}
	if err := ValidName(name); err != nil {
		return invalid("environment: %v", err)
	}
	return nil
}

// open is find, refusing an environment whose target has no provider now.
func (h environmentService) open(ctx context.Context, env *envv1alpha1.Environment) (*environment, error) {
	e, err := h.find(ctx, env)
	if err != nil {
		return nil, err
	}
	if e.perr != nil {
		e.release()
		return nil, e.perr
	}
	return e, nil
}

// find finds the environment, counts this call as its use and, for a box
// tower stopped for idleness, starts it and waits until it is READY, within
// ctx. An environment stopped at workstream's request is refused. A box is
// in use, never idle, until the caller calls release.
func (h environmentService) find(ctx context.Context, env *envv1alpha1.Environment) (*environment, error) {
	release := func() {}
	if env.GetBox() != "" {
		// Before Wake: a box in use is not stopped while it wakes.
		release = h.s.beginUse(env.GetProject(), env.GetBox())
	}
	e, err := h.findIn(ctx, env)
	if err != nil {
		release()
		return nil, err
	}
	e.release = release
	return e, nil
}

func (h environmentService) findIn(ctx context.Context, env *envv1alpha1.Environment) (*environment, error) {
	s := h.s
	e := &environment{key: resource.Key{Project: env.GetProject()}}
	var target, instance string
	switch {
	case env.GetBox() != "":
		e.kind, e.key.Name = "box", env.GetBox()
		if err := s.Wake(ctx, e.key.Project, e.key.Name); err != nil {
			return nil, err
		}
		b, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, e.key)
		if err != nil {
			return nil, apiError(err)
		}
		if !ok || b.GetMetadata().GetDeleteTime() != nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", e))
		}
		e.box, e.workstream = b, b.GetSpec().GetWorkstream()
		target, instance = b.GetStatus().GetTarget(), b.GetStatus().GetInstance()
		if refs := b.GetSpec().GetPrivilegedSecrets(); len(refs) > 0 {
			values, err := openSecretValues(ctx, s, e.key.Project, refs)
			if err != nil {
				return nil, failedPrecondition("%s is privileged, and %s", e, err)
			}
			e.secretEnv = map[string]string{}
			for i, ref := range refs {
				e.secretEnv[ref.GetAs()] = string(values[i])
			}
		}
	default:
		e.kind, e.key.Name = "machine", env.GetMachine()
		m, ok, err := getTyped[*envv1alpha1.Machine](ctx, s, registry.Machine, e.key)
		if err != nil {
			return nil, apiError(err)
		}
		if !ok || m.GetMetadata().GetDeleteTime() != nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", e))
		}
		if m.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
			return nil, failedPrecondition("%s is stopped at workstream's request; set its run_state to start it", e)
		}
		if phase := m.GetStatus().GetPhase(); phase != envv1alpha1.MachinePhase_MACHINE_PHASE_READY {
			return nil, failedPrecondition("%s is not ready: it is %s", e, strings.TrimPrefix(phase.String(), "MACHINE_PHASE_"))
		}
		e.workstream = m.GetSpec().GetWorkstream()
		target, instance = m.GetStatus().GetTarget(), m.GetStatus().GetInstance()
	}
	if target == "" || instance == "" {
		return nil, failedPrecondition("%s has no instance on a target yet", e)
	}
	t, live, err := s.liveTarget(ctx, target)
	if err != nil {
		return nil, apiError(err)
	}
	if !live {
		return nil, failedPrecondition("%s's target %q is gone", e, target)
	}
	e.target = t
	e.ref = provider.Instance{Project: e.key.Project, Name: instance}
	if e.p, e.perr = s.providerFor(target); e.perr != nil {
		e.perr = connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's target %q: %w", e, target, e.perr))
	}
	return e, nil
}

// providerError is the API's error for a provider's, about e.
func providerError(e *environment, err error) error {
	var ce *connect.Error
	code := connect.CodeUnknown
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, hostd.ErrTooManyCalls), errors.Is(err, provider.ErrNoSpace):
		code = connect.CodeResourceExhausted
	case errors.Is(err, provider.ErrUnavailable):
		code = connect.CodeUnavailable
	case errors.Is(err, provider.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		code = connect.CodeNotFound
	case errors.Is(err, provider.ErrLeaseLost):
		code = connect.CodeAborted
	case errors.Is(err, provider.ErrFailed):
		code = connect.CodeFailedPrecondition
	}
	return connect.NewError(code, fmt.Errorf("%s: %w", e, err))
}

// output is a writer sending what a command writes, in pieces, as
// responses send makes; send is serialized, so stdout and stderr keep
// their order.
type output struct {
	mu   *sync.Mutex
	send func([]byte) error
}

func (o output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for len(p) > 0 {
		k := min(len(p), outputChunk)
		if err := o.send(slices.Clone(p[:k])); err != nil {
			return n, err
		}
		n, p = n+k, p[k:]
	}
	return n, nil
}

// commandLine is the command a request names: argv, or a shell line run
// by /bin/sh -c.
func commandLine(argv *envv1alpha1.Argv, shell string) ([]string, error) {
	switch {
	case len(argv.GetArgs()) > 0:
		return argv.GetArgs(), nil
	case shell != "":
		return []string{"/bin/sh", "-c", shell}, nil
	}
	return nil, invalid("a command is argv or a shell line")
}

// commandEnv is the environment a command in e is given: what the caller
// gave, and a privileged box's Secret's values, which win.
func (e *environment) commandEnv(given map[string]string) map[string]string {
	env := maps.Clone(given)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, e.secretEnv)
	return env
}

func (h environmentService) Exec(ctx context.Context, req *connect.Request[envv1alpha1.ExecRequest], stream *connect.ServerStream[envv1alpha1.ExecResponse]) error {
	m := req.Msg
	c, err := h.begin(ctx, m.GetEnvironment())
	if err != nil {
		return err
	}
	h.held(ctx)
	command, err := commandLine(m.GetArgv(), m.GetShell())
	if err != nil {
		return err
	}
	var timeout time.Duration
	if m.Timeout != nil {
		if !m.GetTimeout().IsValid() || m.GetTimeout().AsDuration() <= 0 {
			return invalid("timeout must be a positive duration")
		}
		timeout = m.GetTimeout().AsDuration()
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return err
	}
	defer e.release()
	env := e.commandEnv(m.GetEnv())
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	var mu sync.Mutex
	send := func(r *envv1alpha1.ExecResponse) error { return stream.Send(r) }
	stdout := output{mu: &mu, send: func(b []byte) error {
		return send(&envv1alpha1.ExecResponse{Response: &envv1alpha1.ExecResponse_Stdout{Stdout: b}})
	}}
	stderr := output{mu: &mu, send: func(b []byte) error {
		return send(&envv1alpha1.ExecResponse{Response: &envv1alpha1.ExecResponse_Stderr{Stderr: b}})
	}}
	start := h.s.clock.Now()
	code, err := e.p.Exec(runCtx, provider.ExecRequest{Instance: e.ref, Command: command, Env: env, Dir: m.GetDir(), Stdout: stdout, Stderr: stderr})
	exit := &envv1alpha1.ExecExit{Code: int32(code)}
	if err != nil && timeout > 0 && ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		exit, err = &envv1alpha1.ExecExit{Code: timedOutCode, TimedOut: true}, nil
	}
	// Never the command's arguments, its environment's values or its
	// output: any may hold a secret.
	h.s.log.Info("exec", "caller", c.String(), "environment", e.String(), "program", command[0],
		"env_names", slices.Sorted(maps.Keys(m.GetEnv())), "privileged", e.secretEnv != nil,
		"exit_code", exit.GetCode(), "timed_out", exit.GetTimedOut(), "took", h.s.clock.Since(start), "failed", err != nil)
	if err != nil {
		return providerError(e, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return send(&envv1alpha1.ExecResponse{Response: &envv1alpha1.ExecResponse_Exit{Exit: exit}})
}

// errReadEnough stops a read once it has what was asked and a byte more.
var errReadEnough = errors.New("read enough")

// window is a writer passing on the bytes of [offset, offset+limit) of
// what is written, and stopping the write past them.
type window struct {
	skip, left int64
	more       bool
	out        output
}

func (w *window) Write(p []byte) (int, error) {
	n := len(p)
	if w.skip > 0 {
		k := min(int64(len(p)), w.skip)
		w.skip -= k
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if w.left == 0 {
		w.more = true
		return 0, errReadEnough
	}
	k := min(int64(len(p)), w.left)
	if _, err := w.out.Write(p[:k]); err != nil {
		return 0, err
	}
	w.left -= k
	if int64(len(p)) > k {
		w.more = true
		return 0, errReadEnough
	}
	return n, nil
}

func (h environmentService) ReadFile(ctx context.Context, req *connect.Request[envv1alpha1.ReadFileRequest], stream *connect.ServerStream[envv1alpha1.ReadFileResponse]) error {
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return err
	}
	h.held(ctx)
	if m.GetPath() == "" {
		return invalid("path is required")
	}
	limit := m.GetLimit()
	if m.GetOffset() < 0 || limit < 0 || limit > MaxReadFile {
		return invalid("offset is at least 0, and limit between 0 and %d bytes (0 is %d)", MaxReadFile, MaxReadFile)
	}
	if limit == 0 {
		limit = MaxReadFile
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return err
	}
	defer e.release()
	w := &window{skip: m.GetOffset(), left: limit, out: output{mu: &sync.Mutex{}, send: func(b []byte) error {
		return stream.Send(&envv1alpha1.ReadFileResponse{Response: &envv1alpha1.ReadFileResponse_Data{Data: b}})
	}}}
	if err := e.p.ReadFile(ctx, e.ref, m.GetPath(), w); err != nil && !errors.Is(err, errReadEnough) {
		return providerError(e, err)
	}
	return stream.Send(&envv1alpha1.ReadFileResponse{Response: &envv1alpha1.ReadFileResponse_End{End: &envv1alpha1.ReadFileEnd{Eof: !w.more}}})
}

func (h environmentService) WriteFile(ctx context.Context, req *connect.Request[envv1alpha1.WriteFileRequest]) (*connect.Response[envv1alpha1.WriteFileResponse], error) {
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return nil, err
	}
	if m.GetPath() == "" {
		return nil, invalid("path is required")
	}
	if len(m.GetData()) > MaxWriteFile {
		return nil, invalid("data is %d bytes; a call writes at most %d, so write a large file in chunks", len(m.GetData()), MaxWriteFile)
	}
	if m.GetOffset() < 0 {
		return nil, invalid("offset is at least 0")
	}
	mode := fs.FileMode(m.GetMode())
	if mode == 0 {
		mode = 0o644
	}
	if mode&^fs.ModePerm != 0 {
		return nil, invalid("mode is permission bits alone, at most 0777")
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	if err := e.p.WriteFile(ctx, e.ref, m.GetPath(), mode, m.GetOffset(), bytes.NewReader(m.GetData())); err != nil {
		return nil, providerError(e, err)
	}
	return connect.NewResponse(&envv1alpha1.WriteFileResponse{}), nil
}

func (h environmentService) CopyFile(ctx context.Context, req *connect.Request[envv1alpha1.CopyFileRequest]) (*connect.Response[envv1alpha1.CopyFileResponse], error) {
	m := req.Msg
	if _, err := h.begin(ctx, m.GetFrom()); err != nil {
		return nil, err
	}
	if err := checkEnvironment(m.GetTo()); err != nil {
		return nil, err
	}
	if m.GetFromPath() == "" || m.GetToPath() == "" {
		return nil, invalid("from_path and to_path are required")
	}
	if m.GetFrom().GetProject() != m.GetTo().GetProject() {
		return nil, invalid("a copy is between two environments of one project")
	}
	from, err := h.open(ctx, m.GetFrom())
	if err != nil {
		return nil, err
	}
	defer from.release()
	to, err := h.open(ctx, m.GetTo())
	if err != nil {
		return nil, err
	}
	if from.workstream != to.workstream {
		return nil, invalid("a copy is between two environments of one workstream; %s and %s are not", from, to)
	}
	if from.target.GetMetadata().GetName() != to.target.GetMetadata().GetName() {
		return nil, failedPrecondition("%s and %s run on different targets", from, to)
	}
	if err := from.p.Copy(ctx, from.ref, m.GetFromPath(), to.ref, m.GetToPath()); err != nil {
		return nil, providerError(from, err)
	}
	return connect.NewResponse(&envv1alpha1.CopyFileResponse{}), nil
}

// checkoutDir is where a box's checkout of the index is: the first at
// /workspace, each other at /repos/<repository>.
func checkoutDir(i int, repository string) string {
	if i == 0 {
		return "/workspace"
	}
	return "/repos/" + repository
}

// repositoryToken is the token a repository's credential holds, "" for one
// read anonymously.
func (s *Server) repositoryToken(ctx context.Context, repo *envv1alpha1.Repository) (string, error) {
	name := repo.GetSpec().GetCredentialSecret()
	if name == "" {
		return "", nil
	}
	data, err := s.OpenSecret(s.system(ctx), resource.Key{Project: repo.GetMetadata().GetProject(), Name: name})
	if err != nil {
		return "", failedPrecondition("repository %q's credential %q cannot be opened: %s", repo.GetMetadata().GetName(), name, messageOf(err))
	}
	return string(data["token"]), nil
}

func (h environmentService) FetchOrigin(ctx context.Context, req *connect.Request[envv1alpha1.FetchOriginRequest]) (*connect.Response[envv1alpha1.FetchOriginResponse], error) {
	s := h.s
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return nil, err
	}
	if m.GetEnvironment().GetBox() == "" {
		return nil, invalid("FetchOrigin is a box's: a machine has no checkouts")
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	res := &envv1alpha1.FetchOriginResponse{}
	target := e.target.GetMetadata().GetName()
	for i, co := range e.box.GetSpec().GetCheckouts() {
		key := resource.Key{Project: e.key.Project, Name: co.GetRepository()}
		repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, key)
		if err != nil {
			return nil, apiError(err)
		}
		if !ok || repo.GetMetadata().GetDeleteTime() != nil {
			return nil, failedPrecondition("%s checks out repository %q, which is gone", e, key.Name)
		}
		token, err := s.repositoryToken(ctx, repo)
		if err != nil {
			return nil, err
		}
		mirror := mirrorName(key.Project, key.Name)
		head, err := e.p.Fetch(ctx, provider.FetchRequest{Mirror: mirror, URL: repo.GetSpec().GetUrl(), Token: token, Branch: repo.GetSpec().GetDefaultBranch()})
		if err != nil {
			return nil, providerError(e, fmt.Errorf("fetching repository %q: %w", key.Name, err))
		}
		if err := s.recordFetch(ctx, controller.Key{Project: key.Project, Name: key.Name}, target, head); err != nil {
			return nil, apiError(err)
		}
		if err := e.p.UpdateOrigin(ctx, e.ref, mirror, checkoutDir(i, key.Name)); err != nil {
			return nil, providerError(e, fmt.Errorf("updating the origin of %s: %w", checkoutDir(i, key.Name), err))
		}
		res.Checkouts = append(res.Checkouts, &envv1alpha1.FetchedCheckout{Repository: key.Name, Head: head})
	}
	return connect.NewResponse(res), nil
}

func (h environmentService) Push(ctx context.Context, req *connect.Request[envv1alpha1.PushRequest]) (*connect.Response[envv1alpha1.PushResponse], error) {
	s := h.s
	m := req.Msg
	c, err := h.begin(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if m.GetEnvironment().GetBox() == "" {
		return nil, invalid("Push is a box's: a machine has no checkouts")
	}
	if err := checkBranch(m.GetBranch()); err != nil {
		return nil, err
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	i := slices.IndexFunc(e.box.GetSpec().GetCheckouts(), func(co *envv1alpha1.Checkout) bool { return co.GetRepository() == m.GetRepository() })
	if i < 0 {
		return nil, invalid("%s has no checkout of repository %q", e, m.GetRepository())
	}
	key := resource.Key{Project: e.key.Project, Name: m.GetRepository()}
	repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, key)
	if err != nil {
		return nil, apiError(err)
	}
	if !ok || repo.GetMetadata().GetDeleteTime() != nil {
		return nil, failedPrecondition("repository %q is gone", key.Name)
	}
	if repo.GetSpec().GetCredentialSecret() == "" {
		return nil, failedPrecondition("repository %q has no credential: tower pushes only with one", key.Name)
	}
	token, err := s.repositoryToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	err = e.p.Push(ctx, provider.PushRequest{
		Instance: e.ref, Dir: checkoutDir(i, key.Name), Mirror: mirrorName(key.Project, key.Name), URL: repo.GetSpec().GetUrl(),
		Token: token, Branch: m.GetBranch(), Lease: m.GetLease(),
	})
	s.log.Info("push", "caller", c.String(), "environment", e.String(), "repository", key.Name, "branch", m.GetBranch(),
		"lease", m.GetLease(), "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	return connect.NewResponse(&envv1alpha1.PushResponse{}), nil
}

// checkBranch refuses what is no branch name a push may take: no option,
// no refspec, no pattern.
func checkBranch(b string) error {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "refs/") ||
		strings.ContainsAny(b, " :~^?*[\\\x7f") || strings.Contains(b, "..") || strings.Contains(b, "@{") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") || strings.IndexFunc(b, func(r rune) bool { return r < 0x20 }) >= 0 {
		return invalid("branch %q is not a branch name", b)
	}
	return nil
}

func (h environmentService) IssueTicket(ctx context.Context, req *connect.Request[envv1alpha1.IssueTicketRequest]) (*connect.Response[envv1alpha1.IssueTicketResponse], error) {
	s := h.s
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return nil, err
	}
	if m.GetPort() < 1 || m.GetPort() > 65535 {
		return nil, invalid("port %d is not a port", m.GetPort())
	}
	e, err := h.find(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	if e.target.GetSpec().GetHangar() != nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			fmt.Errorf("%s runs on hangar target %q, which forwards no port yet: no direct stream to it can be ticketed", e, e.target.GetMetadata().GetName()))
	}
	if s.cfg.Tickets == nil {
		return nil, failedPrecondition("this tower has no ticket signing key, so it issues no tickets")
	}
	if e.perr != nil {
		return nil, e.perr
	}
	where, err := e.p.Ticket(ctx, e.ref, m.GetPort())
	if err != nil {
		return nil, providerError(e, err)
	}
	expires := s.clock.Now().Add(s.cfg.TicketTTL).Truncate(time.Second)
	if !where.Expires.IsZero() && where.Expires.Before(expires) {
		expires = where.Expires.Truncate(time.Second)
	}
	tk, err := s.cfg.Tickets.Issue(ticket.Claims{
		Target: e.target.GetMetadata().GetName(), Project: e.ref.Project, Instance: e.ref.Name, Port: m.GetPort(), Expires: expires,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&envv1alpha1.IssueTicketResponse{Url: where.URL, Ticket: tk, ExpireTime: timestamppb.New(expires)}), nil
}
