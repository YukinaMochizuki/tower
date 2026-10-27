package hangar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// A box's checkouts and pushes go to the system container's server over
// hangar's endpoint service where it can carry them, else through tower,
// why kept for the target's facts (docs/decisions/traffic.md).

// LabelHangarNetwork names, on a NetworkEndpoint, the hangar Network it
// is the way of.
const LabelHangarNetwork = "tower/hangar-network"

// The hangar conditions an endpoint's use is read from (hangar:
// internal/apiserver/endpointservices.go).
const (
	endpointReady     = "Ready"
	endpointServing   = "Serving"
	endpointConnected = "Connected"
)

// grantTTL is how long a credential holds: the exec into the box that
// presents it starts at once.
const grantTTL = 2 * time.Minute

// endpointRetry is how long an endpoint hangar refused is not asked for
// again, as one over the project's quota; probeRetry how long a network
// whose probe failed is relayed for before the endpoint is tried again:
// a probe that does not answer costs a transfer its timeout.
const (
	endpointRetry = time.Minute
	probeRetry    = 5 * time.Minute
)

// endpointName names a workstream network's NetworkEndpoint.
func endpointName(network string) string {
	sum := sha256.Sum256([]byte(network))
	return "tower-ep-" + hex.EncodeToString(sum[:])[:12]
}

// unsupported reports whether hangar answered that it has no such kind:
// one without hangar#224.
func unsupported(err error) bool { return connect.CodeOf(err) == connect.CodeUnimplemented }

// noEndpoints is why an old hangar's transfers are relayed.
const noEndpoints = "hangar has no EndpointService (hangar#224)"

// endpointState is what the provider knows of its endpoint, and how its
// mirrors' transfers went.
type endpointState struct {
	mu sync.Mutex
	// known says whether hangar was asked; supported, then, whether it has
	// the kinds.
	known, supported bool
	// service is why the service is not offered, "" when it is.
	service string
	// made are the networks whose endpoint is made, as of this run; refused
	// when hangar last refused one, and why.
	made    map[string]bool
	refused map[string]refusal
	// probed are the networks whose probe from a box last failed, when,
	// and why.
	probed map[string]refusal
	// direct and relayed count the transfers each way, relayedBytes what
	// the relays carried through tower, lastRelay why the last was relayed.
	direct, relayed int64
	relayedBytes    int64
	lastRelay       string
}

type refusal struct {
	at  time.Time
	why string
}

func (s *endpointState) setSupported(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known, s.supported = true, ok
	if !ok {
		s.service = noEndpoints
	}
}

func (s *endpointState) isSupported() (known, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.known, s.supported
}

func (s *endpointState) setService(why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.service = why
}

// MirrorTransfers is how a target's checkouts, origins and pushes went:
// Direct between a box and the system container over the endpoint,
// Relayed through tower carrying RelayedBytes, the last relayed for
// LastRelay.
type MirrorTransfers struct {
	Direct, Relayed int64
	RelayedBytes    int64
	LastRelay       string
}

// MirrorTransfers says how the target's mirror transfers went, as of this
// run.
func (t *Target) MirrorTransfers() MirrorTransfers {
	t.ep.mu.Lock()
	defer t.ep.mu.Unlock()
	return MirrorTransfers{Direct: t.ep.direct, Relayed: t.ep.relayed, RelayedBytes: t.ep.relayedBytes, LastRelay: t.ep.lastRelay}
}

