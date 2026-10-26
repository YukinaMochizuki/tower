package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// tower runs the provider of each hangar target itself (internal/provider/
// hangar): one hangar.Target per Target with spec.hangar, made from the
// target's spec and the Secrets it names, and made again when either
// changes. It acts on tower's API as the system, through the apiserver's
// own handler in memory, as hostd acts on it over the network.

// systemProject is tower's reserved project, which holds the Secrets of
// targets.
const systemProject = "system"

// HangarStreamPath is where tower serves direct streams to hangar
// targets' instances: HangarStreamPath<target>/<project>/<instance>/<port>.
const HangarStreamPath = "/streams/hangar/"

// hangarTargets are the hangar targets' providers this tower runs.
type hangarTargets struct {
	mu      sync.Mutex
	running map[string]*runningHangar
	// failed says why a target has no provider, by its name.
	failed map[string]string
	wg     sync.WaitGroup
}

// runningHangar is one hangar target's provider, running.
type runningHangar struct {
	// inputs is a hash of what it was made from.
	inputs string
	t      *hangar.Target
	cancel context.CancelFunc
	done   chan struct{}
}

// newHangarController follows Targets, and the Secrets of the reserved
// project they name, to run each hangar target's provider.
func (s *Server) newHangarController() *controller.Controller {
	return &controller.Controller{
		Name: "hangar-targets", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
		Reconciler: controller.ReconcilerFunc(s.reconcileHangarTarget),
		Watches: []controller.Watch{
			{Kind: registry.Target.Kind, Map: controller.Self},
			{Kind: registry.Secret.Kind, Map: s.hangarTargetsOfSecret, SkipResync: true},
		},
	}
}

// hangarTargetsOfSecret are the hangar targets naming a secret of the
// reserved project.
func (s *Server) hangarTargetsOfSecret(ctx context.Context, o store.Object) []controller.Key {
	if o.Project != systemProject {
		return nil
	}
	targets, err := listTyped[*targetsv1alpha1.Target](ctx, s, registry.Target, "")
	if err != nil {
		s.log.Warn("listing the targets of a secret failed", "secret", o.Name, "error", err)
		return nil
	}
	var keys []controller.Key
	for _, t := range targets {
		h := t.GetSpec().GetHangar()
		if h != nil && (h.GetCredentialSecret() == o.Name || h.GetRegistryPullSecret() == o.Name ||
			h.GetRegistryPushSecret() == o.Name || h.GetTailnetAuthSecret() == o.Name) {
			keys = append(keys, controller.Key{Name: t.GetMetadata().GetName()})
		}
	}
	return keys
}

// reconcileHangarTarget runs the target's provider as its spec and secrets
// say, making it again when they changed, and stops one whose target is
// gone or not hangar's.
func (s *Server) reconcileHangarTarget(ctx context.Context, key controller.Key) error {
	t, live, err := s.liveTarget(ctx, key.Name)
	if err != nil {
		return err
	}
	h := t.GetSpec().GetHangar()
	if !live || h == nil {
		s.stopHangar(key.Name, "")
		return nil
	}
	cfg, inputs, err := s.hangarConfig(ctx, t)
	if errors.Is(err, errSecretMissing) {
		s.stopHangar(key.Name, err.Error())
		// Read again when a secret it names is written.
		return nil
	}
	if err != nil {
		// Read again soon; what runs keeps running.
		return err
	}
	s.hangar.mu.Lock()
	cur := s.hangar.running[key.Name]
	s.hangar.mu.Unlock()
	if cur != nil && cur.inputs == inputs {
		return nil
	}
	s.stopHangar(key.Name, "")
	run := &runningHangar{inputs: inputs, t: hangar.New(cfg), done: make(chan struct{})}
	var rctx context.Context
	rctx, run.cancel = context.WithCancel(s.runCtx)
	s.hangar.mu.Lock()
	if s.hangar.running == nil {
		s.hangar.running = map[string]*runningHangar{}
	}
	s.hangar.running[key.Name] = run
	delete(s.hangar.failed, key.Name)
	s.hangar.wg.Add(1)
	s.hangar.mu.Unlock()
	if s.targetCtrl != nil {
		s.targetCtrl.Enqueue(controller.Key{Name: key.Name})
	}
	s.log.Info("running a hangar target's provider", "target", key.Name, "endpoint", h.GetEndpoint(), "project", h.GetProject())
	go func() {
		defer s.hangar.wg.Done()
		defer close(run.done)
		if err := run.t.Run(rctx); err != nil && rctx.Err() == nil {
			s.log.Error("a hangar target's provider stopped", "target", key.Name, "error", err)
		}
	}()
	return nil
}

