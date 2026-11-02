package main

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Request bodies compressed with zstd: Codex Desktop sends most turns that
// way, and a compressed body neither dedups nor shows its images. The encoder
// decompresses it and frames the plain body like any other; the decoder
// compresses the rebuilt body again and restores Content-Encoding, so the
// origin receives the coding the client chose. The encoder only does this
// while the decoder says, on /health, that it can restore the coding. A body
// that does not decode, would decode larger than -max-body-mib, or finds no
// memory goes on exactly as it came: nothing is refused.

const (
	// hdrCoding, on a framed request: the Content-Encoding the decoder
	// restores on the rebuilt body.
	hdrCoding = "X-Edge-Dedup-Coding"
	// hdrCodings, on /health: the codings the decoder can restore.
	hdrCodings = "X-Edge-Dedup-Codings"
	codingZstd = "zstd"
)

// requestCoding is the request's Content-Encoding when it is a single coding
// the dedup path can undo, else "" for none and "other" for any other.
func requestCoding(h http.Header) string {
	values := h.Values("Content-Encoding")
	switch {
	case len(values) == 0:
		return ""
	case len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), codingZstd):
		return codingZstd
	case len(values) == 1 && strings.TrimSpace(values[0]) == "":
		return ""
	}
	return "other"
}

var (
	zstdDecoders sync.Map // max decoded bytes (int64) -> *zstd.Decoder
	zstdEncoder  = sync.OnceValue(func() *zstd.Encoder {
		// EncodeAll runs on up to this many requests at once; more wait. The
		// fastest level: the output only crosses a LAN, or, rarely, is a
		// resend the tunnel already carried once.
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(min(4, runtime.GOMAXPROCS(0))), zstd.WithLowerEncoderMem(true))
		if err != nil {
			panic(err)
		}
		return enc
	})
)

// zstdDecoder returns a decoder whose DecodeAll fails past limit bytes, or
// past the capacity of the buffer it decodes into.
func zstdDecoder(limit int64) *zstd.Decoder {
	if d, ok := zstdDecoders.Load(limit); ok {
		return d.(*zstd.Decoder)
	}
	// Each concurrent DecodeAll keeps window-sized buffers in a pool that
	// outlives it, outside the inflight budget: two at once bound that, and
	// a 25 MiB body decodes in tens of milliseconds even on an egress.
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(uint64(limit)), zstd.WithDecodeAllCapLimit(true),
		zstd.WithDecoderConcurrency(2), zstd.WithDecoderLowmem(true))
	if err != nil {
		panic(err)
	}
	actual, loaded := zstdDecoders.LoadOrStore(limit, d)
	if loaded {
		d.Close()
	}
	return actual.(*zstd.Decoder)
}

// offersCoding reports whether a decoder's comma-separated codings include
// coding.
func offersCoding(list, coding string) bool {
	for _, c := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(c), coding) {
			return true
		}
	}
	return false
}

// zstdSize is the decoded size a zstd body declares, or -1 if it does not.
func zstdSize(raw []byte) int64 {
	var h zstd.Header
	if h.Decode(raw) != nil || !h.HasFCS || h.FrameContentSize > 1<<40 {
		return -1
	}
	// A body of several frames is larger than its first declares.
	if h.FrameContentSize == 0 {
		return -1
	}
	return int64(h.FrameContentSize)
}

// zstdMaxRatio bounds what a body may decode to, relative to its compressed
// size. A body that would decode larger, or says it would, passes on as it
// came: so compressible, it is small on the wire anyway, and no header can
// make the encoder reserve more than this per byte received.
const zstdMaxRatio = 64

// zstdBlockMax is the largest zstd block: an attempt that ran out of room
// stopped within one of its end.
const zstdBlockMax = 128 << 10

// zstdGuessRatio sizes the first attempt at a body that does not declare its
// decoded size (streaming encoders, such as Codex's, leave it out): Codex's
// JSON with screenshots decodes to 2-5 times its size.
const zstdGuessRatio = 4

// decodeZstd decompresses raw within the encoder's body limit. It decodes
// only into memory the inflight budget already covers: held grows before each
// attempt, and an attempt that fails short of the bound doubles the room and
// starts over (a frame without a declared size reveals it only by running
// out). ok is false when the request should pass on as it came.
func (e *encoder) decodeZstd(ctx context.Context, raw []byte, held *share) (body []byte, ok bool) {
	bound := min(e.maxBody, zstdMaxRatio*int64(len(raw)))
	size := zstdSize(raw)
	switch {
	case size > bound:
		return nil, false
	case size < 0:
		size = min(bound, zstdGuessRatio*int64(len(raw)))
	}
	// A plain body of c bytes holds it, a frame about as large and raw, kept
	// for a resend: 2c + len(raw). held came with twice raw.
	covered := int64(len(raw)) / 2
	dec := zstdDecoder(e.maxBody)
	for {
		if size > covered {
			if !held.grow(ctx, 2*(size-covered), e.inflightWait) {
				return nil, false
			}
			covered = size
		}
		body, err := dec.DecodeAll(raw, make([]byte, 0, size))
		if err == nil {
			if len(body) == 0 {
				// Nothing to dedup, and a frame cannot carry it.
				return nil, false
			}
			// The guess may have been generous: keep what the body needs.
			held.shrink(2*int64(len(body)) + int64(len(raw)) + requestOverhead)
			return body, true
		}
		// Out of room shows as more than one error, always with the output
		// filled close to its capacity (within a block). Anything short of
		// that is a body that does not decode at any size.
		if size >= bound || len(body)+zstdBlockMax < int(size) && len(body) < int(size)/2 {
			return nil, false
		}
		size = min(bound, 2*size)
	}
}

// encodeZstd compresses body for a request that came with zstd.
func encodeZstd(body []byte) []byte {
	return zstdEncoder().EncodeAll(body, make([]byte, 0, len(body)/4+64))
}