// ensureEndpointService offers the system container's server to the
// target's networks, and returns the security groups the container's
// interface names: its own once hangar has the kinds, none on a hangar
// without them, which then keeps the container as it was. A failure that
// is not hangar lacking them leaves the container as it is, and the
// relay carries the mirrors meanwhile.
func (t *Target) ensureEndpointService(ctx context.Context, name string, labels map[string]string) ([]string, error) {
	want := &hangarnetwork.EndpointServiceSpec{
		Target:          &hangarnetwork.EndpointServiceTarget{Object: &hangarnetwork.EndpointServiceTarget_Container{Container: name}},
		Protocol:        hangarnetwork.Protocol_PROTOCOL_TCP,
		Port:            system.ServePort,
		NetworkSelector: LabelTarget + "=" + t.cfg.Target,
	}
	svc, err := t.api.EndpointServices().Get(ctx, name)
	if err == nil && svc.GetMetadata().GetDeleteTime() == nil && !proto.Equal(svc.GetSpec(), want) {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		_, err = t.api.EndpointServices().Apply(ctx, &hangarnetwork.EndpointService{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels}, Spec: want})
	}
	if unsupported(err) {
		t.ep.setSupported(false)
		return nil, nil
	}
	if err != nil {
		t.ep.setService("offering the server to the networks: " + err.Error())
		t.log.Warn("the system container's endpoint service", "error", err)
		return t.groupsKept(ctx, name)
	}
	t.ep.setSupported(true)
	if err := t.ensureSecurityGroup(ctx); err != nil {
		t.ep.setService("letting the networks in: " + err.Error())
		t.log.Warn("the system container's security group", "error", err)
		return t.groupsKept(ctx, name)
	}
	t.ep.setService("")
	return []string{name}, nil
}

// groupsKept are the security groups the system container's interface
// names now, none for a container not made yet: a failure to make them
// changes nothing. Not read, they are not guessed, which would restart
// the container: the error is returned.
func (t *Target) groupsKept(ctx context.Context, name string) ([]string, error) {
	ct, err := t.api.Containers().Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(ct.GetSpec().GetInterfaces()) == 0 {
		return nil, nil
	}
	return slices.Clone(ct.GetSpec().GetInterfaces()[0].GetSecurityGroups()), nil
}

// ensureSecurityGroup lets each of the target's workstream networks reach
// the server's port on the system container through its endpoint: a
// connection through one comes from that network's transit address,
// which a rule's network peer stands for.
func (t *Target) ensureSecurityGroup(ctx context.Context) error {
	t.sgMu.Lock()
	defer t.sgMu.Unlock()
	name := systemName(t.cfg.Target)
	nets, err := t.workstreamNetworks(ctx)
	if err != nil {
		return err
	}
	want := &hangarnetwork.SecurityGroupSpec{Network: name}
	for _, n := range nets {
		want.Ingress = append(want.Ingress, &hangarnetwork.IngressRule{
			Protocol: hangarnetwork.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP, Ports: strconv.Itoa(system.ServePort),
			From: &hangarnetwork.SecurityGroupPeer{Peer: &hangarnetwork.SecurityGroupPeer_Network{Network: n.GetMetadata().GetName()}},
		})
	}
	sg, err := t.api.SecurityGroups().Get(ctx, name)
	if err == nil && proto.Equal(sg.GetSpec(), want) {
		return nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = t.api.SecurityGroups().Apply(ctx, &hangarnetwork.SecurityGroup{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: t.systemLabels()}, Spec: want})
	return err
}

// workstreamNetworks are the target's networks but its system network,
// by name, none being deleted.
func (t *Target) workstreamNetworks(ctx context.Context) ([]*hangarnetwork.Network, error) {
	all, err := t.api.Networks().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	var out []*hangarnetwork.Network
	for _, n := range all {
		meta := n.GetMetadata()
		if t.mine(meta.GetLabels()) && meta.GetLabels()[LabelRole] != "system" && meta.GetDeleteTime() == nil {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b *hangarnetwork.Network) int {
		return cmpString(a.GetMetadata().GetName(), b.GetMetadata().GetName())
	})
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ensureNetworkEndpoint gives a workstream network its way to the system
// container's server, once, where the target has a system container and
// hangar the kinds. What hangar refuses, as a project out of its quota of
// endpoints, is kept as why the network's transfers are relayed, and asked
// again after endpointRetry; it fails nothing of the instance.
func (t *Target) ensureNetworkEndpoint(ctx context.Context, network, project string) {
	if t.cfg.SystemImage == "" {
		return
	}
	if known, ok := t.ep.isSupported(); known && !ok {
		return
	}
	t.ep.mu.Lock()
	r, refused := t.ep.refused[network]
	done := t.ep.made[network] || (refused && t.clock.Since(r.at) < endpointRetry)
	t.ep.mu.Unlock()
	if done {
		return
	}
	name := endpointName(network)
	ne, err := t.api.NetworkEndpoints().Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		ne, err = t.api.NetworkEndpoints().Apply(ctx, &hangarnetwork.NetworkEndpoint{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: map[string]string{LabelTarget: t.cfg.Target, LabelHangarNetwork: network, LabelProject: project}},
			Spec:     &hangarnetwork.NetworkEndpointSpec{Network: network, Service: systemName(t.cfg.Target)},
		})
	}
	switch {
	case unsupported(err):
		t.ep.setSupported(false)
		return
	case err == nil && ne.GetMetadata().GetDeleteTime() != nil:
		err = errors.New("its endpoint is being deleted")
	}
	t.ep.mu.Lock()
	if err != nil {
		why := "hangar refused its endpoint: " + err.Error()
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			why = "the project is out of its quota of network endpoints: " + err.Error()
		}
		t.ep.refused[network] = refusal{at: t.clock.Now(), why: why}
	} else {
		t.ep.made[network] = true
		delete(t.ep.refused, network)
	}
	t.ep.mu.Unlock()
	if err != nil {
		t.log.Warn("a network's endpoint to the system container", "network", network, "error", err)
		return
	}
	if err := t.ensureSecurityGroup(ctx); err != nil {
		t.log.Warn("the system container's security group", "error", err)
	}
}

