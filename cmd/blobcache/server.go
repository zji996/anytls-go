package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hitBufferLimit is how much plaintext a hit decrypts before it answers: a
// damaged file is then still turned into a pass-through with nothing sent.
// Longer ranges stream and, if a later chunk fails, abort the connection.
const hitBufferLimit = 4 << 20

// passthroughHeaders are the responses the contract lists; the origin's others
// are not forwarded.
var passthroughHeaders = []string{
	"Content-Type",
	"Content-Disposition",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"Cache-Control",
	"Content-Security-Policy",
	"X-Content-Type-Options",
}

type counters struct {
	hits        atomic.Int64
	misses      atomic.Int64
	passthrough atomic.Int64
	originBytes atomic.Int64
	servedBytes atomic.Int64
}

// server serves GET /b/<token>: from the cache when it can, and from the
// origin, streaming into the cache, when it cannot.
type server struct {
	origin     *url.URL
	credential string
	signKey    []byte
	cache      *cache
	maxObject  int64
	client     *http.Client
	counters   counters
	started    time.Time

	base   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	inflight map[string]*fill
}

// fill is one in-progress origin read of an object; everyone who misses while
// it runs waits for it instead of fetching a second copy.
type fill struct {
	done chan struct{}
}

func newServer(origin *url.URL, credential string, signKey []byte, c *cache, maxObject int64) *server {
	base, cancel := context.WithCancel(context.Background())
	s := &server{
		origin:     origin,
		credential: credential,
		signKey:    signKey,
		cache:      c,
		maxObject:  maxObject,
		counters:   counters{},
		started:    time.Now(),
		base:       base,
		cancel:     cancel,
		inflight:   make(map[string]*fill),
		client: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   16,
			DisableCompression:    true,
		}},
	}
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, ok := strings.CutPrefix(r.URL.Path, "/b/")
	if !ok || raw == "" || strings.Contains(raw, "/") {
		http.NotFound(w, r)
		return
	}
	tok, err := parseToken(raw, s.signKey, time.Now())
	if err != nil {
		log.Printf("blobcache: rejected %s %s", r.Method, pathLabel(r.URL.Path))
		http.NotFound(w, r)
		return
	}
	if s.cache.locked() || tok.Size > s.maxObject {
		s.counters.passthrough.Add(1)
		s.relay(w, r, raw)
		return
	}
	if s.serveHit(w, r, tok) {
		return
	}
	s.counters.misses.Add(1)
	if r.Header.Get("Range") != "" {
		// A range miss is answered by the origin; the whole object is still
		// worth keeping, so fetch it once in the background.
		s.counters.passthrough.Add(1)
		log.Printf("blobcache: range miss obj=%s size=%d", shortObj(tok.Obj), tok.Size)
		s.relay(w, r, raw)
		s.fillBackground(raw, r.Host, tok)
		return
	}
	log.Printf("blobcache: miss obj=%s size=%d", shortObj(tok.Obj), tok.Size)
	s.fillAndServe(w, r, raw, tok)
}

// fillAndServe is a miss without Range: the first request reads the origin once
// while writing the cache, the others wait and are served from it.
func (s *server) fillAndServe(w http.ResponseWriter, r *http.Request, raw string, tok *tokenPayload) {
	s.mu.Lock()
	if f, busy := s.inflight[tok.Obj]; busy {
		s.mu.Unlock()
		select {
		case <-f.done:
		case <-r.Context().Done():
			return
		}
		if !s.serveHit(w, r, tok) {
			s.counters.passthrough.Add(1)
			s.relay(w, r, raw)
		}
		return
	}
	f := &fill{done: make(chan struct{})}
	s.inflight[tok.Obj] = f
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, tok.Obj)
		s.mu.Unlock()
		close(f.done)
	}()
	if err := s.fetch(w, raw, r.Host, tok, r.Method != http.MethodHead); err != nil {
		log.Printf("blobcache: fill obj=%s: %s", shortObj(tok.Obj), reason(err))
	}
}

// fillBackground caches a range miss in the background, once per object.
func (s *server) fillBackground(raw, host string, tok *tokenPayload) {
	s.mu.Lock()
	if _, busy := s.inflight[tok.Obj]; busy {
		s.mu.Unlock()
		return
	}
	f := &fill{done: make(chan struct{})}
	s.inflight[tok.Obj] = f
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, tok.Obj)
			s.mu.Unlock()
			close(f.done)
		}()
		if err := s.fetch(nil, raw, host, tok, false); err != nil {
			log.Printf("blobcache: background fill obj=%s: %s", shortObj(tok.Obj), reason(err))
		}
	}()
}

