// Package guestbin is tower-guest, the static program a provider puts in
// each of its guests (cmd/tower-guest): hostd binds it read-only into a
// container and copies it into a VM, and the hangar provider writes it
// into a hangar machine over hangar's exec. Every operation in a guest
// runs through it, by docker exec or ssh on hostd and by hangar's exec on
// hangar, so every kind of guest is driven alike (internal/guest/
// guestops):
//
//   - shim: one operation, named by a header on its stdin: run a command
//     (exec), read, write, copy out and copy in files, send a path to
//     another guest or receive one (transfer.go), take a tar from a server
//     or post one to it (http.go), start a process under a supervisor,
//     signal one, list them and read their logs;
//   - init: a container's first process, which reaps orphans and runs the
//     image's own entrypoint, and outlives it;
//   - supervise: a process's supervisor, detached from the call that
//     started it (internal/guest/proclog).
//
// Its stdin is framed (Frame) so that, beside the command's own input, it
// carries the header (what to do, with the environment of this call
// alone, which so never enters a command line or a file) and the
// provider's signal; a stdin that ends without its end frame, as when the
// call or the provider is gone, kills what it runs.
package guestbin

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Frame types on a shim's stdin.
const (
	// FrameHeader is the operation, a Header as JSON; it comes first.
	FrameHeader = 'h'
	// FrameData is the command's stdin, or what is written.
	FrameData = 'd'
	// FrameEOF ends the data: the command's stdin is closed.
	FrameEOF = 'e'
	// FrameSignal sends its byte, a signal's number, to the command's
	// process group.
	FrameSignal = 's'
)

// maxFrame bounds one frame's length.
const maxFrame = 1 << 20

// WriteFrame writes a frame.
func WriteFrame(w io.Writer, typ byte, data []byte) error {
	if len(data) > maxFrame {
		return fmt.Errorf("a frame of %d bytes is over %d", len(data), maxFrame)
	}
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadFrame reads a frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("a frame of %d bytes is over %d", n, maxFrame)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return hdr[0], data, nil
}

// Header is a shim's operation.
type Header struct {
	Op string `json:"op"`
	// Exec and Start: the command, the environment added for it alone, and
	// its working directory.
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	// Read, Write, CopyOut: the path; CopyIn: the directory.
	Path   string `json:"path,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	// Processes: their root directory, the process, its name, its log's
	// cap and how many finished ones are kept, a signal, and where a log
	// is read from.
	Root   string `json:"root,omitempty"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	LogCap int64  `json:"logCap,omitempty"`
	Keep   int    `json:"keep,omitempty"`
	Signal int    `json:"signal,omitempty"`
	From   int64  `json:"from,omitempty"`
	Follow bool   `json:"follow,omitempty"`
	// Start: whether the command runs in a cgroup of its own, measured
	// (internal/guest/cgroup), and the memory that cgroup is held to, none
	// if 0. A guest where no cgroup can be made runs it unmeasured.
	Measure   bool  `json:"measure,omitempty"`
	MemoryMax int64 `json:"memoryMax,omitempty"`
	// Send and Receive (transfer.go): where the receiver listens, the
	// token both share, and the top name the sent path takes.
	Addr  string `json:"addr,omitempty"`
	Token string `json:"token,omitempty"`
	To    string `json:"to,omitempty"`
	// Fetch and Post (http.go): the URL, the probe's URL, and the
	// credential, as Token.
	URL   string `json:"url,omitempty"`
	Probe string `json:"probe,omitempty"`
}

// The operations.
const (
	OpExec    = "exec"
	OpRead    = "read"
	OpWrite   = "write"
	OpCopyOut = "copyout"
	OpCopyIn  = "copyin"
	OpStart   = "start"
	OpSignal  = "signal"
	OpList    = "list"
	OpLog     = "log"
	OpSend    = "send"
	OpReceive = "receive"
	OpFetch   = "fetch"
	OpPost    = "post"
)

// Exit codes a shim says what went wrong with in every operation but
// exec, which exits as its command did, 127 for one that cannot start, as
// a shell does.
const (
	// ExitNotExist is a path, or a process, that is not there.
	ExitNotExist = 200
	// ExitBadOffset is a write not where the file ends.
	ExitBadOffset = 201
	// ExitFailed is any other failure, its message on stderr.
	ExitFailed = 202
	// ExitUnreachable is a fetch or post whose probe failed, nothing sent.
	ExitUnreachable = 203
	// ExitCannotStart is exec's for a command that cannot start.
	ExitCannotStart = 127
)

// WriteHeader writes the header frame.
func WriteHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameHeader, b)
}

// readHeader reads the header frame.
func readHeader(r io.Reader) (Header, error) {
	typ, data, err := ReadFrame(r)
	if err != nil {
		return Header{}, err
	}
	if typ != FrameHeader {
		return Header{}, errors.New("the first frame is no header")
	}
	var h Header
	return h, json.Unmarshal(data, &h)
}

// FrameWriter writes data frames, its Close the end frame.
type FrameWriter struct{ W io.Writer }

func (f FrameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), maxFrame)
		if err := WriteFrame(f.W, FrameData, p[:c]); err != nil {
			return n, err
		}
		n += c
		p = p[c:]
	}
	return n, nil
}

func (f FrameWriter) Close() error { return WriteFrame(f.W, FrameEOF, nil) }

// ReadData reads the data frames on in to w until the end frame, as an
// operation takes what its call sends; an in that ends without it is an
// error.
func ReadData(in io.Reader, w io.Writer) error { return dataFrames(in, w) }
