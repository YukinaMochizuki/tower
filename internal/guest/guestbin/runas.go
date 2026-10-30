package guestbin

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"syscall"

	"github.com/YukinaMochizuki/tower/internal/guest"
)

// A provider whose shim runs as the container's user cannot measure what
// it starts: only root makes a cgroup under /tower. The hangar provider
// asks who that user is (OpIdentity, run as the user), then runs a start's
// shim as root, its As having the supervisor drop to that user as it
// starts the command: its uid, gid and groups, and its HOME; nothing else
// of the environment differs between the two
// (docs/decisions/process-usage.md). The supervisor stays root to measure
// the command; the files it makes are root's, which the user reads and
// writes none of (proclog.Readable). Every other operation, signal, list
// and log among them, runs as the user.

// self is who this process runs as.
func self() guest.Identity {
	id := guest.Identity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Home: os.Getenv("HOME")}
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			id.Groups = append(id.Groups, uint32(g))
		}
	}
	return id
}

// identityOp writes who the shim runs as.
func identityOp(stdout, stderr io.Writer) int {
	if err := json.NewEncoder(stdout).Encode(self()); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

// dropTo is who a command started as as runs as, nil for as the
// supervisor runs: as names none, or the one it runs as already, which it
// may not even set again (setgroups wants CAP_SETGID).
func dropTo(as *guest.Identity) *guest.Identity {
	if as == nil {
		return nil
	}
	me := self()
	if me.UID == as.UID && me.GID == as.GID && sameGroups(me.Groups, as.Groups) {
		return nil
	}
	return as
}

func sameGroups(a, b []uint32) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// credential is the credential a command dropping to as starts with: its
// groups set, never the supervisor's kept. Starting as a uid other than 0
// takes all of root's capabilities from it, as the kernel does without
// SECBIT_KEEP_CAPS, which nothing here sets.
func credential(as *guest.Identity) *syscall.Credential {
	return &syscall.Credential{Uid: as.UID, Gid: as.GID, Groups: slices.Clone(as.Groups)}
}

// commandEnv is a command's environment: the shim's, its HOME the one as
// had if it drops to as, with env over them.
func commandEnv(env map[string]string, as *guest.Identity) []string {
	if as == nil || as.Home == "" {
		return environ(env)
	}
	if _, ok := env["HOME"]; ok {
		return environ(env)
	}
	with := map[string]string{"HOME": as.Home}
	for k, v := range env {
		with[k] = v
	}
	return environ(with)
}
