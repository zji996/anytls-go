package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func zstdBody(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeAll(plain, nil)
}

func unzstd(t *testing.T, b []byte) []byte {
	t.Helper()
	dec, _ := zstd.NewReader(nil)
	out, err := dec.DecodeAll(b, nil)
	if err != nil {
		t.Fatalf("not zstd: %v", err)
	}
	return out
}

// postCoded posts body with Content-Encoding coding and returns the status.
func (c *reverseChain) postCoded(t *testing.T, body []byte, coding string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.encSrv.URL+"/v1/responses", bytes.NewReader(body))
	req.Host = "a.example"
	req.Header.Set("Content-Encoding", coding)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (o *origin) lastHeader() http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hdrs[len(o.hdrs)-1]
}

func TestRequestCoding(t *testing.T) {
	for in, want := range map[string]string{"": "", "zstd": codingZstd, " ZSTD ": codingZstd, "gzip": "other", "zstd, gzip": "other"} {
		h := http.Header{}
		if in != "" {
			h.Set("Content-Encoding", in)
		}
		if got := requestCoding(h); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	h := http.Header{"Content-Encoding": {"zstd", "zstd"}}
	if requestCoding(h) != "other" {
		t.Error("two codings must not be undone as one")
	}
}

func TestZstdBodiesDedupAndReachTheOriginInZstd(t *testing.T) {
	c := newReverseChain(t)
	if !c.enc.zstdOK.Load() {
		t.Fatal("the decoder did not offer to restore zstd")
	}
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	var sent []int64
	for turn := 1; turn <= 3; turn++ {
		plain := conversation(turn, images)
		before := c.enc.stats.sent.Load()
		if code := c.postCoded(t, zstdBody(t, plain), "zstd"); code != 200 {
			t.Fatalf("turn %d: status %d", turn, code)
		}
		sent = append(sent, c.enc.stats.sent.Load()-before)
		if got := c.a.lastHeader().Get("Content-Encoding"); got != codingZstd {
			t.Fatalf("turn %d: origin got Content-Encoding %q, want zstd", turn, got)
		}
		for _, h := range []string{hdrCoding, hdrFrame, hdrLen, hdrEpoch} {
			if c.a.lastHeader().Get(h) != "" {
				t.Fatalf("turn %d: %s leaked to the origin", turn, h)
			}
		}
		if !bytes.Equal(unzstd(t, c.a.last()), plain) {
			t.Fatalf("turn %d: the origin's body does not decode to the client's", turn)
		}
	}
	// Turn 3 repeats turns 1-2: only the new image crosses as a literal.
	if sent[2] > 400<<10 {
		t.Fatalf("turn 3 sent %d bytes: the decoded history did not dedup", sent[2])
	}
	snap := c.enc.snapshot()
	if snap["zstd"] != int64(3) || snap["zstd_passed"] != int64(0) || c.dec.snapshot()["zstd_restored"] != int64(3) {
		t.Fatalf("stats: encoder %v decoder %v", snap, c.dec.snapshot())
	}
}

func TestZstdThatCannotBeDecodedPassesAsItCame(t *testing.T) {
	c := newReverseChain(t)
	c.enc.maxBody = 1 << 20
	cases := map[string][]byte{
		// Valid zstd, but larger than -max-body-mib once decoded.
		"too large": zstdBody(t, prose(4<<20)),
		"not zstd":  bytes.Repeat([]byte("not a zstd frame at all "), 4096),
	}
	for name, raw := range cases {
		if code := c.postCoded(t, raw, "zstd"); code != 200 {
			t.Fatalf("%s: status %d", name, code)
		}
		if !bytes.Equal(c.a.last(), raw) || c.a.lastHeader().Get("Content-Encoding") != codingZstd {
			t.Fatalf("%s: the origin did not get the client's bytes and coding", name)
		}
	}
	if snap := c.enc.snapshot(); snap["zstd_passed"] != int64(2) || snap["zstd"] != int64(0) {
		t.Fatalf("stats: %v", snap)
	}
}

func TestZstdWaitsForADecoderThatRestoresIt(t *testing.T) {
	c := newReverseChain(t)
	c.enc.zstdOK.Store(false) // a decoder from before zstd support
	raw := zstdBody(t, conversation(2, []string{image(300 << 10), image(300 << 10)}))
	if code := c.postCoded(t, raw, "zstd"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if !bytes.Equal(c.a.last(), raw) || c.enc.snapshot()["zstd"] != int64(0) {
		t.Fatal("without a restoring decoder the body must pass untouched")
	}
}

func TestZstdResendAfterDecoderLossCarriesTheClientsBytes(t *testing.T) {
	c := newReverseChain(t)
	images := []string{image(300 << 10), image(300 << 10)}
	c.postCoded(t, zstdBody(t, conversation(1, images)), "zstd")
	// The decoder forgets everything: the next frame refers to chunks it
	// lacks, and the encoder resends the body plain.
	c.dec.cache = newLRU(64 << 20)
	raw := zstdBody(t, conversation(2, images))
	if code := c.postCoded(t, raw, "zstd"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if c.enc.stats.retries.Load() != 1 {
		t.Fatalf("%d retries, want 1", c.enc.stats.retries.Load())
	}
	if !bytes.Equal(c.a.last(), raw) || c.a.lastHeader().Get("Content-Encoding") != codingZstd {
		t.Fatal("an unrewritten resend must carry the client's own zstd bytes")
	}
}

func TestZstdWithWebpImagesResendsTheRewrittenBody(t *testing.T) {
	c := newReverseChain(t)
	var calls atomic.Int64
	c.enc.images = webpStage(2, 2*time.Second, halfTranscoder(&calls))
	images := []string{image(300 << 10), image(300 << 10)}
	want := func(turn int) []byte {
		w := make([]string, turn)
		for i := range w {
			w[i] = asWebp(images[i])
		}
		return conversation(turn, w)
	}
	c.postCoded(t, zstdBody(t, conversation(1, images)), "zstd")
	if !bytes.Equal(unzstd(t, c.a.last()), want(1)) {
		t.Fatal("turn 1: the origin did not get the WebP")
	}
	c.dec.cache = newLRU(64 << 20)
	c.postCoded(t, zstdBody(t, conversation(2, images)), "zstd")
	if c.enc.stats.retries.Load() != 1 || c.a.lastHeader().Get("Content-Encoding") != codingZstd {
		t.Fatal("turn 2 must be resent in zstd")
	}
	if !bytes.Equal(unzstd(t, c.a.last()), want(2)) {
		t.Fatal("turn 2: a resend must keep the WebPs, not the client's PNGs")
	}
}

// BenchmarkZstd is the cost on either side of the tunnel for a body like
// the largest Codex sends: about 25 MiB of history with screenshots.
func BenchmarkZstd(b *testing.B) {
	images := make([]string, 24)
	for i := range images {
		images[i] = image(400 << 10)
	}
	plain := append(conversation(len(images), images), prose(10<<20)...)
	raw := encodeZstd(plain)
	b.Run("decode", func(b *testing.B) {
		b.SetBytes(int64(len(plain)))
		for b.Loop() {
			if _, err := zstdDecoder(64<<20).DecodeAll(raw, make([]byte, 0, len(plain))); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode", func(b *testing.B) {
		b.SetBytes(int64(len(plain)))
		for b.Loop() {
			encodeZstd(plain)
		}
	})
}

// repetitive is text that zstd shrinks far more than 8x, yet not below a
// chunk: numbered lines of the same sentence.
func repetitive(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "{\"turn\":%d,\"text\":\"all work and no play makes jack a dull boy\"}\n", i)
	}
	return b.Bytes()
}

// zstdStream compresses like a streaming client: no decoded size in the
// header.
func zstdStream(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()
	if zstdSize(buf.Bytes()) >= 0 {
		t.Fatal("the stream declares its size")
	}
	return buf.Bytes()
}

func TestZstdWithoutDeclaredSizeDecodesWithinTheBudget(t *testing.T) {
	c := newReverseChain(t)
	// Text compresses far better than the first guess of 4x.
	plain := append(repetitive(1<<20), prose(128<<10)...)
	raw := zstdStream(t, plain)
	if len(plain) < 16*len(raw) {
		t.Fatalf("ratio %d: the body must outgrow the first guess", len(plain)/len(raw))
	}
	if code := c.postCoded(t, raw, "zstd"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if !bytes.Equal(unzstd(t, c.a.last()), plain) || c.enc.snapshot()["zstd"] != int64(1) {
		t.Fatal("a body larger than guessed must still be decoded and framed")
	}
	if used := c.enc.inflight.used.Load(); used != 0 {
		t.Fatalf("%d inflight bytes left held after the request", used)
	}
}

func TestZstdWithoutRoomInTheBudgetPassesAsItCame(t *testing.T) {
	c := newReverseChain(t)
	plain := append(repetitive(1<<20), prose(128<<10)...)
	raw := zstdStream(t, plain)
	// Room for the compressed body, not for decoding it.
	c.enc.inflight = &budget{limit: 2*int64(len(raw)) + requestOverhead + 1<<20}
	c.enc.inflightWait = 10 * time.Millisecond
	if code := c.postCoded(t, raw, "zstd"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if !bytes.Equal(c.a.last(), raw) || c.enc.snapshot()["zstd_passed"] != int64(1) {
		t.Fatal("without memory to decode, the body must pass as it came")
	}
	if used := c.enc.inflight.used.Load(); used != 0 {
		t.Fatalf("%d inflight bytes left held after the request", used)
	}
}

func TestOffersCoding(t *testing.T) {
	for list, want := range map[string]bool{"zstd": true, " br, ZSTD ": true, "zstd ": true, "": false, "br": false, "zstdx": false} {
		if offersCoding(list, codingZstd) != want {
			t.Errorf("%q: want %v", list, want)
		}
	}
}

func TestZstdThatDecodesEmptyPassesAsItCame(t *testing.T) {
	c := newReverseChain(t)
	// A skippable frame pads the body past a chunk; it decodes to nothing.
	skip := append([]byte{0x50, 0x2a, 0x4d, 0x18}, binary.LittleEndian.AppendUint32(nil, 8<<10)...)
	raw := append(append(skip, make([]byte, 8<<10)...), zstdBody(t, nil)...)
	if code := c.postCoded(t, raw, "zstd"); code != 200 {
		t.Fatalf("status %d, want the origin's answer", code)
	}
	if !bytes.Equal(c.a.last(), raw) || c.enc.snapshot()["zstd_passed"] != int64(1) {
		t.Fatal("an empty decode must pass as it came")
	}
}

func TestDecoderWithASmallerBodyLimitTakesTheResend(t *testing.T) {
	c := newReverseChain(t)
	c.dec.maxBody = 1 << 20 // the encoder allows 64 MiB
	plain := conversation(2, []string{image(800 << 10), image(800 << 10)})
	if code := c.postCoded(t, zstdBody(t, plain), "zstd"); code != 200 {
		t.Fatalf("status %d, want the origin's answer", code)
	}
	if c.enc.stats.retries.Load() != 1 || !bytes.Equal(unzstd(t, c.a.last()), plain) {
		t.Fatal("a body over the decoder's limit must be resent plain, not refused")
	}
}

func TestDecoderSendsAnUnknownCodingPlain(t *testing.T) {
	c := newReverseChain(t)
	body := conversation(1, []string{image(200 << 10)})
	res, err := encodeFrame(body, newLRU(64<<20), compressAuto)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, c.decAddr+"/v1/responses", bytes.NewReader(res.frame))
	req.Host = "a.example"
	req.Header.Set(hdrFrame, "1")
	req.Header.Set(hdrLen, fmt.Sprint(len(body)))
	req.Header.Set(hdrCoding, "br")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := c.a.lastHeader()
	if resp.StatusCode != 200 || !bytes.Equal(c.a.last(), body) || h.Get("Content-Encoding") != "" || h.Get(hdrCoding) != "" {
		t.Fatalf("status %d, CE %q: an unknown coding must arrive as the plain body", resp.StatusCode, h.Get("Content-Encoding"))
	}
}

func TestResendBytesAreMadeOnce(t *testing.T) {
	f := &framedRequest{body: repetitive(1 << 20), coding: codingZstd}
	outs := make(chan []byte, 4)
	for range 4 {
		go func() { b, _ := f.resendBytes(); outs <- b }()
	}
	first := <-outs
	for range 3 {
		if got := <-outs; &got[0] != &first[0] {
			t.Fatal("concurrent resends compressed the body more than once")
		}
	}
	if !bytes.Equal(unzstd(t, first), f.body) {
		t.Fatal("the resend does not decode to the body")
	}
}

func TestZstdDeclaredSizeBuysNoMoreThanTheRatioBound(t *testing.T) {
	e := newEncoder(route{url: mustURL(t, "http://127.0.0.1:1")}, 1<<20, 64<<20)
	e.inflight = &budget{limit: 256 << 20}
	// A frame header that claims 60 MiB, then one empty last block.
	raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0xe0}
	raw = binary.LittleEndian.AppendUint64(raw, 60<<20)
	raw = append(raw, 0x01, 0x00, 0x00)
	held := e.inflight.take(2*int64(len(raw)) + requestOverhead)
	before := e.inflight.used.Load()
	if _, ok := e.decodeZstd(context.Background(), raw, held); ok {
		t.Fatal("a claim beyond the ratio bound must pass as it came")
	}
	if grown := e.inflight.used.Load() - before; grown != 0 {
		t.Fatalf("a %d-byte body reserved %d more bytes", len(raw), grown)
	}
	held.free()
}

func TestZstdStreamsDecodeAtEverySize(t *testing.T) {
	e := newEncoder(route{url: mustURL(t, "http://127.0.0.1:1")}, 1<<20, 64<<20)
	e.inflight = &budget{}
	for n := 512 << 10; n <= 8<<20; n = n*5/4 + 4093 {
		plain := repetitive(n)
		// Flushed every KiB, as a streaming client writes: many short blocks.
		var buf bytes.Buffer
		w, _ := zstd.NewWriter(&buf)
		for i := 0; i < len(plain); i += 1024 {
			w.Write(plain[i:min(len(plain), i+1024)])
			w.Flush()
		}
		w.Close()
		held := e.inflight.take(2 * int64(buf.Len()))
		got, ok := e.decodeZstd(context.Background(), buf.Bytes(), held)
		if !ok || !bytes.Equal(got, plain) {
			t.Fatalf("%d bytes (%d compressed): decoded %v", len(plain), buf.Len(), ok)
		}
	}
}

func TestResendAfterTheAnswerIsRefusedEveryTime(t *testing.T) {
	f := &framedRequest{body: repetitive(64 << 10), coding: codingZstd, held: (&budget{}).take(0), known: newLRU(1 << 20)}
	req, _ := http.NewRequest(http.MethodPost, "http://a.example/", nil)
	f.done()
	for range 2 {
		if b, ok := f.resendBytes(); ok || b != nil {
			t.Fatal("a resend after done must not carry a body")
		}
		if plain, err := plainCopy(req, f); err == nil || plain != nil {
			t.Fatal("a resend after done must fail, not send an empty body")
		}
	}
}

func TestZstdGarbageCostsNoMoreThanTheFirstGuess(t *testing.T) {
	e := newEncoder(route{url: mustURL(t, "http://127.0.0.1:1")}, 1<<20, 64<<20)
	e.inflight = &budget{limit: 1 << 30}
	raw := bytes.Repeat([]byte("not zstd at all "), 1<<16) // 1 MiB
	held := e.inflight.take(2*int64(len(raw)) + requestOverhead)
	before := e.inflight.used.Load()
	peak := int64(0)
	if _, ok := e.decodeZstd(context.Background(), raw, held); ok {
		t.Fatal("garbage must not decode")
	}
	peak = e.inflight.used.Load() - before
	// The first attempt at 4x: 2*(4-1/2) times raw, and no doubling after.
	if limit := int64(7 * len(raw)); peak > limit {
		t.Fatalf("garbage grew the budget by %d MiB, want at most %d", peak>>20, limit>>20)
	}
	held.free()
}

func TestZstdKeepsOnlyWhatTheBodyNeeds(t *testing.T) {
	e := newEncoder(route{url: mustURL(t, "http://127.0.0.1:1")}, 1<<20, 64<<20)
	e.inflight = &budget{limit: 1 << 30}
	// Screenshots barely compress: the 4x guess is far too generous.
	plain := conversation(4, []string{image(400 << 10), image(400 << 10), image(400 << 10), image(400 << 10)})
	raw := zstdStream(t, plain)
	held := e.inflight.take(2*int64(len(raw)) + requestOverhead)
	body, ok := e.decodeZstd(context.Background(), raw, held)
	if !ok || !bytes.Equal(body, plain) {
		t.Fatal("decode failed")
	}
	if got, want := e.inflight.used.Load(), 2*int64(len(body))+int64(len(raw))+requestOverhead; got != want {
		t.Fatalf("holding %d bytes after decoding, want %d", got, want)
	}
	held.free()
}
