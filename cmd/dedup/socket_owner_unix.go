//go:build unix

package main

import (
	"fmt"
	"io/fs"
	"syscall"
)

func checkSocketOwner(info fs.FileInfo, uid int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine socket owner")
	}
	if uint64(stat.Uid) != uint64(uid) {
		return fmt.Errorf("socket UID %d differs from effective UID %d", stat.Uid, uid)
	}
	return nil
}