// fetch reads the whole object from the origin, writes it to the client when w
// is not nil, and caches it. It does not stop when the client goes away: only a
// cache that refuses the object ends the copy early.
func (s *server) fetch(w http.ResponseWriter, raw, host string, tok *tokenPayload, live bool) error {
	// Detached from the client's context: a client that disconnects mid-body
	// must not leave a half-written object behind.
	ctx, cancel := context.WithCancel(s.base)
	defer cancel()
	req, err := s.originRequest(ctx, host, raw, "")
	if err != nil {
		if w != nil {
			s.relayError(w)
		}
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if w != nil {
			s.relayError(w)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || (resp.ContentLength >= 0 && resp.ContentLength != tok.Size) {
		if w != nil {
			s.counters.passthrough.Add(1)
			s.copyUpstream(w, false, resp)
		}
		return errors.New("origin did not return the object")
	}
	m := metaFromResponse(resp, tok)
	writer, werr := s.cache.create(tok.Obj, m)
	if werr != nil {
		log.Printf("blobcache: not caching obj=%s: %s", shortObj(tok.Obj), reason(werr))
	}
	var body io.Writer
	if w != nil {
		setHitHeaders(w.Header(), m, tok, tok.Size)
		w.WriteHeader(http.StatusOK)
		// The client gets its headers while the body is still arriving.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if live {
			body = w
		}
	}
	stream := &streamer{s: s, writer: writer, client: body}
	total, rerr := stream.copy(resp.Body)
	if writer != nil && !stream.dropped {
		if rerr == nil && total == tok.Size {
			if err := writer.Commit(); err != nil {
				log.Printf("blobcache: commit obj=%s: %s", shortObj(tok.Obj), reason(err))
			}
		} else {
			writer.Abort()
		}
	}
	if rerr != nil {
		if w != nil && live {
			// The response promised more than the origin delivered.
			panic(http.ErrAbortHandler)
		}
		return rerr
	}
	if total != tok.Size {
		if w != nil && live {
			panic(http.ErrAbortHandler)
		}
		return errors.New("origin body shorter than the token size")
	}
	return nil
}

// streamer copies an origin body to the cache writer (until it fails) and to
// the client (until it fails); neither staying alive depends on the other.
type streamer struct {
	s       *server
	writer  *objectWriter
	client  io.Writer
	dropped bool // the cache refused the object; the client keeps streaming
}

func (st *streamer) copy(body io.Reader) (int64, error) {
	buf := make([]byte, chunkSize)
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			st.s.counters.originBytes.Add(int64(n))
			total += int64(n)
			if st.writer != nil {
				if err := st.writer.Write(buf[:n]); err != nil {
					log.Printf("blobcache: cache write: %s", reason(err))
					st.writer.Abort()
					st.writer = nil
					st.dropped = true
				}
			}
			if st.client != nil {
				got, err := st.client.Write(buf[:n])
				st.s.counters.servedBytes.Add(int64(got))
				if err != nil {
					st.client = nil
				}
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// serveHit answers from the cache and reports whether it could. Anything it
// cannot decrypt is deleted and counted as a miss.
func (s *server) serveHit(w http.ResponseWriter, r *http.Request, tok *tokenPayload) bool {
	o, err := s.cache.open(tok.Obj)
	if err != nil {
		if !errors.Is(err, errLocked) && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("blobcache: cache read obj=%s: %s", shortObj(tok.Obj), reason(err))
		}
		return false
	}
	defer o.Close()
	size := o.Size()
	rng := parseRange(r.Header.Get("Range"), size)
	if rng.ignore {
		rng = byteRange{lo: 0, hi: size - 1}
	}
	if rng.unsatisfiable {
		h := w.Header()
		setHitHeaders(h, o.meta, tok, 0)
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		s.counters.hits.Add(1)
		return true
	}
	length := rng.hi - rng.lo + 1
	setHitHeaders(w.Header(), o.meta, tok, length)
	if rng.partial {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(rng.lo, 10)+"-"+strconv.FormatInt(rng.hi, 10)+"/"+strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodHead || length == 0 {
		w.WriteHeader(statusFor(rng))
		s.counters.hits.Add(1)
		return true
	}
	if length <= hitBufferLimit {
		buf := make([]byte, length)
		if err := o.readAt(rng.lo, buf); err != nil {
			log.Printf("blobcache: decrypt obj=%s: %s; dropping", shortObj(tok.Obj), reason(err))
			s.cache.dropName(o.name)
			return false
		}
		w.WriteHeader(statusFor(rng))
		got, _ := w.Write(buf)
		s.counters.servedBytes.Add(int64(got))
		s.counters.hits.Add(1)
		return true
	}
	w.WriteHeader(statusFor(rng))
	got, err := o.copyRange(w, rng.lo, rng.hi)
	s.counters.servedBytes.Add(got)
	if err != nil {
		log.Printf("blobcache: decrypt obj=%s: %s; dropping", shortObj(tok.Obj), reason(err))
		s.cache.dropName(o.name)
		panic(http.ErrAbortHandler)
	}
	s.counters.hits.Add(1)
	return true
}

func statusFor(r byteRange) int {
	if r.partial {
		return http.StatusPartialContent
	}
	return http.StatusOK
}

// setHitHeaders writes the headers every cached answer carries. length is the
// body length; Content-Disposition comes from the token, so the same object
// can be previewed and downloaded.
func setHitHeaders(h http.Header, m meta, tok *tokenPayload, length int64) {
	if m.Type != "" {
		h.Set("Content-Type", m.Type)
	}
	if m.CSP != "" {
		h.Set("Content-Security-Policy", m.CSP)
	}
	if m.Nosniff {
		h.Set("X-Content-Type-Options", "nosniff")
	}
	if tok.Disp != "" {
		h.Set("Content-Disposition", tok.Disp)
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
}

func metaFromResponse(resp *http.Response, tok *tokenPayload) meta {
	m := meta{
		Size:    tok.Size,
		Type:    resp.Header.Get("Content-Type"),
		CSP:     resp.Header.Get("Content-Security-Policy"),
		Nosniff: strings.EqualFold(resp.Header.Get("X-Content-Type-Options"), "nosniff"),
	}
	if m.Type == "" {
		m.Type = tok.Type
	}
	return m
}

// relay answers straight from the origin, for locked, oversized and ranged
// misses and for everything that cannot be cached.
func (s *server) relay(w http.ResponseWriter, r *http.Request, raw string) {
	req, err := s.originRequest(r.Context(), r.Host, raw, r.Header.Get("Range"))
	if err != nil {
		log.Printf("blobcache: origin request %s: %s", pathLabel(r.URL.Path), reason(err))
		s.relayError(w)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("blobcache: origin %s: %s", pathLabel(r.URL.Path), reason(err))
		s.relayError(w)
		return
	}
	defer resp.Body.Close()
	s.copyUpstream(w, r.Method == http.MethodHead, resp)
}

// copyUpstream forwards an already fetched origin response.
func (s *server) copyUpstream(w http.ResponseWriter, head bool, resp *http.Response) {
	h := w.Header()
	for _, name := range passthroughHeaders {
		for _, v := range resp.Header.Values(name) {
			h.Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if head {
		return
	}
	buf := make([]byte, chunkSize)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			s.counters.originBytes.Add(int64(n))
			got, werr := w.Write(buf[:n])
			s.counters.servedBytes.Add(int64(got))
			if werr != nil {
				return
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func (s *server) relayError(w http.ResponseWriter) {
	http.Error(w, "upstream unavailable", http.StatusBadGateway)
}

// originRequest builds GET /internal/blob/<token> against the origin, keeping
// the client's Host and forwarding only the Range header: cookies and
// authorization never travel further.
func (s *server) originRequest(ctx context.Context, host, raw, rangeHeader string) (*http.Request, error) {
	u := *s.origin
	u.Path = strings.TrimSuffix(s.origin.Path, "/") + "/internal/blob/" + raw
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("X-Edge-Credential", s.credential)
	req.Header.Set("Accept-Encoding", "identity")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return req, nil
}

// byteRange is a resolved single range; hi is inclusive.
type byteRange struct {
	lo, hi        int64
	partial       bool
	ignore        bool // malformed: answer as if no Range was sent
	unsatisfiable bool // well formed but outside the object: 416
}

// parseRange reads one bytes range the way the contract describes: a single
// segment, a suffix (-n) and an open end (a-). Anything with a comma is served
// whole, as the contract says for multipart ranges.
func parseRange(header string, size int64) byteRange {
	whole := byteRange{lo: 0, hi: size - 1}
	text := strings.TrimSpace(header)
	if len(text) < len("bytes=") || !strings.EqualFold(text[:len("bytes=")], "bytes=") {
		return whole
	}
	spec := text[len("bytes="):]
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		if strings.Contains(spec, ",") {
			return whole
		}
		return byteRange{ignore: true}
	}
	startText, endText, ok := strings.Cut(spec, "-")
	if !ok {
		return byteRange{ignore: true}
	}
	startText, endText = strings.TrimSpace(startText), strings.TrimSpace(endText)
	if startText == "" {
		n, err := strconv.ParseInt(endText, 10, 64)
		if err != nil || n < 0 {
			return byteRange{ignore: true}
		}
		if n == 0 || size == 0 {
			return byteRange{unsatisfiable: true}
		}
		if n >= size {
			return byteRange{lo: 0, hi: size - 1, partial: true}
		}
		return byteRange{lo: size - n, hi: size - 1, partial: true}
	}
	lo, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || lo < 0 {
		return byteRange{ignore: true}
	}
	hi := size - 1
	if endText != "" {
		hi, err = strconv.ParseInt(endText, 10, 64)
		if err != nil || hi < lo {
			return byteRange{ignore: true}
		}
		if hi >= size {
			hi = size - 1
		}
	}
	if lo >= size {
		return byteRange{unsatisfiable: true}
	}
	return byteRange{lo: lo, hi: hi, partial: true}
}

// reason is a loggable cause: no request URL, which carries the token, and no
// file name, which is derived from the object key.
func reason(err error) string {
	if err == nil {
		return ""
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return err.Error()
}

// shortObj is the only part of obj a log line may carry.
func shortObj(obj string) string {
	if len(obj) > 6 {
		return obj[:6]
	}
	return obj
}

// pathLabel names a request in the log without printing its token.
func pathLabel(path string) string {
	if strings.HasPrefix(path, "/b/") {
		return "/b/<token>"
	}
	return path
}
