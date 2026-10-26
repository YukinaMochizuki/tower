// Package guest is what tower runs in a guest, whichever provider made
// it: tower-guest (guestbin), what it keeps of each process (proclog), and
// the operations a provider does through it (guestops). hostd runs it in
// its containers and VMs, and the hangar provider in hangar's. This
// package holds what those operations take and give back, which hostd's
// drivers name as their own (internal/hostd/driver).
package guest

import (
	"errors"
	"io"
	"time"
)

// Errors the operations return, wrapped or not.
var (
	// ErrBadOffset means a chunk of a file does not start where the file
	// ends.
	ErrBadOffset = errors.New("guest: the chunk does not start where the file ends")
	// ErrNoProcess means the guest keeps no process of the id.
	ErrNoProcess = errors.New("guest: no such process")
)

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

// ProcessSpec is a process started in a guest apart from any call.
type ProcessSpec struct {
	Name string
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Env is added to the guest's environment for this process alone, and
	// never kept.
	Env map[string]string
	Dir string
	// LogCap bounds what its log holds, its oldest output dropped first;
	// Keep is how many finished processes its guest keeps from now on,
	// its oldest going first.
	LogCap int64
	Keep   int
}

// ProcessExit is how a process ended: Code, 128 plus the signal's number
// for one a signal ended, and Signal, 0 if it exited.
type ProcessExit struct {
	Code, Signal int
}

// ProcessInfo is a process of a guest as the guest keeps it.
type ProcessInfo struct {
	ID, Name string
	Command  []string
	Started  time.Time
	// Exit is nil while it runs.
	Exit *ProcessExit
}

// LogChunk is what a process wrote, at its offset in its log, stdout and
// stderr counted together.
type LogChunk struct {
	Stderr bool
	Offset int64
	Data   []byte
}

// LogEnd is where a read of a process's log ended: the oldest offset it
// holds, the offset to read on from, and how the process ended, nil while
// it runs.
type LogEnd struct {
	HeldFrom, Next int64
	Exit           *ProcessExit
}
