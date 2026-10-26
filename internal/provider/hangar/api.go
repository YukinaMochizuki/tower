package hangar

import (
	"context"
	"errors"
	"io"
	"net"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

// API is what the provider asks of the one hangar project its target is,
// as an ordinary client of hangar with that project's service token
// (docs/decisions/layers.md). Connect is the real one, over hangar's API;
// fake is one in memory, for tests. Every object it names is in that
// project, whatever its metadata says.
type API interface {
	Networks() Kind[*hangarnetwork.Network]
	Disks() Kind[*hangarstorage.Disk]
	Containers() Kind[*hangarcompute.Container]
	Secrets() Kind[*hangarsecrets.Secret]
	CustomImages() Kind[*hangarstorage.CustomImage]
	// ProjectName is the project's name, which a Disk names to be made from
	// one of its CustomImages.
	ProjectName() string
	// Machine reads the machine a container runs as (MachineOf); one not
	// made yet is ErrNotFound.
	Machine(ctx context.Context, name string) (*hangarcompute.Machine, error)
	// Project reads the project, its limits and usage among its status.
	Project(ctx context.Context) (*hangartenancy.Project, error)
	// Exec runs a process in an object's machine, as hangar's exec
	// WebSocket does, until it ends or ctx does, and returns how it ended.
	Exec(ctx context.Context, req ExecRequest) (ExecExit, error)
	// PortForward connects to a port of an object's machine, past its
	// security groups, as hangar's port forward does.
	PortForward(ctx context.Context, plural, name string, port int32) (net.Conn, error)
}

// Kind is one kind of hangar object of the project, by name. Apply
// replaces the object's spec, labels and annotations, as hangar's apply
// does, and returns it as stored.
type Kind[T any] interface {
	// Get reads an object; one not there is ErrNotFound. One being deleted
	// is there, with its delete_time.
	Get(ctx context.Context, name string) (T, error)
	// List lists the objects whose labels match a hangar label selector,
	// such as "a=b"; empty lists them all.
	List(ctx context.Context, selector string) ([]T, error)
	Apply(ctx context.Context, obj T) (T, error)
	// Delete starts deleting an object; one already gone is no error.
	// hangar may keep it, being deleted, until what it holds is released.
	Delete(ctx context.Context, name string) error
}

// ErrNotFound means the project holds no object of the name.
var ErrNotFound = errors.New("hangar: not found")

// Plurals of the kinds whose machines exec and port forwards name.
const (
	PluralContainers      = "containers"
	PluralVirtualMachines = "virtualmachines"
)

// ExecRequest is a process run in an object's machine.
type ExecRequest struct {
	// Plural and Name name the object, such as containers and t-1234.
	Plural, Name string
	Start        *hangarcompute.MachineExecStart
	// Stdin is copied to the process's stdin, which closes when it ends,
	// if Start asks for one.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// ExecExit is how an exec's process ended: its code, 128 plus the
// signal's number when a signal ended it, and that signal, 0 if none.
type ExecExit struct {
	Code, Signal int
}
