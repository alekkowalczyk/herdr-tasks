//go:build darwin

package claudehook

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// swapFiles exchanges the files at a and b atomically, or returns errNoSwap
// when the filesystem can't.
func swapFiles(a, b string) error {
	err := unix.RenamexNp(a, b, unix.RENAME_SWAP)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EINVAL):
		return errNoSwap
	}
	return &os.LinkError{Op: "swap", Old: a, New: b, Err: err}
}
