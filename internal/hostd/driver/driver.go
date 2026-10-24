// Package driver is what hostd asks of its host, behind five drivers:
// storage (ZFS), containers (Docker), VMs (QEMU), images (tower's registry
// and a builder) and git (the mirrors). A driver does what it is told and
// lists what is there; it keeps no record beyond the host itself, and it
// decides nothing (docs/decisions/layers.md). Everything hostd makes
// carries the labels below, which a driver keeps with it and lists back,
// so hostd tells what it made from what others did. drivertest is each
// driver's contract; fake is one host of all five, for tests.
package driver

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"time"
)

// Labels hostd sets on what it makes. A driver keeps them as given: as
// container labels, ZFS user properties, a VM's spec.
const (
	// LabelOwner is the name of the hostd that made it, its target's:
	// hostd touches only what carries its own.
	LabelOwner = "tower.hangarlab.cloud/hostd"
	// LabelUID, LabelProject and LabelInstance name the Instance it serves.
	LabelUID      = "tower.hangarlab.cloud/instance-uid"
	LabelProject  = "tower.hangarlab.cloud/project"
	LabelInstance = "tower.hangarlab.cloud/instance"
)

// Errors a driver returns, wrapped or not.
var (
	// ErrNotFound means what was named is not on the host.
	ErrNotFound = errors.New("driver: not on the host")
	// ErrBusy means it is in use: a disk a guest is made on, a network
	// guests are on.
	ErrBusy = errors.New("driver: in use")
	// ErrNoSpace means the host has no room for it.
	ErrNoSpace = errors.New("driver: no space left on the host")
	// ErrNotRunning means the guest is not running.
	ErrNotRunning = errors.New("driver: the guest is not running")
	// ErrStartFailed means a guest did not start; its message says why.
	ErrStartFailed = errors.New("driver: the guest did not start")
	// ErrBadOffset means a chunk of a file does not start where the file
	// ends.
	ErrBadOffset = errors.New("driver: the chunk does not start where the file ends")
	// ErrLeaseLost means a push was refused: the branch is not at its
	// lease.
	ErrLeaseLost = errors.New("driver: the branch moved since its lease")
	// ErrConflict means something of the name is there, made otherwise.
	ErrConflict = errors.New("driver: something of the name is there, made otherwise")
)

// Labels are what hostd sets on what it makes.
type Labels map[string]string

// Drivers are a host's.
type Drivers struct {
	Storage    Storage
	Containers Containers
	VMs        VMs
	Images     Images
	Git        Git
	Host       Host
}

// Host says what the host has besides its pool.
type Host interface {
	Info(ctx context.Context) (HostInfo, error)
}

// HostInfo is the host's memory and CPUs.
type HostInfo struct {
	MemoryTotal, MemoryFree uint64
	CPUs                    int
}

// DiskKind is what a disk is on the pool.
type DiskKind int

const (
	// Dataset is a filesystem, mounted into a container at /workspace.
	Dataset DiskKind = iota + 1
	// Volume is a block device, a VM's disk.
	Volume
)

// DiskSpec is a disk to make.
type DiskSpec struct {
	Name string
	Kind DiskKind
	// Size bounds what it holds, in bytes: a dataset's quota, a volume's
	// size.
	Size uint64
	// From is the golden it is cloned from; empty is a blank disk.
	From   string
	Labels Labels
}

// Disk is a disk on the pool.
type Disk struct {
	DiskSpec
	// Path is where its guest finds it: a dataset's mountpoint, a
	// volume's device.
	Path string
	// Used is what it holds beyond the golden it was cloned from, in
	// bytes.
	Used    uint64
	Created time.Time
}

