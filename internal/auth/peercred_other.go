//go:build !linux

package auth

import (
	"errors"
	"net"
)

// PeerOf reports no credentials off Linux, so the admin socket
// authenticates no one there.
func PeerOf(net.Conn) (*Peer, error) {
	return nil, errors.New("peer credentials are only read on Linux")
}
