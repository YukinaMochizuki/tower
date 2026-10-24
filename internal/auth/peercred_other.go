//go:build !linux

package auth

import (
	"errors"
	"net"
)

// PeerOf reports no credentials off Linux, so the node and admin sockets
// authenticate no one there. Only the CLI is built for other systems.
func PeerOf(net.Conn) (*Peer, error) {
	return nil, errors.New("peer credentials are only read on Linux")
}
