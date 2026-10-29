package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testSignKey    = mustHex(strings.Repeat("3c", 32))
	testCredential = mustHex(strings.Repeat("a1", 32))
	testMaster     = mustHex(strings.Repeat("2b", 32))
)

// origin is a stand-in for the Gateway side of the contract: it checks the edge
// credential, verifies the token with the shared signing key, and answers like
// GET /internal/blob/<token> must.
type origin struct {
	signKey    []byte
	credential []byte

	mu           sync.Mutex
	bodies       map[string][]byte
	requests     int
	rangeHeaders []string
	seenHeaders  []http.Header
	seenHosts    []string

	gate        chan struct{} // when set, the response waits for it to be closed
	bodyGate    chan struct{} // when set, only the body waits for it
	started     chan struct{} // closed on the first request
	startOnce   sync.Once
	status      int // when set, answered with this status instead of 200
	lengthDelta int // extra bytes appended, and Content-Length raised, to break the size
	csp         string
}

func newOrigin(signKey, credential []byte) *origin {
	return &origin{signKey: signKey, credential: credential, bodies: map[string][]byte{}}
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Edge-Credential") != hex.EncodeToString(o.credential) {
		http.Error(w, "credential", http.StatusUnauthorized)
		return
	}
	raw, ok := strings.CutPrefix(r.URL.Path, "/internal/blob/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok, err := parseToken(raw, o.signKey, time.Now())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	o.mu.Lock()
	o.requests++
	o.rangeHeaders = append(o.rangeHeaders, r.Header.Get("Range"))
	o.seenHeaders = append(o.seenHeaders, r.Header.Clone())
	o.seenHosts = append(o.seenHosts, r.Host)
	body, known := o.bodies[tok.Obj]
	body = append([]byte(nil), body...)
	o.mu.Unlock()
	if o.started != nil {
		o.startOnce.Do(func() { close(o.started) })
	}
	if o.gate != nil {
		<-o.gate
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", tok.Type)
	h.Set("Content-Disposition", tok.Disp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=300")
	if o.csp != "" {
		h.Set("Content-Security-Policy", o.csp)
	}
	if o.status != 0 {
		w.WriteHeader(o.status)
		w.Write(body)
		return
	}
	body = append(body, bytes.Repeat([]byte{'!'}, o.lengthDelta)...)
	writeBlob(w, r, body, o.bodyGate)
}

// writeBlob answers a body with the range semantics of the contract. When gate
// is set, the headers go out first and the body waits for it to be closed.
func writeBlob(w http.ResponseWriter, r *http.Request, body []byte, gate chan struct{}) {
	size := int64(len(body))
	rng := parseRange(r.Header.Get("Range"), size)
	if rng.ignore {
		rng = byteRange{lo: 0, hi: size - 1}
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	if rng.unsatisfiable {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(rng.hi-rng.lo+1, 10))
	status := http.StatusOK
	if rng.partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.lo, rng.hi, size))
	}
	w.WriteHeader(status)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if gate != nil {
		<-gate
	}
	if r.Method == http.MethodHead {
		return
	}
	if size > 0 {
		w.Write(body[rng.lo : rng.hi+1])
	}
}

func (o *origin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests
}

func (o *origin) headers() []http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]http.Header(nil), o.seenHeaders...)
}

func (o *origin) hosts() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seenHosts...)
}

func (o *origin) ranges() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.rangeHeaders...)
}

type options struct {
	size      int64
	reserve   int64
	maxObject int64
	locked    bool
}

type harness struct {
	t         *testing.T
	dir       string
	opts      options
	orig      *origin
	originSrv *httptest.Server
	originURL string
	blob      *httptest.Server
	srv       *server
	c         *cache
	socket    string
	sockErr   error
}

