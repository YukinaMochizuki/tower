// Package docker is hostd's container driver, on Docker Engine through
// its Go client (driver.Containers). Each container runs tower-guest init
// as its first process, which runs the image's own entrypoint and reaps
// orphans, with its disk mounted at /workspace, tower-guest bound
// read-only at /.tower/tower-guest and its processes' directory, under
// hostd's state directory, at /.tower/processes; everything done in it
// goes through tower-guest shim, by docker exec (internal/guest/guestops).
// Its workstream networks are Docker bridge networks, which the VM driver
// puts its VMs on too (Networks); a guest on no network is on Docker's
// default bridge. A container that joins a tailnet runs as root, its
// tailscaled alone keeping NET_ADMIN, and everything else as the image's
// user without it (docs/decisions/tailnet.md). What the driver makes
// carries the label tower.hangarlab.cloud/driver, and its spec as a label
// of its own; what does not is never listed, changed or removed.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Labels the driver sets on what it makes, beside hostd's.
const (
	// LabelDriver says which driver made it: "container", "vm" (a VM's
	// container, internal/hostd/driver/chvm) or "network".
	LabelDriver = "tower.hangarlab.cloud/driver"
	// LabelSpec is a guest's spec as it was made, as JSON.
	LabelSpec = "tower.hangarlab.cloud/spec"
)

// Paths in a container.
const (
	Workspace    = "/workspace"
	ReposDir     = "/repos"
	GuestBinary  = "/.tower/tower-guest"
	ProcessesDir = "/.tower/processes"
)

// DefaultNetwork is the network of a guest on none: Docker's own bridge.
const DefaultNetwork = "bridge"

// BridgePrefix begins the Linux name of every workstream network's
// bridge, so the host's firewall tells hostd's guests from every other
// container on the host (deploy/hostd/guest-network.nft). Not "tower-":
// tower-build0, the build steps' bridge, has rules of its own.
const BridgePrefix = "twr"

// BridgeName is the Linux name of a workstream network's bridge:
// BridgePrefix and a hash of the network's name, within the 15 bytes an
// interface name holds.
func BridgeName(network string) string {
	sum := sha256.Sum256([]byte(network))
	return BridgePrefix + hex.EncodeToString(sum[:6])
}

// Config configures the driver.
type Config struct {
	// Client is Docker's.
	Client *client.Client
	// StateDir is hostd's state directory: the containers' processes are
	// kept in StateDir/processes/<container>.
	StateDir string
	// GuestBinary is tower-guest on the host, statically linked.
	GuestBinary string
	// Tailscaled and Tailscale are tailscale's daemon and CLI on the host,
	// bound into each container that joins a tailnet, whose state is kept
	// in StateDir/tailnet/<container>; empty, the host makes none.
	Tailscaled, Tailscale string
	// DNS are the servers a container resolves through (Docker's resolver
	// forwards to them), public ones: the host's firewall drops what a
	// guest sends to a private address, the host's own resolver's among
	// them (deploy/hostd/guest-network.nft). Empty is the host's.
	DNS []netip.Addr
	// StopTimeout is how long a container has to stop before it is killed;
	// zero means 10 seconds.
	StopTimeout time.Duration
}

// Driver is the container driver. It is safe for concurrent use.
type Driver struct {
	cfg Config
	cli *client.Client
	// mu serializes Create, so one of a name is made once.
	mu sync.Mutex
}

var _ driver.Containers = (*Driver)(nil)

// New checks Docker answers and tower-guest is there.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	if _, err := os.Stat(cfg.GuestBinary); err != nil {
		return nil, fmt.Errorf("tower-guest: %w", err)
	}
	for _, p := range []string{cfg.Tailscaled, cfg.Tailscale} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("tailscale: %w", err)
		}
	}
	if _, err := cfg.Client.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	return &Driver{cfg: cfg, cli: cfg.Client}, nil
}

// Dial makes a client of Docker at host, such as unix:///run/docker.sock;
// empty is Docker's default.
func Dial(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.New(opts...)
}

func (d *Driver) Version(ctx context.Context) (string, error) {
	v, err := d.cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", err
	}
	return "docker " + v.Version, nil
}

// spec is what LabelSpec holds.
type spec struct {
	Disk    string `json:"disk"`
	Network string `json:"network,omitempty"`
	Alias   string `json:"alias,omitempty"`
	CPUs    int    `json:"cpus,omitempty"`
	Memory  uint64 `json:"memory,omitempty"`
	Image   string `json:"image"`
	// Tailnet is the tailnet it joins, nil for none. User is then the
	// image's user, which the container, run as root, runs all but its
	// tailscaled as.
	Tailnet *driver.ContainerTailnet `json:"tailnet,omitempty"`
	User    string                   `json:"user,omitempty"`
}

// user is the user its processes run as: the image's.
func (s spec) user(c container.InspectResponse) string {
	if s.Tailnet != nil {
		return s.User
	}
	if c.Config != nil {
		return c.Config.User
	}
	return ""
}

func specOf(labels map[string]string) (spec, bool) {
	var s spec
	if labels[LabelDriver] != "container" || json.Unmarshal([]byte(labels[LabelSpec]), &s) != nil {
		return spec{}, false
	}
	return s, true
}