// Golden is a disk captured to be cloned from: a ZFS snapshot, kept apart
// from the disk it was captured from (docs/decisions/hostd-storage.md).
type Golden struct {
	Name string
	// Kind is its disk's.
	Kind DiskKind
	// Size is what it holds, in bytes.
	Size uint64
	// Clones are the disks cloned from it.
	Clones int
	// Deferred says it is destroyed once its clones go.
	Deferred bool
	Labels   Labels
	Created  time.Time
}

// Pool is the ZFS pool, as the host reports it.
type Pool struct {
	Name string
	// Health is zpool's, such as ONLINE or DEGRADED.
	Health     string
	Size, Free uint64
}

// Storage holds the instances' disks and the goldens they are cloned
// from. Deleting what is not there is not an error.
type Storage interface {
	Version(ctx context.Context) (string, error)
	// EnsureDisk makes the disk once, and returns it as it is: a disk of
	// the name made otherwise (another kind or golden) is ErrConflict; a
	// golden that is not there, or deferred, ErrNotFound.
	EnsureDisk(ctx context.Context, spec DiskSpec) (Disk, error)
	Disks(ctx context.Context) ([]Disk, error)
	// DestroyDisk destroys a disk; one a guest is made on is ErrBusy.
	DestroyDisk(ctx context.Context, name string) error
	// Capture makes, once, the golden of the name from a disk, which then
	// holds what the disk held and outlives it.
	Capture(ctx context.Context, disk, golden string, labels Labels) (Golden, error)
	Goldens(ctx context.Context) ([]Golden, error)
	// DestroyGolden destroys a golden, or one with clones once its last
	// clone goes, as zfs destroy -d does: it is listed Deferred until
	// then, and no disk is cloned from it.
	DestroyGolden(ctx context.Context, name string) error
	Pool(ctx context.Context) (Pool, error)
}

// GuestSpec is what a container and a VM are both made of.
type GuestSpec struct {
	Name string
	// Disk is the disk's Path: a container's /workspace, a VM's root.
	Disk string
	// Network is the workstream network it joins, and Alias its name
	// there.
	Network, Alias string
	// CPUs and Memory bound it.
	CPUs   int
	Memory uint64
	Labels Labels
}

// Guest is a container or a VM as the host has it.
type Guest struct {
	GuestSpec
	// Image is a container's image, or a VM's base image.
	Image   string
	Running bool
	// StartedAt is when it last started, Address its address on its
	// network while it runs.
	StartedAt time.Time
	Address   string
	Created   time.Time
}

// Network is a workstream network on the host.
type Network struct {
	Name    string
	Labels  Labels
	Created time.Time
}

