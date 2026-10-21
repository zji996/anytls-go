package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// origin records request bodies and streams two SSE events per request.
type origin struct {
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	hosts  []string
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.bodies = append(o.bodies, body)
	o.hdrs = append(o.hdrs, r.Header.Clone())
	o.hosts = append(o.hosts, r.Host)
	o.mu.Unlock()
	if r.URL.Path == "/hints" {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
	}
	if r.URL.Path == "/stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "data: second\n\n")
		return
	}
	fmt.Fprintf(w, "ok %d", len(body))
}

func (o *origin) last() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.bodies[len(o.bodies)-1]
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func direct(t *testing.T, s string) route {
	t.Helper()
	return route{url: mustURL(t, s)}
}

type chain struct {
	origin  *origin
	enc     *encoder
	dec     *decoder
	encSrv  *httptest.Server
	decSrv  *httptest.Server
	origSrv *httptest.Server
}

func newChain(t *testing.T, decoderCache int64) *chain {
	t.Helper()
	c := &chain{origin: &origin{}}
	c.origSrv = httptest.NewServer(c.origin)
	c.dec = newDecoder(direct(t, c.origSrv.URL), decoderCache, 64<<20)
	c.decSrv = httptest.NewServer(c.dec)
	c.enc = newEncoder(direct(t, c.decSrv.URL), decoderCache*9/10, 64<<20)
	c.encSrv = httptest.NewServer(c.enc)
	t.Cleanup(func() { c.encSrv.Close(); c.decSrv.Close(); c.origSrv.Close() })
	// The encoder frames only after a decoder has answered once.
	c.get(t, "/health")
	return c
}