// hostdLabels are labels without the driver's own.
func hostdLabels(labels map[string]string) driver.Labels {
	out := driver.Labels{}
	for k, v := range labels {
		if k != LabelDriver && k != LabelSpec {
			out[k] = v
		}
	}
	return out
}

// BoxPidsLimit is the most processes and threads a container runs at once.
const BoxPidsLimit = 8192

func ptr[T any](v T) *T { return &v }

func (d *Driver) processesDir(name string) string {
	return filepath.Join(d.cfg.StateDir, "processes", name)
}

// tailnetDir is where a container that joins a tailnet keeps its node's
// state, on the host: not on its disk, which a boost or a golden copies.
func (d *Driver) tailnetDir(name string) string {
	return filepath.Join(d.cfg.StateDir, "tailnet", name)
}

// tailnetControl is the directory bound read-only into a container that
// joins a tailnet at guest.TailnetControlDir, where the driver tells its
// init to leave (guest.TailnetLeaveFile): beside tailnetSettings, out of
// the box's reach.
func (d *Driver) tailnetControl(name string) string {
	return filepath.Join(d.cfg.StateDir, "tailnet-control", name)
}

// tailnetSettings is where a container's tailnet's hostname and routes are
// kept as SetTailnet last set them on its node, which its spec label, fixed
// at its making, cannot: outside the directory bound into it.
func (d *Driver) tailnetSettings(name string) string {
	return filepath.Join(d.cfg.StateDir, "tailnet-settings", name+".json")
}

// tailnetOf is the tailnet a container joins now: its spec's, with the
// hostname and routes SetTailnet last set.
func (d *Driver) tailnetOf(name string, s spec) *driver.ContainerTailnet {
	if s.Tailnet == nil {
		return nil
	}
	t := *s.Tailnet
	if b, err := os.ReadFile(d.tailnetSettings(name)); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	return &t
}

func (d *Driver) Create(ctx context.Context, cs driver.ContainerSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.create(ctx, cs)
}

// create is Create, d.mu held.
func (d *Driver) create(ctx context.Context, cs driver.ContainerSpec) error {
	want := spec{Disk: cs.Disk, Network: cs.Network, Alias: cs.Alias, CPUs: cs.CPUs, Memory: cs.Memory, Image: cs.Image, Tailnet: cs.Tailnet}
	if cur, err := d.cli.ContainerInspect(ctx, cs.Name, client.ContainerInspectOptions{}); err == nil {
		have, ours := specOf(cur.Container.Config.Labels)
		if !ours || have.Image != want.Image || have.Disk != want.Disk || have.Network != want.Network || have.Alias != want.Alias ||
			(have.Tailnet == nil) != (want.Tailnet == nil) {
			return fmt.Errorf("container %s is there, made otherwise: %w", cs.Name, driver.ErrConflict)
		}
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	if st, err := os.Stat(cs.Disk); err != nil || !st.IsDir() {
		return fmt.Errorf("disk %s: %w", cs.Disk, driver.ErrNotFound)
	}
	netName := cs.Network
	if netName == "" {
		netName = DefaultNetwork
	} else if _, ok, err := (Networks{d.cli}).get(ctx, cs.Network); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("network %s: %w", cs.Network, driver.ErrNotFound)
	}
	img, err := d.cli.ImageInspect(ctx, cs.Image)
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("image %s is not pulled: %w", cs.Image, driver.ErrNotFound)
	}
	if err != nil {
		return err
	}
	if cs.Tailnet != nil && (d.cfg.Tailscaled == "" || d.cfg.Tailscale == "") {
		return fmt.Errorf("container %s joins a tailnet, and this host has no tailscale (hostd's --tailscaled and --tailscale): %w", cs.Name, driver.ErrStartFailed)
	}
	// The image's own entrypoint and command run under init.
	var argv []string
	if c := img.Config; c != nil {
		argv = append(slices.Clone(c.Entrypoint), c.Cmd...)
	}
	var imageEnv []string
	if img.Config != nil {
		imageEnv = img.Config.Env
		if cs.Tailnet != nil {
			want.User = img.Config.User
		}
	}
	specJSON, err := json.Marshal(want)
	if err != nil {
		return err
	}
	labels := maps.Clone(cs.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelDriver], labels[LabelSpec] = "container", string(specJSON)
	procs := d.processesDir(cs.Name)
	if err := os.MkdirAll(procs, 0o700); err != nil {
		return err
	}
	stop := int(d.cfg.StopTimeout / time.Second)
	initArgs := []string{GuestBinary, "init"}
	if cs.Tailnet != nil {
		initArgs = append(initArgs, "--tailnet", "--as", want.User)
		if !homeSet(imageEnv, cs.Env) {
			initArgs = append(initArgs, "--as-home")
		}
	}
	cfg := &container.Config{
		Image: cs.Image, Labels: labels, Hostname: hostname(cs),
		Entrypoint: append(append(initArgs, "--"), argv...), Cmd: []string{},
		StopTimeout: &stop,
	}
	if img.Config != nil {
		cfg.WorkingDir, cfg.User = img.Config.WorkingDir, img.Config.User
	}
	if cs.Tailnet != nil {
		// Its init starts tailscaled as root, then runs the image's
		// command as its user without NET_ADMIN.
		cfg.User = "0"
	}
	// Over the image's, as docker run -e gives them: each value as it is.
	for _, k := range slices.Sorted(maps.Keys(cs.Env)) {
		cfg.Env = append(cfg.Env, k+"="+cs.Env[k])
	}
	if cs.Tailnet != nil {
		for _, dir := range []string{d.tailnetDir(cs.Name), d.tailnetControl(cs.Name)} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
	}
	host := d.hostConfig(cs, netName, procs)
	endpoint := &network.EndpointSettings{}
	if cs.Alias != "" && netName != DefaultNetwork {
		endpoint.Aliases = []string{cs.Alias}
	}
	_, err = d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: cs.Name, Config: cfg, HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: endpoint}},
	})
	if cerrdefs.IsConflict(err) {
		return fmt.Errorf("container %s: %w: %w", cs.Name, err, driver.ErrConflict)
	}
	return err
}

