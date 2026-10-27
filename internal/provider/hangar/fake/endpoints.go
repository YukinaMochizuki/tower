package fake

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// hangar#224's kinds: a NetworkEndpoint is a listener on 127.0.0.1 that
// carries what connects to the system container's tower-system serve from
// its network's transit address (127.100.0.0/16), as hangar's NAT does,
// if a security group of the container lets that network in.

// endpointProxy is a NetworkEndpoint's listener.
type endpointProxy struct {
	ln   net.Listener
	name string
}

// server is a system container's tower-system serve.
type server struct {
	cmd     *exec.Cmd
	addr    string
	control string
	dir     string
}

// Endpoint kinds' accessors.
func (f *Fake) EndpointServices() hangar.Kind[*hangarnetwork.EndpointService] { return f.epsvcs }
func (f *Fake) NetworkEndpoints() hangar.Kind[*hangarnetwork.NetworkEndpoint] { return f.neps }
func (f *Fake) SecurityGroups() hangar.Kind[*hangarnetwork.SecurityGroup]     { return f.sgs }

// WithoutEndpoints makes the fake a hangar without hangar#224: the three
// kinds' calls answer Unimplemented.
func (f *Fake) WithoutEndpoints() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noEndpoints = true
}

// SetEndpointsDown makes every NetworkEndpoint report it is not
// Connected, why its Ready's message, as one across nodes would; ""
// reports them as they are again.
func (f *Fake) SetEndpointsDown(why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointsDown = why
}

// DropEndpointTraffic makes every NetworkEndpoint drop what connects to
// it while it reports Connected, as hangar#226 may; false carries it
// again.
func (f *Fake) DropEndpointTraffic(drop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropEndpoints = drop
}

// BlackholeEndpoints makes every NetworkEndpoint take what connects to
// it and answer nothing, while it reports Connected; false carries it
// again.
func (f *Fake) BlackholeEndpoints(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blackhole = on
}

// SetServerDown ends every system container's tower-system serve and
// keeps it down, as one that crashed; false lets it start again.
func (f *Fake) SetServerDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverDown = down
	if down {
		for uid := range f.servers {
			f.stopServerLocked(uid)
		}
	}
}

// EndpointConnections counts what connected through every endpoint,
// carried or not.
func (f *Fake) EndpointConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.endpointConns
}

// Credentials are the credentials the shims' headers carried, in every
// container: what a test looks for where they must not be.
func (f *Fake) Credentials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.credentials)
}

// ContainerRoot is a container's filesystem on the host, "" for none.
func (f *Fake) ContainerRoot(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ct, ok := f.containers.objs[name]
	if !ok {
		return ""
	}
	return f.root(ct.GetMetadata().GetUid())
}

// Object names of the endpoint kinds, for tests to look at.
func (f *Fake) EndpointServiceNames() []string { return f.epsvcs.names() }
func (f *Fake) NetworkEndpointNames() []string { return f.neps.names() }
func (f *Fake) SecurityGroupNames() []string   { return f.sgs.names() }

// unimplemented is what an old hangar answers its gated kinds' calls.
func (f *Fake) unimplementedLocked(gated bool) error {
	if gated && f.noEndpoints {
		return connect.NewError(connect.CodeUnimplemented, errors.New("no such procedure"))
	}
	return nil
}

// networkApplied gives a new network its transit /31. f.mu is held.
func (f *Fake) networkApplied(n *hangarnetwork.Network, isNew bool) error {
	if isNew {
		i := f.transits
		f.transits++
		n.Status = &hangarnetwork.NetworkStatus{TransitIpv4: fmt.Sprintf("127.100.%d.%d/31", i/128, (i%128)*2)}
	}
	return nil
}

