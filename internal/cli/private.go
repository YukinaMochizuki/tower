package cli

import (
	"fmt"
	"os"
)

// A file that holds a secret is private: only its owner may read it, mode
// 0600.

// createPrivate opens path for writing, creating it only its owner may
// read; excl refuses a file that exists, else one that does is truncated
// and keeps its mode, as os.WriteFile's does.
func createPrivate(path string, excl bool) (*os.File, error) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if excl {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	return os.OpenFile(path, flag, 0o600)
}

// exposure says who besides its owner may read or change the open file f,
// "" when no one may, and the command that makes it private.
func exposure(f *os.File) (who, fix string, err error) {
	fi, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return "", "", nil
	}
	return fmt.Sprintf("mode %04o", fi.Mode().Perm()), "chmod 600 " + f.Name(), nil
}