// hostConfig is how Docker is to run a container of cs on netName, its
// processes kept in procs: a TUN device, NET_ADMIN and tailscale for one
// that joins a tailnet alone.
func (d *Driver) hostConfig(cs driver.ContainerSpec, netName, procs string) *container.HostConfig {
	host := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: cs.Disk, Target: Workspace},
			{Type: mount.TypeBind, Source: d.cfg.GuestBinary, Target: GuestBinary, ReadOnly: true},
			{Type: mount.TypeBind, Source: procs, Target: ProcessesDir},
		},
		// What tower writes into a box that no disk may hold, its secret
		// files, in memory: emptied as the container stops, written again
		// at each start. Its user is any the image runs as, so the
		// directory is everyone's, as /tmp is; the box is its boundary.
		Tmpfs: map[string]string{driver.SecretsDir: "mode=1777,size=16m,noexec,nosuid,nodev"},
		// No raw sockets (no spoofing on the workstream's network), no
		// privilege gained through setuid (sudo elevates nothing in a box),
		// and a bound on its processes.
		CapDrop:       []string{"NET_RAW"},
		SecurityOpt:   []string{"no-new-privileges"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Resources:     container.Resources{NanoCPUs: int64(cs.CPUs) * 1e9, Memory: int64(cs.Memory), PidsLimit: ptr(int64(BoxPidsLimit))},
		NetworkMode:   container.NetworkMode(netName),
		DNS:           d.cfg.DNS,
	}
	if cs.Tailnet != nil {
		// tailscaled's TUN device and NET_ADMIN, which init drops for all
		// else (docs/decisions/tailnet.md).
		host.CapAdd = []string{"NET_ADMIN"}
		host.Devices = []container.DeviceMapping{{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"}}
		host.Mounts = append(host.Mounts,
			mount.Mount{Type: mount.TypeBind, Source: d.cfg.Tailscaled, Target: guest.Tailscaled, ReadOnly: true},
			mount.Mount{Type: mount.TypeBind, Source: d.cfg.Tailscale, Target: guest.Tailscale, ReadOnly: true},
			mount.Mount{Type: mount.TypeBind, Source: d.tailnetDir(cs.Name), Target: guest.TailnetDir},
			mount.Mount{Type: mount.TypeBind, Source: d.tailnetControl(cs.Name), Target: guest.TailnetControlDir, ReadOnly: true},
		)
	}
	return host
}

// homeSet reports whether a container's environment sets HOME, its
// image's (each "NAME=value") or its spec's; one that does not takes its
// user's from its passwd entry, as Docker gives it.
func homeSet(image []string, env map[string]string) bool {
	if _, ok := env["HOME"]; ok {
		return true
	}
	return slices.ContainsFunc(image, func(e string) bool { return strings.HasPrefix(e, "HOME=") })
}

// hostname is a container's: its alias, or its name.
func hostname(cs driver.ContainerSpec) string {
	h := cs.Alias
	if h == "" {
		h = cs.Name
	}
	if len(h) > 63 {
		h = h[:63]
	}
	return h
}

// inspect reads one of the driver's containers; one not there, or not
// the driver's, is ErrNotFound.
func (d *Driver) inspect(ctx context.Context, name string) (container.InspectResponse, spec, error) {
	r, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return container.InspectResponse{}, spec{}, fmt.Errorf("container %s: %w", name, driver.ErrNotFound)
	}
	if err != nil {
		return container.InspectResponse{}, spec{}, err
	}
	s, ours := specOf(r.Container.Config.Labels)
	if !ours {
		return container.InspectResponse{}, spec{}, fmt.Errorf("container %s is not this driver's: %w", name, driver.ErrNotFound)
	}
	return r.Container, s, nil
}

// running reads a running container of the driver's.
func (d *Driver) running(ctx context.Context, name string) (container.InspectResponse, spec, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return c, s, err
	}
	if c.State == nil || !c.State.Running {
		return c, s, fmt.Errorf("container %s: %w", name, driver.ErrNotRunning)
	}
	return c, s, nil
}

func (d *Driver) guestOf(c container.InspectResponse, s spec) driver.Guest {
	name := strings.TrimPrefix(c.Name, "/")
	g := driver.Guest{
		GuestSpec: driver.GuestSpec{Name: name, Disk: s.Disk, Network: s.Network, Alias: s.Alias, CPUs: s.CPUs, Memory: s.Memory, Labels: hostdLabels(c.Config.Labels)},
		Image:     s.Image,
		Tailnet:   d.tailnetOf(name, s),
	}
	g.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
	if c.State != nil && c.State.Running {
		g.Running = true
		g.StartedAt, _ = time.Parse(time.RFC3339Nano, c.State.StartedAt)
		netName := s.Network
		if netName == "" {
			netName = DefaultNetwork
		}
		if c.NetworkSettings != nil {
			if ep := c.NetworkSettings.Networks[netName]; ep != nil && ep.IPAddress.IsValid() {
				g.Address = ep.IPAddress.String()
			}
		}
	}
	return g
}

