package main

import (
	"runtime"
	"syscall"
	"testing"
)

func TestLowerThreadPriorityNicesOnlyTheCallingThread(t *testing.T) {
	// The raw syscall returns 20 - nice.
	before, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int)
	go func() {
		// Left locked: the thread exits with the goroutine.
		runtime.LockOSThread()
		lowerThreadPriority()
		p, _ := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
		got <- p
	}()
	if p := <-got; 20-p != min(20-before+imageNice, 19) {
		t.Fatalf("worker nice %d, want %d", 20-p, 20-before+imageNice)
	}
	if after, _ := syscall.Getpriority(syscall.PRIO_PROCESS, 0); after != before {
		t.Fatalf("the process nice moved from %d to %d", 20-before, 20-after)
	}
}