func newHarness(t *testing.T, opt options) *harness {
	t.Helper()
	if opt.size == 0 {
		opt.size = 16 << 20
	}
	if opt.maxObject == 0 {
		opt.maxObject = 1 << 20
	}
	dir := t.TempDir()
	c, err := newCache(dir, opt.size, opt.reserve)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if !opt.locked {
		if err := c.setKey(testMaster); err != nil {
			t.Fatalf("setKey: %v", err)
		}
	}
	o := newOrigin(testSignKey, testCredential)
	originSrv := httptest.NewServer(o)
	base, err := url.Parse(originSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(base, hex.EncodeToString(testCredential), testSignKey, c, opt.maxObject)
	blobSrv := httptest.NewServer(srv)
	h := &harness{t: t, dir: dir, opts: opt, orig: o, originSrv: originSrv, originURL: originSrv.URL, blob: blobSrv, srv: srv, c: c}
	socketDir := t.TempDir()
	h.socket = filepath.Join(socketDir, "control.sock")
	if ln, err := listenControl(h.socket); err != nil {
		h.sockErr = err
	} else {
		go http.Serve(ln, srv.controlMux())
		t.Cleanup(func() { ln.Close() })
	}
	t.Cleanup(func() {
		srv.cancel()
		blobSrv.Close()
		originSrv.Close()
		srv.client.CloseIdleConnections()
	})
	return h
}

// controlSocket is the harness's control socket, or a skip on platforms
// without unix sockets.
func (h *harness) controlSocket() string {
	h.t.Helper()
	if h.sockErr != nil {
		h.t.Skipf("control socket unavailable: %v", h.sockErr)
	}
	return h.socket
}

func (h *harness) unlock() {
	h.t.Helper()
	if err := h.c.setKey(testMaster); err != nil {
		h.t.Fatalf("unlock: %v", err)
	}
}

// put registers a body and returns a token for it.
func (h *harness) put(obj string, body []byte, typ, disp string) string {
	h.t.Helper()
	h.orig.mu.Lock()
	h.orig.bodies[obj] = body
	h.orig.mu.Unlock()
	token, err := signToken(testSignKey, tokenPayload{
		V: 1, Obj: obj, Loc: "loc-1",
		Exp:  time.Now().Add(5 * time.Minute).Unix(),
		Size: int64(len(body)), Type: typ, Disp: disp,
	})
	if err != nil {
		h.t.Fatalf("sign: %v", err)
	}
	return token
}

type result struct {
	status  int
	header  http.Header
	body    []byte
	elapsed time.Duration
}

func (h *harness) do(method, token, rangeHeader string) result {
	h.t.Helper()
	req, err := http.NewRequest(method, h.blob.URL+"/b/"+token, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	req.Header.Set("Cookie", "session=must-not-travel")
	req.Header.Set("Authorization", "Bearer must-not-travel")
	start := time.Now()
	resp, err := h.blob.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: body, elapsed: time.Since(start)}
}

func (h *harness) get(token, rangeHeader string) result {
	h.t.Helper()
	return h.do(http.MethodGet, token, rangeHeader)
}

func (h *harness) headers() []http.Header { return h.orig.headers() }

func (h *harness) hosts() []string { return h.orig.hosts() }

// listObjects lists the object files in a cache directory.
func listObjects(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names
}

func (h *harness) files() []string { return listObjects(h.t, h.dir) }

func (h *harness) tmpFiles() []string {
	h.t.Helper()
	ents, err := os.ReadDir(filepath.Join(h.dir, tmpDirName))
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// waitStatus polls until the report matches, because a hit counts itself after
// its last body byte has already reached the client.
func (h *harness) waitStatus(ok func(statusReport) bool) statusReport {
	h.t.Helper()
	return waitStatusOf(h.t, h.srv, ok)
}

func waitStatusOf(t *testing.T, s *server, ok func(statusReport) bool) statusReport {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := s.status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never matched: %+v", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) waitObjects(want int) statusReport {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := h.srv.status()
		if st.Objects == want {
			return st
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("cache holds %d objects, want %d", st.Objects, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// makeBody is deterministic filler that differs per seed and per position.
func makeBody(n int, seed byte) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(i*31) ^ seed
	}
	return body
}