func (d *Driver) Inspect(ctx context.Context, name string) (driver.Guest, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return driver.Guest{}, err
	}
	return d.guestOf(c, s), nil
}

func (d *Driver) List(ctx context.Context) ([]driver.Guest, error) {
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", LabelDriver+"=container")})
	if err != nil {
		return nil, err
	}
	var out []driver.Guest
	for _, c := range res.Items {
		r, s, err := d.inspect(ctx, c.ID)
		if errors.Is(err, driver.ErrNotFound) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, err
		}
		out = append(out, d.guestOf(r, s))
	}
	slices.SortFunc(out, func(a, b driver.Guest) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (d *Driver) Start(ctx context.Context, name string) error {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c.State != nil && c.State.Running {
		return nil
	}
	if _, err := d.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container %s: %w: %w", name, err, driver.ErrStartFailed)
	}
	return d.giveUser(ctx, c, s, d.processesDir(name))
}

// giveUser gives the user the image runs as its processes' directory and
// an empty workspace, both root's as hostd made them, so a container that
// is not root's (a devcontainer's remoteUser, say) can run processes and
// write its own /workspace; and /repos, made in the container as root if
// missing and given while empty, where a box's later checkouts go, made by
// tower-guest as that user. A user named, not
// numbered, is looked up in the container's own /etc/passwd and
// /etc/group, which the host's do not speak for.
func (d *Driver) giveUser(ctx context.Context, c container.InspectResponse, s spec, procs string) error {
	user := s.user(c)
	if user == "" {
		return nil
	}
	uid, gid, ok := numericUser(user)
	if !ok {
		var err error
		if uid, gid, err = d.namedUser(ctx, c.ID, user); err != nil {
			return err
		}
	}
	if uid == 0 {
		return nil
	}
	// Its processes' directory, where its supervisor, running as it, makes
	// one for each; hostd reads what it holds as a guest's (proclog).
	if err := chownRoots(procs, uid, gid); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.Disk)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := chownRoots(s.Disk, uid, gid); err != nil {
			return err
		}
	}
	return d.giveRepos(ctx, c.ID, uid, gid)
}

// giveRepos makes ReposDir in the container as root, on its own root file
// system, and gives it to uid:gid while it is empty, through tower-guest
// give-dir: nothing of the image's runs as root, and without NET_ADMIN.
// Every exec the driver runs in a container is tower-guest's, by its
// absolute path, and drops NET_ADMIN before anything else.
func (d *Driver) giveRepos(ctx context.Context, id string, uid, gid int) error {
	if err := d.asRoot(ctx, id, giveReposArgs(uid, gid)...); err != nil {
		return fmt.Errorf("giving %s to the container's user: %w", ReposDir, err)
	}
	return nil
}

// giveReposArgs are tower-guest's arguments that give ReposDir to uid:gid.
func giveReposArgs(uid, gid int) []string {
	return []string{"give-dir", strconv.Itoa(uid), strconv.Itoa(gid), ReposDir}
}

// chownRoots gives dir to uid:gid if root owns it, as hostd made it.
func chownRoots(dir string, uid, gid int) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is no directory", dir)
	}
	if sys, ok := st.Sys().(interface{ Uid() uint32 }); ok && sys.Uid() != 0 {
		return nil
	}
	return os.Lchown(dir, uid, gid)
}

// numericUser reads a user given as uid or uid:gid.
func numericUser(user string) (int, int, bool) {
	u, g, hasGroup := strings.Cut(user, ":")
	uid, err := strconv.Atoi(u)
	if err != nil {
		return 0, 0, false
	}
	gid := uid
	if hasGroup {
		if gid, err = strconv.Atoi(g); err != nil {
			return 0, 0, false
		}
	}
	return uid, gid, true
}

// namedUser looks a user, name[:group], up in the container's files: its
// uid and primary gid in /etc/passwd, a group named in /etc/group. One it
// does not name is no error here: the container fails to run as it.
func (d *Driver) namedUser(ctx context.Context, id, user string) (int, int, error) {
	name, group, hasGroup := strings.Cut(user, ":")
	passwd, err := d.containerFile(ctx, id, "/etc/passwd")
	if err != nil {
		return 0, 0, err
	}
	uid, gid, ok := lookupEntry(passwd, name, 2, 3)
	if !ok {
		if n, err := strconv.Atoi(name); err == nil {
			uid, gid, ok = n, n, true
		}
	}
	if !ok {
		return 0, 0, nil
	}
	if hasGroup {
		if n, err := strconv.Atoi(group); err == nil {
			gid = n
		} else {
			groups, err := d.containerFile(ctx, id, "/etc/group")
			if err != nil {
				return 0, 0, err
			}
			if g, _, found := lookupEntry(groups, group, 2, 2); found {
				gid = g
			}
		}
	}
	return uid, gid, nil
}

