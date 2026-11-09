package guestbin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest"
)

// A container that joins a tailnet (docs/decisions/tailnet.md): tower-guest
// init starts its tailscaled, TUN and its state in guest.TailnetDir, and
// its DNS forwarder (dns.go) before the image's entrypoint, and keeps
// guest.TailnetStatusFile saying how the join stands. tailnet-up, run as
// root by the container's driver with the key on its stdin, joins only a
// node with no state.

// tailnetPaths are where a container's tailnet lives; tests move them.
type tailnetPaths struct {
	dir, tailscaled, tailscale, socket, resolvConf string
}

var defaultTailnetPaths = tailnetPaths{
	dir: guest.TailnetDir, tailscaled: guest.Tailscaled, tailscale: guest.Tailscale,
	socket: guest.TailscaleSocket, resolvConf: "/etc/resolv.conf",
}

// joinFile, in the tailnet's directory, is how the last join tried went,
// while its node is still logged out: a rejected key says so until a key
// is given again.
const joinFile = "join.json"

// tailnetStatusEvery is how often init reads tailscaled's state into the
// status file.
const tailnetStatusEvery = 15 * time.Second

// tailscaledProc is init's tailscaled, started again when it ends.
type tailscaledProc struct {
	p   tailnetPaths
	pid int
}

// start starts tailscaled with its state in the tailnet's directory and a
// TUN device of its own. Its logs go to the container's, never uploaded.
func (t *tailscaledProc) start() error {
	cmd := exec.Command(t.p.tailscaled,
		"--state="+filepath.Join(t.p.dir, "tailscaled.state"), "--statedir="+t.p.dir,
		"--socket="+t.p.socket, "--tun=tailscale0", "--no-logs-no-support")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	t.pid = cmd.Process.Pid
	return nil
}

// startTailnet readies the container's tailnet before its entrypoint: the
// split DNS, then tailscaled, then a loop keeping the status file. A part
// that fails is said in the status file and the container's log; the
// container runs on.
func startTailnet(ctx context.Context, p tailnetPaths) (*tailscaledProc, error) {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p.socket), 0o755); err != nil {
		return nil, err
	}
	// The image's tailscale CLI, if it has none, finds tailscaled's socket
	// where it looks by default.
	if _, err := exec.LookPath("tailscale"); err != nil {
		_ = os.Symlink(p.tailscale, "/usr/local/bin/tailscale")
	}
	upstreams, err := splitResolvConf(p.resolvConf, filepath.Join(p.dir, "resolv.conf"))
	if err == nil {
		err = dnsSplit{upstreams: upstreams, magic: MagicDNS}.serve(ctx, SplitDNS)
	}
	if err != nil {
		writeTailnetStatus(p, guest.TailnetStatus{Reason: "Failed", Message: "its DNS forwarder did not start: " + err.Error()})
		return nil, fmt.Errorf("the DNS forwarder: %w", err)
	}
	t := &tailscaledProc{p: p}
	if err := t.start(); err != nil {
		writeTailnetStatus(p, guest.TailnetStatus{Reason: "Failed", Message: "tailscaled did not start: " + err.Error()})
		return nil, fmt.Errorf("tailscaled: %w", err)
	}
	writeTailnetStatus(p, guest.TailnetStatus{Reason: "Starting", Message: "tailscaled is starting"})
	go func() {
		for {
			refreshTailnetStatus(ctx, p)
			select {
			case <-ctx.Done():
				return
			case <-time.After(tailnetStatusEvery):
			}
		}
	}()
	return t, nil
}

