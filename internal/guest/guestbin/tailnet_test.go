package guestbin

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// TestBoxUserIsResolvedAsDockerDoes: a user named by name or uid, with or
// without a group, takes its uid, gid, groups and HOME from the
// container's own files; a uid they lack runs as itself with gid 0.
func TestBoxUserIsResolvedAsDockerDoes(t *testing.T) {
	dir := t.TempDir()
	passwd, group := filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	write(t, passwd, "root:x:0:0:root:/root:/bin/sh\nvscode:x:1000:1000::/home/vscode:/bin/bash\n")
	write(t, group, "root:x:0:\nvscode:x:1000:\ndocker:x:999:vscode,other\nstaff:x:50:\n")
	for _, tt := range []struct {
		user string
		want guest.Identity
	}{
		{"", guest.Identity{UID: 0, GID: 0, Home: "/root"}},
		{"vscode", guest.Identity{UID: 1000, GID: 1000, Groups: []uint32{999}, Home: "/home/vscode"}},
		{"1000", guest.Identity{UID: 1000, GID: 1000, Groups: []uint32{999}, Home: "/home/vscode"}},
		{"vscode:staff", guest.Identity{UID: 1000, GID: 50, Groups: []uint32{999}, Home: "/home/vscode"}},
		{"4242:7", guest.Identity{UID: 4242, GID: 7, Home: "/"}},
		{"4242", guest.Identity{UID: 4242, GID: 0, Home: "/"}},
	} {
		got, err := lookupRunAs(tt.user, true, passwd, group)
		if err != nil {
			t.Errorf("%q: %v", tt.user, err)
			continue
		}
		if got.id.UID != tt.want.UID || got.id.GID != tt.want.GID || got.id.Home != tt.want.Home || !slices.Equal(got.id.Groups, tt.want.Groups) {
			t.Errorf("%q is %+v, want %+v", tt.user, got.id, tt.want)
		}
	}
	if _, err := lookupRunAs("nobody-here", false, passwd, group); err == nil {
		t.Error("a user the container's files do not name resolved")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// dropCapEnv names the capability dropCapHelper takes for a box's,
// dropUserEnv the user it becomes, and dropModeEnv the way: "become",
// "shim" (an exec through the shim, as the driver runs one with RunAsEnv)
// or "dropped" (tower-guest dropped, as init runs the image's command).
const (
	dropCapEnv  = "TOWER_GUESTBIN_TEST_DROPCAP"
	dropUserEnv = "TOWER_GUESTBIN_TEST_DROPUSER"
	dropModeEnv = "TOWER_GUESTBIN_TEST_DROPMODE"
)

// dropCapHelper takes a capability for a box's, as NET_ADMIN is, and has
// a child print its own /proc/self/status the way dropModeEnv says: what
// a command tower-guest starts in a box holds.
func dropCapHelper() int {
	c, err := strconv.Atoi(os.Getenv(dropCapEnv))
	if err != nil {
		return 2
	}
	boxCapability = uintptr(c)
	user := os.Getenv(dropUserEnv)
	status := []string{"/bin/cat", "/proc/self/status"}
	switch os.Getenv(dropModeEnv) {
	case "become":
		uid, _ := strconv.Atoi(cmp.Or(user, "0"))
		if err := become(boxUser{id: guest.Identity{UID: uint32(uid), GID: uint32(uid)}}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 4
		}
	case "shim":
		_ = os.Setenv(guest.RunAsEnv, user)
		// Its stdin stays open while the command runs, as a call's does.
		r, w, err := os.Pipe()
		if err != nil {
			return 6
		}
		defer func() { _ = w.Close() }()
		if err := WriteHeader(w, Header{Op: OpExec, Command: status}); err != nil {
			return 6
		}
		_ = WriteFrame(w, FrameEOF, nil)
		return shim(r, os.Stdout, os.Stderr)
	case "dropped":
		return droppedMain(append([]string{"--as", user, "--"}, status...))
	}
	out, err := exec.Command(status[0], status[1:]...).Output()
	if err != nil {
		return 5
	}
	_, _ = os.Stdout.Write(out)
	return 0
}

// capSets reads a /proc status's capability sets, by their names (CapBnd,
// CapEff, ...).
func capSets(t *testing.T, status string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for line := range strings.Lines(status) {
		name, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.HasPrefix(name, "Cap") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		out[name] = n
	}
	return out
}

// TestDroppedCapabilityIsGoneForWhatRunsAfter: a box's capability is in
// none of the sets of a command tower-guest starts, root's or another
// user's: after become, through the shim the driver runs with RunAsEnv,
// and through dropped, as init runs the image's command. It takes
// CAP_NET_BIND_SERVICE for NET_ADMIN, which a test process holds where it
// may, as root with CAP_SETPCAP.
func TestDroppedCapabilityIsGoneForWhatRunsAfter(t *testing.T) {
	t.Parallel()
	const capability = unix.CAP_NET_BIND_SERVICE
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc")
	}
	own := capSets(t, string(b))
	if own["CapEff"]&(1<<unix.CAP_SETPCAP) == 0 || own["CapBnd"]&(1<<capability) == 0 {
		t.Skip("needs root with CAP_SETPCAP and CAP_NET_BIND_SERVICE")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"become", "shim", "dropped"} {
		for _, user := range []string{"", "65534"} {
			cmd := exec.Command(self)
			cmd.Env = append(os.Environ(), helperEnv+"="+helperDropCap, dropCapEnv+"="+strconv.Itoa(capability), dropUserEnv+"="+user, dropModeEnv+"="+mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s as %q: %v: %s", mode, user, err, stderr.String())
			}
			sets := capSets(t, string(out))
			for _, set := range []string{"CapBnd", "CapEff", "CapPrm", "CapInh", "CapAmb"} {
				if sets[set]&(1<<capability) != 0 {
					t.Errorf("%s as %q: a command holds the capability in %s: %x", mode, user, set, sets[set])
				}
			}
			if sets["CapBnd"]&(1<<unix.CAP_CHOWN) == 0 {
				t.Errorf("%s as %q: the drop took more than the capability: CapBnd %x", mode, user, sets["CapBnd"])
			}
			if user != "" && sets["CapEff"] != 0 {
				t.Errorf("%s: a command run as %s holds capabilities: %x", mode, user, sets["CapEff"])
			}
		}
	}
}

// dnsServer answers every query on a UDP and a TCP socket of its own, its
// one A record addr, and counts what it answered.
type dnsServer struct {
	addr     string
	answered atomic.Int32
}

func newDNSServer(t *testing.T, a netip.Addr) *dnsServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = l.Close() })
	s := &dnsServer{addr: pc.LocalAddr().String()}
	answer := func(q []byte) []byte {
		var m dnsmessage.Message
		if err := m.Unpack(q); err != nil || len(m.Questions) == 0 {
			return nil
		}
		s.answered.Add(1)
		m.Response = true
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
			Body:   &dnsmessage.AResource{A: a.As4()},
		}}
		b, _ := m.Pack()
		return b
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if a := answer(buf[:n]); a != nil {
				_, _ = pc.WriteTo(a, from)
			}
		}
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				q, err := readTCPMessage(c)
				if err == nil {
					_ = writeTCPMessage(c, answer(q))
				}
			}()
		}
	}()
	return s
}

