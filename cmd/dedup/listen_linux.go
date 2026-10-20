package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePort lets a new instance bind the address while the old one drains.
func reusePort(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}
