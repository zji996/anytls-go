package main

import (
	"bytes"
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

func waitIdle(t *testing.T, s *imageShadow) {
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
	s := newImageShadow(90, 1, 16, 64, halfTranscoder(&calls))
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
	s := newImageShadow(90, 1, 16, 64, func(b64 []byte) ([]byte, error) {
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
	s := newImageShadow(90, 1, 1, 64, func(b64 []byte) ([]byte, error) {
		<-block
		return b64[:1], nil
	})
	defer close(block)
	jpeg := strings.Replace(image(100<<10), "image/png", "image/jpeg", 1)
	s.observe(conversation(3, []string{image(10 << 10), jpeg, image(100 << 10)}))
	// Wait until the worker holds the first image and the queue is empty.
	for deadline := time.Now().Add(5 * time.Second); len(s.jobs) > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the first image")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// One slot left in the queue: of two more new images one is dropped.
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
	if s, err := parseImages("off", 90, 1); s != nil || err != nil {
		t.Fatalf("off: %v %v", s, err)
	}
	for _, bad := range []string{"on", "webp"} {
		if _, err := parseImages(bad, 90, 1); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if _, err := parseImages("shadow", 0, 1); err == nil {
		t.Fatal("quality 0 must be rejected")
	}
}

func TestShadowLeavesRequestsThroughTheChainUnchanged(t *testing.T) {
	c := newReverseChain(t)
	var calls atomic.Int64
	c.enc.images = newImageShadow(90, 1, 16, 64, halfTranscoder(&calls))
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
	s := newImageShadow(90, 1, 64, 1024, halfTranscoder(&calls))
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