func (c *chain) get(t *testing.T, path string) {
	t.Helper()
	resp, err := http.Get(c.encSrv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get(hdrEpoch) != "" {
		t.Fatal("decoder epoch leaked to the client")
	}
}

func (c *chain) post(t *testing.T, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.encSrv.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != fmt.Sprintf("ok %d", len(body)) {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if !bytes.Equal(c.origin.last(), body) {
		t.Fatalf("origin got %d bytes, want %d identical", len(c.origin.last()), len(body))
	}
}

func image(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

// conversation mimics an agent resending its full history every turn.
func conversation(turns int, images []string) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-5","input":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"role":"user","content":[{"type":"input_text","text":"turn %d please look at this"},{"type":"input_image","image_url":%q}]}`, i, images[i])
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func TestRepeatedHistoryIsSentOnce(t *testing.T) {
	c := newChain(t, 64<<20)
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10), image(300 << 10)}
	var total int
	for turn := 1; turn <= 4; turn++ {
		body := conversation(turn, images)
		total += len(body)
		c.post(t, body)
	}
	s := c.enc.snapshot()
	sent := s["sent_bytes"].(int64)
	if ratio := float64(total) / float64(sent); ratio < 2.2 {
		t.Fatalf("ratio %.2f too low (sent %d of %d)", ratio, sent, total)
	}
	if s["retries"].(int64) != 0 {
		t.Fatalf("unexpected retries: %v", s)
	}
	if got := c.origin.hdrs[len(c.origin.hdrs)-1].Get("X-Forwarded-For"); got != "203.0.113.7" {
		t.Fatalf("X-Forwarded-For %q, want the client's", got)
	}
	for _, h := range []string{hdrFrame, hdrLen, hdrEpoch} {
		if c.origin.hdrs[len(c.origin.hdrs)-1].Get(h) != "" {
			t.Fatalf("%s leaked to origin", h)
		}
	}
}

func TestEvictedChunksFallBackToPlain(t *testing.T) {
	// Decoder cache far smaller than the encoder believes: references miss.
	c := newChain(t, 64<<20)
	c.dec.cache = newLRU(256 << 10)
	images := []string{image(400 << 10), image(400 << 10)}
	c.post(t, conversation(1, images))
	c.post(t, conversation(2, images))
	if c.enc.snapshot()["retries"].(int64) == 0 {
		t.Fatal("expected a plain resend after a miss")
	}
	c.post(t, conversation(2, images))
}

func TestDecoderRestartResetsIndex(t *testing.T) {
	c := newChain(t, 64<<20)
	images := []string{image(300 << 10), image(300 << 10)}
	c.post(t, conversation(1, images))
	// Swap in a fresh decoder (new epoch, empty cache) behind the same URL.
	fresh := newDecoder(direct(t, c.origSrv.URL), 64<<20, 64<<20)
	c.decSrv.Config.Handler = fresh
	c.post(t, conversation(2, images))
	if c.enc.snapshot()["resets"].(int64) == 0 {
		t.Fatal("encoder did not notice the new decoder")
	}
	c.post(t, conversation(2, images))
	if fresh.snapshot()["framed"].(int64) == 0 {
		t.Fatal("fresh decoder never received a frame")
	}
}

func TestNoDecoderMeansNoFrames(t *testing.T) {
	o := &origin{}
	srv := httptest.NewServer(o)
	defer srv.Close()
	enc := newEncoder(direct(t, srv.URL), 64<<20, 64<<20)
	encSrv := httptest.NewServer(enc)
	defer encSrv.Close()
	body := conversation(2, []string{image(200 << 10), image(200 << 10)})
	for i := 0; i < 2; i++ {
		resp, err := http.Post(encSrv.URL+"/v1/responses", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !bytes.Equal(o.last(), body) {
			t.Fatal("origin without decoder must see the plain body")
		}
	}
}

func TestStreamingIsNotBuffered(t *testing.T) {
	c := newChain(t, 64<<20)
	body := conversation(1, []string{image(100 << 10)})
	start := time.Now()
	resp, err := http.Post(c.encSrv.URL+"/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "data: first\n" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("first event %q after %v", line, time.Since(start))
	}
}

func TestChunkingIsShiftResistant(t *testing.T) {
	data := []byte(image(1 << 20))
	before := map[digest]bool{}
	for _, p := range chunks(data) {
		before[sha(p)] = true
	}
	shifted := append([]byte("some new prefix text "), data...)
	var reused, total int
	for _, p := range chunks(shifted) {
		total += len(p)
		if before[sha(p)] {
			reused += len(p)
		}
	}
	if float64(reused)/float64(total) < 0.95 {
		t.Fatalf("only %d of %d bytes reused after a prefix insert", reused, total)
	}
}

func TestDecoderErrorsKeepFraming(t *testing.T) {
	c := newChain(t, 64<<20)
	// A malformed framed request answered 400 by the decoder still carries the epoch.
	req, _ := http.NewRequest(http.MethodPost, c.decSrv.URL+"/v1/responses", strings.NewReader("junk"))
	req.Header.Set(hdrFrame, "1")
	req.Header.Set(hdrLen, "4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(hdrEpoch) == "" {
		t.Fatalf("decoder rejection: status %d epoch %q", resp.StatusCode, resp.Header.Get(hdrEpoch))
	}
	images := []string{image(200 << 10), image(200 << 10)}
	c.post(t, conversation(1, images))
	c.post(t, conversation(2, images))
	if c.enc.snapshot()["framed"].(int64) != 2 {
		t.Fatalf("framing stopped: %v", c.enc.snapshot())
	}
}

func TestEpochSurvivesAnOddUnmarkedResponse(t *testing.T) {
	c := newChain(t, 64<<20)
	resp := &http.Response{StatusCode: 502, Header: http.Header{}, Request: httptest.NewRequest("GET", "/x", nil)}
	c.enc.proxy.ModifyResponse(resp)
	if c.enc.epoch.Load().(string) == "" {
		t.Fatal("one unmarked response must not pause framing")
	}
	for i := 0; i < unmarkedLimit; i++ {
		c.enc.proxy.ModifyResponse(resp)
	}
	if c.enc.epoch.Load().(string) != "" {
		t.Fatal("a run of unmarked responses must pause framing")
	}
	c.get(t, "/health")
	if c.enc.epoch.Load().(string) == "" || c.enc.snapshot()["resets"].(int64) != 0 {
		t.Fatalf("same decoder came back: want framing resumed without reset, got %v", c.enc.snapshot())
	}
}

// strictOrigin rejects anything that is not JSON, like Sub2API does with a frame.
type strictOrigin struct{ origin }

func (o *strictOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.bodies = append(o.bodies, body)
	o.mu.Unlock()
	if bytes.HasPrefix(body, []byte(frameMagic)) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "ok %d", len(body))
}

func TestFrameMisdeliveredToOriginIsResentPlain(t *testing.T) {
	o := &strictOrigin{}
	srv := httptest.NewServer(o)
	defer srv.Close()
	enc := newEncoder(direct(t, srv.URL), 64<<20, 64<<20)
	enc.learnEpoch("stale-decoder") // believes a decoder is in the path
	encSrv := httptest.NewServer(enc)
	defer encSrv.Close()
	body := conversation(1, []string{image(200 << 10)})
	resp, err := http.Post(encSrv.URL+"/v1/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != fmt.Sprintf("ok %d", len(body)) {
		t.Fatalf("client saw %d %q", resp.StatusCode, got)
	}
	if !bytes.Equal(o.last(), body) {
		t.Fatal("origin did not receive the original body on the resend")
	}
	s := enc.snapshot()
	if s["misdelivered"].(int64) != 1 || s["epoch"].(string) != "" {
		t.Fatalf("want one misdelivery and framing paused, got %v", s)
	}
	// Paused: the next request goes plain without a round trip of garbage.
	before := len(o.bodies)
	resp, _ = http.Post(encSrv.URL+"/v1/responses", "application/json", bytes.NewReader(body))
	resp.Body.Close()
	if len(o.bodies) != before+1 || !bytes.Equal(o.last(), body) {
		t.Fatal("paused encoder must send exactly one plain request")
	}
}

func TestFrameAcceptedByOriginIsNotResent(t *testing.T) {
	o := &origin{} // accepts anything with 200
	srv := httptest.NewServer(o)
	defer srv.Close()
	enc := newEncoder(direct(t, srv.URL), 64<<20, 64<<20)
	enc.learnEpoch("stale-decoder")
	encSrv := httptest.NewServer(enc)
	defer encSrv.Close()
	body := conversation(1, []string{image(200 << 10)})
	resp, err := http.Post(encSrv.URL+"/v1/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(o.bodies) != 1 {
		t.Fatalf("a 2xx must not be retried (would run twice); origin saw %d requests", len(o.bodies))
	}
	if enc.snapshot()["epoch"].(string) != "" {
		t.Fatal("framing must pause after an unmarked answer to a frame")
	}
}

// Relaying an upstream 1xx clears the decoder's response headers; the epoch
// must still reach the encoder or framing pauses.
func TestInterimResponsesKeepFraming(t *testing.T) {
	c := newChain(t, 64<<20)
	body := conversation(1, []string{image(200 << 10)})
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, c.encSrv.URL+"/hints", bytes.NewReader(body))
		req.Header.Set("Expect", "100-continue")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(c.origin.last(), body) {
			t.Fatalf("status %d, origin got %d of %d bytes", resp.StatusCode, len(c.origin.last()), len(body))
		}
	}
	if s := c.enc.snapshot(); s["framed"].(int64) != 2 || s["misdelivered"].(int64) != 0 || s["epoch"].(string) == "" {
		t.Fatalf("framing disturbed by a 1xx: %v", s)
	}
	if got := c.origin.hdrs[len(c.origin.hdrs)-1].Get("Expect"); got != "" {
		t.Fatalf("Expect %q forwarded to the origin", got)
	}
}