// backend is what tailscale status --json says of the node.
type backend struct {
	BackendState string `json:"BackendState"`
	Self         *struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

// readBackend asks tailscaled's local API for the node's state, over its
// socket: no child process, which init's reaper would take from under a
// wait.
func readBackend(ctx context.Context, p tailnetPaths) (backend, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", p.socket)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/status?peers=false", nil)
	if err != nil {
		return backend{}, err
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	res, err := c.Do(req)
	if err != nil {
		return backend{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return backend{}, err
	}
	if res.StatusCode != http.StatusOK {
		return backend{}, fmt.Errorf("its local API answered %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var b backend
	if err := json.Unmarshal(body, &b); err != nil || b.BackendState == "" {
		return b, errors.New("its local API says no state")
	}
	return b, nil
}

// loggedOut reports whether a node has no state to come back with.
func (b backend) loggedOut() bool {
	return b.BackendState == "NeedsLogin" || b.BackendState == "NoState"
}

// statusOf is how the join stands, from tailscaled's state and the last
// join tried.
func statusOf(b backend, last *guest.TailnetStatus) guest.TailnetStatus {
	switch b.BackendState {
	case "Running":
		msg := "joined"
		if s := b.Self; s != nil {
			msg = strings.TrimSpace(fmt.Sprintf("joined as %s %s", strings.TrimSuffix(s.DNSName, "."), strings.Join(s.TailscaleIPs, " ")))
		}
		return guest.TailnetStatus{Reason: "Joined", Message: msg}
	case "NeedsLogin", "NoState":
		if last != nil {
			return *last
		}
		return guest.TailnetStatus{Reason: "NoKey", Message: "its node has no state, and no key was given to join with"}
	case "NeedsMachineAuth":
		return guest.TailnetStatus{Reason: "Failed", Message: "the tailnet holds the node for an admin to approve"}
	case "Stopped":
		return guest.TailnetStatus{Reason: "Failed", Message: "its node is down (tailscale down)"}
	}
	return guest.TailnetStatus{Reason: "Starting", Message: "tailscaled says " + b.BackendState}
}

func refreshTailnetStatus(ctx context.Context, p tailnetPaths) {
	b, err := readBackend(ctx, p)
	if err != nil {
		writeTailnetStatus(p, guest.TailnetStatus{Reason: "Starting", Message: "tailscaled does not answer yet: " + err.Error()})
		return
	}
	writeTailnetStatus(p, statusOf(b, lastJoin(p)))
}

// lastJoin is how the last join tried went, nil for none since its node
// last had state.
func lastJoin(p tailnetPaths) *guest.TailnetStatus {
	b, err := os.ReadFile(filepath.Join(p.dir, joinFile))
	if err != nil {
		return nil
	}
	var s guest.TailnetStatus
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// writeTailnetStatus writes s as the status file, whole.
func writeTailnetStatus(p tailnetPaths, s guest.TailnetStatus) {
	_ = writeJSONFile(filepath.Join(p.dir, guest.TailnetStatusFile), s)
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// keyRejected matches what tailscale up says of a key the control server
// refused: invalid, expired, or used up.
var keyRejected = regexp.MustCompile(`(?i)invalid key|auth ?key|key (has )?expired|not valid|unable to validate`)

// leaveOnly is init in a container its driver started only for its node
// to leave its tailnet (guest.TailnetLeaveFile): it starts tailscaled, logs
// the node out, stops tailscaled and returns, the container stopping with
// it; nothing of the image's runs, and the DNS is left as it is.
func leaveOnly(ctx context.Context, p tailnetPaths, stderr io.Writer) int {
	t := &tailscaledProc{p: p}
	if err := os.MkdirAll(filepath.Dir(p.socket), 0o755); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if err := t.start(); err != nil {
		return failed(stderr, ExitFailed, fmt.Errorf("tailscaled: %w", err))
	}
	code := tailnetDown(ctx, p, stderr)
	_ = syscall.Kill(t.pid, syscall.SIGTERM)
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(t.pid, &ws, 0, nil)
	return code
}

// tailnetFlags reads --hostname NAME and --accept-routes.
func tailnetFlags(args []string) (hostname string, acceptRoutes bool, err error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--hostname" && len(args) > 1:
			hostname, args = args[1], args[2:]
		case args[0] == "--accept-routes":
			acceptRoutes, args = true, args[1:]
		default:
			return "", false, fmt.Errorf("unknown argument %q", args[0])
		}
	}
	return hostname, acceptRoutes, nil
}

// tailnetSetMain is tower-guest tailnet-set --hostname NAME
// [--accept-routes]: the node's hostname and routes as its tailnet now
// asks, set on it as it runs, joined or not, with no restart.
func tailnetSetMain(args []string, stderr io.Writer) int {
	// It talks to tailscaled's socket alone: it needs no NET_ADMIN.
	if err := dropNetAdmin(); err != nil {
		return failed(stderr, ExitCannotStart, err)
	}
	return tailnetSet(context.Background(), defaultTailnetPaths, args, stderr)
}

func tailnetSet(ctx context.Context, p tailnetPaths, args []string, stderr io.Writer) int {
	hostname, acceptRoutes, err := tailnetFlags(args)
	if err != nil {
		return failed(stderr, 2, fmt.Errorf("tailnet-set: %w", err))
	}
	setArgs := []string{"--socket=" + p.socket, "set", fmt.Sprintf("--accept-routes=%t", acceptRoutes)}
	if hostname != "" {
		setArgs = append(setArgs, "--hostname="+hostname)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.tailscale, setArgs...).CombinedOutput()
	if err != nil {
		return failed(stderr, ExitFailed, fmt.Errorf("tailscale set: %w: %s", err, strings.TrimSpace(string(out))))
	}
	refreshTailnetStatus(ctx, p)
	return 0
}

// tailnetDownMain is tower-guest tailnet-down: the node logs out of its
// tailnet, which forgets it, before the container's driver takes its
// state away. A node that cannot log out (no tailscaled, no network) is
// no error: its state goes all the same.
func tailnetDownMain(stderr io.Writer) int {
	if err := dropNetAdmin(); err != nil {
		return failed(stderr, ExitCannotStart, err)
	}
	return tailnetDown(context.Background(), defaultTailnetPaths, stderr)
}

func tailnetDown(ctx context.Context, p tailnetPaths, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// A container started for its node to log out has a tailscaled just
	// starting: wait for it to answer.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if _, err := readBackend(ctx, p); err == nil {
			break
		}
	}
	if out, err := exec.CommandContext(ctx, p.tailscale, "--socket="+p.socket, "logout").CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(stderr, "tower-guest: tailscale logout: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(filepath.Join(p.dir, joinFile))
	return 0
}

// tailnetUpMain is tower-guest tailnet-up --hostname NAME [--accept-routes],
// the key on its stdin: the join, run once tower gives the key. A node
// with state is left as it is. It exits 0 once it tried, the status file
// saying how that went; another code says it could not try.
func tailnetUpMain(args []string, stdin io.Reader, stderr io.Writer) int {
	if err := dropNetAdmin(); err != nil {
		return failed(stderr, ExitCannotStart, err)
	}
	return tailnetUp(context.Background(), defaultTailnetPaths, args, stdin, stderr)
}

func tailnetUp(ctx context.Context, p tailnetPaths, args []string, stdin io.Reader, stderr io.Writer) int {
	hostname, acceptRoutes, err := tailnetFlags(args)
	if err != nil {
		return failed(stderr, 2, fmt.Errorf("tailnet-up: %w", err))
	}
	key, err := io.ReadAll(io.LimitReader(stdin, 4<<10))
	if err != nil {
		return failed(stderr, ExitFailed, fmt.Errorf("reading the key: %w", err))
	}
	key = bytes.TrimSpace(key)
	if len(key) == 0 {
		return failed(stderr, ExitFailed, errors.New("no key on stdin"))
	}
	var b backend
	for deadline := time.Now().Add(30 * time.Second); ; {
		if b, err = readBackend(ctx, p); err == nil && b.BackendState != "NoState" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return failed(stderr, ExitFailed, fmt.Errorf("tailscaled does not answer: %w", err))
	}
	if !b.loggedOut() {
		// Its node has state: it comes back as itself, with no key.
		writeTailnetStatus(p, statusOf(b, nil))
		return 0
	}
	upArgs := []string{"--socket=" + p.socket, "up", "--auth-key=file:/dev/stdin", "--accept-dns=false", "--timeout=2m"}
	if hostname != "" {
		upArgs = append(upArgs, "--hostname="+hostname)
	}
	if acceptRoutes {
		upArgs = append(upArgs, "--accept-routes")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, p.tailscale, upArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(key), &out, &out
	runErr := cmd.Run()
	words := strings.TrimSpace(strings.ReplaceAll(out.String(), string(key), "[key]"))
	if len(words) > 512 {
		words = words[len(words)-512:]
	}
	joinPath := filepath.Join(p.dir, joinFile)
	if runErr == nil {
		_ = os.Remove(joinPath)
		refreshTailnetStatus(ctx, p)
		return 0
	}
	s := guest.TailnetStatus{Reason: "Failed", Message: "tailscale up failed: " + words}
	if keyRejected.MatchString(words) {
		s = guest.TailnetStatus{Reason: "KeyRejected", Message: "the tailnet refused its key: " + words}
	}
	if err := writeJSONFile(joinPath, s); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	writeTailnetStatus(p, s)
	return 0
}
