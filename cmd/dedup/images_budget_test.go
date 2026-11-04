package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// headerPayload needs no IDAT: mock codecs only inspect the dimensions.
func headerPayload(w, h uint32) []byte {
	var raw [33]byte
	copy(raw[:], "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(raw[8:12], 13)
	copy(raw[12:], "IHDR")
	binary.BigEndian.PutUint32(raw[16:20], w)
	binary.BigEndian.PutUint32(raw[20:24], h)
	raw[24], raw[25] = 8, 2
	binary.BigEndian.PutUint32(raw[29:], crc32.ChecksumIEEE(raw[12:29]))
	return base64.StdEncoding.AppendEncode(nil, raw[:])
}

func TestPNGHeaderSize(t *testing.T) {
	valid := headerPayload(4000, 3000)
	// Invalid base64 after IHDR must not be read, even with a large payload.
	withTail := append(bytes.Clone(valid), strings.Repeat("!", 1<<20)...)
	wrapped := []byte("\r\n" + string(valid[:20]) + "\r\n" + string(valid[20:]))
	for _, in := range [][]byte{valid, withTail, wrapped} {
		w, h, err := pngHeaderSize(in)
		if err != nil || w != 4000 || h != 3000 {
			t.Fatalf("dimensions %dx%d: %v", w, h, err)
		}
	}
	badCRC := bytes.Clone(valid)
	badCRC[43] = 'A'
	for name, in := range map[string][]byte{
		"empty": nil, "truncated": valid[:43],
		"non-PNG":        bytes.Repeat([]byte("QUJD"), 11),
		"invalid base64": bytes.Repeat([]byte("!"), 44),
		"bad CRC":        badCRC, "zero width": headerPayload(0, 10),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := pngHeaderSize(in); err == nil {
				t.Fatal("invalid header accepted")
			}
		})
	}
	if n := imageDecodeEstimate(valid); n != 12_000_000*imageDecodeBytesPerPixel || n > imageDecodeBudgetMiB<<20 {
		t.Fatalf("12 MP must fit: %d bytes", n)
	}
	if n := imageDecodeEstimate(headerPayload(6016, 3384)); n > imageDecodeBudgetMiB<<20 {
		t.Fatalf("a 6K screenshot must fit: %d bytes", n)
	}
	if n := imageDecodeEstimate(headerPayload(1<<31-1, 1<<31-1)); n <= imageDecodeBudgetMiB<<20 {
		t.Fatalf("huge dimensions must not overflow: %d", n)
	}
}

func waitDecodeWaiter(t *testing.T, b *imageDecodeBudget) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		waiting := b.changed != nil
		b.mu.Unlock()
		if waiting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job never waited for decode budget")
}

func receiveBudget(t *testing.T, ch <-chan bool) bool {
	t.Helper()
	select {
	case ok := <-ch:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("budget acquisition did not finish")
		return false
	}
}

func TestImageDecodeBudget(t *testing.T) {
	b := imageDecodeBudget{limit: 100}
	if !b.acquire(60, nil) || b.used != 60 || b.acquire(101, nil) {
		t.Fatal("incorrect acquire accounting or oversized acquisition accepted")
	}
	cancel := make(chan struct{})
	defer close(cancel)
	big := make(chan bool, 1)
	go func() { big <- b.acquire(80, cancel) }()
	waitDecodeWaiter(t, &b)
	small := make(chan bool, 1)
	go func() { small <- b.acquire(20, cancel) }()
	if !receiveBudget(t, small) {
		t.Fatal("small job did not bypass big waiter")
	}
	b.release(20)
	select {
	case <-big:
		t.Fatal("big job acquired before enough bytes were freed")
	default:
	}
	b.release(60)
	if !receiveBudget(t, big) {
		t.Fatal("big job failed to acquire freed budget")
	}
	b.release(80)
	if b.used != 0 {
		t.Fatalf("leaked %d bytes", b.used)
	}
}

func TestImageDecodeBudgetCancellation(t *testing.T) {
	b := imageDecodeBudget{limit: 100}
	b.acquire(100, nil)
	cancel := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- b.acquire(1, cancel) }()
	waitDecodeWaiter(t, &b)
	close(cancel)
	if receiveBudget(t, done) {
		t.Fatal("cancelled waiter acquired budget")
	}
	b.release(100)
	if b.used != 0 {
		t.Fatalf("cancelled job leaked %d bytes", b.used)
	}
}

func dimensionURL(w, h uint32) string {
	return "data:image/png;base64," + string(headerPayload(w, h)) + strings.Repeat("A", imageMinB64)
}

func TestImageTooLargeStaysPNG(t *testing.T) {
	for _, mode := range []string{imagesShadow, imagesWebp} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			s := newImageStage(imageConfig{mode: mode, quality: 85, workers: 2, wait: time.Second,
				queueBytes: 1 << 20, cacheBytes: 1 << 20}, halfTranscoder(&calls))
			body := conversation(1, []string{dimensionURL(6000, 5000)})
			for range 2 {
				if got := s.process(context.Background(), body); !bytes.Equal(got, body) {
					t.Fatal("over-budget image changed")
				}
			}
			waitIdle(t, s)
			snap := s.snapshot()
			if calls.Load() != 0 || snap["too_large"] != int64(1) || snap["decode_budget_mib"] != imageDecodeBudgetMiB || snap["distinct"] != int64(0) {
				t.Fatalf("over-budget image must settle once without transcoding: calls=%d stats=%v", calls.Load(), snap)
			}
		})
	}
}

func TestImageWorkersShareDecodeBudget(t *testing.T) {
	started := make(chan int64, 3)
	release := make(chan struct{})
	defer close(release)
	s := shadowStage(3, 1<<20, func(b64 []byte) ([]byte, error) {
		_, h, _ := pngHeaderSize(b64)
		started <- h
		if h == 60 {
			<-release
		}
		return b64[:1], nil
	})
	s.decodeBudget.limit = 100 * imageDecodeBytesPerPixel
	next := func(want int64) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("started %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("job %d never started", want)
		}
	}
	s.observe(conversation(1, []string{dimensionURL(1, 60)}))
	next(60)
	s.observe(conversation(1, []string{dimensionURL(1, 80)}))
	waitDecodeWaiter(t, &s.decodeBudget)
	s.observe(conversation(1, []string{dimensionURL(1, 20)}))
	next(20)
	// Free the first job without double-closing the deferred channel.
	release <- struct{}{}
	next(80)
	waitIdle(t, s)
	s.decodeBudget.mu.Lock()
	used := s.decodeBudget.used
	s.decodeBudget.mu.Unlock()
	if used != 0 {
		t.Fatalf("workers leaked %d decode bytes", used)
	}
}