// stopHangar stops a target's provider, if one runs, and keeps why it has
// none ("" for no reason to say).
func (s *Server) stopHangar(target, why string) {
	s.hangar.mu.Lock()
	run := s.hangar.running[target]
	delete(s.hangar.running, target)
	if s.hangar.failed == nil {
		s.hangar.failed = map[string]string{}
	}
	if why != "" {
		s.hangar.failed[target] = why
	} else {
		delete(s.hangar.failed, target)
	}
	s.hangar.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
	if s.targetCtrl != nil {
		s.targetCtrl.Enqueue(controller.Key{Name: target})
	}
}

// hangarConfig is the provider a hangar target's spec and secrets make,
// and a hash of what it is made from.
func (s *Server) hangarConfig(ctx context.Context, t *targetsv1alpha1.Target) (hangar.Config, string, error) {
	h := t.GetSpec().GetHangar()
	name := t.GetMetadata().GetName()
	cred, err := s.openTargetSecret(ctx, "credential", h.GetCredentialSecret())
	if err != nil {
		return hangar.Config{}, "", err
	}
	token := strings.TrimSpace(string(cred["token"]))
	if token == "" {
		return hangar.Config{}, "", fmt.Errorf("its credential %q holds no \"token\": %w", h.GetCredentialSecret(), errSecretMissing)
	}
	var pull []byte
	if p := h.GetRegistryPullSecret(); p != "" {
		data, err := s.openTargetSecret(ctx, "registry pull secret", p)
		if err != nil {
			return hangar.Config{}, "", err
		}
		if pull = data[".dockerconfigjson"]; len(pull) == 0 {
			return hangar.Config{}, "", fmt.Errorf("its registry pull secret %q holds no \".dockerconfigjson\": %w", p, errSecretMissing)
		}
	}
	var push system.Login
	if p := h.GetRegistryPushSecret(); p != "" {
		data, err := s.openTargetSecret(ctx, "registry push secret", p)
		if err != nil {
			return hangar.Config{}, "", err
		}
		push = system.Login{Username: string(data["username"]), Password: string(data["password"])}
		if push.Username == "" {
			return hangar.Config{}, "", fmt.Errorf("its registry push secret %q holds no \"username\": %w", p, errSecretMissing)
		}
	}
	var authKey string
	if a := h.GetTailnetAuthSecret(); a != "" {
		data, err := s.openTargetSecret(ctx, "tailnet auth secret", a)
		if err != nil {
			return hangar.Config{}, "", err
		}
		if authKey = strings.TrimSpace(string(data["authkey"])); authKey == "" {
			return hangar.Config{}, "", fmt.Errorf("its tailnet auth secret %q holds no \"authkey\": %w", a, errSecretMissing)
		}
	}
	sum := sha256.New()
	for _, part := range [][]byte{
		[]byte(h.GetEndpoint()), []byte(h.GetProject()), []byte(token), pull,
		[]byte(h.GetRegistryHost()), []byte(push.Username), []byte(push.Password), []byte(authKey),
	} {
		_, _ = fmt.Fprintf(sum, "%d:", len(part))
		_, _ = sum.Write(part)
	}
	api := s.cfg.HangarAPI
	if api == nil {
		api = func(endpoint, project, token string) hangar.API {
			return hangar.NewConnect(endpoint, project, token, nil)
		}
	}
	cfg := hangar.Config{
		Target: name, API: api(h.GetEndpoint(), h.GetProject(), token), Client: s.systemClient(), PullSecret: pull,
		Guest: s.cfg.TowerGuest, StreamPrefix: HangarStreamPath + name + "/", Clock: s.clock, Log: s.log,
		SystemImage: s.cfg.SystemImage, Registry: s.cfg.Registry, RegistryHost: h.GetRegistryHost(), RegistryPlainHTTP: s.cfg.RegistryPlainHTTP,
		PushLogin: push, TailnetAuthKey: authKey,
	}
	if s.cfg.Tickets != nil {
		cfg.TicketKey = s.cfg.Tickets.PublicKey()
	}
	if s.cfg.PublicURL != "" {
		cfg.StreamURL = strings.TrimSuffix(s.cfg.PublicURL, "/") + cfg.StreamPrefix
	}
	return cfg, hex.EncodeToString(sum.Sum(nil)), nil
}

