// Package provider is what tower asks of a target, whichever provides it,
// hangar or hostd. It has two halves:
//
//   - Instances are declarative. tower's controllers write Instance objects
//     for a target; its provider makes them real and reports their status
//     through the API. hostd watches the Instances of its own target with
//     its ROLE_HOSTD token; a hangar provider reconciles them inside tower
//     as a controller of its own. Either way nothing in this interface makes,
//     starts or stops an instance.
//   - Everything else is imperative, and is Provider: commands, files,
//     images, goldens, git mirrors and stream tickets.
//
// A provider holds no policy (docs/decisions/layers.md): it does what it is
// asked and says what happened. Secret values reach it only as the
// environment of one command, never in an object. providertest is the
// contract every provider is held to.
package provider

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Errors a provider returns, wrapped or not, so policy can tell them apart.
var (
	// ErrUnavailable means the target did not answer: tower cannot tell
	// what happened, and tries again.
	ErrUnavailable = errors.New("provider: the target does not answer")
	// ErrNotFound means what was named is not on the target: an instance
	// that vanished, an artifact deleted.
	ErrNotFound = errors.New("provider: not found on the target")
	// ErrNoSpace means the target has no room for what was asked.
	ErrNoSpace = errors.New("provider: no space left on the target")
	// ErrFailed means the work ran and failed, such as a build whose
	// Dockerfile does not build; its message says why.
	ErrFailed = errors.New("provider: the work failed")
	// ErrLeaseLost means a push was refused because the branch is not at
	// the commit its lease named.
	ErrLeaseLost = errors.New("provider: the branch moved since its lease")
)

// SecretsDir is a directory every container instance has in memory,
// emptied as it stops, which no disk of the target holds: where tower
// writes a box's secret files.
const SecretsDir = "/run/tower/secrets"

// GuestBinary is tower-guest in a container instance, which tower runs
// its own subcommands in a guest through Exec as (a boost's sync,
// docs/decisions/boost.md): where hangar's provider and hostd's Docker
// driver put it.
const GuestBinary = "/.tower/tower-guest"

// Instance names an Instance object of the target, by its project and
// name.
type Instance struct {
	Project string
	Name    string
}

