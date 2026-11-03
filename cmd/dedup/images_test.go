package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFindDataImagesSpansPayloads(t *testing.T) {
	png, jpeg := image(100<<10), "data:image/jpeg;base64,"+strings.Repeat("QUJD", 10)
	body := []byte(`{"input":[{"type":"input_image","image_url":"` + png + `"},` +
		`{"image_url":{"url":"` + jpeg + `"}},` +
		`{"text":"see data:image/png;base64,AAAA in prose"},` +
		`{"image_url":"data:image/png;base64,QU\/JD"},` +
		`{"image_url":"data:image/png;base64,"}]}`)
	got := findDataImages(body)
	if len(got) != 2 {
		t.Fatalf("found %d images, want the PNG and the JPEG: %+v", len(got), got)
	}
	if got[0].mime != "png" || string(body[got[0].start:got[0].end]) != strings.TrimPrefix(png, "data:image/png;base64,") {
		t.Fatalf("PNG span wrong: %+v", got[0])
	}
	if got[1].mime != "jpeg" || string(body[got[1].start:got[1].end]) != strings.Repeat("QUJD", 10) {
		t.Fatalf("JPEG span wrong: %+v", got[1])
	}
	if findDataImages([]byte(`{"x":"data:image/png;base64,QUJD`)) != nil {
		t.Fatal("an unterminated payload must not be reported")
	}
}

// halfTranscoder pretends every image shrinks to half, counting calls.
func halfTranscoder(calls *atomic.Int64) transcodeFunc {
	return func(b64 []byte) ([]byte, error) {
		calls.Add(1)
		return b64[:len(b64)/2], nil
	}
}

func shadowStage(workers, queueBytes int, transcode transcodeFunc) *imageStage {
	return newImageStage(imageConfig{mode: imagesShadow, quality: 90, workers: workers,
		queueBytes: queueBytes, cacheBytes: 1 << 20}, transcode)
}

func webpStage(workers int, wait time.Duration, transcode transcodeFunc) *imageStage {
	return newImageStage(imageConfig{mode: imagesWebp, quality: 85, workers: workers, wait: wait,
		queueBytes: 64 << 20, cacheBytes: 64 << 20}, transcode)
}

func waitIdle(t *testing.T, s *imageStage) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		idle := len(s.pending) == 0 && len(s.jobs) == 0
		s.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the shadow pool did not drain")
}

func TestImageShadowTranscodesEachImageOnceAndAccountsResends(t *testing.T) {
	var calls atomic.Int64
	s := shadowStage(1, 1<<20, halfTranscoder(&calls))
	images := []string{image(200 << 10), image(200 << 10)}
	first := conversation(1, images[:1])
	second := conversation(2, images)
	keep := bytes.Clone(second)

	s.observe(first)
	waitIdle(t, s)
	s.observe(second) // image 1 resent (known), image 2 new
	waitIdle(t, s)
	s.observe(second) // both known now
	waitIdle(t, s)

	if !bytes.Equal(second, keep) {
		t.Fatal("observe changed the body")
	}
	if calls.Load() != 2 {
		t.Fatalf("transcoded %d times, want once per distinct image", calls.Load())
	}
	snap := s.snapshot()
	if snap["seen"] != int64(5) || snap["known"] != int64(3) || snap["distinct"] != int64(2) {
		t.Fatalf("counts: %v", snap)
	}
	if r := snap["distinct_ratio"].(float64); r < 1.99 || r > 2.01 {
		t.Fatalf("distinct_ratio %.3f, want 2", r)
	}
	if r := snap["known_ratio"].(float64); r < 1.99 || r > 2.01 {
		t.Fatalf("known_ratio %.3f, want 2", r)
	}
}

func TestImageShadowKeepsOriginalsThatBarelyShrinkOrFail(t *testing.T) {
	small := image(100 << 10)
	broken := image(100 << 10)
	s := shadowStage(1, 1<<20, func(b64 []byte) ([]byte, error) {
		if bytes.Equal(b64, []byte(strings.TrimPrefix(broken, "data:image/png;base64,"))) {
			return nil, errors.New("not a PNG")
		}
		return b64[:len(b64)*9/10], nil // only 10% smaller: not worth it
	})
	s.observe(conversation(2, []string{small, broken}))
	waitIdle(t, s)
	snap := s.snapshot()
	if snap["kept"] != int64(1) || snap["failed"] != int64(1) {
		t.Fatalf("want one kept and one failed: %v", snap)
	}
	if snap["distinct_bytes"] != snap["distinct_webp_bytes"] {
		t.Fatalf("kept and failed images must count at full size: %v", snap)
	}
}

