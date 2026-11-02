package main

import "syscall"

// imageNice is how far image workers step back. Linux keeps a nice value per
// thread, and the caller has locked its goroutine to the thread.
const imageNice = 10

func lowerThreadPriority() {
	// Raising one's own nice value needs no privilege; if it fails anyway
	// the worker merely runs at normal priority.
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), imageNice)
}
