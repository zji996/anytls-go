package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Images: agents attach screenshots as base64 PNG data URLs and resend every
// one of them each turn. Re-encoding them as WebP before the tunnel shrinks
// what crosses it and what the origin receives. A small background pool of
// low-priority workers transcodes each new image once; the request path finds
// the images and looks up results already known.
//
// shadow only measures: requests are left untouched and new images are queued
// without waiting. webp swaps the WebP in. A request waits up to -images-wait
// for its new images; one that is not ready by then, or that the queue could
// not finish in time, goes out as PNG and stays PNG for as long as the cache
// remembers it. An upstream prompt cache matches a conversation's resent
// images byte for byte, so an image must not change form between turns.
//
// The codec lives behind the dedupimages build tag (see images_webp.go and
// images_webp_native.go), so builds without it, such as the router front, link
// no image code at all. Of the two codecs, nodynamic picks libwebp transpiled
// to Go and webpnative picks libwebp compiled in through cgo; they encode to
// the same bytes.

const (
	imagesOff    = "off"
	imagesShadow = "shadow"
	imagesWebp   = "webp"
)

// imageQueueMiB bounds the copies of new images waiting for a worker. A
// restart empties the size cache, and the first busy minutes then bring dozens
// of new screenshots at once.
const imageQueueMiB = 64

// imageMinB64 skips icons and thumbnails: transcoding them saves too little.
const imageMinB64 = 64 << 10

// imageCacheMiB holds the WebPs of recent images in webp mode, about two
// thousand screenshots. An image that falls out was not resent for long
// enough that an upstream prompt cache has let it go too.
const imageCacheMiB = 128

// imageEntryBytes is what a cache entry costs besides its WebP. In shadow
// mode, which keeps no WebP, it bounds the cache to 64Ki entries.
const imageEntryBytes = 160

// imageStartRate is the transcode speed assumed until one is measured, in
// base64 bytes a second a worker: about 0.5 s for a 530 KiB screenshot.
const imageStartRate = 1 << 20

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
	// whose result was already known (the rest were queued or dropped).
	seen, seenBytes, known, knownBytes, knownWebp atomic.Int64
	// small images and formats other than PNG, left alone.
	small, otherFormat, otherBytes atomic.Int64
	// Each distinct image the pool transcoded.
	distinct, distinctBytes, distinctWebp atomic.Int64
	kept, failed, busy, tooLarge          atomic.Int64
	nanos                                 atomic.Int64
	// Bytes of the bodies that were looked at, images or not.
	bodies, bodyBytes atomic.Int64
	// webp: occurrences sent as WebP and their PNG and WebP bytes; images
	// left PNG because they were not ready in time (late) or would not have
	// been (shed); requests that waited for new images, and for how long.
	replaced, replacedBytes, replacedWebp atomic.Int64
	late, shed                            atomic.Int64
	waits, waitNanos, waitMaxNanos        atomic.Int64
}

// imageResult is what a request carries for one image.
type imageResult struct {
	size int    // base64 bytes: the WebP's, or the PNG's
	webp []byte // base64 WebP to send instead; nil sends the PNG
}

type imageJob struct {
	key [32]byte
	b64 []byte
	// done is closed once the result is in the cache, or the job is called
	// off. Under mu: started once a worker took it, cancelled once called off.
	done               chan struct{}
	started, cancelled bool
}

// imageStage measures (shadow) or applies (webp) WebP re-encoding.
type imageStage struct {
	mode             string
	quality, workers int
	wait             time.Duration
	transcode        transcodeFunc
	jobs             chan *imageJob
	stats            imageCounters
	decodeBudget     imageDecodeBudget

	mu         sync.Mutex
	cache      *imageLRU // sha256 of the payload -> result
	pending    map[[32]byte]*imageJob
	queued     int // bytes of the payloads in jobs not yet started
	queueLimit int
	rate       float64 // base64 bytes a second a worker, smoothed
}

type imageConfig struct {
	mode             string
	quality, workers int
	wait             time.Duration // webp: how long a request waits for new images
	queueBytes       int           // payloads waiting for a worker
	cacheBytes       int
}

// newImageStage starts the workers.
func newImageStage(c imageConfig, transcode transcodeFunc) *imageStage {
	s := &imageStage{
		mode:         c.mode,
		quality:      c.quality,
		workers:      c.workers,
		wait:         c.wait,
		transcode:    transcode,
		jobs:         make(chan *imageJob, c.queueBytes/imageMinB64+1),
		cache:        newImageLRU(c.cacheBytes),
		pending:      make(map[[32]byte]*imageJob),
		queueLimit:   c.queueBytes,
		rate:         imageStartRate,
		decodeBudget: imageDecodeBudget{limit: imageDecodeBudgetMiB << 20},
	}
	for range c.workers {
		go s.work()
	}
	return s
}

