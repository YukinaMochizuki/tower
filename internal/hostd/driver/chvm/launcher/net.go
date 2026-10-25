package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The VM's network is its container's network namespace: the container
// has the VM's address on its workstream network, Docker's DNS knows it by
// its alias, and the launcher forwards that address to the VM, which sits
// behind a tap on a link of its own:
//
//	workstream network ── eth0 (the container's address) ──┐
//	                                                        │ DNAT / masquerade
//	VM 10.200.0.2 ── tap0 10.200.0.1 ───────────────────────┘
//
// Every VM has the same address on its own link, so what a golden's
// builder baked of it (k3s's node address, its etcd member) holds in each
// clone. The launcher answers the VM's DHCP with that address, is its
// resolver at the tap's address, forwarding to Docker's own resolver in
// the namespace so the VM resolves its workstream's aliases, and serves
// cloud-init's NoCloud metadata at MetadataAddr.
const (
	tapName = "tap0"
	// TapAddr is the launcher's address on the VM's link; VMAddr the VM's.
	TapAddr      = "10.200.0.1"
	VMAddr       = "10.200.0.2"
	linkBits     = 30
	MetadataAddr = "169.254.169.254"
	// dockerDNS is Docker's resolver in a container's namespace.
	dockerDNS = "127.0.0.11:53"
	// containerLink is the container's interface on its network.
	containerLink = "eth0"
	nftTable      = "tower_vm"
)

// openTap opens a tap of the name for the VM, single-queue with virtio-net
// headers, as Cloud Hypervisor takes one by fd; it is gone once its file
// is closed (hangar: internal/node/driver/hostnet/tap.go).
func openTap(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err == nil {
		ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
		err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("opening tap %s: %w", name, err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun:"+name), nil
}

// containerAddr is the container's IPv4 address on its network.
func containerAddr() (netip.Addr, error) {
	link, err := netlink.LinkByName(containerLink)
	if err != nil {
		return netip.Addr{}, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP.To4()); ok {
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s has no IPv4 address", containerLink)
}

// setupNetwork makes the namespace the VM's and starts serving it; the
// tap's file is Cloud Hypervisor's.
func setupNetwork(ctx context.Context, s Spec, log *slog.Logger) (*os.File, error) {
	addr, err := containerAddr()
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(b)) != "1" {
		return nil, errors.New("IPv4 forwarding is off in the container (its sysctl net.ipv4.ip_forward=1)")
	}
	tap, err := openTap(tapName)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) {
		_ = tap.Close()
		return nil, err
	}
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		return fail(err)
	}
	tapAddr, _ := netlink.ParseAddr(fmt.Sprintf("%s/%d", TapAddr, linkBits))
	if err := netlink.AddrReplace(link, tapAddr); err != nil {
		return fail(fmt.Errorf("addressing %s: %w", tapName, err))
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fail(err)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fail(err)
	}
	md, _ := netlink.ParseAddr(MetadataAddr + "/32")
	if err := netlink.AddrReplace(lo, md); err != nil {
		return fail(fmt.Errorf("addressing the metadata service: %w", err))
	}
	if err := forward(addr); err != nil {
		return fail(fmt.Errorf("forwarding %s to the VM: %w", addr, err))
	}
	if err := serveDHCP(ctx, s, log); err != nil {
		return fail(err)
	}
	if err := serveDNS(ctx, log); err != nil {
		return fail(err)
	}
	if err := serveMetadata(ctx, s, log); err != nil {
		return fail(err)
	}
	log.Info("the VM's network is up", "address", addr, "vm", VMAddr)
	return tap, nil
}

// forward writes the namespace's table: what reaches the container's
// address from its network goes to the VM, and what the VM sends out goes
// as the container's address.
func forward(addr netip.Addr) error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	t := &nftables.Table{Name: nftTable, Family: nftables.TableFamilyIPv4}
	c.DelTable(t)
	_ = c.Flush() // none there is no error
	t = c.AddTable(t)
	pre := c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
	post := c.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
	vm := netip.MustParseAddr(VMAddr)
	ifname := func(name string) []byte {
		b := make([]byte, unix.IFNAMSIZ)
		copy(b, name)
		return b
	}
	c.AddRule(&nftables.Rule{Table: t, Chain: pre, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(containerLink)},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: addr.AsSlice()},
		&expr.Immediate{Register: 1, Data: vm.AsSlice()},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}})
	c.AddRule(&nftables.Rule{Table: t, Chain: post, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(containerLink)},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: vm.AsSlice()},
		&expr.Masq{},
	}})
	return c.Flush()
}