// socks5 is a no-auth SOCKS5 CONNECT proxy for IPv4 targets, standing in for
// an anytls client.
func socksProxy(t *testing.T) (*url.URL, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	tunnels := new(atomic.Int64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var b [256]byte
				if _, err := io.ReadFull(c, b[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, b[:b[1]]); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, b[:10]); err != nil || b[1] != 1 || b[3] != 1 {
					return
				}
				up, err := net.Dial("tcp", fmt.Sprintf("%d.%d.%d.%d:%d", b[4], b[5], b[6], b[7], int(b[8])<<8|int(b[9])))
				if err != nil {
					return
				}
				defer up.Close()
				tunnels.Add(1)
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return &url.URL{Scheme: "socks5", Host: ln.Addr().String()}, tunnels
}

// The LAN encoder reaches a decoder on a VPS through an anytls client, and the
// decoder calls a public HTTPS API under that API's own name.
func TestEncoderViaProxyToPublicOrigin(t *testing.T) {
	c := &chain{origin: &origin{}}
	c.origSrv = httptest.NewServer(c.origin)
	c.dec = newDecoder(route{url: mustURL(t, c.origSrv.URL), upstreamHost: true}, 64<<20, 64<<20)
	c.decSrv = httptest.NewServer(c.dec)
	via, tunnels := socksProxy(t)
	c.enc = newEncoder(route{url: mustURL(t, c.decSrv.URL), via: via}, 60<<20, 64<<20)
	c.encSrv = httptest.NewServer(c.enc)
	t.Cleanup(func() { c.encSrv.Close(); c.decSrv.Close(); c.origSrv.Close() })
	c.get(t, "/health")

	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	for turn := 1; turn <= 3; turn++ {
		c.post(t, conversation(turn, images))
	}
	if tunnels.Load() == 0 {
		t.Fatal("encoder did not use -via")
	}
	if got := c.dec.snapshot()["framed"].(int64); got != 3 {
		t.Fatalf("decoder rebuilt %d frames, want 3", got)
	}
	want := mustURL(t, c.origSrv.URL).Host
	for _, h := range c.origin.hosts {
		if h != want {
			t.Fatalf("origin saw Host %q, want %q", h, want)
		}
	}
}

// A frame lost on the way to the decoder must not leave its chunks in the
// index, or the next turn names them and pays for a plain resend.
func TestLostFrameCostsNoResend(t *testing.T) {
	c := newChain(t, 64<<20)
	var lose atomic.Bool
	lose.Store(true)
	c.decSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lose.Load() {
			c.dec.ServeHTTP(w, r)
			return
		}
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	images := []string{image(300 << 10), image(300 << 10)}
	resp, err := http.Post(c.encSrv.URL+"/v1/responses", "application/json", bytes.NewReader(conversation(1, images)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("lost frame answered %d, want 502", resp.StatusCode)
	}
	if entries, _ := c.enc.known.stats(); entries != 0 {
		t.Fatalf("index kept %d chunks of the lost frame", entries)
	}
	lose.Store(false)
	c.get(t, "/health")
	c.post(t, conversation(2, images))
	if retries := c.enc.snapshot()["retries"].(int64); retries != 0 {
		t.Fatalf("encoder resent %d times after a lost frame", retries)
	}
}

// An answered frame's chunks are held: the next turn refers to them.
func TestAnsweredFramesAreReferenced(t *testing.T) {
	c := newChain(t, 64<<20)
	images := []string{image(300 << 10), image(300 << 10)}
	c.post(t, conversation(1, images))
	c.post(t, conversation(2, images))
	s := c.enc.snapshot()
	if s["retries"].(int64) != 0 || s["ref_bytes"].(int64) < 300<<10 {
		t.Fatalf("second turn did not reference the first: %v", s)
	}
}
