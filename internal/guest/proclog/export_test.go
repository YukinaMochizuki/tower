package proclog

import "os"

// SetAfterMkdir sets what runs between Create's making a process's
// directory and its opening it, nil for nothing.
func SetAfterMkdir(f func(root, id string)) { afterMkdir = f }

// SetRemoveAll sets how Prune removes a process's directory, nil for as
// it does.
func SetRemoveAll(f func(r *os.Root, name string) error) {
	if f == nil {
		f = (*os.Root).RemoveAll
	}
	removeAll = f
}