// process is the request-path hook: it returns the body to send.
func (s *imageStage) process(ctx context.Context, body []byte) []byte {
	if s.mode == imagesWebp {
		return s.rewrite(ctx, body)
	}
	s.observe(body)
	return body
}

// pngImages lists the PNGs in body worth transcoding, counting the rest.
func (s *imageStage) pngImages(body []byte) []dataImage {
	s.stats.bodies.Add(1)
	s.stats.bodyBytes.Add(int64(len(body)))
	var out []dataImage
	for _, img := range findDataImages(body) {
		b64 := body[img.start:img.end]
		switch {
		case img.mime != "png":
			s.stats.otherFormat.Add(1)
			s.stats.otherBytes.Add(int64(len(b64)))
		case len(b64) < imageMinB64:
			s.stats.small.Add(1)
		default:
			s.stats.seen.Add(1)
			s.stats.seenBytes.Add(int64(len(b64)))
			out = append(out, img)
		}
	}
	return out
}

func (s *imageStage) countKnown(b64 []byte, r imageResult) {
	s.stats.known.Add(1)
	s.stats.knownBytes.Add(int64(len(b64)))
	s.stats.knownWebp.Add(int64(r.size))
}

// enqueueLocked queues a transcode of b64, or returns nil when the queue is
// full.
func (s *imageStage) enqueueLocked(key [32]byte, b64 []byte) *imageJob {
	if s.queued+len(b64) > s.queueLimit {
		return nil
	}
	job := &imageJob{key: key, b64: bytes.Clone(b64), done: make(chan struct{})}
	select {
	case s.jobs <- job:
	default:
		return nil
	}
	s.pending[key] = job
	s.queued += len(b64)
	return job
}

// observe accounts for the images in body. It neither changes nor keeps body.
func (s *imageStage) observe(body []byte) {
	for _, img := range s.pngImages(body) {
		b64 := body[img.start:img.end]
		key := sha256.Sum256(b64)
		s.mu.Lock()
		r, ok := s.cache.get(key)
		if !ok && s.pending[key] == nil && !s.settleTooLargeLocked(key, b64) && s.enqueueLocked(key, b64) == nil {
			s.stats.busy.Add(1)
		}
		s.mu.Unlock()
		if ok {
			s.countKnown(b64, r)
		}
	}
}

// finishesInTimeLocked reports whether a new job of n bytes would be done within
// left: the queue ahead is shared among the workers, the job itself is not.
// The measured per-worker rate includes budget waits and CPU contention, so
// oversubscribing cores does not multiply a fixed single-core throughput.
func (s *imageStage) finishesInTimeLocked(n int, left time.Duration) bool {
	ahead := float64(s.queued) / (s.rate * float64(s.workers))
	own := float64(n) / s.rate
	return time.Duration((ahead+own)*float64(time.Second)) <= left
}

// settleTooLargeLocked sheds images that cannot fit even with no other jobs.
func (s *imageStage) settleTooLargeLocked(key [32]byte, b64 []byte) bool {
	if imageDecodeEstimate(b64) <= s.decodeBudget.limit {
		return false
	}
	s.stats.tooLarge.Add(1)
	s.settlePNGLocked(key, len(b64))
	return true
}

// settlePNGLocked fixes key as sent as PNG.
func (s *imageStage) settlePNGLocked(key [32]byte, n int) {
	s.cache.put(key, imageResult{size: n})
}