// lookupEntry finds name in a colon-separated file (/etc/passwd,
// /etc/group) and reads two of its fields as numbers.
func lookupEntry(content []byte, name string, first, second int) (int, int, bool) {
	for _, line := range strings.Split(string(content), "\n") {
		f := strings.Split(line, ":")
		if len(f) <= max(first, second) || f[0] != name {
			continue
		}
		a, err1 := strconv.Atoi(f[first])
		b, err2 := strconv.Atoi(f[second])
		if err1 == nil && err2 == nil {
			return a, b, true
		}
	}
	return 0, 0, false
}

// containerFile reads a file of a container, started or not, from Docker;
// one not there is empty.
func (d *Driver) containerFile(ctx context.Context, id, p string) ([]byte, error) {
	res, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: p})
	if cerrdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Content.Close() }()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("reading %s of container %s: %w", p, id, err)
	}
	return io.ReadAll(io.LimitReader(tr, 4<<20))
}

func (d *Driver) Stop(ctx context.Context, name string) error {
	c, _, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c.State == nil || !c.State.Running {
		return nil
	}
	timeout := int(d.cfg.StopTimeout / time.Second)
	_, err = d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

func (d *Driver) Remove(ctx context.Context, name string) error {
	c, s, err := d.inspect(ctx, name)
	if errors.Is(err, driver.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := retire(ctx, dockerNode{d, name}, d.tailnetOf(name, s) != nil, c.State != nil && c.State.Running); err != nil {
		return err
	}
	// Its processes go with it.
	return os.RemoveAll(d.processesDir(name))
}

// retire removes a container and what the host keeps of its node, which,
// if it joins a tailnet, logs out first, so its tailnet forgets it.
func retire(ctx context.Context, n nodeOps, joins, running bool) error {
	if joins {
		leaveTailnet(ctx, n, running)
	}
	if err := n.remove(ctx); err != nil {
		return err
	}
	return n.forget()
}

// forgetNode removes what the host keeps of a container's node: its
// state, its status, and the hostname and routes last set on it.
func (d *Driver) forgetNode(name string) error {
	if err := os.Remove(d.tailnetSettings(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(d.tailnetControl(name)); err != nil {
		return err
	}
	return os.RemoveAll(d.tailnetDir(name))
}

func (d *Driver) EnsureNetwork(ctx context.Context, name string, labels driver.Labels) error {
	return Networks{d.cli}.Ensure(ctx, name, labels)
}

func (d *Driver) Networks(ctx context.Context) ([]driver.Network, error) {
	return Networks{d.cli}.List(ctx)
}

func (d *Driver) RemoveNetwork(ctx context.Context, name string) error {
	return Networks{d.cli}.Remove(ctx, name)
}

// ops are the operations in a container, through tower-guest shim.
func (d *Driver) ops(name string, s spec, c container.InspectResponse) guestops.Ops {
	r := runner{cli: d.cli, container: name}
	if s.Tailnet != nil {
		// Run as root, which drops NET_ADMIN and becomes the image's user
		// before anything else.
		r.user = "0"
		r.env = []string{guest.RunAsEnv + "=" + s.User}
		// Its Config.Env holds the image's environment and its spec's.
		if c.Config == nil || !homeSet(c.Config.Env, nil) {
			r.env = append(r.env, guest.RunAsHomeEnv+"=1")
		}
	}
	return guestops.Ops{R: r, ProcRoot: ProcessesDir, HostProcRoot: d.processesDir(name)}
}

func (d *Driver) Exec(ctx context.Context, name string, spec driver.ExecSpec) (int, error) {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return 0, err
	}
	return d.ops(name, s, c).Exec(ctx, spec)
}

func (d *Driver) ReadFile(ctx context.Context, name, path string, w io.Writer) error {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return err
	}
	return d.ops(name, s, c).ReadFile(ctx, path, w)
}

func (d *Driver) WriteFile(ctx context.Context, name, path string, mode os.FileMode, offset int64, r io.Reader) error {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return err
	}
	return d.ops(name, s, c).WriteFile(ctx, path, mode, offset, r)
}

func (d *Driver) CopyOut(ctx context.Context, name, path string, w io.Writer) error {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return err
	}
	return d.ops(name, s, c).CopyOut(ctx, path, w)
}

func (d *Driver) CopyIn(ctx context.Context, name, dir string, r io.Reader) error {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return err
	}
	return d.ops(name, s, c).CopyIn(ctx, dir, r)
}

func (d *Driver) Dial(ctx context.Context, name string, port int32) (net.Conn, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	g := d.guestOf(c, s)
	if !g.Running || g.Address == "" {
		return nil, fmt.Errorf("container %s: %w", name, driver.ErrNotRunning)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(g.Address, strconv.Itoa(int(port))))
}

func (d *Driver) StartProcess(ctx context.Context, name string, spec driver.ProcessSpec) (string, error) {
	c, s, err := d.running(ctx, name)
	if err != nil {
		return "", err
	}
	return d.ops(name, s, c).StartProcess(ctx, spec)
}

func (d *Driver) ListProcesses(ctx context.Context, name string) ([]driver.ProcessInfo, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	return d.ops(name, s, c).ListProcesses(ctx)
}

func (d *Driver) ReadProcessLog(ctx context.Context, name, id string, from int64, follow bool, out func(driver.LogChunk) error) (driver.LogEnd, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return driver.LogEnd{}, err
	}
	end, err := d.ops(name, s, c).ReadProcessLog(ctx, id, from, follow, out)
	return end, driver.FromGuest(err)
}

