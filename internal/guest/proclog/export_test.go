package proclog

// SetAfterMkdir sets what runs between Create's making a process's
// directory and its opening it, nil for nothing.
func SetAfterMkdir(f func(root, id string)) { afterMkdir = f }