// errSecretMissing is a target's secret that is not there, or not as it
// must be: waited for, until it is written.
var errSecretMissing = errors.New("the secret is missing")

// openTargetSecret opens a secret a target names, of the reserved
// project; one not there, or that no key opens, is errSecretMissing.
func (s *Server) openTargetSecret(ctx context.Context, what, name string) (map[string][]byte, error) {
	data, err := s.OpenSecret(s.system(ctx), resource.Key{Project: systemProject, Name: name})
	if err == nil {
		return data, nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeFailedPrecondition:
		return nil, fmt.Errorf("its %s %q cannot be opened: %s: %w", what, name, messageOf(err), errSecretMissing)
	}
	return nil, fmt.Errorf("its %s %q: %w", what, name, err)
}

// hangarProvider is a hangar target's provider, as running.
func (s *Server) hangarProvider(target string) (*hangar.Target, error) {
	s.hangar.mu.Lock()
	defer s.hangar.mu.Unlock()
	if run := s.hangar.running[target]; run != nil {
		return run.t, nil
	}
	if why := s.hangar.failed[target]; why != "" {
		return nil, fmt.Errorf("hangar target %q: %w: %s", target, errNoProvider, why)
	}
	return nil, fmt.Errorf("hangar target %q: %w: its provider has not started yet", target, errNoProvider)
}

// hangarStatus is what each hangar target's provider is doing.
func (s *Server) hangarStatus() []controller.Status {
	s.hangar.mu.Lock()
	defer s.hangar.mu.Unlock()
	var st []controller.Status
	for _, run := range s.hangar.running {
		st = append(st, run.t.Status()...)
	}
	return st
}

// serveHangarStream serves a direct stream to a hangar target's instance
// through its provider, which relays it to hangar.
func (s *Server) serveHangarStream(w http.ResponseWriter, r *http.Request) {
	target, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, HangarStreamPath), "/")
	p, err := s.hangarProvider(target)
	if err != nil {
		// Said as a stream with no ticket, or one not for it, is: a caller
		// learns nothing of which targets there are.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "a stream takes a ticket tower issued, as its bearer token", http.StatusUnauthorized)
			return
		}
		http.Error(w, "refused: the ticket is not for this stream", http.StatusForbidden)
		return
	}
	p.Streams().ServeHTTP(w, r)
}

// systemClient is a client of tower's API acting as the system, served in
// memory by the apiserver's own handler.
func (s *Server) systemClient() *client.Client {
	s.inproc.once.Do(func() {
		ln := newPipeListener()
		srv := &http.Server{Handler: s.Handler(auth.Chain{systemAuth{}})}
		go func() { _ = srv.Serve(ln) }()
		go func() {
			<-s.runCtx.Done()
			_ = srv.Close()
		}()
		hc := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ln.dial(ctx) },
		}}
		s.inproc.client = client.New(hc, "http://tower.internal")
	})
	return s.inproc.client
}

// inprocClient is systemClient's, made once.
type inprocClient struct {
	once   sync.Once
	client *client.Client
}

// systemAuth takes every request as the system's: only for the in-memory
// listener, which nothing but the apiserver itself reaches.
type systemAuth struct{}

func (systemAuth) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.SystemPrincipal, true, nil
}

// pipeListener is a listener whose connections are in memory.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	server, c := net.Pipe()
	select {
	case l.conns <- server:
		return c, nil
	case <-l.closed:
		return nil, errors.New("the apiserver has stopped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "tower.internal" }

// hangarReachable sets a hangar target's Reachable in st false while tower
// runs no provider for it, saying why; its provider sets it otherwise.
func (s *Server) hangarReachable(target string, st *targetsv1alpha1.TargetStatus) {
	_, err := s.hangarProvider(target)
	if err == nil {
		return
	}
	st.LastError = err.Error()
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), &metav1.Condition{
		Type: TargetReachable, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "NoProvider", Message: err.Error(),
	}, s.clock.Now())
}