func (d *Driver) SignalProcess(ctx context.Context, name, id string, sig int) error {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c.State == nil || !c.State.Running {
		// Its processes ended with it.
		return nil
	}
	return driver.FromGuest(d.ops(name, s, c).SignalProcess(ctx, id, sig))
}

// runner runs tower-guest shim in a container by docker exec, as user
// with env when set.
type runner struct {
	cli       *client.Client
	container string
	user      string
	env       []string
}

func (r runner) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return r.run(ctx, []string{GuestBinary, "shim"}, stdin, stdout, stderr)
}

func (r runner) run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ex, err := r.cli.ExecCreate(ctx, r.container, client.ExecCreateOptions{
		Cmd: cmd, User: r.user, Env: r.env, AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if cerrdefs.IsNotFound(err) {
		return 0, fmt.Errorf("container %s: %w", r.container, driver.ErrNotFound)
	}
	if cerrdefs.IsConflict(err) {
		return 0, fmt.Errorf("container %s: %w: %w", r.container, err, driver.ErrNotRunning)
	}
	if err != nil {
		return 0, err
	}
	att, err := r.cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	defer att.Close()
	// The call's end closes the connection, which ends the shim's stdin:
	// it kills what it runs.
	stop := context.AfterFunc(ctx, func() { _ = att.Conn.Close() })
	defer stop()
	go func() {
		_, _ = io.Copy(att.Conn, stdin)
		_ = att.CloseWrite()
	}()
	_, err = stdcopy.StdCopy(stdout, stderr, bufio.NewReader(att.Reader))
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	// The exec's exit code is set once its process is reaped, a moment
	// after its output ends.
	for range 100 {
		in, err := r.cli.ExecInspect(context.WithoutCancel(ctx), ex.ID, client.ExecInspectOptions{})
		if err != nil {
			return 0, err
		}
		if !in.Running {
			return in.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("container %s: the exec %s did not end with its output", r.container, ex.ID)
}

// Networks are hostd's workstream networks: Docker bridge networks
// carrying LabelDriver "network".
type Networks struct{ Cli *client.Client }

// get reads one network of the name; ok false if there is none, or one not
// the driver's.
func (n Networks) get(ctx context.Context, name string) (network.Inspect, bool, error) {
	r, err := n.Cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return network.Inspect{}, false, nil
	}
	if err != nil {
		return network.Inspect{}, false, err
	}
	if r.Network.Name != name || r.Network.Labels[LabelDriver] != "network" {
		return network.Inspect{}, false, nil
	}
	return r.Network, true, nil
}

// Ensure makes a network once.
func (n Networks) Ensure(ctx context.Context, name string, labels driver.Labels) error {
	r, err := n.Cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		if r.Network.Name == name && r.Network.Labels[LabelDriver] != "network" {
			return fmt.Errorf("network %s is there, and not hostd's: %w", name, driver.ErrConflict)
		}
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return err
	}
	l := maps.Clone(labels)
	if l == nil {
		l = map[string]string{}
	}
	l[LabelDriver] = "network"
	_, err = n.Cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:  "bridge",
		Options: map[string]string{"com.docker.network.bridge.name": BridgeName(name)},
		Labels:  l,
	})
	if cerrdefs.IsConflict(err) {
		return nil // made meanwhile
	}
	return err
}

// List lists the networks.
func (n Networks) List(ctx context.Context) ([]driver.Network, error) {
	res, err := n.Cli.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("label", LabelDriver+"=network")})
	if err != nil {
		return nil, err
	}
	var out []driver.Network
	for _, s := range res.Items {
		l := driver.Labels{}
		for k, v := range s.Labels {
			if k != LabelDriver {
				l[k] = v
			}
		}
		out = append(out, driver.Network{Name: s.Name, Labels: l, Created: s.Created})
	}
	slices.SortFunc(out, func(a, b driver.Network) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Remove removes a network; one a container is on is ErrBusy.
func (n Networks) Remove(ctx context.Context, name string) error {
	r, ok, err := n.get(ctx, name)
	if err != nil || !ok {
		return err
	}
	if len(r.Containers) > 0 {
		return fmt.Errorf("network %s has %d guests on it: %w", name, len(r.Containers), driver.ErrBusy)
	}
	// A guest made and not started is on it too, though Docker lists only
	// those running among its endpoints, and would let it go. It is found
	// by the network's name: a guest made and not started has no id of it.
	on, err := n.Cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("network", name)})
	if err != nil {
		return err
	}
	if len(on.Items) > 0 {
		return fmt.Errorf("network %s has %d guests on it: %w", name, len(on.Items), driver.ErrBusy)
	}
	_, err = n.Cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if cerrdefs.IsConflict(err) || (err != nil && strings.Contains(err.Error(), "active endpoints")) {
		return fmt.Errorf("network %s: %w: %w", name, err, driver.ErrBusy)
	}
	return err
}

