package main

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// setMemoryLimit holds the heap near what the caches and budgets allow; with
// only GOGC a full cache lets it grow to twice its live size. GOMEMLIMIT wins.
func setMemoryLimit(mib int64) {
	if mib <= 0 || os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(mib << 20)
	log.Printf("Go memory limit %d MiB", mib)
}

// preferOOMKill raises oom_score_adj so that, short of memory, the kernel
// kills this process before the proxy it serves. Raising needs no privilege;
// a higher score set by the service manager is kept.
func preferOOMKill(score int) {
	const path = "/proc/self/oom_score_adj"
	if score <= 0 {
		return
	}
	cur, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(cur))); err == nil && n >= score {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(score)), 0); err != nil {
		log.Printf("oom_score_adj: %v", err)
	}
}