// resolve asks the forwarder at addr for name over network, and returns
// the address it answered.
func resolve(t *testing.T, network, addr, name string) netip.Addr {
	t.Helper()
	m := query(t, network, addr, name)
	if len(m.Answers) != 1 {
		t.Fatalf("%s over %s: %d answers", name, network, len(m.Answers))
	}
	return netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A)
}

// query asks the forwarder at addr for name over network, as ID 7, and
// returns its answer.
func query(t *testing.T, network, addr, name string) dnsmessage.Message {
	t.Helper()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true}, Questions: []dnsmessage.Question{{
		Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}}}
	b, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// A forwarder that stays silent fails the query at once.
	_ = c.SetDeadline(time.Now().Add(3 * dnsTimeout))
	var a []byte
	if network == "udp" {
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("%s over %s: %v", name, network, err)
		}
		a = buf[:n]
	} else {
		if err := writeTCPMessage(c, b); err != nil {
			t.Fatal(err)
		}
		if a, err = readTCPMessage(c); err != nil {
			t.Fatalf("%s over %s: %v", name, network, err)
		}
	}
	var m dnsmessage.Message
	if err := m.Unpack(a); err != nil {
		t.Fatalf("%s over %s: %v", name, network, err)
	}
	return m
}

// TestDNSSplitsTheTailnetFromTheRest: names under ts.net go to MagicDNS,
// every other name, a peer's alias included, to the container's own
// servers, the first that answers, over UDP and TCP alike.
func TestDNSSplitsTheTailnetFromTheRest(t *testing.T) {
	t.Parallel()
	embedded := newDNSServer(t, netip.MustParseAddr("172.18.0.5"))
	magic := newDNSServer(t, netip.MustParseAddr("100.64.0.9"))
	// A server that does not answer, which a resolver passes over.
	dead, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()
	d := dnsSplit{upstreams: []string{dead.LocalAddr().String(), embedded.addr}, magic: magic.addr}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = l.Close() })
	go func() { _ = d.serveUDP(pc) }()
	go func() { _ = d.serveTCP(l) }()
	for _, tt := range []struct{ network, addr string }{{"udp", pc.LocalAddr().String()}, {"tcp", l.Addr().String()}} {
		for name, want := range map[string]string{
			"box-a.tail1234.ts.net.": "100.64.0.9",
			"Box-B.TAIL1234.TS.NET.": "100.64.0.9",
			"peer-alias.":            "172.18.0.5",
			"github.com.":            "172.18.0.5",
			"ts.net.example.com.":    "172.18.0.5",
			"notts.net.":             "172.18.0.5",
		} {
			if got := resolve(t, tt.network, tt.addr, name); got.String() != want {
				t.Errorf("%s over %s went to %s, want %s", name, tt.network, got, want)
			}
		}
	}
	if magic.answered.Load() == 0 || embedded.answered.Load() == 0 {
		t.Errorf("answered: magic %d, embedded %d", magic.answered.Load(), embedded.answered.Load())
	}
}

