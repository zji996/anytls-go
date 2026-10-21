//go:build unix

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func socketTestDir(t *testing.T) string {
	t.Helper()
	// Keep below sockaddr_un's path limit, even with long subtest names.
	dir, err := os.MkdirTemp("", "dedup-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(dir, 0700)
		os.RemoveAll(dir)
	})
	return dir
}

func TestBindUnixPreservesNonSocketPaths(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink-file", "symlink-socket", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := socketTestDir(t)
			path := filepath.Join(dir, "decoder")
			target := filepath.Join(dir, "target")
			const contents = "must survive failed bind"
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(path, []byte(contents), 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
				if err == nil {
					err = os.WriteFile(filepath.Join(path, "child"), []byte(contents), 0600)
				}
			case "symlink-file":
				err = os.WriteFile(target, []byte(contents), 0600)
			case "symlink-socket":
				var ln net.Listener
				ln, err = bind("unix:" + target)
				if err == nil {
					t.Cleanup(func() { ln.Close() })
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "symlink-file" || kind == "symlink-socket" || kind == "dangling-symlink" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := bind("unix:" + path)
			if err == nil {
				ln.Close()
				t.Fatal("bind replaced a non-socket path")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("path changed after failed bind: info %v, error %v", after, err)
			}
			switch kind {
			case "file", "directory", "symlink-file":
				check := path
				if kind == "directory" {
					check = filepath.Join(path, "child")
				}
				got, err := os.ReadFile(check)
				if err != nil || string(got) != contents {
					t.Fatalf("contents changed: %q, %v", got, err)
				}
			}
			if before.Mode()&os.ModeSymlink != 0 {
				if got, err := os.Readlink(path); err != nil || got != target {
					t.Fatalf("symlink changed: %q, %v", got, err)
				}
			}
			if kind == "symlink-socket" {
				conn, err := net.DialTimeout("unix", target, time.Second)
				if err != nil {
					t.Fatalf("symlink's target socket was damaged: %v", err)
				}
				conn.Close()
			}
		})
	}
}

func TestBindUnixReplacesSameUIDSocket(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "decoder")
	old, err := bind("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := bind("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	replacement, err := os.Lstat(path)
	if err != nil || os.SameFile(before, replacement) {
		t.Fatalf("socket was not replaced: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(replacement, after) {
		t.Fatalf("old listener removed successor socket: %v", err)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	next.(*net.UnixListener).SetDeadline(time.Now().Add(time.Second))
	accepted, err := next.Accept()
	if err != nil {
		t.Fatalf("replacement socket did not accept: %v", err)
	}
	accepted.Close()
}

func TestUnixSocketRejectsDifferentUID(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "decoder")
	ln, err := bind("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the same check/unlink function with a different effective UID,
	// without requiring root to chown the test socket.
	if err := removeExistingSocket(path, os.Geteuid()+1); err == nil {
		t.Fatal("accepted socket owned by another UID")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("different-UID socket was changed: %v", err)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestBindUnixReportsInspectionAndRemovalErrors(t *testing.T) {
	dir := socketTestDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := bind("unix:" + filepath.Join(file, "decoder")); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("inspection error = %v, want ENOTDIR", err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "keep" {
		t.Fatalf("inspection failure damaged parent file: %q, %v", got, err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions for removal error test")
	}
	path := filepath.Join(dir, "decoder")
	ln, err := bind("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := bind("unix:" + path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("removal error = %v, want permission error", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("failed removal changed socket: %v", err)
	}
}
