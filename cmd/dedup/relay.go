package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	hdrFrame   = "X-Edge-Dedup"         // "1" on a framed request
	hdrLen     = "X-Edge-Dedup-Len"     // original body length
	hdrEpoch   = "X-Edge-Dedup-Epoch"   // decoder instance; on every decoder response
	hdrMissing = "X-Edge-Dedup-Missing" // 409: count of unknown references (hashes in body)
	hdrReset   = "X-Edge-Dedup-Reset"   // 409: decoder restarted, drop the whole index
	hdrH2C     = "X-Edge-Dedup-H2c"     // "1" on /health: the decoder also speaks HTTP/2 without TLS
)

// dedupHost names the decoder itself; anytls servers route streams to it to
// their local decoder.
const dedupHost = "dedup.anytls.arpa"

// route is where a relay forwards requests.
type route struct {
	// url is nil for the HTTPS origin named by each request's Host.
	url *url.URL
	// table, if set instead of url, picks a plain HTTP service by Host.
	table *routeTable
	// via is an HTTP or SOCKS5 proxy in front of url, e.g. an anytls client.
	via *url.URL
	// upstreamHost sends url's host instead of the client's Host, as a public
	// HTTPS origin expects.
	upstreamHost bool
	// tls and resolve point origin requests at a test origin.
	tls     *tls.Config
	resolve map[string]string
}

func newProxy(up route) *httputil.ReverseProxy {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
	if up.via != nil {
		transport.Proxy = http.ProxyURL(up.via)
	}
	if len(up.resolve) > 0 {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if to, ok := up.resolve[addr]; ok {
				addr = to
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	var rt http.RoundTripper = transport
	if up.url == nil && up.table == nil {
		// Like Sub2API's OpenAI transport: HTTP/2 with ping health checks.
		transport.ForceAttemptHTTP2 = true
		transport.TLSClientConfig = up.tls
		transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 15 * time.Second}
		rt = gzipLast(transport)
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			if up.table != nil {
				// The decoder only forwards hosts it found a service for.
				r.SetURL(r.In.Context().Value(serviceCtxKey{}).(*url.URL))
				r.Out.Host = r.In.Host
			} else if up.url == nil {
				r.Out.URL.Scheme = "https"
				r.Out.URL.Host = r.In.Host
				r.Out.Host = r.In.Host
			} else {
				r.SetURL(up.url)
				if !up.upstreamHost {
					r.Out.Host = r.In.Host
				}
			}
			// Reading the body already answered 100-continue on this hop.
			r.Out.Header.Del("Expect")
			// Rewrite drops forwarding headers; keep what Caddy set.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v, ok := r.In.Header[h]; ok {
					r.Out.Header[h] = v
				}
			}
		},
		Transport:     rt,
		FlushInterval: -1,
		ErrorLog:      log.Default(),
	}
}

// gzipLast sends a request carrying Go's own "Accept-Encoding: gzip" through
// a transport that adds the header itself, because Go writes it after every
// other header and an origin can see the order. That transport also
// decompresses the response, which the client then gets as identity.
func gzipLast(plain *http.Transport) http.RoundTripper {
	compress := plain.Clone()
	compress.DisableCompression = false
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if ae := req.Header["Accept-Encoding"]; len(ae) == 1 && ae[0] == "gzip" &&
			req.Method != http.MethodHead && req.Header.Get("Range") == "" {
			req = req.Clone(req.Context())
			req.Header.Del("Accept-Encoding")
			return compress.RoundTrip(req)
		}
		return plain.RoundTrip(req)
	})
}

type counters struct {
	framed, plain, retries, resets atomic.Int64
	misdelivered, fallbacks, busy  atomic.Int64
	original, sent, refBytes       atomic.Int64
}

// ---- encoder (egress side) ----------------------------------------------