// Gateway is a network's gateway and prefix, as Docker's IPAM gave it.
func (n Networks) Gateway(ctx context.Context, name string) (netip.Prefix, error) {
	r, err := n.Cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Prefix{}, err
	}
	for _, c := range r.Network.IPAM.Config {
		if c.Subnet.Addr().Is4() && c.Gateway.IsValid() {
			return netip.PrefixFrom(c.Gateway, c.Subnet.Bits()), nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("network %s has no IPv4 subnet", name)
}

func (d *Driver) Tailnet(ctx context.Context, name string) (driver.TailnetState, error) {
	_, s, err := d.inspect(ctx, name)
	if err != nil {
		return driver.TailnetState{}, err
	}
	if s.Tailnet == nil {
		return driver.TailnetState{}, fmt.Errorf("container %s joins no tailnet: %w", name, driver.ErrNotFound)
	}
	return tailnetStatusIn(ctx, d.tailnetDir(name)), nil
}

// tailnetStatusIn is how the join stands as tower-guest says it in a
// container's tailnet directory, which the guest holds: what is not a
// status file tower-guest wrote whole says so, and is never trusted.
func tailnetStatusIn(ctx context.Context, dir string) driver.TailnetState {
	b, err := readGuestFile(ctx, dir, guest.TailnetStatusFile, maxTailnetStatus)
	if errors.Is(err, os.ErrNotExist) {
		return driver.TailnetState{Reason: "Starting", Message: "its tailscaled has not started yet"}
	}
	if err != nil {
		return driver.TailnetState{Reason: "Failed", Message: "its status cannot be read: " + err.Error()}
	}
	// tower-guest writes it whole, by a rename: one that does not parse
	// says no reason, as one not written yet.
	var st guest.TailnetStatus
	_ = json.Unmarshal(b, &st)
	if st.Reason == "" {
		return driver.TailnetState{Reason: "Starting", Message: "its status says nothing yet"}
	}
	return driver.TailnetState{Reason: st.Reason, Message: st.Message}
}

func (d *Driver) JoinTailnet(ctx context.Context, name, key string) error {
	_, s, err := d.running(ctx, name)
	if err != nil {
		return err
	}
	if s.Tailnet == nil {
		return fmt.Errorf("container %s joins no tailnet: %w", name, driver.ErrNotFound)
	}
	cmd := append([]string{GuestBinary, "tailnet-up"}, tailnetArgs(d.tailnetOf(name, s))...)
	// As root, keeping NET_ADMIN, with the key on its stdin alone.
	var out bytes.Buffer
	code, err := runner{cli: d.cli, container: name, user: "0"}.run(ctx, cmd, strings.NewReader(key), &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("container %s: tower-guest tailnet-up exited %d: %s", name, code, strings.TrimSpace(strings.ReplaceAll(out.String(), key, "[key]")))
	}
	return nil
}

// maxTailnetStatus bounds the status file hostd reads of a container's
// node.
const maxTailnetStatus = 64 << 10

// guestReadTimeout bounds a read of a file a guest wrote.
const guestReadTimeout = 5 * time.Second

// readGuestFile reads a file the guest writes in a directory it holds,
// bound into it, as proclog reads a process's: confined to dir, never
// through a link (O_NOFOLLOW), a regular file alone, opened without
// blocking, at most limit bytes, within guestReadTimeout. A link, a FIFO,
// a device or a file too large the guest put there is an error, never
// followed, waited on or read whole.
func readGuestFile(ctx context.Context, dir, name string, limit int64) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := func() ([]byte, error) {
			r, err := os.OpenRoot(dir)
			if err != nil {
				return nil, err
			}
			defer func() { _ = r.Close() }()
			// os.Root follows a link that stays in it: the name itself must
			// not be one, and what is opened must be what was looked at.
			lst, err := r.Lstat(name)
			if err != nil {
				return nil, err
			}
			if !lst.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular file", name)
			}
			f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			st, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if !st.Mode().IsRegular() || !os.SameFile(lst, st) {
				return nil, fmt.Errorf("%s changed as it was opened", name)
			}
			b, err := io.ReadAll(io.LimitReader(f, limit+1))
			if err == nil && int64(len(b)) > limit {
				err = fmt.Errorf("%s is over %d bytes", name, limit)
			}
			return b, err
		}()
		done <- result{b, err}
	}()
	ctx, cancel := context.WithTimeout(ctx, guestReadTimeout)
	defer cancel()
	select {
	case r := <-done:
		return r.b, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("reading %s: %w", name, ctx.Err())
	}
}

// guestCommand is a tower-guest subcommand as the driver execs it in a
// container: by its absolute path, never a name the box's PATH resolves.
func guestCommand(args ...string) []string {
	return append([]string{GuestBinary}, args...)
}

// tailnetArgs are tower-guest's flags for a tailnet's hostname and routes.
func tailnetArgs(t *driver.ContainerTailnet) []string {
	args := []string{"--hostname", t.Hostname}
	if t.AcceptRoutes {
		args = append(args, "--accept-routes")
	}
	return args
}

// asRoot runs a tower-guest subcommand in a running container as root,
// keeping its capabilities, and fails with its words on a code other
// than 0.
func (d *Driver) asRoot(ctx context.Context, name string, args ...string) error {
	var out bytes.Buffer
	code, err := runner{cli: d.cli, container: name, user: "0"}.run(ctx, guestCommand(args...), strings.NewReader(""), &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("container %s: tower-guest %s exited %d: %s", name, args[0], code, strings.TrimSpace(out.String()))
	}
	return nil
}