func TestImageShadowSkipsSmallAndOtherFormatsAndDropsWhenBusy(t *testing.T) {
	block := make(chan struct{})
	s := shadowStage(1, 150<<10, func(b64 []byte) ([]byte, error) {
		<-block
		return b64[:1], nil
	})
	defer close(block)
	jpeg := strings.Replace(image(100<<10), "image/png", "image/jpeg", 1)
	s.observe(conversation(3, []string{image(10 << 10), jpeg, image(100 << 10)}))
	// Wait until the worker holds the first image and the queue is empty.
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.mu.Lock()
		queued := s.queued
		s.mu.Unlock()
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the first image")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The queue holds 150 KiB: of two more new images one is dropped.
	s.observe(conversation(2, []string{image(100 << 10), image(100 << 10)}))
	snap := s.snapshot()
	if snap["small"] != int64(1) || snap["other_format"] != int64(1) {
		t.Fatalf("small/other: %v", snap)
	}
	if snap["busy"] != int64(1) {
		t.Fatalf("busy %v, want 1 dropped while the pool is full", snap["busy"])
	}
}

func TestParseImagesModes(t *testing.T) {
	if s, err := parseImages("off", 90, 1, time.Second); s != nil || err != nil {
		t.Fatalf("off: %v %v", s, err)
	}
	for _, bad := range []string{"on", "png"} {
		if _, err := parseImages(bad, 90, 1, time.Second); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if _, err := parseImages("shadow", 0, 1, time.Second); err == nil {
		t.Fatal("quality 0 must be rejected")
	}
	if _, err := parseImages("webp", 85, 4, 0); err == nil {
		t.Fatal("webp without a wait must be rejected")
	}
	for _, workers := range []int{0, 65} {
		if _, err := parseImages("shadow", 85, workers, time.Second); err == nil {
			t.Fatalf("%d workers must be rejected", workers)
		}
	}
}

func TestImageShadowWorkersTranscodeInParallel(t *testing.T) {
	const workers = 4
	var running atomic.Int64
	started := make(chan struct{}, workers)
	release := make(chan struct{})
	s := shadowStage(workers, 1<<20, func(b64 []byte) ([]byte, error) {
		running.Add(1)
		started <- struct{}{}
		<-release
		return b64[:1], nil
	})
	images := make([]string, workers)
	for i := range images {
		images[i] = image(100 << 10)
	}
	s.observe(conversation(len(images), images))
	for range workers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d images being transcoded at once", running.Load(), workers)
		}
	}
	close(release)
	waitIdle(t, s)
	if snap := s.snapshot(); snap["distinct"] != int64(workers) || snap["workers"] != workers {
		t.Fatalf("want %d distinct by %d workers: %v", workers, workers, snap)
	}
}

func TestShadowLeavesRequestsThroughTheChainUnchanged(t *testing.T) {
	c := newReverseChain(t)
	var calls atomic.Int64
	c.enc.images = shadowStage(1, 1<<20, halfTranscoder(&calls))
	images := []string{image(200 << 10), image(200 << 10)}
	for turn := 1; turn <= 2; turn++ {
		body := conversation(turn, images)
		if code, _ := c.post(t, "a.example", body); code != 200 {
			t.Fatalf("turn %d: status %d", turn, code)
		}
		if !bytes.Equal(c.a.last(), body) {
			t.Fatalf("turn %d: the origin got a different body", turn)
		}
	}
	waitIdle(t, c.enc.images)
	if calls.Load() != 2 {
		t.Fatalf("transcoded %d images, want 2", calls.Load())
	}
	if _, ok := c.enc.snapshot()["images"]; !ok {
		t.Fatal("encoder stats lack the images section")
	}
}

