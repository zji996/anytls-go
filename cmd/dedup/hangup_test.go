package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestFrontReverseReloadsOnRealHangup drives the signal path itself: SIGUSR1
// would not do, so the test sends SIGHUP to its own process, which the front
// has registered.
func TestFrontReverseReloadsOnRealHangup(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	r, err := newReverseFront(c.front, c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	onHangup(func() error { return r.reload(c.cfg) })
	writeFile(t, c.originsFile, "example.com\nhangup.test\n")
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Skipf("cannot signal this process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !r.origins.match("hangup.test") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !r.origins.match("hangup.test") {
		t.Fatal("SIGHUP did not reload the reverse origins")
	}
}

// TestBackDomainsFileReloadsOnRealHangup is the same for the decoder's
// -domains-file.
func TestBackDomainsFileReloadsOnRealHangup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domains")
	writeFile(t, path, "example.com\n")
	dec := newDecoder(route{}, 1<<20, 1<<20)
	if err := loadBackDomains(dec, "", path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "example.com\nhangup.test\n")
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Skipf("cannot signal this process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !dec.allowed("hangup.test") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !dec.allowed("hangup.test") {
		t.Fatal("SIGHUP did not reload the decoder's domains file")
	}
	// A broken file keeps the list in use, through the signal path too.
	writeFile(t, path, "*.bad.test\n")
	syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)
	if !dec.allowed("hangup.test") {
		t.Fatal("a broken file dropped the list in use")
	}
}
