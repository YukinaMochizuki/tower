package guestbin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/tower/internal/guest"
)

// In a container that joins a tailnet, Docker gives every process
// NET_ADMIN, which only its tailscaled is to have, so nothing else in the
// box changes its interfaces, routes or packet filter itself. A root box
// can still drive its own tailscaled through its socket, as far as its
// key's tags and the tailnet's policy let that node
// (docs/decisions/tailnet.md). Its first
// process and its shims run as root so they can drop NET_ADMIN from their
// bounding set, which needs CAP_SETPCAP, and then become the image's user.
// What they run inherits neither the capability nor a way back to it:
// no-new-privileges holds every exec to the bounding set.

// boxCapability is the capability a box's processes run without:
// NET_ADMIN, which a test that cannot hold it takes another for.
var boxCapability uintptr = unix.CAP_NET_ADMIN

// dropNetAdmin takes CAP_NET_ADMIN out of this process's bounding,
// effective, permitted, inheritable and ambient sets.
func dropNetAdmin() error { return dropCapability(boxCapability) }

// dropCapability takes a capability out of this process's bounding,
// effective, permitted, inheritable and ambient sets. Linux keeps them per
// thread, so it changes every thread at once; in a binary linked with cgo,
// where that cannot be done, it changes the calling thread alone and locks
// the goroutine to it, so what it starts next starts from that thread.
func dropCapability(capability uintptr) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("reading capabilities: %w", err)
	}
	bit := uint32(1) << (capability % 32)
	i := capability / 32
	data[i].Effective &^= bit
	data[i].Permitted &^= bit
	data[i].Inheritable &^= bit
	calls := []struct {
		what   string
		nr     uintptr
		a1, a2 uintptr
		a3     uintptr
	}{
		// The bounding set first: it needs CAP_SETPCAP, which capset keeps.
		{"the bounding set", unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0},
		{"the ambient set", unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, capability},
		{"the other sets", unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0},
	}
	allThreads := true
	for _, c := range calls {
		var errno syscall.Errno
		if allThreads {
			_, _, errno = syscall.AllThreadsSyscall(c.nr, c.a1, c.a2, c.a3)
			if errno == syscall.ENOTSUP {
				allThreads = false
				runtime.LockOSThread()
			}
		}
		if !allThreads {
			_, _, errno = syscall.RawSyscall(c.nr, c.a1, c.a2, c.a3)
		}
		// EINVAL: a kernel without the capability or the ambient set.
		if errno != 0 && errno != syscall.EINVAL {
			return fmt.Errorf("dropping capability %d from %s: %w", capability, c.what, errno)
		}
	}
	runtime.KeepAlive(&hdr)
	runtime.KeepAlive(&data)
	return nil
}

// boxUser is who a process of the box runs as: the image's user, resolved
// in the container's own /etc/passwd and /etc/group as Docker resolves it,
// and whether its HOME is set from there.
type boxUser struct {
	id      guest.Identity
	setHome bool
}

// becomeFromEnv, in a shim the container's driver started as root with
// guest.RunAsEnv set, drops NET_ADMIN and becomes the user it names, then
// takes the variables out of the environment. Without RunAsEnv it does
// nothing.
func becomeFromEnv() error {
	user, ok := os.LookupEnv(guest.RunAsEnv)
	if !ok {
		return nil
	}
	setHome := os.Getenv(guest.RunAsHomeEnv) == "1"
	_ = os.Unsetenv(guest.RunAsEnv)
	_ = os.Unsetenv(guest.RunAsHomeEnv)
	as, err := lookupRunAs(user, setHome, "/etc/passwd", "/etc/group")
	if err != nil {
		return err
	}
	return become(as)
}

