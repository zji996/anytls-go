package pipe

import (
	"testing"
	"time"
)

func TestPipeDeadlineCanBeClearedAndReused(t *testing.T) {
	deadline := MakePipeDeadline()
	deadline.Set(time.Now().Add(20 * time.Millisecond))
	deadline.Set(time.Time{})

	select {
	case <-deadline.Wait():
		t.Fatal("cleared deadline fired")
	case <-time.After(40 * time.Millisecond):
	}

	deadline.Set(time.Now().Add(20 * time.Millisecond))
	select {
	case <-deadline.Wait():
	case <-time.After(time.Second):
		t.Fatal("reused deadline did not fire")
	}
}