// leaseTime is what the VM's lease says: its address never changes.
const leaseTime = 24 * time.Hour

// serveDHCP answers the VM's DHCP, by its MAC alone, with its address.
func serveDHCP(ctx context.Context, s Spec, log *slog.Logger) error {
	conn, err := server4.NewIPv4UDPConn(tapName, &net.UDPAddr{IP: net.IPv4zero, Port: dhcpv4.ServerPort})
	if err != nil {
		return fmt.Errorf("serving DHCP: %w", err)
	}
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	mac, err := net.ParseMAC(s.MAC)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req, err := dhcpv4.FromBytes(buf[:n])
			if err != nil || req.OpCode != dhcpv4.OpcodeBootRequest || req.ClientHWAddr.String() != mac.String() {
				continue
			}
			reply := dhcpReply(req, s)
			if reply == nil {
				continue
			}
			to := &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}
			if !peer.IP.IsUnspecified() && !peer.IP.Equal(net.IPv4bcast) {
				to = peer
			}
			if _, err := conn.WriteToUDP(reply.ToBytes(), to); err != nil {
				log.Debug("answering the VM's DHCP failed", "error", err)
			}
		}
	}()
	return nil
}

// dhcpReply is the answer to a request of the VM's, as hangar's DHCP
// answers a machine's (hangar: internal/node/dhcp).
func dhcpReply(req *dhcpv4.DHCPv4, s Spec) *dhcpv4.DHCPv4 {
	var answer dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		answer = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		answer = dhcpv4.MessageTypeAck
		if ip := req.RequestedIPAddress(); ip != nil && !ip.IsUnspecified() && !ip.Equal(net.ParseIP(VMAddr)) {
			answer = dhcpv4.MessageTypeNak
		}
	case dhcpv4.MessageTypeInform:
		answer = dhcpv4.MessageTypeAck
	default:
		return nil
	}
	gw := net.ParseIP(TapAddr).To4()
	mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(answer), dhcpv4.WithServerIP(gw), dhcpv4.WithOption(dhcpv4.OptServerIdentifier(gw))}
	if answer != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithNetmask(net.CIDRMask(linkBits, 32)),
			dhcpv4.WithRouter(gw),
			dhcpv4.WithDNS(gw),
		)
		if s.Hostname != "" {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(s.Hostname)))
		}
		if req.MessageType() != dhcpv4.MessageTypeInform {
			mods = append(mods, dhcpv4.WithYourIP(net.ParseIP(VMAddr).To4()), dhcpv4.WithLeaseTime(uint32(leaseTime/time.Second)))
		}
	}
	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		return nil
	}
	return reply
}

// serveDNS forwards the VM's queries, at the tap's address, to Docker's
// resolver in the namespace, which knows the workstream's aliases and
// forwards the rest out.
func serveDNS(ctx context.Context, log *slog.Logger) error {
	udp, err := net.ListenPacket("udp4", TapAddr+":53")
	if err != nil {
		return fmt.Errorf("serving DNS: %w", err)
	}
	tcp, err := net.Listen("tcp4", TapAddr+":53")
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("serving DNS: %w", err)
	}
	context.AfterFunc(ctx, func() { _ = udp.Close(); _ = tcp.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, peer, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			go func() {
				up, err := net.Dial("udp4", dockerDNS)
				if err != nil {
					log.Debug("forwarding a DNS query failed", "error", err)
					return
				}
				defer func() { _ = up.Close() }()
				_ = up.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := up.Write(q); err != nil {
					return
				}
				resp := make([]byte, 64<<10)
				m, err := up.Read(resp)
				if err != nil {
					return
				}
				_, _ = udp.WriteTo(resp[:m], peer)
			}()
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				up, err := net.DialTimeout("tcp4", dockerDNS, 10*time.Second)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return nil
}

// serveMetadata serves cloud-init's NoCloud seed: the VM's meta-data,
// with its instance id and hostname, and its user-data.
func serveMetadata(ctx context.Context, s Spec, log *slog.Logger) error {
	l, err := net.Listen("tcp4", MetadataAddr+":80")
	if err != nil {
		return fmt.Errorf("serving metadata: %w", err)
	}
	mux := http.NewServeMux()
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", s.InstanceID, s.Hostname)
	mux.HandleFunc("GET /nocloud/meta-data", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, meta) })
	mux.HandleFunc("GET /nocloud/user-data", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, s.UserData) })
	mux.HandleFunc("GET /nocloud/vendor-data", func(w http.ResponseWriter, _ *http.Request) {})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("the metadata service ended", "error", err)
		}
	}()
	return nil
}
