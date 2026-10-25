package chvm

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
)

// cappedLog is a log file kept within a size: a write that would take it
// past max moves it to path.1, replacing the one there, and starts it
// afresh, so the two hold at most twice max. Each write lands whole in one
// file, so writing whole records rotates at record boundaries; a single
// write larger than max takes a file of its own.
type cappedLog struct {
	path string
	max  int64
	f    *os.File
	size int64
	// head, if set, starts every file a rotation begins, as a console
	// log's files start with the boot they are of.
	head []byte
	// open opens the file at path; nil means openLog. Tests make it fail.
	open func(path string, flag int) (*os.File, error)
	// write writes to the file; nil means its Write. Tests make it fall
	// short.
	write func(f *os.File, p []byte) (int, error)
}

// openCapped opens the log at path to append to it, as a VM started again
// does.
func openCapped(path string, max int64) (*cappedLog, error) {
	f, err := openLog(path, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &cappedLog{path: path, max: max, f: f, size: fi.Size()}, nil
}

func openLog(path string, flag int) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|flag, 0o600)
}

func (c *cappedLog) Write(p []byte) (int, error) {
	if c.size > 0 && c.size+int64(len(p)) > c.max {
		if err := c.rotate(); err != nil {
			return 0, err
		}
	}
	return c.whole(p)
}

// whole writes p, a whole record or several, or none of it: a write that
// falls short, as on a full disk, is cut back off the file, so no part of
// a record stays in it to swallow what comes after.
func (c *cappedLog) whole(p []byte) (int, error) {
	write := c.write
	if write == nil {
		write = (*os.File).Write
	}
	n, err := write(c.f, p)
	if n == len(p) {
		c.size += int64(n)
		return n, err
	}
	if terr := c.f.Truncate(c.size); terr != nil {
		c.size += int64(n)
		return n, errors.Join(err, terr)
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return 0, err
}

// rotate moves the log to path.1 and starts it afresh. A rotation that
// moved the log but could not open it again left no file at path: the next
// one only opens it.
func (c *cappedLog) rotate() error {
	if err := os.Rename(c.path, c.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	open := c.open
	if open == nil {
		open = openLog
	}
	f, err := open(c.path, os.O_TRUNC)
	if err != nil {
		return err
	}
	_ = c.f.Close()
	c.f, c.size = f, 0
	if len(c.head) > 0 {
		_, _ = c.whole(c.head)
	}
	return nil
}

func (c *cappedLog) Close() error { return c.f.Close() }

// pumpConsole copies the guest's framed console from r to log until r ends,
// and returns the last exit status the guest recorded, nil if none. The
// console is the tenant's to write: what is not a well-formed record of a
// known stream is dropped, and nothing on it can stop the pump short of r
// ending. Neither can the log failing, as on a full disk: records are then
// dropped, so the guest is never held up by its log.
func pumpConsole(r io.Reader, log io.Writer, logErr func(error)) *console.ExitStatus {
	var exit *console.ExitStatus
	rd := console.NewReader(r)
	var buf []byte
	failing := false
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return exit
		}
		// ErrCorrupt comes with the record found after skipped bytes, or
		// with none, for a header claiming too long a payload.
		if err != nil && (!errors.Is(err, console.ErrCorrupt) || rec.Payload == nil) {
			continue
		}
		// Anything else, Boot records included, is not the guest's to
		// write.
		switch rec.Stream {
		case console.Stdout, console.Stderr, console.Init:
		case console.Exit:
			st, err := console.ExitOf(rec)
			if err != nil {
				continue
			}
			exit = &st
		default:
			continue
		}
		buf = console.AppendRecord(buf[:0], rec)
		if _, err := log.Write(buf); err != nil {
			if !failing && logErr != nil {
				logErr(err)
			}
			failing = true
			continue
		}
		failing = false
	}
}

// pumpRaw copies r to log until r ends, as the serial console is kept.
// What the log fails to take is dropped, and the next read is written
// again, so a log that fails for a while, as on a full disk or a directory
// made immutable, never holds up the writer: Cloud Hypervisor's serial
// device, which stalls the guest when it blocks.
func pumpRaw(r io.Reader, log io.Writer, logErr func(error)) {
	buf := make([]byte, 32<<10)
	failing := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := log.Write(buf[:n]); werr != nil {
				if !failing && logErr != nil {
					logErr(werr)
				}
				failing = true
			} else {
				failing = false
			}
		}
		if err != nil {
			return
		}
	}
}

// exitCode is what the launcher exits with for how the guest's process
// ended, as the guest recorded it: its exit code, or ExitNoRecord for no
// record, or one no process could have ended with.
func exitCode(st *console.ExitStatus) int {
	if st == nil || st.Code < 0 || st.Code > 255 {
		return ExitNoRecord
	}
	return int(st.Code)
}
