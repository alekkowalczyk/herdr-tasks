//go:build linux

package claudehook

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// swapFiles exchanges the files at a and b atomically, or returns errNoSwap
// when the kernel or filesystem can't (or a sandbox blocks renameat2).
func swapFiles(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EPERM):
		return errNoSwap
	}
	return &os.LinkError{Op: "swap", Old: a, New: b, Err: err}
}