func (d *Driver) SetTailnet(ctx context.Context, name string, t *driver.ContainerTailnet) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if t != nil && d.tailnetOf(name, s) == nil && (d.cfg.Tailscaled == "" || d.cfg.Tailscale == "") {
		return fmt.Errorf("container %s joins a tailnet, and this host has no tailscale (hostd's --tailscaled and --tailscale): %w", name, driver.ErrStartFailed)
	}
	g := d.guestOf(c, s)
	cs := driver.ContainerSpec{GuestSpec: g.GuestSpec, Image: s.Image, Env: map[string]string{}, Tailnet: t}
	cs.Labels = hostdLabels(c.Config.Labels)
	// Its Config.Env holds the image's environment and its spec's: given
	// again whole, each value as it was.
	for _, e := range c.Config.Env {
		k, v, _ := strings.Cut(e, "=")
		cs.Env[k] = v
	}
	return changeTailnet(ctx, dockerNode{d, name}, d.tailnetOf(name, s), t, c.State != nil && c.State.Running, cs)
}

// nodeOps are what a change of a container's tailnet does to it, apart
// from Docker for its tests.
type nodeOps interface {
	// hasState says its node has state to log out; leaveStopped starts the
	// stopped container in leave mode, whose init logs its node out and
	// exits, running nothing of the image's; down logs a running one's
	// node out (tower-guest tailnet-down); set sets its node's hostname
	// and routes as it runs (tower-guest tailnet-set) and keeps them; stop
	// and remove stop and remove the container, keeping its disk and
	// processes' records; forget removes what the host keeps of its node;
	// create makes it again, stopped.
	hasState() bool
	leaveStopped(ctx context.Context) error
	down(ctx context.Context) error
	set(ctx context.Context, t *driver.ContainerTailnet) error
	stop(ctx context.Context) error
	remove(ctx context.Context) error
	forget() error
	create(ctx context.Context, cs driver.ContainerSpec) error
}

// changeTailnet changes the tailnet a container joins from have to want,
// running or not: a hostname or routes are set on a running node, one
// stopped taking them after its start; a container that joins or leaves
// is made again as cs, its node, if it leaves, logged out first, started
// for it if stopped, and forgotten.
func changeTailnet(ctx context.Context, n nodeOps, have, want *driver.ContainerTailnet, running bool, cs driver.ContainerSpec) error {
	switch {
	case have == nil && want == nil:
		return nil
	case have != nil && want != nil:
		if *have == *want || !running {
			return nil
		}
		return n.set(ctx, want)
	}
	if have != nil {
		leaveTailnet(ctx, n, running)
	}
	// Docker gives a container its devices and capabilities as it makes
	// it: one that joins or leaves is made again, on the same disk.
	if err := n.stop(ctx); err != nil {
		return err
	}
	if err := n.remove(ctx); err != nil {
		return err
	}
	if err := n.forget(); err != nil {
		return err
	}
	return n.create(ctx, cs)
}

// leaveTailnet logs a container's node out, so its tailnet forgets it: a
// running one's through tower-guest; a stopped one's by starting it in
// leave mode, its init starting tailscaled to log out and nothing of the
// image's, and only if its node has state, there being nothing to log out
// otherwise. It is tried, not required: a node that cannot log out (a
// container that does not start, a tailnet out of reach) leaves all the
// same, its state deleted after, and workstream deletes its device.
func leaveTailnet(ctx context.Context, n nodeOps, running bool) {
	switch {
	case running:
		_ = n.down(ctx)
	case n.hasState():
		_ = n.leaveStopped(ctx)
	}
}

// dockerNode is nodeOps on the driver's container of the name.
type dockerNode struct {
	d    *Driver
	name string
}

func (n dockerNode) hasState() bool {
	_, err := os.Lstat(filepath.Join(n.d.tailnetDir(n.name), guest.TailnetStateFile))
	return err == nil
}

// leaveTimeout bounds a stopped container's start in leave mode: its init
// waits up to 20 seconds for tailscaled and 30 for the logout.
const leaveTimeout = 90 * time.Second

func (n dockerNode) leaveStopped(ctx context.Context) error {
	marker := filepath.Join(n.d.tailnetControl(n.name), guest.TailnetLeaveFile)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(marker) }()
	ctx, cancel := context.WithTimeout(ctx, leaveTimeout)
	defer cancel()
	if _, err := n.d.cli.ContainerStart(ctx, n.name, client.ContainerStartOptions{}); err != nil {
		return err
	}
	wait := n.d.cli.ContainerWait(ctx, n.name, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
		return nil
	case err := <-wait.Error:
		_ = n.stop(context.WithoutCancel(ctx))
		return err
	}
}

func (n dockerNode) down(ctx context.Context) error { return n.d.asRoot(ctx, n.name, "tailnet-down") }

func (n dockerNode) set(ctx context.Context, t *driver.ContainerTailnet) error {
	if err := n.d.asRoot(ctx, n.name, append([]string{"tailnet-set"}, tailnetArgs(t)...)...); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.d.tailnetSettings(n.name)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(n.d.tailnetSettings(n.name), b, 0o600)
}

func (n dockerNode) stop(ctx context.Context) error {
	timeout := int(n.d.cfg.StopTimeout / time.Second)
	if _, err := n.d.cli.ContainerStop(ctx, n.name, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (n dockerNode) remove(ctx context.Context) error {
	if _, err := n.d.cli.ContainerRemove(ctx, n.name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (n dockerNode) forget() error { return n.d.forgetNode(n.name) }

func (n dockerNode) create(ctx context.Context, cs driver.ContainerSpec) error {
	return n.d.create(ctx, cs)
}