// ExecSpec is one command run in a guest.
type ExecSpec struct {
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Env is added to the guest's environment for this command alone.
	Env map[string]string
	// Dir is the working directory; empty is the guest's default.
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Guests is what the container and VM drivers both do, a guest named as
// its spec named it. A guest not there is ErrNotFound; one not running,
// where it must be, ErrNotRunning.
type Guests interface {
	Version(ctx context.Context) (string, error)
	// EnsureNetwork makes a workstream network once.
	EnsureNetwork(ctx context.Context, name string, labels Labels) error
	Networks(ctx context.Context) ([]Network, error)
	// RemoveNetwork removes a network; one a guest is on is ErrBusy.
	RemoveNetwork(ctx context.Context, name string) error
	// Start starts a guest, or leaves it running; one that does not
	// start is ErrStartFailed.
	Start(ctx context.Context, name string) error
	// Stop stops a guest, or leaves it stopped.
	Stop(ctx context.Context, name string) error
	// Remove stops and removes a guest, keeping its disk.
	Remove(ctx context.Context, name string) error
	Inspect(ctx context.Context, name string) (Guest, error)
	List(ctx context.Context) ([]Guest, error)
	// Exec runs a command in a running guest until it ends or ctx does,
	// which kills it, and returns its exit code.
	Exec(ctx context.Context, name string, spec ExecSpec) (int, error)
	// ReadFile streams a file of a running guest to w; one not there is
	// fs.ErrNotExist.
	ReadFile(ctx context.Context, name, path string, w io.Writer) error
	// WriteFile writes a file of a running guest from r, making its
	// directory: offset 0 replaces it, any other offset must be where it
	// ends (ErrBadOffset).
	WriteFile(ctx context.Context, name, path string, mode fs.FileMode, offset int64, r io.Reader) error
	// CopyOut writes a tar of a path of a running guest, a file or a
	// directory, its names relative to the path's parent, as docker cp
	// does; one not there is fs.ErrNotExist.
	CopyOut(ctx context.Context, name, path string, w io.Writer) error
	// CopyIn extracts a tar into a directory of a running guest, making
	// it.
	CopyIn(ctx context.Context, name, dir string, r io.Reader) error
	// Dial connects to a port of a running guest.
	Dial(ctx context.Context, name string, port int32) (net.Conn, error)
}

// ContainerSpec is a container to make.
type ContainerSpec struct {
	GuestSpec
	// Image is a reference by digest the host has pulled.
	Image string
}

// Containers runs containers: Docker on a real host.
type Containers interface {
	Guests
	// Create makes a container, stopped, once: one of the name made
	// otherwise is ErrConflict.
	Create(ctx context.Context, spec ContainerSpec) error
}

// VMSpec is a VM to make.
type VMSpec struct {
	GuestSpec
	// BaseImage, such as "ubuntu-24.04", is written onto a blank disk when
	// the VM first boots.
	BaseImage string
}

// VMs runs VMs: QEMU on a real host, reached over ssh.
type VMs interface {
	Guests
	// Create makes a VM, stopped, once: one of the name made otherwise is
	// ErrConflict.
	Create(ctx context.Context, spec VMSpec) error
}

// Images pulls from tower's registry and builds into it.
type Images interface {
	Version(ctx context.Context) (string, error)
	// Pull brings an image, a reference by digest, onto the host; one the
	// registry lacks is ErrNotFound.
	Pull(ctx context.Context, ref string) error
	// Build builds a context, a tar with its Dockerfile, pushes the image
	// to repository in the registry and returns its reference by digest.
	// The same context builds the same digest.
	Build(ctx context.Context, repository string, buildContext io.Reader) (string, error)
}

// FetchSpec brings a branch of a repository into a mirror.
type FetchSpec struct {
	Mirror, URL string
	// Token reads the repository; empty is anonymous.
	Token  string
	Branch string
}

// PushSpec pushes a branch from a mirror to its repository.
type PushSpec struct {
	Mirror, URL string
	// Token writes the repository; it never enters a guest.
	Token  string
	Branch string
	// Lease is the commit the branch must be at, empty for a branch that
	// must not exist yet.
	Lease string
}

// Git keeps the host's mirrors, one per repository. A checkout in a guest
// crosses as a tar, which the guest's driver extracts or writes.
type Git interface {
	Version(ctx context.Context) (string, error)
	// Fetch brings the branch into the mirror, making it, and returns the
	// commit it is at.
	Fetch(ctx context.Context, spec FetchSpec) (string, error)
	// Checkout writes a tar of a clone of the mirror at ref (a branch, tag
	// or commit; empty is its branch), its origin the mirror, and returns
	// the commit checked out. A mirror never fetched is ErrNotFound.
	Checkout(ctx context.Context, mirror, ref string, w io.Writer) (string, error)
	// Origin writes a tar of what, extracted into a checkout's .git,
	// brings its origin/* up to the mirror and leaves the rest as it is.
	Origin(ctx context.Context, mirror string, w io.Writer) error
	// Push takes the commits of a checkout's .git, read as a tar from
	// gitDir, into the mirror, and pushes its HEAD to the branch with
	// --force-with-lease on Lease.
	Push(ctx context.Context, spec PushSpec, gitDir io.Reader) error
	// CommitsBehind counts the commits on the mirror's branch since commit.
	CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error)
}