// endpointApplied refuses a new endpoint past the quota, and listens for
// it. f.mu is held.
func (f *Fake) endpointApplied(ne *hangarnetwork.NetworkEndpoint, isNew bool) error {
	if !isNew {
		return nil
	}
	if m := f.limitsLocked().NetworkEndpoints; m != nil && int32(len(f.neps.objs)) >= *m {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the project holds %d network endpoints, its limit", *m))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p := &endpointProxy{ln: ln, name: ne.GetMetadata().GetName()}
	if f.proxies == nil {
		f.proxies = map[string]*endpointProxy{}
	}
	f.proxies[p.name] = p
	go f.serveEndpoint(p)
	return nil
}

func (f *Fake) endpointDeleting(ne *hangarnetwork.NetworkEndpoint) error {
	if p, ok := f.proxies[ne.GetMetadata().GetName()]; ok {
		_ = p.ln.Close()
		delete(f.proxies, p.name)
	}
	return nil
}

// sgDeleting refuses a group an interface names, as hangar does.
func (f *Fake) sgDeleting(sg *hangarnetwork.SecurityGroup) error {
	name := sg.GetMetadata().GetName()
	for _, ct := range f.containers.objs {
		for _, i := range ct.GetSpec().GetInterfaces() {
			if i.GetNetwork() == sg.GetSpec().GetNetwork() && slices.Contains(i.GetSecurityGroups(), name) {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("security group %q is in use by container %s", name, ct.GetMetadata().GetName()))
			}
		}
	}
	return nil
}

// serviceRead says whether the service serves. f.mu is held.
func (f *Fake) serviceRead(svc *hangarnetwork.EndpointService) {
	reason, msg, ok := "Serving", "", true
	ct, found := f.containers.objs[svc.GetSpec().GetTarget().GetContainer()]
	switch {
	case !found:
		reason, msg, ok = "TargetNotFound", "its container is not there", false
	case f.machines["ct-"+ct.GetMetadata().GetName()].GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		reason, msg, ok = "ServicePending", "its container does not run", false
	}
	svc.Status = &hangarnetwork.EndpointServiceStatus{Conditions: []*hangarmeta.Condition{ready(ok, reason, msg)}, TransitPort: 1024}
	if found {
		svc.Status.Network = ct.GetSpec().GetInterfaces()[0].GetNetwork()
	}
}

// endpointRead says whether the endpoint is Connected, and where it is
// reached. f.mu is held.
func (f *Fake) endpointRead(ne *hangarnetwork.NetworkEndpoint) {
	reason, msg, ok := "Connected", "", true
	svc, found := f.epsvcs.objs[ne.GetSpec().GetService()]
	n, netFound := f.networks.objs[ne.GetSpec().GetNetwork()]
	switch {
	case !netFound:
		reason, msg, ok = "NetworkNotReady", "its network is not there", false
	case !found:
		reason, msg, ok = "ServicePending", "its service is not there", false
	case !selects(svc.GetSpec().GetNetworkSelector(), n.GetMetadata().GetLabels()):
		reason, msg, ok = "NotAllowed", "the service does not select its network", false
	case f.endpointsDown != "":
		reason, msg, ok = "NotConnected", f.endpointsDown, false
	}
	if found && ok {
		f.serviceRead(svc)
		if c := svc.GetStatus().GetConditions()[0]; c.GetStatus() != hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE {
			reason, msg, ok = "ServicePending", c.GetMessage(), false
		}
	}
	ne.Status = &hangarnetwork.NetworkEndpointStatus{Conditions: []*hangarmeta.Condition{ready(ok, reason, msg)}, Ipv4: "127.0.0.1"}
	if p, ok := f.proxies[ne.GetMetadata().GetName()]; ok {
		ne.Status.Port = int32(p.ln.Addr().(*net.TCPAddr).Port)
	}
}

func ready(ok bool, reason, msg string) *hangarmeta.Condition {
	st := hangarmeta.ConditionStatus_CONDITION_STATUS_FALSE
	if ok {
		st = hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE
	}
	return &hangarmeta.Condition{Type: "Ready", Status: st, Reason: reason, Message: msg}
}

// selects matches labels against a selector of "k=v" terms; empty
// selects every network.
func selects(selector string, labels map[string]string) bool {
	for _, term := range strings.Split(selector, ",") {
		if k, v, ok := strings.Cut(term, "="); ok && labels[k] != v {
			return false
		}
	}
	return true
}

// serveEndpoint carries each connection to its endpoint's service.
func (f *Fake) serveEndpoint(p *endpointProxy) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go f.carry(p.name, conn)
	}
}

// carry carries one connection, as hangar's NAT does, or drops it.
func (f *Fake) carry(endpoint string, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	f.mu.Lock()
	blackhole := f.blackhole
	f.mu.Unlock()
	if blackhole {
		// Until the other end gives up.
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	from, to, err := f.route(endpoint)
	if err != nil {
		return
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: from.AsSlice()}}
	up, err := d.Dial("tcp", to)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, conn)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	_, _ = io.Copy(conn, up)
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	wg.Wait()
}