// forgetEndpoint forgets that a network's endpoint was made: its network
// is going, and one made again of its name has an endpoint of its own.
func (t *Target) forgetEndpoint(network string) {
	t.ep.mu.Lock()
	defer t.ep.mu.Unlock()
	delete(t.ep.made, network)
	delete(t.ep.refused, network)
	delete(t.ep.probed, network)
}

// transitSource is where what a network sends through its endpoints
// comes from: the upper address of its transit /31 (hangar's
// NetworkStatus.transit_ipv4, whose lower address is the host's end).
// hangar documents that it is the network's transit address, not which
// of the two; the upper is what a service sees, as tested on hangar
// 2c787ad.
func transitSource(cidr string) (netip.Addr, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || p.Bits() != 31 || !p.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("transit %q is no IPv4 /31", cidr)
	}
	return p.Masked().Addr().Next(), nil
}

// sources are the target's workstream networks as the server checks
// where a request comes from: each with its tower project and transit
// address, those without one yet left out.
func (t *Target) sources(ctx context.Context) ([]system.Source, error) {
	nets, err := t.workstreamNetworks(ctx)
	if err != nil {
		return nil, err
	}
	out := []system.Source{}
	for _, n := range nets {
		if src, err := sourceOf(n); err == nil {
			out = append(out, src)
		}
	}
	return out, nil
}

func sourceOf(n *hangarnetwork.Network) (system.Source, error) {
	addr, err := transitSource(n.GetStatus().GetTransitIpv4())
	if err != nil {
		return system.Source{}, fmt.Errorf("network %s: %w", n.GetMetadata().GetName(), err)
	}
	project := n.GetMetadata().GetLabels()[LabelProject]
	if project == "" {
		return system.Source{}, fmt.Errorf("network %s names no tower project", n.GetMetadata().GetName())
	}
	return system.Source{Network: n.GetMetadata().GetName(), Project: project, Address: addr.String()}, nil
}

// route is how a box reaches the server: base, its URL over the box's
// network's endpoint, and src, that network as the server sees it.
type route struct {
	base string
	src  system.Source
}