// rewrite returns body with each PNG whose WebP is ready by the deadline
// swapped for that WebP. It never changes body itself.
func (s *imageStage) rewrite(ctx context.Context, body []byte) []byte {
	imgs := s.pngImages(body)
	if len(imgs) == 0 {
		return body
	}
	start := time.Now()
	deadline := start.Add(s.wait)
	keys := make([][32]byte, len(imgs))
	results := make(map[[32]byte]imageResult, len(imgs))
	waiting := make(map[[32]byte]*imageJob)
	s.mu.Lock()
	for i, img := range imgs {
		b64 := body[img.start:img.end]
		key := sha256.Sum256(b64)
		keys[i] = key
		if _, ok := results[key]; ok {
			continue
		}
		if waiting[key] != nil {
			continue
		}
		if r, ok := s.cache.get(key); ok {
			results[key] = r
			continue
		}
		if job := s.pending[key]; job != nil {
			waiting[key] = job
			continue
		}
		if s.settleTooLargeLocked(key, b64) {
			results[key] = imageResult{size: len(b64)}
			continue
		}
		var job *imageJob
		if s.finishesInTimeLocked(len(b64), time.Until(deadline)) {
			job = s.enqueueLocked(key, b64)
		}
		if job == nil {
			// Waiting would not get it in time: back-pressure sends it
			// as it came, now and from now on.
			s.stats.shed.Add(1)
			s.settlePNGLocked(key, len(b64))
			results[key] = imageResult{size: len(b64)}
			continue
		}
		waiting[key] = job
	}
	s.mu.Unlock()

	if len(waiting) > 0 {
		s.awaitJobs(ctx, waiting, deadline)
		waited := int64(time.Since(start))
		s.stats.waits.Add(1)
		s.stats.waitNanos.Add(waited)
		for {
			prev := s.stats.waitMaxNanos.Load()
			if waited <= prev || s.stats.waitMaxNanos.CompareAndSwap(prev, waited) {
				break
			}
		}
		s.mu.Lock()
		for key, job := range waiting {
			if r, ok := s.cache.get(key); ok {
				results[key] = r
				continue
			}
			if ctx.Err() != nil {
				// The client left: decide nothing on its account.
				continue
			}
			if !job.cancelled {
				// Late: call it off and send the PNG, now and from now on.
				s.cancelLocked(job)
				s.stats.late.Add(1)
				s.settlePNGLocked(key, len(job.b64))
			}
		}
		s.mu.Unlock()
	}
	return s.splice(body, imgs, keys, results)
}

