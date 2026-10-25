// Package docker is hostd's container driver, on Docker Engine through
// its Go client (driver.Containers). Each container runs tower-guest init
// as its first process, which runs the image's own entrypoint and reaps
// orphans, with its disk mounted at /workspace, tower-guest bound
// read-only at /.tower/tower-guest and its processes' directory, under
// hostd's state directory, at /.tower/processes; everything done in it
// goes through tower-guest shim, by docker exec (internal/hostd/guestops).
// Its workstream networks are Docker bridge networks, which the VM driver
// puts its VMs on too (Networks); a guest on no network is on Docker's
// default bridge. What the driver makes carries the label
// tower.hangarlab.cloud/driver, and its spec as a label of its own; what
// does not is never listed, changed or removed.
package docker

import (
	"archive/tar"
	"bufio"
	"context"
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
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/guestops"
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
	GuestBinary  = "/.tower/tower-guest"
	ProcessesDir = "/.tower/processes"
)

// DefaultNetwork is the network of a guest on none: Docker's own bridge.
const DefaultNetwork = "bridge"

// Config configures the driver.
type Config struct {
	// Client is Docker's.
	Client *client.Client
	// StateDir is hostd's state directory: the containers' processes are
	// kept in StateDir/processes/<container>.
	StateDir string
	// GuestBinary is tower-guest on the host, statically linked.
	GuestBinary string
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

func (d *Driver) processesDir(name string) string {
	return filepath.Join(d.cfg.StateDir, "processes", name)
}

func (d *Driver) Create(ctx context.Context, cs driver.ContainerSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := spec{Disk: cs.Disk, Network: cs.Network, Alias: cs.Alias, CPUs: cs.CPUs, Memory: cs.Memory, Image: cs.Image}
	if cur, err := d.cli.ContainerInspect(ctx, cs.Name, client.ContainerInspectOptions{}); err == nil {
		have, ours := specOf(cur.Container.Config.Labels)
		if !ours || have.Image != want.Image || have.Disk != want.Disk || have.Network != want.Network || have.Alias != want.Alias {
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
	// The image's own entrypoint and command run under init.
	var argv []string
	if c := img.Config; c != nil {
		argv = append(slices.Clone(c.Entrypoint), c.Cmd...)
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
	cfg := &container.Config{
		Image: cs.Image, Labels: labels, Hostname: hostname(cs),
		Entrypoint: append([]string{GuestBinary, "init", "--"}, argv...), Cmd: []string{},
		StopTimeout: &stop,
	}
	if img.Config != nil {
		cfg.WorkingDir, cfg.User = img.Config.WorkingDir, img.Config.User
	}
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
		Tmpfs:         map[string]string{driver.SecretsDir: "mode=1777,size=16m,noexec,nosuid,nodev"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Resources:     container.Resources{NanoCPUs: int64(cs.CPUs) * 1e9, Memory: int64(cs.Memory)},
		NetworkMode:   container.NetworkMode(netName),
	}
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
func (d *Driver) running(ctx context.Context, name string) (container.InspectResponse, error) {
	c, _, err := d.inspect(ctx, name)
	if err != nil {
		return c, err
	}
	if c.State == nil || !c.State.Running {
		return c, fmt.Errorf("container %s: %w", name, driver.ErrNotRunning)
	}
	return c, nil
}

func guestOf(c container.InspectResponse, s spec) driver.Guest {
	name := strings.TrimPrefix(c.Name, "/")
	g := driver.Guest{
		GuestSpec: driver.GuestSpec{Name: name, Disk: s.Disk, Network: s.Network, Alias: s.Alias, CPUs: s.CPUs, Memory: s.Memory, Labels: hostdLabels(c.Config.Labels)},
		Image:     s.Image,
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
	return guestOf(c, s), nil
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
		out = append(out, guestOf(r, s))
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
// write its own /workspace. A user named, not
// numbered, is looked up in the container's own /etc/passwd and
// /etc/group, which the host's do not speak for.
func (d *Driver) giveUser(ctx context.Context, c container.InspectResponse, s spec, procs string) error {
	user := ""
	if c.Config != nil {
		user = c.Config.User
	}
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
	if err != nil || len(entries) > 0 {
		return err
	}
	return chownRoots(s.Disk, uid, gid)
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
	if _, _, err := d.inspect(ctx, name); errors.Is(err, driver.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	// Its processes go with it.
	return os.RemoveAll(d.processesDir(name))
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
func (d *Driver) ops(name string) guestops.Ops {
	return guestops.Ops{R: runner{d.cli, name}, ProcRoot: ProcessesDir, HostProcRoot: d.processesDir(name)}
}

func (d *Driver) Exec(ctx context.Context, name string, spec driver.ExecSpec) (int, error) {
	if _, err := d.running(ctx, name); err != nil {
		return 0, err
	}
	return d.ops(name).Exec(ctx, spec)
}

func (d *Driver) ReadFile(ctx context.Context, name, path string, w io.Writer) error {
	if _, err := d.running(ctx, name); err != nil {
		return err
	}
	return d.ops(name).ReadFile(ctx, path, w)
}

func (d *Driver) WriteFile(ctx context.Context, name, path string, mode os.FileMode, offset int64, r io.Reader) error {
	if _, err := d.running(ctx, name); err != nil {
		return err
	}
	return d.ops(name).WriteFile(ctx, path, mode, offset, r)
}

func (d *Driver) CopyOut(ctx context.Context, name, path string, w io.Writer) error {
	if _, err := d.running(ctx, name); err != nil {
		return err
	}
	return d.ops(name).CopyOut(ctx, path, w)
}

func (d *Driver) CopyIn(ctx context.Context, name, dir string, r io.Reader) error {
	if _, err := d.running(ctx, name); err != nil {
		return err
	}
	return d.ops(name).CopyIn(ctx, dir, r)
}

func (d *Driver) Dial(ctx context.Context, name string, port int32) (net.Conn, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	g := guestOf(c, s)
	if !g.Running || g.Address == "" {
		return nil, fmt.Errorf("container %s: %w", name, driver.ErrNotRunning)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(g.Address, strconv.Itoa(int(port))))
}

func (d *Driver) StartProcess(ctx context.Context, name string, spec driver.ProcessSpec) (string, error) {
	if _, err := d.running(ctx, name); err != nil {
		return "", err
	}
	return d.ops(name).StartProcess(ctx, spec)
}

func (d *Driver) ListProcesses(ctx context.Context, name string) ([]driver.ProcessInfo, error) {
	if _, _, err := d.inspect(ctx, name); err != nil {
		return nil, err
	}
	return d.ops(name).ListProcesses(ctx)
}

func (d *Driver) ReadProcessLog(ctx context.Context, name, id string, from int64, follow bool, out func(driver.LogChunk) error) (driver.LogEnd, error) {
	if _, _, err := d.inspect(ctx, name); err != nil {
		return driver.LogEnd{}, err
	}
	return d.ops(name).ReadProcessLog(ctx, id, from, follow, out)
}

func (d *Driver) SignalProcess(ctx context.Context, name, id string, sig int) error {
	c, _, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c.State == nil || !c.State.Running {
		// Its processes ended with it.
		return nil
	}
	return d.ops(name).SignalProcess(ctx, id, sig)
}

// runner runs tower-guest shim in a container by docker exec.
type runner struct {
	cli       *client.Client
	container string
}

func (r runner) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ex, err := r.cli.ExecCreate(ctx, r.container, client.ExecCreateOptions{
		Cmd: []string{GuestBinary, "shim"}, AttachStdin: true, AttachStdout: true, AttachStderr: true,
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
	_, err = n.Cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: l})
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