// routeOf finds how the box p reaches the server over its network's
// endpoint, or says why it does not.
func (t *Target) routeOf(ctx context.Context, p placed) (route, string) {
	if known, ok := t.ep.isSupported(); known && !ok {
		return route{}, noEndpoints
	} else if !known {
		return route{}, "the system container's endpoint is not made yet"
	}
	if why := t.sys.notServing(); why != "" {
		return route{}, why
	}
	if p.in.GetSpec().GetNetwork() == "" {
		return route{}, "the instance is on no network"
	}
	network := networkName(t.cfg.Target, p.ref.Project, p.in.GetSpec().GetNetwork())
	svc, err := t.api.EndpointServices().Get(ctx, systemName(t.cfg.Target))
	if err != nil {
		return route{}, "reading the endpoint service: " + err.Error()
	}
	if c := conditionOf(svc.GetStatus().GetConditions(), endpointReady); c.GetStatus() != hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != endpointServing {
		return route{}, fmt.Sprintf("the endpoint service is not serving: %s: %s", c.GetReason(), c.GetMessage())
	}
	ne, err := t.api.NetworkEndpoints().Get(ctx, endpointName(network))
	if errors.Is(err, ErrNotFound) {
		// Made before hangar had the kinds, or deleted since.
		t.forgetEndpoint(network)
		t.ensureNetworkEndpoint(ctx, network, p.ref.Project)
		ne, err = t.api.NetworkEndpoints().Get(ctx, endpointName(network))
	}
	if errors.Is(err, ErrNotFound) {
		t.ep.mu.Lock()
		r, ok := t.ep.refused[network]
		t.ep.mu.Unlock()
		if ok {
			return route{}, fmt.Sprintf("network %s: %s", network, r.why)
		}
		return route{}, fmt.Sprintf("network %s has no endpoint yet", network)
	}
	if err != nil {
		return route{}, "reading the network's endpoint: " + err.Error()
	}
	c := conditionOf(ne.GetStatus().GetConditions(), endpointReady)
	if c.GetStatus() != hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != endpointConnected {
		return route{}, fmt.Sprintf("network %s's endpoint is not connected: %s: %s", network, c.GetReason(), c.GetMessage())
	}
	ip, err := netip.ParseAddr(ne.GetStatus().GetIpv4())
	if err != nil || ne.GetStatus().GetPort() == 0 {
		return route{}, fmt.Sprintf("network %s's endpoint has no address yet", network)
	}
	n, err := t.api.Networks().Get(ctx, network)
	if err != nil {
		return route{}, "reading the network: " + err.Error()
	}
	src, err := sourceOf(n)
	if err != nil {
		return route{}, err.Error()
	}
	return route{base: "http://" + netip.AddrPortFrom(ip, uint16(ne.GetStatus().GetPort())).String(), src: src}, ""
}

// errRelay is a transfer the endpoint cannot carry, before anything was
// sent: it is relayed instead. probe says the box's probe failed.
type errRelay struct {
	why   string
	probe bool
}

func (e errRelay) Error() string { return e.why }

// direct runs one transfer between the box and the server over r: a
// credential for it granted to the server, then the box's fetch of the
// operation's tar into path, or post of path for a push. An errRelay says
// it did not happen, and why.
func (t *Target) direct(ctx context.Context, o system.Ops, box guestops.Ops, r route, g system.GrantSpec, path string) (system.Result, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return system.Result{}, err
	}
	g.Credential = base64.RawURLEncoding.EncodeToString(b)
	g.Network, g.TTL, g.Sources = r.src.Network, grantTTL, []system.Source{r.src}
	if err := o.Grant(ctx, g); err != nil {
		return system.Result{}, errRelay{why: "granting the box a credential: " + err.Error()}
	}
	x := guestops.HTTPTransfer{
		URL:        r.base + system.PathPrefix + g.Op,
		Probe:      r.base + system.PathPing + "?network=" + url.QueryEscape(r.src.Network),
		Credential: g.Credential,
	}
	var a guestbin.HTTPAnswer
	var err error
	if g.Op == system.GrantPush {
		a, err = box.Post(ctx, x, path)
	} else {
		a, err = box.Fetch(ctx, x, path)
	}
	if errors.Is(err, guest.ErrUnreachable) {
		// Not presented: taken back, so no one presents it later.
		revoke, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemCheck)
		if rerr := o.Revoke(revoke, g.Credential); rerr != nil {
			t.log.Warn("taking back a credential not presented", "network", r.src.Network, "error", rerr)
		}
		cancel()
		return system.Result{}, errRelay{why: "the probe from the box: " + err.Error(), probe: true}
	}
	if err != nil {
		return system.Result{}, err
	}
	var res system.Result
	if a.Result != "" {
		if err := json.Unmarshal([]byte(a.Result), &res); err != nil {
			return system.Result{}, fmt.Errorf("the server's result %q: %w", a.Result, err)
		}
	}
	switch a.Status {
	case http.StatusOK:
		return res, nil
	case http.StatusNotFound:
		return system.Result{}, fmt.Errorf("%s: %w", a.Message, driver.ErrNotFound)
	case http.StatusConflict:
		return system.Result{}, fmt.Errorf("%s: %w", a.Message, driver.ErrLeaseLost)
	case http.StatusInsufficientStorage:
		return system.Result{}, fmt.Errorf("%s: %w", a.Message, driver.ErrNoSpace)
	}
	return system.Result{}, fmt.Errorf("tower-system %s: %d: %s: %w", g.Op, a.Status, a.Message, provider.ErrFailed)
}