// awaitJobs waits until every job is done, the deadline passes or ctx ends.
func (s *imageStage) awaitJobs(ctx context.Context, jobs map[[32]byte]*imageJob, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// cancelLocked takes job out of the queue, or, if a worker already has it,
// makes the worker drop its result.
func (s *imageStage) cancelLocked(job *imageJob) {
	job.cancelled = true
	if !job.started {
		s.queued -= len(job.b64)
	}
	delete(s.pending, job.key)
	close(job.done)
}

// splice builds the body to send: each image with a WebP result becomes a
// WebP data URL, the rest stays as it was.
func (s *imageStage) splice(body []byte, imgs []dataImage, keys [][32]byte, results map[[32]byte]imageResult) []byte {
	const mimeTail = len("png;base64,")
	var out []byte
	last := 0
	for i, img := range imgs {
		r := results[keys[i]]
		if r.webp == nil {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(body))
		}
		out = append(out, body[last:img.start-mimeTail]...)
		out = append(out, "webp;base64,"...)
		out = append(out, r.webp...)
		last = img.end
		s.stats.replaced.Add(1)
		s.stats.replacedBytes.Add(int64(img.end - img.start))
		s.stats.replacedWebp.Add(int64(len(r.webp)))
	}
	for i, img := range imgs {
		if r, ok := results[keys[i]]; ok {
			s.countKnown(body[img.start:img.end], r)
		}
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}

// work runs on a thread of its own at a lower CPU priority, so that the
// request path, Caddy and frps on the same cores come first.
func (s *imageStage) work() {
	runtime.LockOSThread()
	lowerThreadPriority()
	for job := range s.jobs {
		s.mu.Lock()
		if job.cancelled {
			s.mu.Unlock()
			continue
		}
		job.started = true
		s.queued -= len(job.b64)
		s.mu.Unlock()
		began := time.Now()
		estimate := imageDecodeEstimate(job.b64)
		if !s.decodeBudget.acquire(estimate, job.done) {
			continue
		}
		r := s.measure(job.b64)
		s.decodeBudget.release(estimate)
		took := time.Since(began)
		s.mu.Lock()
		if took > 0 {
			const weight = 0.2
			s.rate = (1-weight)*s.rate + weight*float64(len(job.b64))/took.Seconds()
		}
		if !job.cancelled {
			s.cache.put(job.key, r)
			delete(s.pending, job.key)
			close(job.done)
		}
		s.mu.Unlock()
	}
}

// measure transcodes one payload and returns what a request carries for it:
// the WebP, or the original when that is not worth it.
func (s *imageStage) measure(b64 []byte) imageResult {
	start := time.Now()
	out, err := s.transcode(b64)
	s.stats.nanos.Add(int64(time.Since(start)))
	r := imageResult{size: len(b64)}
	switch {
	case err != nil:
		s.stats.failed.Add(1)
	case float64(len(out)) > imageKeepRatio*float64(len(b64)):
		s.stats.kept.Add(1)
	default:
		r.size = len(out)
		if s.mode == imagesWebp {
			r.webp = out
		}
	}
	s.stats.distinct.Add(1)
	s.stats.distinctBytes.Add(int64(len(b64)))
	s.stats.distinctWebp.Add(int64(r.size))
	return r
}

func (s *imageStage) snapshot() map[string]any {
	st := &s.stats
	ratio := func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	perMs := func(nanos, n int64) float64 {
		if n == 0 {
			return 0
		}
		return float64(nanos) / float64(n) / 1e6
	}
	s.mu.Lock()
	cached, cacheBytes, rate := s.cache.len(), s.cache.used, s.rate
	s.mu.Unlock()
	snap := map[string]any{
		"mode": s.mode, "quality": s.quality, "workers": s.workers,
		"decode_budget_mib": imageDecodeBudgetMiB, "too_large": st.tooLarge.Load(),
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
		"transcode_ms_avg": perMs(st.nanos.Load(), st.distinct.Load()), "queued": len(s.jobs),
		"rate_kib_s": rate / 1024, "cached": cached, "cache_bytes": cacheBytes,
	}
	if s.mode == imagesWebp {
		snap["wait_ms"] = s.wait.Milliseconds()
		snap["replaced"] = st.replaced.Load()
		snap["replaced_bytes"] = st.replacedBytes.Load()
		snap["replaced_webp_bytes"] = st.replacedWebp.Load()
		snap["late"] = st.late.Load()
		snap["shed"] = st.shed.Load()
		snap["waits"] = st.waits.Load()
		snap["wait_ms_avg"] = perMs(st.waitNanos.Load(), st.waits.Load())
		snap["wait_ms_max"] = float64(st.waitMaxNanos.Load()) / 1e6
	}
	return snap
}

// parseImages builds the image stage for -images, or nil when it is off.
func parseImages(mode string, quality, workers int, wait time.Duration) (*imageStage, error) {
	cache := imageCacheMiB << 20
	switch mode {
	case imagesOff, "":
		return nil, nil
	case imagesShadow:
		cache = 1 << 16 * imageEntryBytes
	case imagesWebp:
		if wait <= 0 || wait > time.Minute {
			return nil, fmt.Errorf("bad -images-wait %v: want (0, 1m]", wait)
		}
	default:
		return nil, fmt.Errorf("bad -images %q: want off, shadow or webp", mode)
	}
	if quality < 1 || quality > 100 {
		return nil, fmt.Errorf("bad -images-quality %d: want 1..100", quality)
	}
	if workers < 1 || workers > 64 {
		return nil, fmt.Errorf("bad -images-workers %d: want 1..64", workers)
	}
	transcode, err := webpTranscoder(quality)
	if err != nil {
		return nil, err
	}
	return newImageStage(imageConfig{
		mode: mode, quality: quality, workers: workers, wait: wait,
		queueBytes: imageQueueMiB << 20, cacheBytes: cache,
	}, transcode), nil
}

// imageLRU keeps results up to a byte budget, dropping the least recently
// used first.
type imageLRU struct {
	limit, used int
	order       *list.List
	items       map[[32]byte]*list.Element
}

type imageEntry struct {
	key [32]byte
	r   imageResult
}

func newImageLRU(limit int) *imageLRU {
	return &imageLRU{limit: limit, order: list.New(), items: make(map[[32]byte]*list.Element)}
}

func entryCost(r imageResult) int { return imageEntryBytes + len(r.webp) }

func (l *imageLRU) len() int { return l.order.Len() }

func (l *imageLRU) get(key [32]byte) (imageResult, bool) {
	el, ok := l.items[key]
	if !ok {
		return imageResult{}, false
	}
	l.order.MoveToFront(el)
	return el.Value.(*imageEntry).r, true
}

func (l *imageLRU) put(key [32]byte, r imageResult) {
	if el, ok := l.items[key]; ok {
		e := el.Value.(*imageEntry)
		l.used += entryCost(r) - entryCost(e.r)
		e.r = r
		l.order.MoveToFront(el)
	} else {
		l.items[key] = l.order.PushFront(&imageEntry{key: key, r: r})
		l.used += entryCost(r)
	}
	for l.used > l.limit && l.order.Len() > 1 {
		last := l.order.Back()
		e := last.Value.(*imageEntry)
		l.order.Remove(last)
		delete(l.items, e.key)
		l.used -= entryCost(e.r)
	}
}
