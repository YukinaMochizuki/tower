// Package proclog is how a guest's processes are kept on disk, apart from
// hostd: a directory per process holding what it is (meta.json, never its
// environment), its log, how it ended (exit.json), and a lock its
// supervisor holds while it lives. The supervisor in the guest
// (internal/guest/guestbin) writes them; hostd reads them back, directly
// for a container, whose directory is on the host, or through the guest's
// own tower-guest for a VM, so a process outlives the call that started
// it and hostd, and a restarted hostd reads it back.
//
// The log is a run of segments, log-<offset>, each holding records of one
// stream's bytes (a byte 'o' or 'e', a big-endian uint32 length, the
// bytes), the segment's name its first byte's offset, stdout and stderr
// counted together. A segment that holds LogCap bytes is followed by a
// new one, and one wholly older than the last LogCap bytes is removed, so
// what is held is the last LogCap bytes at least and twice that at most;
// a read gives out the last LogCap bytes alone.
//
// A process without exit.json whose lock is free has lost its supervisor,
// as when its guest stopped: it reads as killed by SIGKILL.
package proclog

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Files of a process's directory.
const (
	MetaFile = "meta.json"
	ExitFile = "exit.json"
	LockFile = "lock"
	// PIDFile is the process's own pid, its process group's id, in the
	// guest's pid namespace.
	PIDFile   = "pid"
	segPrefix = "log-"
)

// Meta is what a process is: never its environment. Dir is the absolute
// directory it ran in, the guest's default for one that asked for none.
type Meta struct {
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
	Command []string  `json:"command"`
	Dir     string    `json:"dir,omitempty"`
	Started time.Time `json:"started"`
	LogCap  int64     `json:"logCap"`
}

// Exit is how a process ended: Code, 128 plus the signal's number for one
// a signal ended, and Signal, 0 if it exited. Usage is what it used, nil
// for a process not measured.
type Exit struct {
	Code   int    `json:"code"`
	Signal int    `json:"signal"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Usage is what a process measured in a cgroup of its own used, its
// children included: its peak memory, whether an OOM killed it, the CPU
// time it used and the time it waited for a CPU (cpu.pressure's "some"),
// and how long it ran. A field its guest's kernel does not offer is nil.
type Usage struct {
	MemoryPeak *int64         `json:"memoryPeak,omitempty"`
	OOMKilled  *bool          `json:"oomKilled,omitempty"`
	CPUUsage   *time.Duration `json:"cpuUsage,omitempty"`
	CPUWait    *time.Duration `json:"cpuWait,omitempty"`
	Wall       time.Duration  `json:"wall"`
}

// Killed is the exit of a process whose supervisor died with it.
var Killed = Exit{Code: 128 + int(syscall.SIGKILL), Signal: int(syscall.SIGKILL)}

// Info is a process as its directory holds it.
type Info struct {
	Meta
	// Exit is nil while it runs.
	Exit *Exit `json:"exit,omitempty"`
}

// Chunk is what a process wrote, at its offset in its log.
type Chunk struct {
	Stderr bool   `json:"stderr,omitempty"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

// End is where a read of a log ended.
type End struct {
	HeldFrom int64 `json:"heldFrom"`
	Next     int64 `json:"next"`
	Exit     *Exit `json:"exit,omitempty"`
}

// ErrNotFound is a process not in the directory.
var ErrNotFound = errors.New("proclog: no such process")

// NewID is a new process id: its start time, so ids sort oldest first.
func NewID(now time.Time) string { return fmt.Sprintf("p%016x", now.UnixNano()) }

// ValidID reports whether id can name a process's directory.
func ValidID(id string) bool {
	if len(id) != 17 || id[0] != 'p' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 16, 64)
	return err == nil
}

// Writer appends a process's output to its log. It is not safe for
// concurrent use.
type Writer struct {
	dir     string
	cap     int64
	seg     *os.File
	segFrom int64 // the current segment's first offset
	next    int64
}

// NewWriter starts the log of the process in dir, capped at logCap bytes
// (none if 0).
func NewWriter(dir string, logCap int64) (*Writer, error) {
	w := &Writer{dir: dir, cap: logCap}
	return w, w.open(0)
}

