package main

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Image shadow: agents attach screenshots as base64 PNG data URLs and resend
// every one of them each turn. Re-encoding them as WebP before the tunnel
// would shrink what crosses it; the shadow mode only measures by how much. It
// leaves every request untouched: the request path finds the images and looks
// up sizes already known, and a small background pool transcodes new ones.
//
// The codec lives behind the dedupimages build tag (see images_webp.go), so
// builds without it, such as the router front, link no image code at all.

const (
	imagesOff    = "off"
	imagesShadow = "shadow"
)

// imageMaxDecodeMiB is the memory budget per transcode worker: one decoded
// image, its WebP and buffers.
const imageMaxDecodeMiB = 200

// imageMinB64 skips icons and thumbnails: transcoding them saves too little.
const imageMinB64 = 64 << 10

// imageKeepRatio is how small the WebP must be, relative to the PNG, to be
// worth swapping in: a lossy copy of an image that barely shrinks is no gain.
const imageKeepRatio = 0.8

// transcodeFunc turns the base64 payload of a PNG data URL into the base64 of
// its WebP, or fails. It must be deterministic: the same input always gives
// the same output, on every host, so that a resent image dedups and an
// upstream prompt cache keeps matching.
type transcodeFunc func(b64 []byte) ([]byte, error)

// dataImage is one data URL inside a body: mime is e.g. "png", and payload
// spans the base64 text.
type dataImage struct {
	mime       string
	start, end int
}

var dataImagePrefix = []byte(`"data:image/`)

// findDataImages lists the base64 image data URLs in a JSON body. A payload
// holding a JSON escape (such as \/) is left out: rewriting it would need
// unescaping. Any other stray byte fails base64 decoding later, as "failed".
func findDataImages(body []byte) []dataImage {
	var out []dataImage
	for i := 0; ; {
		at := bytes.Index(body[i:], dataImagePrefix)
		if at < 0 {
			return out
		}
		at += i + len(dataImagePrefix)
		i = at
		semi := bytes.IndexByte(body[at:min(len(body), at+32)], ';')
		if semi <= 0 || !bytes.HasPrefix(body[at+semi:], []byte(";base64,")) {
			continue
		}
		mime := string(body[at : at+semi])
		start := at + semi + len(";base64,")
		end := bytes.IndexByte(body[start:], '"')
		if end < 0 {
			return out
		}
		end += start
		i = end
		if end > start && bytes.IndexByte(body[start:end], '\\') < 0 {
			out = append(out, dataImage{mime: mime, start: start, end: end})
		}
	}
}

type imageCounters struct {
	// Every image occurrence the request path saw, and of those, the ones
	// whose WebP size was already known (the rest were queued or dropped).
	seen, seenBytes, known, knownBytes, knownWebp atomic.Int64
	// small images and formats other than PNG, left alone.
	small, otherFormat, otherBytes atomic.Int64
	// Each distinct image the pool transcoded.
	distinct, distinctBytes, distinctWebp atomic.Int64
	kept, failed, busy                    atomic.Int64
	nanos                                 atomic.Int64
	// Bytes of the bodies that were looked at, images or not.
	bodies, bodyBytes atomic.Int64
}

type imageJob struct {
	key [32]byte
	b64 []byte
}

// imageShadow measures what WebP would save without changing a request.
type imageShadow struct {
	quality   int
	transcode transcodeFunc
	jobs      chan imageJob
	stats     imageCounters

	mu      sync.Mutex
	sizes   *sizeLRU          // sha256 of the payload -> base64 bytes it would send
	pending map[[32]byte]bool // queued or being transcoded
}

func newImageShadow(quality, workers, queue, entries int, transcode transcodeFunc) *imageShadow {
	s := &imageShadow{
		quality:   quality,
		transcode: transcode,
		jobs:      make(chan imageJob, queue),
		sizes:     newSizeLRU(entries),
		pending:   make(map[[32]byte]bool),
	}
	for range workers {
		go s.work()
	}
	return s
}

// observe accounts for the images in body. It neither changes nor keeps body.
func (s *imageShadow) observe(body []byte) {
	s.stats.bodies.Add(1)
	s.stats.bodyBytes.Add(int64(len(body)))
	for _, img := range findDataImages(body) {
		b64 := body[img.start:img.end]
		if img.mime != "png" {
			s.stats.otherFormat.Add(1)
			s.stats.otherBytes.Add(int64(len(b64)))
			continue
		}
		if len(b64) < imageMinB64 {
			s.stats.small.Add(1)
			continue
		}
		s.stats.seen.Add(1)
		s.stats.seenBytes.Add(int64(len(b64)))
		key := sha256.Sum256(b64)
		s.mu.Lock()
		size, ok := s.sizes.get(key)
		queued := s.pending[key]
		if !ok && !queued {
			select {
			case s.jobs <- imageJob{key: key, b64: bytes.Clone(b64)}:
				s.pending[key] = true
			default:
				s.stats.busy.Add(1)
			}
		}
		s.mu.Unlock()
		if ok {
			s.stats.known.Add(1)
			s.stats.knownBytes.Add(int64(len(b64)))
			s.stats.knownWebp.Add(int64(size))
		}
	}
}

