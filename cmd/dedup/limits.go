package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// bind binds host:port, shareable with a successor instance on Linux, or
// unix:/path. A same-UID unix socket is replaced, and not removed on close, so a
// draining instance does not delete its successor's socket.
func bind(address string) (net.Listener, error) {
	path, ok := strings.CutPrefix(address, "unix:")
	if !ok {
		lc := net.ListenConfig{Control: reusePort}
		return lc.Listen(context.Background(), "tcp", address)
	}
	if err := removeExistingSocket(path, os.Geteuid()); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

// The installer must control access to the parent directory: these path-based
// checks and unlink are not atomic against concurrent directory mutations.
func removeExistingSocket(path string, uid int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse to replace non-socket %q", path)
	}
	if err := checkSocketOwner(info, uid); err != nil {
		return fmt.Errorf("refuse to replace unix socket %q: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove unix socket %q: %w", path, err)
	}
	return nil
}

// untilSignal blocks until SIGTERM or an interrupt, then runs drain with a
// deadline, so in-flight requests, streamed answers included, can finish.
func untilSignal(timeout time.Duration, drain func(context.Context)) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	sig := <-stop
	log.Printf("%v: draining for up to %v", sig, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	drain(ctx)
	log.Printf("drained")
}

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
