//go:build !darwin && !linux

package claudehook

// swapFiles reports that files can't be swapped atomically here.
func swapFiles(a, b string) error { return errNoSwap }
