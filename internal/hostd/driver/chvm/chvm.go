// Package chvm is hostd's VM driver, on Cloud Hypervisor (driver.VMs).
// Each VM runs in a Docker container of its own, whose first process is
// its launcher (tower-guest vm, package launcher): the container is what
// Docker starts and stops, keeps across restarts of hostd and of Docker
// (live-restore), bounds by cgroup, and puts on the VM's workstream
// network under its alias, so boxes and machines reach each other by
// alias; the launcher makes the container's network namespace the VM's
// and runs Cloud Hypervisor on the VM's zvol, booting through the EDK2
// firmware. A blank disk gets its base image (BaseImages/<name>.raw)
// written before its first boot. cloud-init gives root hostd's ssh key,
// and hostd runs everything in the VM over ssh through tower-guest, which
// it copies in (internal/guest/guestops), as it does in a container by
// docker exec.
//
// Every VM is called machine: its hostname is no VM's own, and is the same
// in a golden's builder and in each clone of it, as k3s writes the
// hostname into its node's name and its etcd member
// (docs/decisions/machine-blueprints.md). A VM is named on its network by
// its alias.
//
// It started as hangar's internal/node/driver/chvm, which runs each VM
// under a transient systemd unit with taps of hangar's network driver;
// tower's VMs are on Docker's networks, so Docker runs their launchers.
package chvm

import (
	"archive/tar"
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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/chvm/launcher"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
)

// Hostname is every VM's (see the package's comment).
const Hostname = vmssh.Hostname

// Defaults of a Config.
const (
	DefaultStopGrace    = 60 * time.Second
	DefaultReadyTimeout = 5 * time.Minute
	// memoryOverhead is what a VM's container may use beyond its guest's
	// memory: Cloud Hypervisor and the launcher.
	memoryOverhead = 512 << 20
)

// Config configures the driver.
type Config struct {
	Docker *client.Client
	// StateDir is hostd's state directory: each VM's directory is
	// StateDir/vms/<name>, hostd's ssh key StateDir/vm-ssh.
	StateDir string
	// GuestBinary is tower-guest on the host, statically linked: each VM's
	// launcher, and copied into the VM.
	GuestBinary string
	// CloudHypervisor and Firmware are Cloud Hypervisor, statically linked,
	// and its EDK2 firmware (CLOUDHV.fd).
	CloudHypervisor, Firmware string
	// BaseImages holds the base images, each NAME.raw
	// (deploy/hostd/base-image.sh).
	BaseImages string
	// DNS are the servers a VM resolves through: its launcher relays the
	// VM's queries to Docker's resolver, which forwards to these, public
	// ones, as docker.Config's DNS. Empty is the host's.
	DNS []netip.Addr
	// StopGrace is how long a guest has to power off once its power button
	// is pressed; zero means DefaultStopGrace.
	StopGrace time.Duration
	// ReadyTimeout bounds how long an operation in a VM just started waits
	// for its ssh; zero means DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// VMUsers are the ids each VM's Cloud Hypervisor runs as, one each;
	// zero means DefaultVMUsers. No user or group of the host may have one.
	VMUsers UserRange
}

// Driver is the VM driver. It is safe for concurrent use.
type Driver struct {
	cfg  Config
	cli  *client.Client
	keys sshKeys
	// guestHash is tower-guest's sha256, which a VM's copy is checked
	// against.
	guestHash string

	mu sync.Mutex
	// ready are the VMs ssh reaches with tower-guest in place, by name, at
	// the start they are ready in.
	ready map[string]*readyVM
}

var _ driver.VMs = (*Driver)(nil)

// vmPidsLimit is the most processes and threads a VM's container runs:
// its launcher and Cloud Hypervisor's threads, one a vCPU and its own.
const vmPidsLimit = 1024

func ptr[T any](v T) *T { return &v }

// vmImage is the image every VM's container runs: empty, its launcher
// bound in.
const vmImage = "tower-vm:empty"