func (s *imageShadow) work() {
	for job := range s.jobs {
		size := s.measure(job.b64)
		s.mu.Lock()
		delete(s.pending, job.key)
		s.sizes.put(job.key, size)
		s.mu.Unlock()
	}
}

// measure transcodes one payload and returns the base64 bytes the request
// would carry: the WebP's, or the original's when that is not worth it.
func (s *imageShadow) measure(b64 []byte) int {
	start := time.Now()
	out, err := s.transcode(b64)
	s.stats.nanos.Add(int64(time.Since(start)))
	size := len(b64)
	switch {
	case err != nil:
		s.stats.failed.Add(1)
	case float64(len(out)) > imageKeepRatio*float64(len(b64)):
		s.stats.kept.Add(1)
	default:
		size = len(out)
	}
	s.stats.distinct.Add(1)
	s.stats.distinctBytes.Add(int64(len(b64)))
	s.stats.distinctWebp.Add(int64(size))
	return size
}

func (s *imageShadow) snapshot() map[string]any {
	st := &s.stats
	ratio := func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	avgMs := 0.0
	if n := st.distinct.Load(); n > 0 {
		avgMs = float64(st.nanos.Load()) / float64(n) / 1e6
	}
	return map[string]any{
		"mode": imagesShadow, "quality": s.quality,
		"bodies": st.bodies.Load(), "body_bytes": st.bodyBytes.Load(),
		"seen": st.seen.Load(), "seen_bytes": st.seenBytes.Load(),
		"known": st.known.Load(), "known_bytes": st.knownBytes.Load(), "known_webp_bytes": st.knownWebp.Load(),
		// Of every occurrence: what the origin (and the uplink to it) would save.
		"known_ratio": ratio(st.knownBytes.Load(), st.knownWebp.Load()),
		"distinct":    st.distinct.Load(), "distinct_bytes": st.distinctBytes.Load(), "distinct_webp_bytes": st.distinctWebp.Load(),
		// Of first sightings: about what the tunnel would save, since repeats
		// already cross it as references.
		"distinct_ratio": ratio(st.distinctBytes.Load(), st.distinctWebp.Load()),
		"kept":           st.kept.Load(), "failed": st.failed.Load(), "busy": st.busy.Load(),
		"small": st.small.Load(), "other_format": st.otherFormat.Load(), "other_format_bytes": st.otherBytes.Load(),
		"transcode_ms_avg": avgMs, "queued": len(s.jobs),
	}
}

// parseImages builds the image stage for -images, or nil when it is off.
func parseImages(mode string, quality, workers int) (*imageShadow, error) {
	switch mode {
	case imagesOff, "":
		return nil, nil
	case imagesShadow:
	default:
		return nil, fmt.Errorf("bad -images %q: want off or shadow", mode)
	}
	if quality < 1 || quality > 100 {
		return nil, fmt.Errorf("bad -images-quality %d: want 1..100", quality)
	}
	if workers < 1 {
		return nil, fmt.Errorf("bad -images-workers %d: want at least 1", workers)
	}
	transcode, err := webpTranscoder(quality)
	if err != nil {
		return nil, err
	}
	return newImageShadow(quality, workers, 16, 1<<16, transcode), nil
}

// sizeLRU remembers a size per key, dropping the least recently used past cap.
type sizeLRU struct {
	cap   int
	order *list.List
	items map[[32]byte]*list.Element
}

type sizeEntry struct {
	key  [32]byte
	size int
}

func newSizeLRU(cap int) *sizeLRU {
	return &sizeLRU{cap: cap, order: list.New(), items: make(map[[32]byte]*list.Element)}
}

func (l *sizeLRU) get(key [32]byte) (int, bool) {
	el, ok := l.items[key]
	if !ok {
		return 0, false
	}
	l.order.MoveToFront(el)
	return el.Value.(*sizeEntry).size, true
}

func (l *sizeLRU) put(key [32]byte, size int) {
	if el, ok := l.items[key]; ok {
		el.Value.(*sizeEntry).size = size
		l.order.MoveToFront(el)
		return
	}
	l.items[key] = l.order.PushFront(&sizeEntry{key: key, size: size})
	for l.order.Len() > l.cap {
		last := l.order.Back()
		l.order.Remove(last)
		delete(l.items, last.Value.(*sizeEntry).key)
	}
}
