package guestbin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// giveDirMain is tower-guest give-dir UID GID DIR, run as root by the
// container's driver: it makes DIR on the container's root file system and
// gives it to UID:GID while it is empty, the image's user's /repos. It
// drops NET_ADMIN first and runs nothing of the image's, so neither a
// command the box put on PATH nor a link it put at DIR runs or is followed
// with root's capabilities: a DIR that is a link is left as it is.
func giveDirMain(args []string, stderr io.Writer) int {
	if err := dropNetAdmin(); err != nil {
		return failed(stderr, ExitCannotStart, err)
	}
	if len(args) != 3 {
		return failed(stderr, 2, errors.New("usage: tower-guest give-dir UID GID DIR"))
	}
	uid, err1 := strconv.Atoi(args[0])
	gid, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil {
		return failed(stderr, 2, fmt.Errorf("give-dir: %q:%q is no uid and gid", args[0], args[1]))
	}
	if err := giveDir(args[2], uid, gid); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

func giveDir(dir string, uid, gid int) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		// A link or a file the box put there: not tower's to give.
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > 0 {
		return nil
	}
	return os.Lchown(dir, uid, gid)
}