// become drops NET_ADMIN, then takes as's groups, gid and uid, all of
// this process's threads at once; a uid other than 0 leaves no capability.
func become(as boxUser) error {
	if err := dropNetAdmin(); err != nil {
		return err
	}
	if as.setHome && as.id.Home != "" {
		_ = os.Setenv("HOME", as.id.Home)
	}
	if as.id.UID == 0 && as.id.GID == 0 && len(as.id.Groups) == 0 {
		return nil
	}
	groups := make([]int, 0, len(as.id.Groups))
	for _, g := range as.id.Groups {
		groups = append(groups, int(g))
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setting the groups of %d: %w", as.id.UID, err)
	}
	if err := syscall.Setgid(int(as.id.GID)); err != nil {
		return fmt.Errorf("setting gid %d: %w", as.id.GID, err)
	}
	if err := syscall.Setuid(int(as.id.UID)); err != nil {
		return fmt.Errorf("setting uid %d: %w", as.id.UID, err)
	}
	return nil
}

// lookupRunAs resolves a user as Docker names one, name or uid with an
// optional :group or :gid, in the passwd and group files given: a name
// the files lack is an error; a uid they lack is that uid with gid 0 and
// HOME "/", as Docker runs it. Its supplementary groups are those that
// list its name.
func lookupRunAs(user string, setHome bool, passwdPath, groupPath string) (boxUser, error) {
	as := boxUser{id: guest.Identity{Home: "/"}, setHome: setHome}
	name, group, hasGroup := strings.Cut(user, ":")
	if name == "" {
		name = "0"
	}
	passwd, err := readColonFile(passwdPath)
	if err != nil {
		return as, err
	}
	userName := ""
	found := false
	for _, f := range passwd {
		if len(f) < 7 {
			continue
		}
		if f[0] != name && f[2] != name {
			continue
		}
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		as.id.UID, as.id.GID, as.id.Home, userName, found = uint32(uid), uint32(gid), f[5], f[0], true
		break
	}
	if !found {
		uid, err := strconv.ParseUint(name, 10, 32)
		if err != nil {
			return as, fmt.Errorf("user %q is not in %s", name, passwdPath)
		}
		as.id.UID = uint32(uid)
	}
	groups, err := readColonFile(groupPath)
	if err != nil {
		return as, err
	}
	if hasGroup {
		gid, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			gid, err = 0, fmt.Errorf("group %q is not in %s", group, groupPath)
			for _, f := range groups {
				if len(f) >= 3 && f[0] == group {
					if n, perr := strconv.ParseUint(f[2], 10, 32); perr == nil {
						gid, err = n, nil
						break
					}
				}
			}
			if err != nil {
				return as, err
			}
		}
		as.id.GID = uint32(gid)
	}
	if userName != "" {
		for _, f := range groups {
			if len(f) < 4 {
				continue
			}
			if !slices.Contains(strings.Split(f[3], ","), userName) {
				continue
			}
			if n, err := strconv.ParseUint(f[2], 10, 32); err == nil && !slices.Contains(as.id.Groups, uint32(n)) {
				as.id.Groups = append(as.id.Groups, uint32(n))
			}
		}
	}
	return as, nil
}

// readColonFile reads a colon-separated file, such as /etc/passwd, one
// slice of fields a line; one not there is empty.
func readColonFile(p string) ([][]string, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out, sc.Err()
}

// droppedMain is tower-guest dropped [--as USER] [--as-home] -- COMMAND...:
// it drops NET_ADMIN, becomes USER, and executes COMMAND in its place,
// found on PATH. tower-guest init starts the image's entrypoint so in a
// container that joins a tailnet.
func droppedMain(args []string) int {
	user, setHome := "", false
	for len(args) > 0 && args[0] != "--" {
		switch {
		case args[0] == "--as" && len(args) > 1:
			user, args = args[1], args[2:]
		case args[0] == "--as-home":
			setHome, args = true, args[1:]
		default:
			fmt.Fprintf(os.Stderr, "tower-guest dropped: unknown argument %q\n", args[0])
			return 2
		}
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tower-guest dropped [--as USER] [--as-home] -- COMMAND...")
		return 2
	}
	args = args[1:]
	as, err := lookupRunAs(user, setHome, "/etc/passwd", "/etc/group")
	if err == nil {
		err = become(as)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tower-guest dropped: %v\n", err)
		return ExitCannotStart
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "tower-guest dropped: %v\n", err)
		return 127
	}
	err = syscall.Exec(bin, args, os.Environ())
	fmt.Fprintf(os.Stderr, "tower-guest dropped: %s: %v\n", args[0], err)
	return 126
}
