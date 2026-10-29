//go:build !unix

package main

import "errors"

func lockMemory([]byte) error { return errors.New("mlock is unavailable on this platform") }

// diskFree is unknown here, so reserve() skips the free-space check.
func diskFree(string) (int64, error) { return 0, errors.New("statfs is unavailable on this platform") }
