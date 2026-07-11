package util

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartRoutineStopsBeforeNextRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	StartRoutine(ctx, 20*time.Millisecond, func() {
		calls.Add(1)
	})
	cancel()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("routine ran %d times after cancellation", got)
	}
}