// route is where an endpoint's connection comes from, its network's
// transit address, and goes, its service's server, if the endpoint is
// Connected, carries traffic, and a security group of the service's
// container lets the network in to its port.
func (f *Fake) route(endpoint string) (netip.Addr, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointConns++
	ne, ok := f.neps.objs[endpoint]
	if !ok || f.dropEndpoints {
		return netip.Addr{}, "", errors.New("dropped")
	}
	f.endpointRead(ne)
	if ne.GetStatus().GetConditions()[0].GetStatus() != hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE {
		return netip.Addr{}, "", errors.New("not connected")
	}
	network := ne.GetSpec().GetNetwork()
	p, err := netip.ParsePrefix(f.networks.objs[network].GetStatus().GetTransitIpv4())
	if err != nil {
		return netip.Addr{}, "", err
	}
	svc := f.epsvcs.objs[ne.GetSpec().GetService()]
	ct := f.containers.objs[svc.GetSpec().GetTarget().GetContainer()]
	if !f.letInLocked(ct, network, svc.GetSpec().GetPort()) {
		return netip.Addr{}, "", errors.New("no security group lets the network in")
	}
	srv, ok := f.servers[ct.GetMetadata().GetUid()]
	if !ok {
		return netip.Addr{}, "", errors.New("nothing listens")
	}
	return p.Masked().Addr().Next(), srv.addr, nil
}

// letInLocked says whether a security group of the container's first
// interface lets what comes from network in to port over TCP. f.mu is
// held.
func (f *Fake) letInLocked(ct *hangarcompute.Container, network string, port int32) bool {
	ifs := ct.GetSpec().GetInterfaces()
	if len(ifs) == 0 {
		return false
	}
	for _, name := range ifs[0].GetSecurityGroups() {
		sg, ok := f.sgs.objs[name]
		if !ok || sg.GetSpec().GetNetwork() != ifs[0].GetNetwork() {
			continue
		}
		for _, r := range sg.GetSpec().GetIngress() {
			if r.GetFrom().GetNetwork() != network {
				continue
			}
			if r.GetProtocol() != hangarnetwork.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP && r.GetProtocol() != hangarnetwork.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ALL {
				continue
			}
			if r.GetPorts() == "" || slices.Contains(strings.Split(r.GetPorts(), ","), strconv.Itoa(int(port))) {
				return true
			}
		}
	}
	return false
}

// startServer runs the system container's tower-system serve, once, on a
// loopback port of its own, its control socket in a directory of its own:
// a socket's path is short, which a test's directory may not be.
func (f *Fake) startServer(uid, registry string) (*server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[uid]; ok {
		return s, nil
	}
	if f.serverDown {
		return nil, errors.New("tower-system serve is down")
	}
	if f.System == "" {
		return nil, errors.New("the fake has no tower-system to run")
	}
	dir, err := os.MkdirTemp("", "tsys")
	if err != nil {
		return nil, err
	}
	control := filepath.Join(dir, "c.sock")
	cmd := exec.Command(f.System, "serve")
	cmd.Env = append(os.Environ(), "TOWER_SYSTEM_ROOT="+filepath.Join(f.root(uid), "var", "lib", "tower"), "TOWER_REGISTRY="+registry,
		"TOWER_SYSTEM_LISTEN=127.0.0.1:0", "TOWER_SYSTEM_CONTROL="+control)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on ")
	if err != nil || !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("tower-system serve said %q: %w", line, err)
	}
	go func() { _, _ = io.Copy(io.Discard, out) }()
	s := &server{cmd: cmd, addr: addr, control: control, dir: dir}
	if f.servers == nil {
		f.servers = map[string]*server{}
	}
	f.servers[uid] = s
	return s, nil
}

// stopServerLocked ends a container's server, if it runs one. f.mu is
// held.
func (f *Fake) stopServerLocked(uid string) {
	s, ok := f.servers[uid]
	if !ok {
		return
	}
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	_ = os.RemoveAll(s.dir)
	delete(f.servers, uid)
}

// controlAnswers says whether a server answers its control socket, as
// tower-system's status asks it.
func controlAnswers(control string) bool {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", control)
		},
	}}
	defer c.CloseIdleConnections()
	resp, err := c.Get("http://tower-system/status")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode/100 == 2
}
