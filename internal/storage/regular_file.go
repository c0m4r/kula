package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openRegular opens a file Kula keeps in its storage directory: a tier, its
// seek-index sidecar or a migration's temporary copy. None is ever a symbolic
// link or anything but a regular file. A link planted in its place would have
// Kula read, or overwrite, whatever it points to, and a FIFO blocks an open
// for reading until a writer comes, or every read after it. O_NOFOLLOW
// refuses a link as the last path component (the directory itself may be
// one), and O_NONBLOCK lets the open of a FIFO return so the type can be
// checked. A regular file's reads and writes ignore O_NONBLOCK.
func openRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symbolic link; storage files must be regular files: %w", path, err)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file (%s); storage files must be regular files", path, info.Mode().Type())
	}
	return f, nil
}