type encoder struct {
	known     *lru
	maxBody   int64
	epoch     atomic.Value // string; empty while no decoder is answering
	lastEpoch atomic.Value // string; the decoder the index belongs to
	unmarked  atomic.Int64 // consecutive responses without a decoder epoch
	proxy     *httputil.ReverseProxy
	base      http.RoundTripper
	// h2 reaches the decoder over HTTP/2 without TLS, pinged so that a path
	// that stops delivering fails the requests on it; nil where the path to
	// the decoder cannot carry it. useH2 is set while the decoder offers it.
	h2    http.RoundTripper
	useH2 atomic.Bool
	stats counters

	// inflight may be shared by the encoders of several exits.
	inflight *budget
	// inflightWait is how long a request may wait for inflight memory before
	// it goes out plain; a plain resend of a large body costs far longer.
	inflightWait time.Duration
	// origin, if set, sends a framed request straight to its origin when the
	// decoder cannot be reached.
	origin   http.RoundTripper
	compress compression
}

// A path with no decoder (frpc pointed straight at the origin) answers without
// an epoch; after this many in a row the encoder stops framing.
const unmarkedLimit = 3

type frameCtxKey struct{}

// serviceCtxKey carries the service a routed decoder picked for a request.
type serviceCtxKey struct{}

type framedRequest struct {
	body    []byte
	lits    []digest
	held    *share
	known   *lru
	settled sync.Once
}

// settle ends the frame's claims on its literals, once: held if the decoder
// read the frame, else forgotten so that no later frame refers to them.
func (f *framedRequest) settle(read bool) {
	f.settled.Do(func() { f.known.settle(f.lits, read) })
}

// done drops the body once a response or a final error is in: nothing will
// be resent after that.
func (f *framedRequest) done() {
	f.body = nil
	f.held.free()
}

func newEncoder(upstream route, indexBytes, maxBody int64) *encoder {
	e := &encoder{known: newLRU(indexBytes), maxBody: maxBody, inflight: &budget{}}
	e.epoch.Store("")
	e.lastEpoch.Store("")
	e.proxy = newProxy(upstream)
	e.base = e.proxy.Transport
	if t, ok := e.base.(*http.Transport); ok && carriesH2C(upstream) {
		e.h2 = h2cTransport(t)
	}
	e.proxy.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return e.roundTrip(e.toDecoder(), req)
	})
	e.proxy.ModifyResponse = func(resp *http.Response) error {
		if epoch := resp.Header.Get(hdrEpoch); epoch != "" {
			e.unmarked.Store(0)
			e.learnEpoch(epoch)
		} else if e.unmarked.Add(1) >= unmarkedLimit && e.epoch.Swap("") != "" {
			log.Printf("%d responses without a decoder epoch (last %d %s %s); framing paused",
				unmarkedLimit, resp.StatusCode, resp.Request.Method, resp.Request.URL.Path)
		}
		for _, h := range []string{hdrEpoch, hdrMissing, hdrReset} {
			resp.Header.Del(h)
		}
		return nil
	}
	return e
}

// readBody reads exactly n bytes into a single allocation. Growing a slice
// instead copies multi-MiB buffers in non-preemptible memmoves; on small ARM
// routers the GC's preemption signals then slow each request by seconds.
func readBody(r io.Reader, n int64) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

// carriesH2C reports whether HTTP/2 without TLS can reach up's decoder: an
// HTTP proxy would get the requests themselves, not a tunnel.
func carriesH2C(up route) bool {
	return up.url != nil && up.url.Scheme == "http" &&
		(up.via == nil || up.via.Scheme == "socks5" || up.via.Scheme == "socks5h")
}

func h2cTransport(plain *http.Transport) *http.Transport {
	t := plain.Clone()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	t.Protocols = &protocols
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: decoderPing, PingTimeout: decoderPing}
	return t
}

