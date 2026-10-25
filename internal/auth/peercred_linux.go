package auth

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// PeerOf returns the credentials the kernel reports for the process at the
// other end of a unix socket connection.
func PeerOf(c net.Conn) (*Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if credErr != nil {
		return nil, credErr
	}
	return &Peer{UID: cred.Uid, PID: cred.Pid}, nil
}
