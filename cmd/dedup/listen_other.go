//go:build !linux

package main

import "syscall"

var reusePort func(network, address string, c syscall.RawConn) error