// BenchmarkImageShadowObserve is the request-path cost for a body like the
// largest seen in production: 24 screenshots of about 550 KiB, all known.
func BenchmarkImageShadowObserve(b *testing.B) {
	var calls atomic.Int64
	s := shadowStage(1, 64<<20, halfTranscoder(&calls))
	images := make([]string, 24)
	for i := range images {
		images[i] = image(410 << 10)
	}
	body := conversation(len(images), images)
	s.observe(body)
	for len(s.jobs) > 0 || calls.Load() < int64(len(images)) {
		time.Sleep(time.Millisecond)
	}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		s.observe(body)
	}
}

// asWebp is what a webp stage with halfTranscoder makes of a PNG data URL.
func asWebp(dataURL string) string {
	b64 := strings.TrimPrefix(dataURL, "data:image/png;base64,")
	return "data:image/webp;base64," + b64[:len(b64)/2]
}

// blockingTranscoder halves images once release is closed.
func blockingTranscoder(calls *atomic.Int64, release <-chan struct{}) transcodeFunc {
	return func(b64 []byte) ([]byte, error) {
		calls.Add(1)
		<-release
		return b64[:len(b64)/2], nil
	}
}

func TestWebpSwapsReadyImagesAndLeavesTheRest(t *testing.T) {
	var calls atomic.Int64
	s := webpStage(2, time.Second, halfTranscoder(&calls))
	a, b, small := image(100<<10), image(120<<10), image(10<<10)
	jpeg := strings.Replace(image(100<<10), "image/png", "image/jpeg", 1)
	body := conversation(5, []string{a, small, jpeg, b, a})
	got := s.process(context.Background(), body)
	want := string(conversation(5, []string{asWebp(a), small, jpeg, asWebp(b), asWebp(a)}))
	if string(got) != want {
		t.Fatal("the body does not carry the WebPs where the PNGs were")
	}
	if !bytes.Equal(body, conversation(5, []string{a, small, jpeg, b, a})) {
		t.Fatal("rewrite changed the body it was given")
	}
	// The next turn resends them: no new transcode, the same bytes.
	again := s.process(context.Background(), body)
	if string(again) != want || calls.Load() != 2 {
		t.Fatalf("resend: same body %v, %d transcodes, want 2", string(again) == want, calls.Load())
	}
	snap := s.snapshot()
	if snap["replaced"] != int64(6) || snap["late"] != int64(0) || snap["shed"] != int64(0) {
		t.Fatalf("stats: %v", snap)
	}
}

// An image the client already sent as WebP (or JPEG) is lossy already: it is
// never transcoded a second time, only the PNGs are.
func TestWebpLeavesClientWebpAlone(t *testing.T) {
	var calls atomic.Int64
	s := webpStage(2, time.Second, halfTranscoder(&calls))
	png := image(100 << 10)
	theirs := strings.Replace(image(120<<10), "image/png", "image/webp", 1)
	body := conversation(2, []string{theirs, png, theirs})
	got := s.process(context.Background(), body)
	if want := conversation(2, []string{theirs, asWebp(png), theirs}); string(got) != string(want) {
		t.Fatal("the client's WebP changed, or the PNG was not swapped")
	}
	if snap := s.snapshot(); calls.Load() != 1 || snap["other_format"] == int64(0) || snap["replaced"] != int64(1) {
		t.Fatalf("%d transcodes, want 1 (the PNG only): %v", calls.Load(), snap)
	}
}

func TestWebpLateImageGoesAsPNGAndStaysPNG(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	// 100 KiB at the assumed 1 MiB/s fits the wait; the transcode never ends.
	s := webpStage(1, 200*time.Millisecond, blockingTranscoder(&calls, release))
	body := conversation(1, []string{image(100 << 10)})
	start := time.Now()
	if got := s.process(context.Background(), body); !bytes.Equal(got, body) {
		t.Fatal("an image not ready in time must go out unchanged")
	}
	if waited := time.Since(start); waited < 200*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %v, want about the 200ms deadline", waited)
	}
	close(release)
	waitIdle(t, s)
	// The WebP is ready now, but this turn must match the last one.
	if got := s.process(context.Background(), body); !bytes.Equal(got, body) {
		t.Fatal("an image sent as PNG must stay PNG")
	}
	if snap := s.snapshot(); snap["late"] != int64(1) || snap["replaced"] != int64(0) {
		t.Fatalf("stats: %v", snap)
	}
}