// New checks what VMs run on, makes hostd's ssh key if there is none, and
// the empty image of the VMs' containers.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.StopGrace == 0 {
		cfg.StopGrace = DefaultStopGrace
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.VMUsers == (UserRange{}) {
		cfg.VMUsers = DefaultVMUsers
	}
	if err := cfg.VMUsers.check(); err != nil {
		return nil, err
	}
	if err := cfg.VMUsers.checkFree("/etc"); err != nil {
		return nil, err
	}
	for _, p := range []string{cfg.GuestBinary, cfg.CloudHypervisor} {
		if err := checkStatic(p); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(cfg.Firmware); err != nil {
		return nil, fmt.Errorf("the VMs' firmware: %w", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("KVM: %w; VMs never run without it", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "vms"), 0o700); err != nil {
		return nil, err
	}
	keys, err := loadKeys(filepath.Join(cfg.StateDir, "vm-ssh"))
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(cfg.GuestBinary)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	d := &Driver{cfg: cfg, cli: cfg.Docker, keys: keys, guestHash: hex.EncodeToString(sum[:]), ready: map[string]*readyVM{}}
	if err := d.ensureImage(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// ensureImage imports the VMs' empty image once.
func (d *Driver) ensureImage(ctx context.Context) error {
	if _, err := d.cli.ImageInspect(ctx, vmImage); err == nil {
		return nil
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "vm/", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := d.cli.ImageImport(ctx, client.ImageImportSource{Source: &b, SourceName: "-"}, vmImage, client.ImageImportOptions{})
	if err != nil {
		return fmt.Errorf("importing the VMs' image: %w", err)
	}
	defer func() { _ = res.Close() }()
	_, err = io.Copy(io.Discard, res)
	return err
}

func (d *Driver) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, d.cfg.CloudHypervisor, "--version").Output()
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return first, nil
}

// spec is what docker.LabelSpec holds of a VM.
type spec struct {
	Disk      string `json:"disk"`
	Network   string `json:"network,omitempty"`
	Alias     string `json:"alias,omitempty"`
	CPUs      int    `json:"cpus,omitempty"`
	Memory    uint64 `json:"memory,omitempty"`
	BaseImage string `json:"baseImage"`
}

func specOf(labels map[string]string) (spec, bool) {
	var s spec
	if labels[docker.LabelDriver] != "vm" || json.Unmarshal([]byte(labels[docker.LabelSpec]), &s) != nil {
		return spec{}, false
	}
	return s, true
}

func hostdLabels(labels map[string]string) driver.Labels {
	out := driver.Labels{}
	for k, v := range labels {
		if k != docker.LabelDriver && k != docker.LabelSpec {
			out[k] = v
		}
	}
	return out
}

func (d *Driver) dir(name string) string { return filepath.Join(d.cfg.StateDir, "vms", name) }

var baseImageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (d *Driver) Create(ctx context.Context, vs driver.VMSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := spec{Disk: vs.Disk, Network: vs.Network, Alias: vs.Alias, CPUs: vs.CPUs, Memory: vs.Memory, BaseImage: vs.BaseImage}
	if cur, err := d.cli.ContainerInspect(ctx, vs.Name, client.ContainerInspectOptions{}); err == nil {
		have, ours := specOf(cur.Container.Config.Labels)
		if !ours || have.BaseImage != want.BaseImage || have.Disk != want.Disk || have.Network != want.Network || have.Alias != want.Alias {
			return fmt.Errorf("VM %s is there, made otherwise: %w", vs.Name, driver.ErrConflict)
		}
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	if !baseImageRE.MatchString(vs.BaseImage) {
		return fmt.Errorf("%q names no base image", vs.BaseImage)
	}
	if _, err := os.Stat(vs.Disk); err != nil {
		return fmt.Errorf("disk %s: %w", vs.Disk, driver.ErrNotFound)
	}
	if vs.Network != "" {
		nets, err := docker.Networks{Cli: d.cli}.List(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(nets, func(n driver.Network) bool { return n.Name == vs.Network }) {
			return fmt.Errorf("network %s: %w", vs.Network, driver.ErrNotFound)
		}
	}
	if err := os.MkdirAll(d.dir(vs.Name), 0o700); err != nil {
		return err
	}
	return d.createContainer(ctx, vs.Name, want, vs.Labels)
}

// createContainer makes a VM's container, its disk's device as it is now.
func (d *Driver) createContainer(ctx context.Context, name string, s spec, labels driver.Labels) error {
	dev, err := filepath.EvalSymlinks(s.Disk)
	if err != nil {
		return fmt.Errorf("disk %s: %w: %w", s.Disk, err, driver.ErrNotFound)
	}
	specJSON, err := json.Marshal(s)
	if err != nil {
		return err
	}
	l := maps.Clone(labels)
	if l == nil {
		l = map[string]string{}
	}
	l[docker.LabelDriver], l[docker.LabelSpec] = "vm", string(specJSON)
	netName := s.Network
	if netName == "" {
		netName = docker.DefaultNetwork
	}
	stop := int((d.cfg.StopGrace + 15*time.Second) / time.Second)
	memory := s.Memory
	if memory == 0 {
		memory = 1 << 30
	}
	cfg := &container.Config{
		Image: vmImage, Labels: l, Hostname: Hostname,
		Entrypoint: []string{"/tower-guest", "vm"}, StopTimeout: &stop,
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: d.dir(name), Target: launcher.Dir},
			{Type: mount.TypeBind, Source: d.cfg.GuestBinary, Target: "/tower-guest", ReadOnly: true},
			{Type: mount.TypeBind, Source: d.cfg.CloudHypervisor, Target: launcher.CloudHypervisor, ReadOnly: true},
			{Type: mount.TypeBind, Source: d.cfg.Firmware, Target: launcher.Firmware, ReadOnly: true},
		},
		// What its launcher needs as it makes the VM's network and starts
		// Cloud Hypervisor as the VM's user, and nothing else; once that
		// runs, the launcher keeps DAC_OVERRIDE and KILL alone
		// (launcher.dropCaps). Docker's default set is dropped.
		CapDrop: []string{"ALL"},
		CapAdd: []string{"NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE", "CHOWN", "FOWNER", "DAC_OVERRIDE",
			"SETUID", "SETGID", "SETPCAP", "KILL"},
		SecurityOpt:   []string{"no-new-privileges"},
		Sysctls:       map[string]string{"net.ipv4.ip_forward": "1"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		NetworkMode:   container.NetworkMode(netName),
		DNS:           d.cfg.DNS,
		Resources: container.Resources{
			PidsLimit: ptr(int64(vmPidsLimit)),
			Memory:    int64(memory) + memoryOverhead,
			Devices: []container.DeviceMapping{
				{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"},
				{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"},
				{PathOnHost: dev, PathInContainer: launcher.Disk, CgroupPermissions: "rwm"},
			},
		},
	}
	endpoint := &network.EndpointSettings{}
	if s.Alias != "" && netName != docker.DefaultNetwork {
		endpoint.Aliases = []string{s.Alias}
	}
	_, err = d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name, Config: cfg, HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: endpoint}},
	})
	if cerrdefs.IsConflict(err) {
		return fmt.Errorf("VM %s: %w: %w", name, err, driver.ErrConflict)
	}
	return err
}

// inspect reads one of the driver's VMs; one not there, or not the
// driver's, is ErrNotFound.
func (d *Driver) inspect(ctx context.Context, name string) (container.InspectResponse, spec, error) {
	r, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return container.InspectResponse{}, spec{}, fmt.Errorf("VM %s: %w", name, driver.ErrNotFound)
	}
	if err != nil {
		return container.InspectResponse{}, spec{}, err
	}
	s, ours := specOf(r.Container.Config.Labels)
	if !ours {
		return container.InspectResponse{}, spec{}, fmt.Errorf("VM %s is not this driver's: %w", name, driver.ErrNotFound)
	}
	return r.Container, s, nil
}

