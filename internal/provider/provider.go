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
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// BuildRequest is an image built on the target and pushed to tower's
// registry.
type BuildRequest struct {
	// Context is the build context, a tar stream with its Dockerfile.
	Context io.Reader
	// Repository is where in the registry the image goes, such as
	// "web/box-images".
	Repository string
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
	// Copy copies a path from one instance of the target to another.
	Copy(ctx context.Context, from Instance, fromPath string, to Instance, toPath string) error
	// BuildImage builds an image and pushes it to the registry, returning
	// its reference by digest. The same context builds the same digest.
	BuildImage(ctx context.Context, req BuildRequest) (image string, err error)
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
	// Push pushes a branch, with --force-with-lease on Lease and never
	// otherwise.
	Push(ctx context.Context, req PushRequest) error
	// Ticket says where a direct stream to one port of a running instance
	// connects.
	Ticket(ctx context.Context, in Instance, port int32) (Ticket, error)
}