// TestResolvConfPointsAtTheForwarder: the container's resolv.conf names
// the forwarder alone and keeps its other lines, and the servers it named
// are where the rest goes, at its first start and every later one.
func TestResolvConfPointsAtTheForwarder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, keep := filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "kept")
	write(t, path, "# Generated by Docker Engine.\nnameserver 127.0.0.11\nsearch example.internal\noptions ndots:0\n")
	for range 2 {
		upstreams, err := splitResolvConf(path, keep)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(upstreams, []string{"127.0.0.11:53"}) {
			t.Errorf("upstreams %v", upstreams)
		}
		b, _ := os.ReadFile(path)
		want := "nameserver 127.0.0.53\n# Generated by Docker Engine.\nsearch example.internal\noptions ndots:0\n"
		if string(b) != want {
			t.Errorf("resolv.conf is %q, want %q", b, want)
		}
	}
}

// fakeTailnet is a tailscale CLI and a tailscaled local API: the CLI
// records its arguments and stdin, and up makes the node Running unless
// it is told to refuse the key, which it quotes, as a control server's
// error may.
type fakeTailnet struct {
	p                        tailnetPaths
	args, stdin, ran, refuse string
	state                    atomic.Pointer[string]
}

func newFakeTailnet(t *testing.T, state string) *fakeTailnet {
	t.Helper()
	return newFakeTailnetAfter(t, state, 0)
}