// toDecoder is the transport for requests to the decoder.
func (e *encoder) toDecoder() http.RoundTripper {
	if e.h2 != nil && e.useH2.Load() {
		return e.h2
	}
	return e.base
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (e *encoder) learnEpoch(epoch string) {
	if epoch == "" {
		return
	}
	if e.epoch.Swap(epoch) == "" {
		log.Printf("decoder %s answering; framing on", epoch)
	}
	if old := e.lastEpoch.Swap(epoch); old != "" && old != epoch {
		// A different decoder instance: its cache is empty.
		e.known.reset()
		e.stats.resets.Add(1)
		log.Printf("decoder epoch %s -> %s; index reset", old, epoch)
	}
}

func (e *encoder) healthy() bool { return e.epoch.Load().(string) != "" }

func (e *encoder) pause(why string, err error) {
	if e.epoch.Swap("") != "" {
		log.Printf("%s: %v; framing paused", why, err)
	}
}

// probe asks the decoder for its epoch, through the same path as requests.
// A decoder that offers HTTP/2 is probed over it from then on; should that
// fail, the next probe goes over HTTP/1 again.
func (e *encoder) probe(ctx context.Context, decoder *url.URL) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, decoder.JoinPath("/health").String(), nil)
	req.Host = dedupHost
	resp, err := e.toDecoder().RoundTrip(req)
	if err != nil {
		e.offerH2(false)
		e.pause("decoder probe", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if epoch := resp.Header.Get(hdrEpoch); resp.StatusCode == http.StatusNoContent && epoch != "" {
		e.offerH2(resp.Header.Get(hdrH2C) == "1")
		e.learnEpoch(epoch)
		return
	}
	e.offerH2(false)
	e.pause("decoder probe", errStatus(resp.StatusCode))
}

func (e *encoder) offerH2(on bool) {
	if e.h2 != nil && e.useH2.Swap(on) != on && on {
		log.Printf("decoder speaks HTTP/2; frames share one pinged connection")
	}
}

type errStatus int

func (s errStatus) Error() string { return "status " + strconv.Itoa(int(s)) }

func (e *encoder) eligible(r *http.Request) bool {
	return e.healthy() &&
		(r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch) &&
		r.ContentLength > minChunk && r.ContentLength <= e.maxBody &&
		r.Header.Get("Content-Encoding") == "" && r.Header.Get("Upgrade") == ""
}

// budget bounds the bytes held by requests being framed or rebuilt.
type budget struct {
	limit int64 // 0 is no limit
	used  atomic.Int64

	mu    sync.Mutex
	freed chan struct{} // closed on the next release, while someone waits
}

// requestOverhead is what a request holds besides its body and frame: flate
// writers alone allocate 0.3-0.8 MiB each.
const requestOverhead = 2 << 20

func (b *budget) reserve(n int64) bool {
	if b.limit == 0 {
		return true
	}
	if b.used.Add(n) > b.limit {
		b.used.Add(-n)
		return false
	}
	return true
}

func (b *budget) release(n int64) {
	if b.limit != 0 {
		b.used.Add(-n)
		b.mu.Lock()
		if b.freed != nil {
			close(b.freed)
			b.freed = nil
		}
		b.mu.Unlock()
	}
}

// take reserves n bytes as a share, or returns nil when over the limit.
func (b *budget) take(n int64) *share {
	if !b.reserve(n) {
		return nil
	}
	return &share{b: b, n: n}
}

// takeWithin is take, waiting up to wait for other shares to shrink.
func (b *budget) takeWithin(ctx context.Context, n int64, wait time.Duration) *share {
	if s := b.take(n); s != nil || wait <= 0 || n > b.limit {
		return s
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		// Subscribe before retrying, so a release in between is not missed.
		b.mu.Lock()
		if b.freed == nil {
			b.freed = make(chan struct{})
		}
		freed := b.freed
		b.mu.Unlock()
		if s := b.take(n); s != nil {
			return s
		}
		select {
		case <-freed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// share is one request's part of a budget. It shrinks as its buffers become
// garbage; free may be called any number of times.
type share struct {
	mu sync.Mutex
	b  *budget
	n  int64
}

func (s *share) shrink(to int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if to < s.n {
		s.b.release(s.n - to)
		s.n = to
	}
}

func (s *share) free() { s.shrink(0) }

// releasingBody serves b once and drops it, calling done, when the transport
// has read it to the end or closed it. A streamed answer can last minutes;
// the request body is not needed for any of it.
type releasingBody struct {
	mu   sync.Mutex
	b    []byte
	done func()
}

func (r *releasingBody) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.b) == 0 {
		r.dropLocked()
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func (r *releasingBody) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked()
	return nil
}

func (r *releasingBody) dropLocked() {
	r.b = nil
	if r.done != nil {
		r.done()
		r.done = nil
	}
}

func (e *encoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !e.eligible(r) {
		e.stats.plain.Add(1)
		e.proxy.ServeHTTP(w, r)
		return
	}
	// The body and a frame, which is at most about as large.
	held := e.inflight.takeWithin(r.Context(), 2*r.ContentLength+requestOverhead, e.inflightWait)
	if held == nil {
		e.stats.busy.Add(1)
		e.stats.plain.Add(1)
		e.proxy.ServeHTTP(w, r)
		return
	}
	defer held.free()
	body, err := readBody(r.Body, r.ContentLength)
	if err != nil {
		http.Error(w, "request body read failed", http.StatusBadRequest)
		return
	}
	res, err := encodeFrame(body, e.known, e.compress)
	if err != nil {
		e.known.settle(res.litHashes, false)
		log.Printf("encode: %v; sending plain", err)
		e.stats.plain.Add(1)
		r.Body = io.NopCloser(bytes.NewReader(body))
		e.proxy.ServeHTTP(w, r)
		return
	}
	// Only the body, kept for resends, and the frame are still referenced.
	held.shrink(int64(len(body) + len(res.frame)))
	e.stats.framed.Add(1)
	e.stats.original.Add(int64(len(body)))
	e.stats.sent.Add(int64(len(res.frame)))
	e.stats.refBytes.Add(int64(res.refBytes))
	framed := &framedRequest{body: body, lits: res.litHashes, held: held, known: e.known}
	// Unanswered claims would keep their chunks from being referenced.
	defer framed.settle(false)
	r = r.WithContext(context.WithValue(r.Context(), frameCtxKey{}, framed))
	r.Body = &releasingBody{b: res.frame, done: func() { held.shrink(int64(len(body))) }}
	r.ContentLength = int64(len(res.frame))
	r.Header.Set(hdrFrame, "1")
	r.Header.Set(hdrLen, strconv.Itoa(len(body)))
	r.Header.Set(hdrEpoch, e.epoch.Load().(string))
	e.proxy.ServeHTTP(w, r)
}

func (e *encoder) roundTrip(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	framed, _ := req.Context().Value(frameCtxKey{}).(*framedRequest)
	resp, err := e.send(base, req, framed)
	if framed != nil {
		framed.done()
	}
	return resp, err
}

func (e *encoder) send(base http.RoundTripper, req *http.Request, framed *framedRequest) (*http.Response, error) {
	resp, err := base.RoundTrip(req)
	if err != nil {
		e.pause("decoder request", err)
		if framed != nil {
			// The decoder may never have read the frame. Should it have, the
			// chunks only travel once more as literals.
			framed.settle(false)
		}
		if framed != nil && e.origin != nil && req.Context().Err() == nil {
			// The body is still here; without a decoder only the origin is left.
			e.stats.fallbacks.Add(1)
			direct := plainCopy(req, framed)
			direct.URL.Scheme, direct.URL.Host = "https", req.Host
			return e.origin.RoundTrip(direct)
		}
		return resp, err
	}
	if framed == nil {
		return resp, nil
	}
	if resp.Header.Get(hdrEpoch) == "" {
		// Something other than a decoder read the frame, e.g. a pooled connection
		// that still leads straight to the origin after a frpc change.
		return e.misdelivered(base, req, framed, resp)
	}
	if resp.StatusCode != http.StatusConflict || resp.Header.Get(hdrMissing) == "" {
		framed.settle(true)
		return resp, nil
	}
	// The decoder lacks some chunks: forget them and resend the original body.
	// It cached the frame's literals all the same, unless it was too busy to
	// read the frame at all.
	framed.settle(resp.Header.Get(hdrMissing) != "0")
	hashes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.Header.Get(hdrReset) != "" {
		e.known.reset()
		e.stats.resets.Add(1)
	} else if resp.Header.Get(hdrMissing) != "0" {
		for i := 0; i+len(digest{}) <= len(hashes); i += len(digest{}) {
			var d digest
			copy(d[:], hashes[i:])
			e.known.remove(d)
		}
	}
	e.learnEpoch(resp.Header.Get(hdrEpoch))
	e.stats.retries.Add(1)
	return base.RoundTrip(plainCopy(req, framed))
}

// misdelivered handles a frame answered without a decoder epoch. A 4xx means
// the reader rejected it unprocessed, so the original is resent; anything else
// is returned as is, because resending could run the request twice.
func (e *encoder) misdelivered(base http.RoundTripper, req *http.Request, framed *framedRequest, resp *http.Response) (*http.Response, error) {
	framed.settle(false)
	if e.epoch.Swap("") != "" {
		log.Printf("frame %s %s answered %d without a decoder epoch; framing paused", req.Method, req.URL.Path, resp.StatusCode)
	}
	if t, ok := base.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return resp, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	e.stats.misdelivered.Add(1)
	return base.RoundTrip(plainCopy(req, framed))
}

func plainCopy(req *http.Request, framed *framedRequest) *http.Request {
	plain := req.Clone(req.Context())
	plain.Body = &releasingBody{b: framed.body}
	plain.ContentLength = int64(len(framed.body))
	plain.GetBody = nil
	for _, h := range []string{hdrFrame, hdrLen, hdrEpoch} {
		plain.Header.Del(h)
	}
	return plain
}

func (e *encoder) snapshot() map[string]any {
	entries, used := e.known.stats()
	orig, sent := e.stats.original.Load(), e.stats.sent.Load()
	ratio := 0.0
	if sent > 0 {
		ratio = float64(orig) / float64(sent)
	}
	return map[string]any{
		"role": "encode", "epoch": e.epoch.Load(), "framed": e.stats.framed.Load(), "plain": e.stats.plain.Load(),
		"retries": e.stats.retries.Load(), "resets": e.stats.resets.Load(), "misdelivered": e.stats.misdelivered.Load(),
		"fallbacks": e.stats.fallbacks.Load(), "busy": e.stats.busy.Load(), "inflight_bytes": e.inflight.used.Load(),
		"original_bytes": orig, "sent_bytes": sent, "ref_bytes": e.stats.refBytes.Load(), "ratio": ratio,
		"index_entries": entries, "index_bytes": used,
	}
}

// ---- decoder (LAN side) --------------------------------------------------

type decoder struct {
	cache    *lru
	inflight budget
	maxFrame int64
	maxBody  int64
	epoch    string
	proxy    *httputil.ReverseProxy
	stats    counters
	misses   atomic.Int64
	// hosts, if set, are the only origins requests may name.
	hosts domainList
	// routes, if set, map each Host to its service; others are refused.
	routes *routeTable
	// h2c is set once a server that also speaks HTTP/2 without TLS serves it.
	h2c bool
}

// decoderPing is how long either end of a decoder connection waits for a
// frame before it pings, and then for the answer. An exit path that stops
// delivering, while the TCP connection under it still retransmits for a
// quarter of an hour, is found within about twice this.
var decoderPing = 15 * time.Second

// newDecoderServer serves d over HTTP/1 and over HTTP/2 without TLS; fronts
// that saw hdrH2C send all their frames over one pinged connection.
func newDecoderServer(d *decoder) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	d.h2c = true
	return &http.Server{
		Handler:           d,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		Protocols:         &protocols,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: decoderPing,
			PingTimeout:     decoderPing,
			// The default 1 MiB per connection would cap the frames sharing
			// one at about 1 MiB per round trip.
			MaxReceiveBufferPerConnection: 16 << 20,
			MaxReceiveBufferPerStream:     8 << 20,
		},
	}
}

func newDecoder(upstream route, cacheBytes, maxBody int64) *decoder {
	var b [8]byte
	rand.Read(b[:])
	d := &decoder{cache: newLRU(cacheBytes), maxBody: maxBody, maxFrame: maxBody + maxBody/8 + 4096, epoch: hex.EncodeToString(b[:])}
	d.routes = upstream.table
	d.proxy = newProxy(upstream)
	d.proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set(hdrEpoch, d.epoch)
		return nil
	}
	d.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set(hdrEpoch, d.epoch)
		w.WriteHeader(http.StatusBadGateway)
	}
	return d
}

