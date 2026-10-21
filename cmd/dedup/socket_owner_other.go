//go:build !unix

package main

import (
	"fmt"
	"io/fs"
)

func checkSocketOwner(_ fs.FileInfo, _ int) error {
	return fmt.Errorf("socket ownership checks unavailable on this platform")
}