func (w *Writer) open(from int64) error {
	f, err := os.OpenFile(filepath.Join(w.dir, segName(from)), os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if w.seg != nil {
		_ = w.seg.Close()
	}
	w.seg, w.segFrom = f, from
	return nil
}

func segName(from int64) string { return fmt.Sprintf("%s%020d", segPrefix, from) }

// Write appends one record of a stream.
func (w *Writer) Write(stderr bool, p []byte) error {
	for len(p) > 0 {
		// A record is at most 1 MiB, so a reader's buffer is bounded.
		n := min(len(p), 1<<20)
		rec := make([]byte, 5+n)
		rec[0] = 'o'
		if stderr {
			rec[0] = 'e'
		}
		binary.BigEndian.PutUint32(rec[1:5], uint32(n))
		copy(rec[5:], p[:n])
		if _, err := w.seg.Write(rec); err != nil {
			return err
		}
		w.next += int64(n)
		p = p[n:]
		if w.cap > 0 && w.next-w.segFrom >= w.cap {
			if err := w.open(w.next); err != nil {
				return err
			}
			w.prune()
		}
	}
	return nil
}

// prune removes the segments wholly older than the last cap bytes.
func (w *Writer) prune() {
	dir, err := os.OpenRoot(w.dir)
	if err != nil {
		return
	}
	segs, err := segments(dir)
	_ = dir.Close()
	if err != nil {
		return
	}
	for i, s := range segs {
		end := w.next
		if i+1 < len(segs) {
			end = segs[i+1]
		}
		if end <= w.next-w.cap {
			_ = os.Remove(filepath.Join(w.dir, segName(s)))
		}
	}
}

// Close closes the log.
func (w *Writer) Close() error { return w.seg.Close() }

// segments are the first offsets of a log's segments, oldest first: the
// files named as the writer names them, and no other.
func segments(dir *os.Root) ([]int64, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		if v, ok := strings.CutPrefix(e.Name(), segPrefix); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && e.Name() == segName(n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// WriteMeta writes a process's meta.json.
func WriteMeta(dir string, m Meta) error {
	return writeJSON(filepath.Join(dir, MetaFile), m)
}

// WriteExit writes how a process ended, atomically.
func WriteExit(dir string, e Exit) error {
	return writeJSON(filepath.Join(dir, ExitFile), e)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Lock takes the lock of a process's directory, which its supervisor holds
// while it lives, and returns the file holding it.
func Lock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// What hostd reads of a process's directory is written in the guest,
// which can put anything there: a symlink to a host file, a FIFO, a file
// of any size, segments of any name. Every read is confined to the
// directory (os.Root), opens without blocking and takes a regular file
// alone, and reads no more than it bounds.
const (
	// maxMeta and maxExit bound meta.json and exit.json.
	maxMeta = 64 << 10
	maxExit = 4 << 10
	// maxRecord is the most a record holds, as the writer writes them.
	maxRecord = 1 << 20
	// maxSegments is the most segments a read looks at, the newest: the
	// writer keeps three at most.
	maxSegments = 8
	// segmentRetries bounds the reads again of a log whose segments went
	// as it was read.
	segmentRetries = 8
)

// MaxLogCap is the most of a log a read gives out, whatever its meta.json
// says.
var MaxLogCap int64 = 1 << 30

// errNotRegular is a file of a process's directory that is not a regular
// file.
var errNotRegular = errors.New("proclog: not a regular file")

// openFile opens a regular file of dir for reading, without blocking.
func openFile(dir *os.Root, name string) (*os.File, error) {
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", name, cmp.Or(err, errNotRegular))
	}
	return f, nil
}

// readSmall reads a regular file of dir of at most limit bytes.
func readSmall(dir *os.Root, name string, limit int64) ([]byte, error) {
	f, err := openFile(dir, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is over %d bytes", name, limit)
	}
	return b, err
}

// openProcess opens a process's directory of root, confined to it.
func openProcess(root, id string) (*os.Root, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	r, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	// Not a symlink: one, even within root, is not a process the
	// supervisor made.
	if st, err := r.Lstat(id); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	dir, err := r.OpenRoot(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	return dir, err
}

// alive reports whether a process's supervisor holds its lock.
func alive(dir *os.Root) bool {
	f, err := openFile(dir, LockFile)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// readExit reads how a process ended: exit.json, or Killed if its
// supervisor is gone without one; nil while it runs.
func readExit(dir *os.Root) (*Exit, error) {
	parse := func(b []byte) (*Exit, error) {
		var e Exit
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		return &e, nil
	}
	b, err := readSmall(dir, ExitFile, maxExit)
	if err == nil {
		return parse(b)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if alive(dir) {
		return nil, nil
	}
	// It may have written its exit as it let go.
	if b, err := readSmall(dir, ExitFile, maxExit); err == nil {
		if e, err := parse(b); err == nil {
			return e, nil
		}
	}
	k := Killed
	return &k, nil
}

// Read reads one process of root.
func Read(root, id string) (Info, error) {
	dir, err := openProcess(root, id)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = dir.Close() }()
	return readInfo(dir, id)
}

func readInfo(dir *os.Root, id string) (Info, error) {
	b, err := readSmall(dir, MetaFile, maxMeta)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Info{}, err
	}
	var in Info
	if err := json.Unmarshal(b, &in.Meta); err != nil {
		return Info{}, err
	}
	in.Exit, err = readExit(dir)
	return in, err
}

// List lists the processes of root, oldest first. A root not there holds
// none.
func List(root string) ([]Info, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		in, err := Read(root, e.Name())
		if errors.Is(err, ErrNotFound) {
			continue // one being made, or removed
		}
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// Prune removes the oldest finished processes of root beyond keep.
func Prune(root string, keep int) error {
	list, err := List(root)
	if err != nil {
		return err
	}
	var finished []string
	for _, p := range list {
		if p.Exit != nil {
			finished = append(finished, p.ID)
		}
	}
	if len(finished) <= keep {
		return nil
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for len(finished) > keep {
		if err := r.RemoveAll(finished[0]); err != nil {
			return err
		}
		finished = finished[1:]
	}
	return nil
}

// FollowPoll is how often a followed log is read again.
var FollowPoll = 20 * time.Millisecond

// ReadLog gives out a process's log from offset from (from the oldest it
// holds, if from is older), then, with follow, what it writes until it
// ends or ctx is done, which is ctx's error.
func ReadLog(ctx context.Context, root, id string, from int64, follow bool, out func(Chunk) error) (End, error) {
	dir, err := openProcess(root, id)
	if err != nil {
		return End{}, err
	}
	defer func() { _ = dir.Close() }()
	in, err := readInfo(dir, id)
	if err != nil {
		return End{}, err
	}
	logCap := in.LogCap
	if logCap <= 0 || logCap > MaxLogCap {
		logCap = MaxLogCap
	}
	at := from
	for {
		// The exit is read before the log, so a log read after it holds
		// everything the process wrote.
		exit, err := readExit(dir)
		if err != nil {
			return End{}, err
		}
		end, err := readSegments(ctx, dir, logCap, &at, out)
		if err != nil {
			return End{}, err
		}
		end.Exit = exit
		if !follow || exit != nil {
			return end, nil
		}
		select {
		case <-ctx.Done():
			return end, ctx.Err()
		case <-time.After(FollowPoll):
		}
	}
}

// readSegments gives out what the log holds from *at on, moving *at past
// it, and says where it ends. A segment that went as it was read is read
// again, a few times at most, and never after ctx is done.
func readSegments(ctx context.Context, dir *os.Root, logCap int64, at *int64, out func(Chunk) error) (End, error) {
	for range segmentRetries {
		if err := ctx.Err(); err != nil {
			return End{}, err
		}
		segs, err := segments(dir)
		if err != nil {
			return End{}, err
		}
		end, retry, err := readOnce(dir, segs, logCap, at, out)
		if !retry {
			return end, err
		}
	}
	return End{}, errors.New("proclog: the log's segments kept going as they were read")
}

// readOnce reads the segments as listed, the newest maxSegments of them;
// retry says one went under it, and the list is read again.
func readOnce(dir *os.Root, segs []int64, logCap int64, at *int64, out func(Chunk) error) (End, bool, error) {
	if len(segs) == 0 {
		return End{}, false, nil
	}
	if len(segs) > maxSegments {
		segs = segs[len(segs)-maxSegments:]
	}
	// next is where the last segment ends, which reading it says.
	type rec struct {
		stderr bool
		off    int64
		data   []byte
	}
	var recs []rec
	next := segs[0]
	for i, s := range segs {
		f, err := openFile(dir, segName(s))
		if errors.Is(err, fs.ErrNotExist) {
			return End{}, true, nil
		}
		if err != nil {
			return End{}, false, err
		}
		off := s
		// A segment holds logCap bytes and a record more, at most.
		r := bufio.NewReader(io.LimitReader(f, logCap+maxRecord+5))
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break // the end, or a record being written
			}
			n := int64(binary.BigEndian.Uint32(hdr[1:]))
			if n > maxRecord || (hdr[0] != 'o' && hdr[0] != 'e') {
				break // no record the writer writes
			}
			// Records wholly before *at are skipped unread.
			if off+n <= *at {
				if _, err := r.Discard(int(n)); err != nil {
					break
				}
				off += n
				continue
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(r, data); err != nil {
				break
			}
			recs = append(recs, rec{hdr[0] == 'e', off, data})
			off += n
		}
		_ = f.Close()
		if i == len(segs)-1 {
			next = off
		}
	}
	held := max(segs[0], next-logCap)
	from := max(*at, held)
	for _, r := range recs {
		if r.off+int64(len(r.data)) <= from {
			continue
		}
		data := r.data
		off := r.off
		if off < from {
			data, off = data[from-off:], from
		}
		if err := out(Chunk{Stderr: r.stderr, Offset: off, Data: data}); err != nil {
			return End{}, false, err
		}
	}
	*at = max(*at, next)
	return End{HeldFrom: held, Next: next}, false, nil
}