// forward proxies r. Proxied answers get the epoch from ModifyResponse or
// ErrorHandler, because relaying an upstream 1xx clears w.Header().
func (d *decoder) forward(w http.ResponseWriter, r *http.Request) {
	w.Header().Del(hdrEpoch)
	d.proxy.ServeHTTP(w, r)
}

func (d *decoder) reject(w http.ResponseWriter, r *http.Request, msg string, err error) {
	log.Printf("reject %s %s: %s: %v", r.Method, r.URL.Path, msg, err)
	http.Error(w, msg, http.StatusBadRequest)
}

func (d *decoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every answer, including errors, tells the encoder a decoder is here.
	w.Header().Set(hdrEpoch, d.epoch)
	if host := hostname(r.Host); host == dedupHost {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		if d.h2c {
			w.Header().Set(hdrH2C, "1")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	} else if d.hosts != nil && !d.hosts.match(host) {
		log.Printf("refused %s %s for host %q", r.Method, r.URL.Path, r.Host)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	} else if d.routes != nil {
		service := d.routes.lookup(r.Host)
		if service == nil {
			log.Printf("refused %s %s for host %q: no route", r.Method, r.URL.Path, r.Host)
			http.Error(w, "host not routed", http.StatusForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), serviceCtxKey{}, service))
	}
	if r.Header.Get(hdrFrame) != "1" {
		d.stats.plain.Add(1)
		d.forward(w, r)
		return
	}
	want, err := strconv.Atoi(r.Header.Get(hdrLen))
	if err != nil || want <= 0 || int64(want) > d.maxBody {
		d.reject(w, r, "bad dedup length", err)
		return
	}
	if r.ContentLength <= 0 || r.ContentLength > d.maxFrame {
		d.reject(w, r, "bad dedup frame length", nil)
		return
	}
	held := d.inflight.take(r.ContentLength + int64(want) + requestOverhead)
	if held == nil {
		// Missing nothing: the encoder resends the original, which streams
		// through unbuffered. Draining keeps the connection usable.
		d.stats.busy.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set(hdrMissing, "0")
		w.WriteHeader(http.StatusConflict)
		return
	}
	defer held.free()
	frame, err := readBody(r.Body, r.ContentLength)
	if err != nil {
		d.reject(w, r, "dedup frame read failed", err)
		return
	}
	body, missing, err := decodeFrame(frame, want, d.cache)
	if err != nil {
		d.reject(w, r, "bad dedup frame", err)
		return
	}
	// The frame is garbage; the rebuilt body is held until sent upstream.
	held.shrink(int64(want))
	if len(missing) > 0 {
		d.misses.Add(int64(len(missing)))
		w.Header().Set(hdrMissing, strconv.Itoa(len(missing)))
		if r.Header.Get(hdrEpoch) != d.epoch {
			w.Header().Set(hdrReset, "1")
		}
		w.WriteHeader(http.StatusConflict)
		for _, m := range missing {
			w.Write(m[:])
		}
		return
	}
	d.stats.framed.Add(1)
	d.stats.original.Add(int64(len(body)))
	d.stats.sent.Add(int64(len(frame)))
	r.Body = &releasingBody{b: body, done: held.free}
	r.ContentLength = int64(len(body))
	for _, h := range []string{hdrFrame, hdrLen, hdrEpoch} {
		r.Header.Del(h)
	}
	d.forward(w, r)
}

func (d *decoder) snapshot() map[string]any {
	entries, used := d.cache.stats()
	return map[string]any{
		"role": "decode", "epoch": d.epoch, "framed": d.stats.framed.Load(), "plain": d.stats.plain.Load(),
		"missing_refs": d.misses.Load(), "busy": d.stats.busy.Load(), "inflight_bytes": d.inflight.used.Load(),
		"original_bytes": d.stats.original.Load(), "frame_bytes": d.stats.sent.Load(),
		"cache_entries": entries, "cache_bytes": used,
	}
}
