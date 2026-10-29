//go:build unix

package main

import (
	"golang.org/x/sys/unix"
)

// lockMemory keeps the master key out of swap. Failure is logged and ignored:
// an unprivileged process may be below its RLIMIT_MEMLOCK.
func lockMemory(b []byte) error {
	return unix.Mlock(b)
}

// diskFree is the space the cache may fill on the filesystem holding dir.
func diskFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