func guestOf(c container.InspectResponse, s spec) driver.Guest {
	g := driver.Guest{
		GuestSpec: driver.GuestSpec{Name: strings.TrimPrefix(c.Name, "/"), Disk: s.Disk, Network: s.Network, Alias: s.Alias, CPUs: s.CPUs, Memory: s.Memory, Labels: hostdLabels(c.Config.Labels)},
		Image:     s.BaseImage,
	}
	g.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
	if c.State != nil && c.State.Running {
		g.Running = true
		g.StartedAt, _ = time.Parse(time.RFC3339Nano, c.State.StartedAt)
		netName := s.Network
		if netName == "" {
			netName = docker.DefaultNetwork
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
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", docker.LabelDriver+"=vm")})
	if err != nil {
		return nil, err
	}
	var out []driver.Guest
	for _, c := range res.Items {
		r, s, err := d.inspect(ctx, c.ID)
		if errors.Is(err, driver.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, guestOf(r, s))
	}
	slices.SortFunc(out, func(a, b driver.Guest) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// startCheck is how long a VM's container must keep running after its
// start for the start to have taken: its launcher fails within it on a
// network, disk or Cloud Hypervisor it cannot have.
const startCheck = 3 * time.Second

func (d *Driver) Start(ctx context.Context, name string) error {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c.State != nil && c.State.Running {
		return nil
	}
	if err := d.prepareBoot(ctx, name, c, s); err != nil {
		return err
	}
	d.forget(name)
	if _, err := d.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting VM %s: %w: %w", name, err, driver.ErrStartFailed)
	}
	deadline := time.After(startCheck)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return nil
		case <-time.After(250 * time.Millisecond):
		}
		r, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if r.Container.State == nil || !r.Container.State.Running {
			return fmt.Errorf("VM %s: %s: %w", name, d.whyEnded(name), driver.ErrStartFailed)
		}
	}
}

// whyEnded says why a VM's launcher ended, from its exit record and the
// end of its console.
func (d *Driver) whyEnded(name string) string {
	var why []string
	if b, err := os.ReadFile(filepath.Join(d.dir(name), launcher.ExitFile)); err == nil {
		var e launcher.Exit
		if json.Unmarshal(b, &e) == nil {
			why = append(why, fmt.Sprintf("its launcher exited %d", e.Code))
			if e.Error != "" {
				why = append(why, e.Error)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(d.dir(name), launcher.ConsoleLog)); err == nil {
		tail := strings.TrimSpace(string(b[max(0, len(b)-512):]))
		if tail != "" {
			why = append(why, "its console ends: "+tail)
		}
	}
	if len(why) == 0 {
		return "its container ended"
	}
	return strings.Join(why, "; ")
}

// prepareBoot readies a VM to boot: its container made again if its
// disk's device is another now, as after a host's reboot; its base image
// written on a blank disk; its launcher's spec.
func (d *Driver) prepareBoot(ctx context.Context, name string, c container.InspectResponse, s spec) error {
	dev, err := filepath.EvalSymlinks(s.Disk)
	if err != nil {
		return fmt.Errorf("disk %s: %w: %w", s.Disk, err, driver.ErrStartFailed)
	}
	if c.HostConfig != nil && !slices.ContainsFunc(c.HostConfig.Devices, func(m container.DeviceMapping) bool { return m.PathOnHost == dev }) {
		if _, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil {
			return err
		}
		if err := d.createContainer(ctx, name, s, hostdLabels(c.Config.Labels)); err != nil {
			return err
		}
	}
	if err := writeBase(s.Disk, filepath.Join(d.cfg.BaseImages, s.BaseImage+".raw")); err != nil {
		return fmt.Errorf("VM %s: %w: %w", name, err, driver.ErrStartFailed)
	}
	memory := s.Memory
	if memory == 0 {
		memory = 1 << 30
	}
	hostKey, hostPub, err := d.vmHostKey(name)
	if err != nil {
		return err
	}
	d.mu.Lock()
	user, err := d.vmUser(name)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	ls := launcher.Spec{
		Name: name, CPUs: max(s.CPUs, 1), MemoryMiB: int64(memory >> 20), MAC: macOf(name),
		InstanceID: name, Hostname: Hostname, UserData: d.userData(hostKey, hostPub), StopGrace: d.cfg.StopGrace,
		VMUser: user,
	}
	b, err := json.Marshal(ls)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d.dir(name), launcher.SpecFile), b, 0o600)
}

// macOf is a VM's MAC, locally administered, from its name.
func macOf(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// userData is a VM's cloud-config (vmssh.UserData): hostd's key for
// root, and the host key hostd made for it, which hostd checks it answers
// with.
func (d *Driver) userData(hostKeyPEM string, hostPub ssh.PublicKey) string {
	return vmssh.UserData{Authorized: d.keys.Authorized, HostKeyPEM: hostKeyPEM, HostKey: hostPub}.String()
}

func (d *Driver) Stop(ctx context.Context, name string) error {
	c, _, err := d.inspect(ctx, name)
	if err != nil {
		return err
	}
	d.forget(name)
	if c.State == nil || !c.State.Running {
		return nil
	}
	timeout := int((d.cfg.StopGrace + 15*time.Second) / time.Second)
	_, err = d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

func (d *Driver) Remove(ctx context.Context, name string) error {
	if _, _, err := d.inspect(ctx, name); errors.Is(err, driver.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	d.forget(name)
	if _, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return os.RemoveAll(d.dir(name))
}

func (d *Driver) EnsureNetwork(ctx context.Context, name string, labels driver.Labels) error {
	return docker.Networks{Cli: d.cli}.Ensure(ctx, name, labels)
}

func (d *Driver) Networks(ctx context.Context) ([]driver.Network, error) {
	return docker.Networks{Cli: d.cli}.List(ctx)
}

func (d *Driver) RemoveNetwork(ctx context.Context, name string) error {
	return docker.Networks{Cli: d.cli}.Remove(ctx, name)
}

// ProcRoot is where a VM keeps its processes (vmssh.ProcRoot).
func ProcRoot(name string) string { return vmssh.ProcRoot(name) }

// ops are the operations in a running VM, over ssh once it answers.
func (d *Driver) ops(ctx context.Context, name string) (guestops.Ops, error) {
	r, err := d.readyVM(ctx, name)
	if err != nil {
		return guestops.Ops{}, err
	}
	return guestops.Ops{R: r.conn, ProcRoot: ProcRoot(name)}, nil
}

func (d *Driver) Exec(ctx context.Context, name string, s driver.ExecSpec) (int, error) {
	o, err := d.ops(ctx, name)
	if err != nil {
		return 0, err
	}
	return o.Exec(ctx, s)
}

func (d *Driver) ReadFile(ctx context.Context, name, path string, w io.Writer) error {
	o, err := d.ops(ctx, name)
	if err != nil {
		return err
	}
	return o.ReadFile(ctx, path, w)
}

func (d *Driver) WriteFile(ctx context.Context, name, path string, mode os.FileMode, offset int64, r io.Reader) error {
	o, err := d.ops(ctx, name)
	if err != nil {
		return err
	}
	return o.WriteFile(ctx, path, mode, offset, r)
}

func (d *Driver) CopyOut(ctx context.Context, name, path string, w io.Writer) error {
	o, err := d.ops(ctx, name)
	if err != nil {
		return err
	}
	return o.CopyOut(ctx, path, w)
}

func (d *Driver) CopyIn(ctx context.Context, name, dir string, r io.Reader) error {
	o, err := d.ops(ctx, name)
	if err != nil {
		return err
	}
	return o.CopyIn(ctx, dir, r)
}

func (d *Driver) StartProcess(ctx context.Context, name string, s driver.ProcessSpec) (string, error) {
	o, err := d.ops(ctx, name)
	if err != nil {
		return "", err
	}
	return o.StartProcess(ctx, s)
}

func (d *Driver) ListProcesses(ctx context.Context, name string) ([]driver.ProcessInfo, error) {
	o, err := d.ops(ctx, name)
	if err != nil {
		return nil, err
	}
	return o.ListProcesses(ctx)
}

func (d *Driver) ReadProcessLog(ctx context.Context, name, id string, from int64, follow bool, out func(driver.LogChunk) error) (driver.LogEnd, error) {
	o, err := d.ops(ctx, name)
	if err != nil {
		return driver.LogEnd{}, err
	}
	end, err := o.ReadProcessLog(ctx, id, from, follow, out)
	return end, driver.FromGuest(err)
}

func (d *Driver) SignalProcess(ctx context.Context, name, id string, sig int) error {
	o, err := d.ops(ctx, name)
	if errors.Is(err, driver.ErrNotRunning) {
		return nil // its processes ended with it
	}
	if err != nil {
		return err
	}
	return driver.FromGuest(o.SignalProcess(ctx, id, sig))
}

// Dial connects to a port of a running VM, once it answers, waiting a
// moment for a service that is still coming up.
func (d *Driver) Dial(ctx context.Context, name string, port int32) (net.Conn, error) {
	r, err := d.readyVM(ctx, name)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(r.addr, strconv.Itoa(int(port)))
	deadline := time.Now().Add(30 * time.Second)
	for {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
