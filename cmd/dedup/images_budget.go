package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sync"
)

// Native q85/m2, 4000x3000: RSS/Go committed deltas in B/pixel were
// RGBA 11.46/5.89, gray 10.92/5.32, paletted 11.61/6.06, so libwebp's C heap
// takes about 5.5. Allow 16 total, 10 Go + 6 C: Go's share also covers the
// decoded PNG bytes of less compressible images, and the soft limit makes the
// GC absorb a Go overrun, while nothing bounds C but this budget.
// Reproduce with TestImageNativeMemory (IMAGE_MEMORY_MEASURE=1).
const imageDecodeBytesPerPixel = 16
const imageDecodeGoBytesPerPixel = 10

// 384 MiB admits one image of up to 25 MP (a 6K screenshot is 20 MP), and
// lets a 12 MP one (183 MiB) share it with a second or with a few screenshots.
const imageDecodeBudgetMiB = 384

// pngHeaderSize reads only the IHDR, not the base64 image data after it.
func pngHeaderSize(b64 []byte) (width, height int64, err error) {
	var raw [33]byte
	var prefix [44]byte // 33 decoded bytes, including the IHDR CRC
	n := 0
	for _, c := range b64 {
		if c == '\r' || c == '\n' {
			continue
		}
		prefix[n] = c
		n++
		if n == len(prefix) {
			break
		}
	}
	if n != len(prefix) {
		return 0, 0, fmt.Errorf("truncated PNG header")
	}
	n, err = base64.StdEncoding.Decode(raw[:], prefix[:])
	if err != nil || n != len(raw) {
		return 0, 0, fmt.Errorf("invalid base64 PNG header")
	}
	if !bytes.Equal(raw[:8], []byte("\x89PNG\r\n\x1a\n")) ||
		binary.BigEndian.Uint32(raw[8:12]) != 13 || string(raw[12:16]) != "IHDR" ||
		crc32.ChecksumIEEE(raw[12:29]) != binary.BigEndian.Uint32(raw[29:]) {
		return 0, 0, fmt.Errorf("invalid PNG IHDR")
	}
	width = int64(binary.BigEndian.Uint32(raw[16:20]))
	height = int64(binary.BigEndian.Uint32(raw[20:24]))
	if width == 0 || height == 0 || width > 1<<31-1 || height > 1<<31-1 {
		return 0, 0, fmt.Errorf("invalid PNG dimensions")
	}
	return width, height, nil
}

func imageDecodeEstimate(b64 []byte) int64 {
	w, h, err := pngHeaderSize(b64)
	if err != nil {
		// The codec rejects malformed headers before allocating image data.
		return 0
	}
	// Compare before multiplying: even a malicious IHDR cannot overflow.
	if w*h > (imageDecodeBudgetMiB<<20)/imageDecodeBytesPerPixel {
		return imageDecodeBudgetMiB<<20 + 1
	}
	return w * h * imageDecodeBytesPerPixel
}

// imageDecodeBudget is non-FIFO: a small job can use spare bytes while a
// larger one sleeps. Releases wake waiters; cancelled jobs stop waiting.
type imageDecodeBudget struct {
	mu          sync.Mutex
	limit, used int64
	changed     chan struct{}
}

func (b *imageDecodeBudget) acquire(n int64, cancelled <-chan struct{}) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit {
		return false
	}
	for {
		select {
		case <-cancelled:
			return false
		default:
		}
		if n <= b.limit-b.used {
			b.used += n
			return true
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-cancelled:
		}
		b.mu.Lock()
	}
}

func (b *imageDecodeBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
	b.mu.Unlock()
}