func TestWebpShedsWhatCannotBeReadyInTime(t *testing.T) {
	var calls atomic.Int64
	// At the assumed 1 MiB/s, 200 KiB takes about 0.2s: more than the wait.
	s := webpStage(1, 100*time.Millisecond, halfTranscoder(&calls))
	body := conversation(1, []string{image(200 << 10)})
	start := time.Now()
	if got := s.process(context.Background(), body); !bytes.Equal(got, body) {
		t.Fatal("a shed image must go out unchanged")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a shed image must not be waited for")
	}
	if got := s.process(context.Background(), body); !bytes.Equal(got, body) || calls.Load() != 0 {
		t.Fatalf("a shed image must stay PNG and never be transcoded (%d calls)", calls.Load())
	}
	if snap := s.snapshot(); snap["shed"] != int64(1) {
		t.Fatalf("stats: %v", snap)
	}
}

func TestWebpCallsOffLateJobsStillQueued(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	s := webpStage(1, 300*time.Millisecond, blockingTranscoder(&calls, release))
	body := conversation(2, []string{image(100 << 10), image(100 << 10)})
	if got := s.process(context.Background(), body); !bytes.Equal(got, body) {
		t.Fatal("late images must go out unchanged")
	}
	s.mu.Lock()
	queued, pending := s.queued, len(s.pending)
	s.mu.Unlock()
	if queued != 0 || pending != 0 {
		t.Fatalf("after the deadline: %d bytes queued, %d pending; want none", queued, pending)
	}
	close(release)
	// The worker finishes the first image and skips the called-off second.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.jobs) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("%d transcodes, want 1: a called-off job must be dropped", calls.Load())
	}
	if snap := s.snapshot(); snap["late"] != int64(2) {
		t.Fatalf("stats: %v", snap)
	}
}

func TestWebpRequestsShareOneTranscode(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	s := webpStage(2, 5*time.Second, blockingTranscoder(&calls, release))
	img := image(100 << 10)
	body := conversation(1, []string{img})
	outs := make(chan []byte, 2)
	for range 2 {
		go func() { outs <- s.process(context.Background(), body) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	want := string(conversation(1, []string{asWebp(img)}))
	for range 2 {
		if got := <-outs; string(got) != want {
			t.Fatal("a request waiting on another's transcode must get its WebP")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d transcodes, want 1", calls.Load())
	}
}

func TestWebpClientThatLeavesDecidesNothing(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	s := webpStage(1, 5*time.Second, blockingTranscoder(&calls, release))
	img := image(100 << 10)
	body := conversation(1, []string{img})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.process(ctx, body); !bytes.Equal(got, body) {
		t.Fatal("a gone client's body must not be rewritten")
	}
	close(release)
	waitIdle(t, s)
	if got := s.process(context.Background(), body); string(got) != string(conversation(1, []string{asWebp(img)})) {
		t.Fatal("the image must still become WebP for the next request")
	}
}

func TestWebpThroughTheChain(t *testing.T) {
	c := newReverseChain(t)
	var calls atomic.Int64
	c.enc.images = webpStage(2, 2*time.Second, halfTranscoder(&calls))
	images := []string{image(200 << 10), image(200 << 10)}
	for turn := 1; turn <= 2; turn++ {
		if code, _ := c.post(t, "a.example", conversation(turn, images)); code != 200 {
			t.Fatalf("turn %d: status %d", turn, code)
		}
		want := make([]string, turn)
		for i := range want {
			want[i] = asWebp(images[i])
		}
		if string(c.a.last()) != string(conversation(turn, want)) {
			t.Fatalf("turn %d: the origin did not get the WebPs", turn)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("transcoded %d images, want 2", calls.Load())
	}
}

func TestImageLRUKeepsWithinItsBytes(t *testing.T) {
	l := newImageLRU(3 * (imageEntryBytes + 100))
	for i := range 5 {
		l.put([32]byte{byte(i)}, imageResult{size: 100, webp: make([]byte, 100)})
	}
	if l.len() != 3 || l.used != 3*(imageEntryBytes+100) {
		t.Fatalf("%d entries, %d bytes", l.len(), l.used)
	}
	if _, ok := l.get([32]byte{1}); ok {
		t.Fatal("the oldest entries must go first")
	}
	l.put([32]byte{4}, imageResult{size: 100})
	if l.used != 2*(imageEntryBytes+100)+imageEntryBytes {
		t.Fatalf("replacing an entry must account its new size: %d", l.used)
	}
}