// overEndpoint runs a transfer over the box's endpoint where it can, and
// says whether it did: false, and why, if it is to be relayed.
func (t *Target) overEndpoint(ctx context.Context, o system.Ops, p placed, box guestops.Ops, g system.GrantSpec, path string) (system.Result, bool, string, error) {
	r, why := t.routeOf(ctx, p)
	if why != "" {
		return system.Result{}, false, why, nil
	}
	t.ep.mu.Lock()
	failed, ok := t.ep.probed[r.src.Network]
	t.ep.mu.Unlock()
	if ok && t.clock.Since(failed.at) < probeRetry {
		return system.Result{}, false, fmt.Sprintf("%s (at %s; tried again after %s)", failed.why, failed.at.UTC().Format(time.RFC3339), probeRetry), nil
	}
	res, err := t.direct(ctx, o, box, r, g, path)
	var relay errRelay
	if errors.As(err, &relay) {
		if relay.probe {
			t.ep.mu.Lock()
			t.ep.probed[r.src.Network] = refusal{at: t.clock.Now(), why: relay.why}
			t.ep.mu.Unlock()
		}
		return system.Result{}, false, relay.why, nil
	}
	t.ep.mu.Lock()
	delete(t.ep.probed, r.src.Network)
	t.ep.mu.Unlock()
	t.ep.mu.Lock()
	t.ep.direct++
	t.ep.mu.Unlock()
	return res, true, "", err
}

// relaying counts a transfer relayed, for why, and logs it.
func (t *Target) relaying(p placed, op, why string) {
	t.ep.mu.Lock()
	t.ep.relayed++
	t.ep.lastRelay = why
	t.ep.mu.Unlock()
	t.log.Info("relaying a mirror transfer through tower", "op", op, "project", p.ref.Project, "instance", p.ref.Name, "why", why)
}

// relayedBytes counts n bytes a relay carried.
func (t *Target) relayedBytes(n int64) {
	t.ep.mu.Lock()
	t.ep.relayedBytes += n
	t.ep.mu.Unlock()
}

// syncSources gives the server every workstream network's source.
func (t *Target) syncSources(ctx context.Context, o system.Ops) error {
	srcs, err := t.sources(ctx)
	if err != nil {
		return err
	}
	return o.Sources(ctx, srcs)
}

// transportFact says how the target's mirror transfers go.
func (t *Target) transportFact() string {
	serving := t.sys.notServing()
	t.ep.mu.Lock()
	defer t.ep.mu.Unlock()
	switch {
	case !t.ep.known:
		return "relay: the endpoint is not made yet"
	case t.ep.service != "":
		return "relay: " + t.ep.service
	case serving != "":
		return "relay: " + serving
	}
	v := "endpoint"
	if n := len(t.ep.refused); n > 0 {
		for network, r := range t.ep.refused {
			v += fmt.Sprintf("; relay on %d network(s), such as %s: %s", n, network, r.why)
			break
		}
	}
	return v
}

// transfersFact counts the target's mirror transfers each way.
func (t *Target) transfersFact() string {
	m := t.MirrorTransfers()
	v := fmt.Sprintf("%d over the endpoint, %d relayed through tower (%d bytes)", m.Direct, m.Relayed, m.RelayedBytes)
	if m.LastRelay != "" {
		v += "; the last relayed as " + m.LastRelay
	}
	return v
}

// counted is r, what it gives counted as carried through tower by a
// relay.
func (t *Target) counted(r io.Reader) io.Reader { return relayCounter{r: r, t: t} }

type relayCounter struct {
	r io.Reader
	t *Target
}

func (c relayCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.t.relayedBytes(int64(n))
	}
	return n, err
}
