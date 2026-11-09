package guestbin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A box that joins a tailnet resolves through a forwarder of tower-guest
// init's own: names under MagicDNSSuffix go to tailscaled's resolver
// (MagicDNS), every other name to the servers the container had, Docker's
// embedded one on a workstream network, which answers its peers' aliases
// and forwards the rest to the public servers hostd gives guests
// (docs/decisions/tailnet.md).

const (
	// MagicDNSSuffix is what a tailnet's MagicDNS names end in.
	MagicDNSSuffix = "ts.net."
	// MagicDNS is tailscaled's resolver, served by the node itself.
	MagicDNS = "100.100.100.100:53"
	// SplitDNS is where the forwarder listens, on the container's loopback.
	SplitDNS = "127.0.0.53:53"
	// dnsTimeout bounds one query's forward.
	dnsTimeout = 5 * time.Second
	// maxDNS is the most a DNS message over TCP holds.
	maxDNS = 65535
)

// dnsSplit forwards each query to MagicDNS or the default upstreams, by
// its name.
type dnsSplit struct {
	// upstreams are where names outside the tailnet go, each tried in
	// turn as a resolver tries its nameservers; magic, those in it.
	upstreams []string
	magic     string
}

// route is where a query goes, by its first question's name; a message
// that does not parse goes upstream, which answers it as it may.
func (d dnsSplit) route(msg []byte) []string {
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		return d.upstreams
	}
	q, err := p.Question()
	if err != nil {
		return d.upstreams
	}
	name := strings.ToLower(q.Name.String())
	if name == MagicDNSSuffix || strings.HasSuffix(name, "."+MagicDNSSuffix) {
		return []string{d.magic}
	}
	return d.upstreams
}

// exchange forwards q to each server of its route in turn, until one
// answers.
func (d dnsSplit) exchange(q []byte, one func(server string, q []byte) ([]byte, error)) ([]byte, error) {
	err := errors.New("no server to forward to")
	for _, server := range d.route(q) {
		var a []byte
		if a, err = one(server, q); err == nil {
			return a, nil
		}
	}
	return nil, err
}

// serveUDP answers queries on pc until it is closed.
func (d dnsSplit) serveUDP(pc net.PacketConn) error {
	buf := make([]byte, maxDNS)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			a, err := d.exchange(q, exchangeUDP)
			if err != nil {
				a = servFail(q)
			}
			if a != nil {
				_, _ = pc.WriteTo(a, from)
			}
		}()
	}
}

// servFail is the answer to q when no server of its route answered:
// SERVFAIL, so the resolver asking says so at once rather than waiting out
// its own timeout; nil for a query that does not parse, which is dropped.
func servFail(q []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil
	}
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired,
			RecursionAvailable: true, RCode: dnsmessage.RCodeServerFailure,
		},
		Questions: qs,
	}
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

func exchangeUDP(server string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", server, dnsTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	a := make([]byte, maxDNS)
	n, err := c.Read(a)
	if err != nil {
		return nil, err
	}
	return a[:n], nil
}

// serveTCP answers queries on l until it is closed, each connection's in
// turn.
func (d dnsSplit) serveTCP(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go d.handleTCP(c)
	}
}

func (d dnsSplit) handleTCP(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		_ = c.SetDeadline(time.Now().Add(2 * dnsTimeout))
		q, err := readTCPMessage(c)
		if err != nil {
			return
		}
		a, err := d.exchange(q, exchangeTCP)
		if err != nil {
			if a = servFail(q); a == nil {
				return
			}
		}
		if err := writeTCPMessage(c, a); err != nil {
			return
		}
	}
}

func exchangeTCP(server string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", server, dnsTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	if err := writeTCPMessage(c, q); err != nil {
		return nil, err
	}
	return readTCPMessage(c)
}

func readTCPMessage(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	m := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(r, m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeTCPMessage(w io.Writer, m []byte) error {
	if len(m) > maxDNS {
		return errors.New("a DNS message over 65535 bytes")
	}
	b := make([]byte, 2+len(m))
	binary.BigEndian.PutUint16(b, uint16(len(m)))
	copy(b[2:], m)
	_, err := w.Write(b)
	return err
}

// serve listens at addr, UDP and TCP, and forwards until ctx is done.
func (d dnsSplit) serve(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		return err
	}
	go func() { _ = d.serveUDP(pc) }()
	go func() { _ = d.serveTCP(l) }()
	context.AfterFunc(ctx, func() {
		_ = pc.Close()
		_ = l.Close()
	})
	return nil
}

// resolvConf is a resolv.conf as the forwarder rewrites it.
type resolvConf struct {
	// servers are its nameservers, the rest its other lines, kept.
	servers []string
	rest    []string
}

func parseResolvConf(b []byte) resolvConf {
	var rc resolvConf
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			rc.servers = append(rc.servers, f[1])
			continue
		}
		if strings.TrimSpace(line) != "" {
			rc.rest = append(rc.rest, line)
		}
	}
	return rc
}

// splitResolvConf points the container's resolver at the forwarder, and
// returns where every name outside the tailnet goes: the nameservers it
// had, kept at keep the first time, for a resolv.conf a start before
// rewrote already. Docker binds resolv.conf into the container, so it is
// written in place.
func splitResolvConf(path, keep string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	listen, _, _ := strings.Cut(SplitDNS, ":")
	rc := parseResolvConf(b)
	if len(rc.servers) == 0 || (len(rc.servers) == 1 && rc.servers[0] == listen) {
		kept, err := os.ReadFile(keep)
		if err != nil {
			return nil, fmt.Errorf("%s names no server but the forwarder, and none was kept: %w", path, err)
		}
		rc = parseResolvConf(kept)
		if len(rc.servers) == 0 {
			return nil, fmt.Errorf("%s names no server", keep)
		}
	} else if err := os.WriteFile(keep, b, 0o644); err != nil {
		return nil, err
	}
	var upstreams []string
	for _, s := range rc.servers {
		if a, err := netip.ParseAddr(s); err == nil {
			upstreams = append(upstreams, netip.AddrPortFrom(a, 53).String())
		}
	}
	out := "nameserver " + listen + "\n"
	for _, l := range rc.rest {
		out += l + "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return nil, err
	}
	return upstreams, nil
}
