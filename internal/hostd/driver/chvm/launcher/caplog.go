package launcher

import (
	"io"
	"os"
	"syscall"
)

// cappedLog is a log file kept within a size: a write that would take it
// past max moves it to path.1, replacing the one there, and starts it
// afresh, so the two hold at most twice max. Each write lands whole in one
// file; a single write larger than max takes a file of its own.
type cappedLog struct {
	path string
	max  int64
	f    *os.File
	size int64
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
	n, err := c.f.Write(p)
	c.size += int64(n)
	return n, err
}

// rotate moves the log to path.1 and starts a new one.
func (c *cappedLog) rotate() error {
	if err := os.Rename(c.path, c.path+".1"); err != nil {
		return err
	}
	f, err := openLog(c.path, os.O_TRUNC)
	if err != nil {
		return err
	}
	_ = c.f.Close()
	c.f, c.size = f, 0
	return nil
}

func (c *cappedLog) Close() error { return c.f.Close() }

// pumpRaw copies r to log until r ends, as the serial console is kept.
// What the log fails to take is dropped, and the next read is written
// again, so a log that fails for a while, as on a full disk, never holds
// up the writer: Cloud Hypervisor's serial device, which stalls the guest
// when it blocks.
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