// ExecRequest is one command run in an instance.
type ExecRequest struct {
	Instance Instance
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Env is added to the instance's environment for this command alone:
	// how secret values reach an environment.
	Env map[string]string
	// Dir is the working directory; empty is the instance's default.
	Dir string
	// AsRoot runs the command as root, not as the user the instance runs
	// as (an image's USER, a devcontainer's remoteUser). hangar's provider
	// runs it so; hostd's refuses it with ErrFailed, as tower asks it of no
	// hostd instance.
	AsRoot bool
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// BuildRequest is an image built on the target and pushed to tower's
// registry.
type BuildRequest struct {
	// Context is the build context, a tar stream with its Dockerfile; nil
	// when Mirror is set.
	Context io.Reader
	// Mirror, when set, is the build context instead: a mirror's work tree
	// at a ref, which stays on the target.
	Mirror *MirrorSource
	// Dir is the context's directory within Context or the work tree, ""
	// for its root, and Dockerfile the Dockerfile's path within Context or
	// the work tree, which may be outside Dir; "" is Dir/Dockerfile.
	// Neither leaves Context or the work tree.
	Dir, Dockerfile string
	// Repository is where in the registry the image goes, such as
	// "web/box-images".
	Repository string
}

// MirrorSource is a mirror's work tree at a ref (a branch, tag or commit;
// empty is the branch the mirror fetched).
type MirrorSource struct {
	Mirror, Ref string
}

// MaxMirrorContent is the most of a file ReadMirror gives the content of.
const MaxMirrorContent = 1 << 20

// MirrorReadRequest is paths of a mirror read at a ref.
type MirrorReadRequest struct {
	MirrorSource
	// Paths are relative to the work tree's root; "" is the root.
	Paths []string
	// Content asks for the content of each file of at most
	// MaxMirrorContent bytes as well.
	Content bool
}

// MirrorRead is what a mirror holds at the paths read.
type MirrorRead struct {
	// Commit is the commit the ref is at.
	Commit string
	// Entries are the paths that are there, by path; a path not there has
	// none.
	Entries map[string]MirrorEntry
}

// MirrorEntry is one path of a mirror's work tree.
type MirrorEntry struct {
	// Object is its git object id, a hash of what it holds: a directory's
	// is its tree's, so it changes with any file under it.
	Object string
	Dir    bool
	// Size is a file's, in bytes.
	Size int64
	// Content is a file's when asked for and no larger than
	// MaxMirrorContent, nil otherwise.
	Content []byte
}

// ImageConfig is what an image says of itself in its registry.
type ImageConfig struct {
	// Digest is its manifest's, such as "sha256:...".
	Digest string
	// User is the user it runs as, "" for root.
	User string
	// Env is its config's environment, each "NAME=value".
	Env []string
}

// Artifact is a golden as the target holds it: an image by digest, a
// hangar CustomImage, a hostd snapshot.
type Artifact struct {
	Name string
	// Size is what it holds on the target, in bytes.
	Size uint64
}

// FetchRequest brings a repository's branch into the target's mirror of
// it.
type FetchRequest struct {
	// Mirror names the mirror on the target, one per repository, such as
	// "web/api".
	Mirror string
	URL    string
	// Token reads the repository; empty reads it anonymously.
	Token  string
	Branch string
}

// PushRequest pushes a branch from an instance's checkout through the
// target's mirror to the repository.
type PushRequest struct {
	Instance Instance
	// Dir is the checkout in the instance, such as /workspace.
	Dir    string
	Mirror string
	URL    string
	// Token writes the repository; it never enters the instance.
	Token  string
	Branch string
	// Lease is the commit the branch must be at for the push to go, empty
	// for a branch that must not exist yet: the push forces only with a
	// lease.
	Lease string
}

// Ticket is where a client streams to one port of one instance directly,
// presenting a ticket tower signs (internal/ticket).
type Ticket struct {
	URL string
	// Expires is when the URL stops working; zero when the provider does
	// not bound it, and the ticket's expiry alone does.
	Expires time.Time
}

// Defaults a provider keeps processes by, unless its target is set
// otherwise: the most a process's log holds, its oldest output dropped
// first, and how many finished processes an instance keeps, its oldest
// going first.
const (
	DefaultProcessLogCap     = 64 << 20
	DefaultFinishedProcesses = 100
)

// ProcessRequest is a process started in an instance.
type ProcessRequest struct {
	Instance Instance
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Env is added to the instance's environment for this process alone.
	Env map[string]string
	Dir string
	// Name is for people.
	Name string
	// MemoryMax is the memory tower holds the instance's measured
	// processes to, all together; 0 for no bound. A provider that measures
	// processes applies it, and decides nothing of it.
	MemoryMax int64
}

// Signal is a signal a process's group can be sent.
type Signal int

// The signals, by their numbers.
const (
	SignalInt  Signal = 2
	SignalKill Signal = 9
	SignalTerm Signal = 15
)

// ProcessExit is how a process ended: its code, which is 128 plus the
// signal's number when a signal ended it, and that signal, 0 if it
// exited.
type ProcessExit struct {
	Code   int
	Signal Signal
	// Usage is nil for a process its provider does not measure.
	Usage *ProcessUsage
}

// ProcessUsage is what a process used, its children included: its peak
// memory, whether an OOM killed it, the CPU time it used and the time it
// waited for a CPU, and how long it ran. A field its instance's kernel
// does not offer is nil.
type ProcessUsage struct {
	MemoryPeak *int64
	OOMKilled  *bool
	CPUUsage   *time.Duration
	CPUWait    *time.Duration
	Wall       time.Duration
}

// Killed is the exit of a process ended by sig.
func Killed(sig Signal) *ProcessExit { return &ProcessExit{Code: 128 + int(sig), Signal: sig} }

// Process is one process of an instance.
type Process struct {
	ID      string
	Name    string
	Command []string
	// Dir is the working directory it was started in, empty for its
	// instance's default or where its provider does not say.
	Dir     string
	Started time.Time
	// Exit is nil while it runs.
	Exit *ProcessExit
}

// OutputStream is which output a chunk was written to.
type OutputStream int

// The outputs.
const (
	Stdout OutputStream = 1
	Stderr OutputStream = 2
)

// OutputChunk is a chunk of a process's log, at its offset, stdout and
// stderr counted together.
type OutputChunk struct {
	Stream OutputStream
	Offset int64
	Data   []byte
}

// Attached is how an attach to a process ended.
type Attached struct {
	// HeldFrom is the oldest offset the log holds; Next is where to attach
	// again to read what comes next.
	HeldFrom, Next int64
	// Exit is nil while the process runs.
	Exit *ProcessExit
}

// Provider is the imperative work tower asks of one target. Every method
// may return ErrUnavailable; one naming an instance or an artifact that is
// not there returns ErrNotFound.
type Provider interface {
	// Exec runs a command in a running instance, streaming its output, and
	// returns its exit code.
	Exec(ctx context.Context, req ExecRequest) (exitCode int, err error)
	// ReadFile streams a file of an instance to w.
	ReadFile(ctx context.Context, in Instance, path string, w io.Writer) error
	// WriteFile writes a file of an instance from r, making its directory.
	// Offset 0 replaces the file; any other offset writes a later chunk,
	// which must start where the file ends (ErrFailed otherwise), so a
	// large file is written in chunks, in order.
	WriteFile(ctx context.Context, in Instance, path string, mode fs.FileMode, offset int64, r io.Reader) error
	// Copy copies a path from one instance of the target to another on the
	// same network, as tower copies within one workstream; the bytes stay
	// on the target (docs/decisions/traffic.md).
	Copy(ctx context.Context, from Instance, fromPath string, to Instance, toPath string) error
	// BuildImage builds an image and pushes it to the registry, returning
	// its reference by digest. The same context builds the same digest.
	BuildImage(ctx context.Context, req BuildRequest) (image string, err error)
	// ResolveImage reads what an image reference names from its registry,
	// tower's or another, without pulling it; one the registry lacks is
	// ErrNotFound.
	ResolveImage(ctx context.Context, ref string) (ImageConfig, error)
	// Capture makes a golden artifact of a stopped instance's disk.
	Capture(ctx context.Context, in Instance) (Artifact, error)
	// DeleteArtifact deletes a golden artifact; one already gone is no
	// error.
	DeleteArtifact(ctx context.Context, artifact string) error
	// Fetch brings a branch into the target's mirror and returns the commit
	// it is at.
	Fetch(ctx context.Context, req FetchRequest) (head string, err error)
	// Checkout clones a mirror into a running instance at dir, at ref (a
	// branch, tag or commit; empty is the branch the mirror fetched), with
	// the mirror as its origin, and returns the commit checked out.
	Checkout(ctx context.Context, in Instance, mirror, ref, dir string) (commit string, err error)
	// UpdateOrigin brings the origin/* of a checkout Checkout made in a
	// running instance up to its mirror, leaving its branches and work tree
	// as they are.
	UpdateOrigin(ctx context.Context, in Instance, mirror, dir string) error
	// CommitsBehind counts the commits on a branch of a mirror since
	// commit.
	CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error)
	// ReadMirror reads paths of a mirror's work tree at a ref. A mirror
	// never fetched, or a ref it lacks, is ErrNotFound.
	ReadMirror(ctx context.Context, req MirrorReadRequest) (MirrorRead, error)
	// Push pushes a branch, with --force-with-lease on Lease and never
	// otherwise.
	Push(ctx context.Context, req PushRequest) error
	// Ticket says where a direct stream to one port of a running instance
	// connects.
	Ticket(ctx context.Context, in Instance, port int32) (Ticket, error)

	// StartProcess starts a process in a running instance and returns its
	// id, unique in the instance. The process does not end with ctx: it
	// runs until it exits, is signalled, or the instance stops, which ends
	// every process of it. Its output goes to a log of its own, capped at
	// DefaultProcessLogCap; an instance keeps DefaultFinishedProcesses
	// finished ones. The instance's status reports how many run.
	StartProcess(ctx context.Context, req ProcessRequest) (string, error)
	// AttachProcess gives out the process's log from offset from (from the
	// oldest offset held, if from is older), then, with follow, what it
	// writes until it ends or ctx is done. A process not there is
	// ErrNotFound.
	AttachProcess(ctx context.Context, in Instance, id string, from int64, follow bool, out func(OutputChunk) error) (Attached, error)
	// SignalProcess signals a process's group; one that has ended is no
	// error.
	SignalProcess(ctx context.Context, in Instance, id string, sig Signal) error
	// ListProcesses lists the instance's processes, oldest first.
	ListProcesses(ctx context.Context, in Instance) ([]Process, error)
}