// newFakeTailnetAfter is newFakeTailnet whose tailscaled answers only
// after a while, as one just started does: its CLI's logout fails until
// then, as tailscale's does with no socket to reach.
func newFakeTailnetAfter(t *testing.T, state string, after time.Duration) *fakeTailnet {
	t.Helper()
	dir := t.TempDir()
	// A short path: a unix socket's is bounded.
	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	f := &fakeTailnet{
		p:    tailnetPaths{dir: filepath.Join(dir, "state"), tailscale: filepath.Join(dir, "tailscale"), socket: filepath.Join(sockDir, "s")},
		args: filepath.Join(dir, "args"), stdin: filepath.Join(dir, "stdin"), ran: filepath.Join(dir, "ran"), refuse: filepath.Join(dir, "refuse"),
	}
	if err := os.MkdirAll(f.p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.state.Store(&state)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" >%[1]s
if [ "$2" = logout ]; then
	[ -S %[5]s ] || { echo "no tailscaled at %[5]s" >&2; exit 1; }
	touch %[1]s.logout
	exit 0
fi
cat >%[2]s
touch %[3]s
if [ -e %[4]s ]; then echo "backend error: invalid key: $(cat %[2]s) not valid" >&2; exit 1; fi
`, f.args, f.stdin, f.ran, f.refuse, f.p.socket)
	write(t, f.p.tailscale, script)
	if err := os.Chmod(f.p.tailscale, 0o755); err != nil {
		t.Fatal(err)
	}
	listen := func() net.Listener {
		l, err := net.Listen("unix", f.p.socket)
		if err != nil {
			panic(err)
		}
		return l
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := *f.state.Load()
		if _, err := os.Stat(f.ran); err == nil {
			if _, err := os.Stat(f.refuse); err != nil {
				st = "Running"
			}
		}
		b := backend{BackendState: st}
		if st == "Running" {
			b.Self = &struct {
				DNSName      string   `json:"DNSName"`
				TailscaleIPs []string `json:"TailscaleIPs"`
			}{DNSName: "box-a.tail1234.ts.net.", TailscaleIPs: []string{"100.64.0.9"}}
		}
		_ = json.NewEncoder(w).Encode(b)
	})}
	if after == 0 {
		l := listen()
		go func() { _ = srv.Serve(l) }()
	} else {
		go func() {
			<-time.After(after)
			_ = srv.Serve(listen())
		}()
	}
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeTailnet) status(t *testing.T) guest.TailnetStatus {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.p.dir, guest.TailnetStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var s guest.TailnetStatus
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

const testKey = "tskey-auth-kTest123-SECRETSECRET"

// TestTailnetUpJoinsANodeWithoutState: the key reaches tailscale up on its
// stdin, never in its arguments, with the hostname and routes asked, and
// the status says it joined.
func TestTailnetUpJoinsANodeWithoutState(t *testing.T) {
	t.Parallel()
	f := newFakeTailnet(t, "NeedsLogin")
	var stderr bytes.Buffer
	if code := tailnetUp(context.Background(), f.p, []string{"--hostname", "box-a", "--accept-routes"}, strings.NewReader(testKey+"\n"), &stderr); code != 0 {
		t.Fatalf("tailnet-up exited %d: %s", code, stderr.String())
	}
	args, _ := os.ReadFile(f.args)
	stdin, _ := os.ReadFile(f.stdin)
	if strings.Contains(string(args), testKey) {
		t.Errorf("the key is in tailscale's arguments: %q", args)
	}
	if string(stdin) != testKey {
		t.Errorf("tailscale read %q on its stdin", stdin)
	}
	for _, want := range []string{"up\n", "--auth-key=file:/dev/stdin\n", "--hostname=box-a\n", "--accept-routes\n", "--accept-dns=false\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("tailscale up was not given %q: %q", want, args)
		}
	}
	if s := f.status(t); s.Reason != provider.TailnetJoined || !strings.Contains(s.Message, "100.64.0.9") {
		t.Errorf("status %+v", s)
	}
}

// TestTailnetUpLeavesANodeWithState: a node that has state is not joined
// again, whatever key is given.
func TestTailnetUpLeavesANodeWithState(t *testing.T) {
	t.Parallel()
	f := newFakeTailnet(t, "Running")
	if code := tailnetUp(context.Background(), f.p, []string{"--hostname", "box-a"}, strings.NewReader(testKey), &bytes.Buffer{}); code != 0 {
		t.Fatalf("tailnet-up exited %d", code)
	}
	if _, err := os.Stat(f.ran); err == nil {
		t.Error("tailscale up ran for a node with state")
	}
	if s := f.status(t); s.Reason != provider.TailnetJoined {
		t.Errorf("status %+v", s)
	}
}

// TestTailnetUpSaysAKeyWasRejected: a key the tailnet refuses is said so,
// without the key, and stays said while the node has no state, as init's
// status loop reads it again.
func TestTailnetUpSaysAKeyWasRejected(t *testing.T) {
	t.Parallel()
	f := newFakeTailnet(t, "NeedsLogin")
	write(t, f.refuse, "")
	if code := tailnetUp(context.Background(), f.p, []string{"--hostname", "box-a"}, strings.NewReader(testKey), &bytes.Buffer{}); code != 0 {
		t.Fatalf("tailnet-up exited %d", code)
	}
	for _, s := range []guest.TailnetStatus{f.status(t), func() guest.TailnetStatus { refreshTailnetStatus(context.Background(), f.p); return f.status(t) }()} {
		if s.Reason != provider.TailnetKeyRejected {
			t.Errorf("status %+v, want KeyRejected", s)
		}
		if strings.Contains(s.Message, testKey) {
			t.Errorf("the status quotes the key: %q", s.Message)
		}
	}
	entries, _ := os.ReadDir(f.p.dir)
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(f.p.dir, e.Name())); bytes.Contains(b, []byte(testKey)) {
			t.Errorf("%s holds the key", e.Name())
		}
	}
}

// TestTailnetStatusReasonsAreTheProviders: what the guest says is one of
// provider.InstanceTailnetJoined's reasons.
func TestTailnetStatusReasonsAreTheProviders(t *testing.T) {
	t.Parallel()
	reasons := []string{provider.TailnetJoined, provider.TailnetKeyRejected, provider.TailnetNoKey, provider.TailnetStarting, provider.TailnetFailed}
	for _, state := range []string{"Running", "NeedsLogin", "NoState", "NeedsMachineAuth", "Stopped", "Starting"} {
		if s := statusOf(backend{BackendState: state}, nil); !slices.Contains(reasons, s.Reason) {
			t.Errorf("%s says %q", state, s.Reason)
		}
	}
}

// TestTailnetSetChangesTheNodeAsItRuns: a new hostname and routes reach
// tailscale set, routes off said as such, with no key.
func TestTailnetSetChangesTheNodeAsItRuns(t *testing.T) {
	t.Parallel()
	f := newFakeTailnet(t, "Running")
	var stderr bytes.Buffer
	if code := tailnetSet(context.Background(), f.p, []string{"--hostname", "box-b"}, &stderr); code != 0 {
		t.Fatalf("tailnet-set exited %d: %s", code, stderr.String())
	}
	args, _ := os.ReadFile(f.args)
	for _, want := range []string{"set\n", "--hostname=box-b\n", "--accept-routes=false\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("tailscale set was not given %q: %q", want, args)
		}
	}
}

// TestTailnetDownLogsOut: leaving, the node logs out, and a rejection
// said before is not said of its next join.
func TestTailnetDownLogsOut(t *testing.T) {
	t.Parallel()
	f := newFakeTailnet(t, "Running")
	write(t, filepath.Join(f.p.dir, joinFile), `{"reason":"KeyRejected"}`)
	if code := tailnetDown(context.Background(), f.p, &bytes.Buffer{}); code != 0 {
		t.Fatalf("tailnet-down exited %d", code)
	}
	if args, _ := os.ReadFile(f.args); !strings.Contains(string(args), "logout\n") {
		t.Errorf("tailscale was run with %q, not logout", args)
	}
	if lastJoin(f.p) != nil {
		t.Error("the last join tried outlived the node")
	}
}

// TestDNSSaysServFailWhenNoServerAnswers: a query no server of its route
// answers is answered SERVFAIL, over UDP and TCP, its ID and question its
// own, so the resolver asking fails at once.
func TestDNSSaysServFailWhenNoServerAnswers(t *testing.T) {
	t.Parallel()
	dead, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.LocalAddr().String()
	_ = dead.Close()
	d := dnsSplit{upstreams: []string{deadAddr}, magic: deadAddr}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = l.Close() })
	go func() { _ = d.serveUDP(pc) }()
	go func() { _ = d.serveTCP(l) }()
	for _, tt := range []struct{ network, addr string }{{"udp", pc.LocalAddr().String()}, {"tcp", l.Addr().String()}} {
		for _, name := range []string{"github.com.", "box-a.tail1234.ts.net."} {
			m := query(t, tt.network, tt.addr, name)
			if m.RCode != dnsmessage.RCodeServerFailure || !m.Response || m.ID != 7 || len(m.Questions) != 1 || m.Questions[0].Name.String() != name {
				t.Errorf("%s over %s: %+v", name, tt.network, m)
			}
		}
	}
}

// TestInitRunsTheEntrypointDropped: in a container that joins a tailnet,
// init runs the image's command through dropped, as the image's user; in
// any other, as it is.
func TestInitRunsTheEntrypointDropped(t *testing.T) {
	t.Parallel()
	const self = "/.tower/tower-guest"
	args := []string{"--tailnet", "--as", "vscode", "--as-home", "--", "/docker-entrypoint.sh", "nginx"}
	tailnet, leave, got, err := initPlan(self, args, false)
	want := []string{self, "dropped", "--as", "vscode", "--as-home", "--", "/docker-entrypoint.sh", "nginx"}
	if err != nil || !tailnet || leave || !slices.Equal(got, want) {
		t.Errorf("init runs %q (tailnet %v, leave %v, %v), want %q", got, tailnet, leave, err, want)
	}
	// Started only for its node to leave: nothing of the image's runs.
	if tailnet, leave, got, err := initPlan(self, args, true); err != nil || !tailnet || !leave || len(got) != 0 {
		t.Errorf("asked to leave, init runs %q (tailnet %v, leave %v, %v)", got, tailnet, leave, err)
	}
	tailnet, leave, got, err = initPlan(self, []string{"--", "/docker-entrypoint.sh", "nginx"}, true)
	if err != nil || tailnet || leave || !slices.Equal(got, []string{"/docker-entrypoint.sh", "nginx"}) {
		t.Errorf("without a tailnet, init runs %q (tailnet %v, leave %v, %v)", got, tailnet, leave, err)
	}
	if _, _, got, _ := initPlan(self, []string{"--tailnet", "--as", "x"}, false); len(got) != 0 {
		t.Errorf("no command is %q", got)
	}
}

// TestGiveDirFollowsNoLink: give-dir makes and gives an empty directory,
// leaves one with something in it as it is, and never follows a link a
// box put where the directory goes.
func TestGiveDirFollowsNoLink(t *testing.T) {
	t.Parallel()
	if os.Getuid() != 0 {
		t.Skip("gives a directory away, which root alone may")
	}
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	if err := giveDir(repos, 4242, 4243); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(repos); err != nil || st.Sys().(*syscall.Stat_t).Uid != 4242 {
		t.Errorf("an empty directory was not given: %v", err)
	}
	target := filepath.Join(root, "etc")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := giveDir(link, 4242, 4243); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(target); st.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Error("give-dir followed a link to what it names")
	}
	if st, _ := os.Lstat(link); st.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Error("give-dir gave away a link the box put where the directory goes")
	}
	full := filepath.Join(root, "full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := giveDir(full, 4242, 4243); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(full); st.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Error("a directory with something in it was given")
	}
}

// TestTailnetDownWaitsForTailscaled: leaving with a tailscaled just
// started, as a stopped box's is, logs the node out once it answers.
func TestTailnetDownWaitsForTailscaled(t *testing.T) {
	t.Parallel()
	f := newFakeTailnetAfter(t, "Running", time.Second)
	var stderr bytes.Buffer
	if code := tailnetDown(context.Background(), f.p, &stderr); code != 0 {
		t.Fatalf("tailnet-down exited %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(f.args + ".logout"); err != nil {
		t.Errorf("the node did not log out: %s", stderr.String())
	}
}

// TestLeaveOnlyLogsOutAndStops: init asked only to leave starts
// tailscaled, logs the node out and stops tailscaled, starting nothing
// else.
func TestLeaveOnlyLogsOutAndStops(t *testing.T) {
	t.Parallel()
	f := newFakeTailnetAfter(t, "Running", 200*time.Millisecond)
	pidFile := filepath.Join(t.TempDir(), "pid")
	f.p.tailscaled = filepath.Join(t.TempDir(), "tailscaled")
	write(t, f.p.tailscaled, "#!/bin/sh\necho $$ >"+pidFile+"\nexec sleep 600\n")
	if err := os.Chmod(f.p.tailscaled, 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := leaveOnly(context.Background(), f.p, &stderr); code != 0 {
		t.Fatalf("leave exited %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(f.args + ".logout"); err != nil {
		t.Errorf("the node did not log out: %s", stderr.String())
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal("tailscaled was not started")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("tailscaled (%d) still runs", pid)
	}
}
