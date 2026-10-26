package guestbin

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
)

// A copy between two guests on one network goes from one to the other
// directly, its bytes never leaving the network (docs/decisions/
// traffic.md): the receiving guest listens (OpReceive) and the sending one
// connects to it (OpSend), each a shim the provider runs at once. The
// sender opens with the token the provider gave both, a line, and the
// receiver takes the first connection that does, and no other.

// receiveTimeout bounds how long a receiver waits for its sender.
const receiveTimeout = 2 * time.Minute

// maxTokenLine bounds the line a connection opens with.
const maxTokenLine = 256

// receiveOp listens on every address of the guest, on a port of the
// kernel's choosing, which it writes as a line; then it extracts into the
// directory Path the tar sent by the first connection presenting Token,
// and ends. Its stdin ending without its end frame ends it.
func receiveOp(h Header, in io.Reader, stdout, stderr io.Writer) int {
	if h.Token == "" || !filepath.IsAbs(h.Path) {
		return failed(stderr, ExitFailed, errors.New("a receive needs a token and an absolute directory"))
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	defer func() { _ = ln.Close() }()
	if _, err := fmt.Fprintln(stdout, ln.Addr().(*net.TCPAddr).Port); err != nil {
		return ExitFailed
	}
	// The call gone, its stdin ends, and the listener goes with it.
	go func() {
		for {
			if _, _, err := ReadFrame(in); err != nil {
				_ = ln.Close()
				return
			}
		}
	}()
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(receiveTimeout))
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return failed(stderr, ExitFailed, fmt.Errorf("waiting for the sender: %w", err))
		}
		r := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := r.ReadSlice('\n')
		if err != nil || len(line) > maxTokenLine ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSuffix(string(line), "\n")), []byte(h.Token)) != 1 {
			_ = conn.Close()
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		_ = ln.Close()
		err = tarx.Extract(r, h.Path)
		_ = conn.Close()
		if err != nil {
			return failed(stderr, ExitFailed, err)
		}
		return 0
	}
}

// sendOp connects to Addr and sends Token, a line, then a tar of Path, its
// top name To when set.
func sendOp(h Header, stderr io.Writer) int {
	if h.Token == "" || h.Addr == "" {
		return failed(stderr, ExitFailed, errors.New("a send needs an address and a token"))
	}
	if _, err := os.Lstat(h.Path); errors.Is(err, os.ErrNotExist) {
		return failed(stderr, ExitNotExist, err)
	}
	conn, err := net.DialTimeout("tcp", h.Addr, 30*time.Second)
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, h.Token+"\n"); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarx.Write(pw, h.Path)) }()
	var r io.Reader = pr
	if h.To != "" {
		r = tarx.Rename(pr, filepath.Base(filepath.Clean(h.Path)), h.To)
	}
	if _, err := io.Copy(conn, r); err != nil {
		_ = pr.CloseWithError(err)
		return failed(stderr, ExitFailed, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	// The receiver closes once it extracted it all.
	_, _ = io.Copy(io.Discard, conn)
	return 0
}
