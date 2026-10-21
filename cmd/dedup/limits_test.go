package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"
)

func heapAfter(fill func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fill()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return after.HeapAlloc - before.HeapAlloc
}

// The flag help promises a decoder cache costs about 1.25x its -cache-mib and
// an encoder index up to an eighth of its -index-mib.
func TestCacheMemoryMatchesFlagHelp(t *testing.T) {
	const capacity = 64 << 20
	body := append([]byte(image(capacity)), prose(capacity/2)...)
	pieces := chunks(body)
	small := capacity / minChunk
	for _, c := range []struct {
		name   string
		limit  float64
		values bool
		size   func(i int) int
	}{
		{"cache", 1.25, true, func(i int) int { return len(pieces[i%len(pieces)]) }},
		{"cache-min-chunks", 1.25, true, func(int) int { return minChunk }},
		{"index-min-chunks", 0.125, false, func(int) int { return minChunk }},
	} {
		var keep *lru
		used := heapAfter(func() {
			keep = newLRU(capacity)
			for i := 0; i < 2*small; i++ {
				n := c.size(i)
				var v []byte
				if c.values {
					v = make([]byte, n)
				}
				var d digest
				d[0], d[1], d[2], d[3] = byte(i), byte(i>>8), byte(i>>16), byte(i>>24)
				keep.put(d, n, v)
			}
		})
		ratio := float64(used) / capacity
		t.Logf("%s: %.3fx of capacity", c.name, ratio)
		if ratio > c.limit {
			t.Errorf("%s: heap %.3fx of capacity, help says %.2fx", c.name, ratio, c.limit)
		}
		runtime.KeepAlive(keep)
	}
}

func TestBusyDecoderGetsOriginals(t *testing.T) {
	c := newChain(t, 64<<20)
	c.dec.inflight.limit = 1
	images := []string{image(300 << 10), image(300 << 10)}
	c.post(t, conversation(1, images))
	c.post(t, conversation(2, images))
	if busy := c.dec.snapshot()["busy"].(int64); busy != 2 {
		t.Fatalf("decoder busy %d times, want 2", busy)
	}
	if retries := c.enc.snapshot()["retries"].(int64); retries != 2 {
		t.Fatalf("encoder resent %d times, want 2", retries)
	}
	// The decoder never read those literals, so none may be referenced later.
	if entries, _ := c.enc.known.stats(); entries != 0 {
		t.Fatalf("index kept %d chunks the decoder dropped", entries)
	}
	if c.enc.inflight.used.Load() != 0 || c.dec.inflight.used.Load() != 0 {
		t.Fatalf("budgets leaked: encoder %d, decoder %d", c.enc.inflight.used.Load(), c.dec.inflight.used.Load())
	}
}

func TestBudgetsReturnToZero(t *testing.T) {
	c := newChain(t, 64<<20)
	c.enc.inflight.limit = 32 << 20
	c.dec.inflight.limit = 32 << 20
	images := []string{image(300 << 10), image(300 << 10)}
	c.post(t, conversation(1, images))
	c.post(t, conversation(2, images))
	if c.enc.snapshot()["framed"].(int64) != 2 || c.dec.snapshot()["busy"].(int64) != 0 {
		t.Fatalf("want both framed and none busy: %v %v", c.enc.snapshot(), c.dec.snapshot())
	}
	if c.enc.inflight.used.Load() != 0 || c.dec.inflight.used.Load() != 0 {
		t.Fatalf("budgets leaked: encoder %d, decoder %d", c.enc.inflight.used.Load(), c.dec.inflight.used.Load())
	}
}

func TestStreamedAnswerHoldsNoBudget(t *testing.T) {
	c := newChain(t, 64<<20)
	c.enc.inflight.limit = 32 << 20
	c.dec.inflight.limit = 32 << 20
	body := conversation(2, []string{image(300 << 10), image(300 << 10)})
	resp, err := http.Post(c.encSrv.URL+"/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); line != "data: first\n" {
		t.Fatalf("first event %q", line)
	}
	// The origin is still streaming; neither end needs the body any more.
	if e, d := c.enc.inflight.used.Load(), c.dec.inflight.used.Load(); e != 0 || d != 0 {
		t.Fatalf("mid-stream budgets: encoder %d, decoder %d", e, d)
	}
	if c.enc.snapshot()["framed"].(int64) != 1 || !bytes.Equal(c.origin.last(), body) {
		t.Fatal("request was not framed and rebuilt intact")
	}
	io.Copy(io.Discard, r)
}

func TestFrontRefusesConnectionsOverLimit(t *testing.T) {
	f := &front{maxConns: 1}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go f.serve(ln)
	held, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if !closedSoon(t, ln.Addr().String()) {
		t.Fatal("a second connection was accepted past -max-conns 1")
	}
	held.Close()
	deadline := time.Now().Add(3 * time.Second)
	for closedSoon(t, ln.Addr().String()) {
		if time.Now().After(deadline) {
			t.Fatal("the slot was not freed after the first connection closed")
		}
	}
	if f.refused.Load() == 0 {
		t.Fatal("refusals not counted")
	}
}

// closedSoon reports whether a new connection is closed by the server rather
// than left waiting for a proxy handshake.
func closedSoon(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = c.Read(make([]byte, 1))
	return !errors.Is(err, os.ErrDeadlineExceeded)
}

func TestPreferOOMKillOnlyRaises(t *testing.T) {
	const path = "/proc/self/oom_score_adj"
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.WriteFile(path, cur, 0) })
	preferOOMKill(300)
	if got, _ := os.ReadFile(path); string(got) != "300\n" {
		t.Fatalf("oom_score_adj %q, want 300", got)
	}
	preferOOMKill(200)
	if got, _ := os.ReadFile(path); string(got) != "300\n" {
		t.Fatalf("lowered to %q", got)
	}
}

func TestBudgetWaitsForRelease(t *testing.T) {
	b := &budget{limit: 10}
	first := b.take(8)
	go func() {
		time.Sleep(50 * time.Millisecond)
		first.shrink(2)
	}()
	start := time.Now()
	second := b.takeWithin(context.Background(), 8, time.Second)
	if second == nil {
		t.Fatal("no share after the first shrank")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("got a share after %v, before any release", waited)
	}
	if b.takeWithin(context.Background(), 8, 30*time.Millisecond) != nil {
		t.Fatal("got a share over the limit")
	}
	if b.takeWithin(context.Background(), 11, time.Hour) != nil {
		t.Fatal("waited for a share larger than the limit")
	}
	first.free()
	second.free()
	if b.used.Load() != 0 {
		t.Fatalf("budget leaked %d", b.used.Load())
	}
}
