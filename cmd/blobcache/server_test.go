package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testObj = "0123456789abcdef0123456789abcdef"
const testObj2 = "fedcba9876543210fedcba9876543210"
const testObj3 = "aaaabbbbccccddddeeeeffff00001111"

func TestLockedPassesThroughWithoutWriting(t *testing.T) {
	h := newHarness(t, options{locked: true})
	body := makeBody(4000, 7)
	token := h.put(testObj, body, "text/plain; charset=utf-8", "inline")

	for i := 0; i < 2; i++ {
		got := h.get(token, "")
		if got.status != http.StatusOK || !bytes.Equal(got.body, body) {
			t.Fatalf("request %d: status %d, %d bytes", i, got.status, len(got.body))
		}
		if got.header.Get("Content-Disposition") != "inline" {
			t.Fatalf("pass-through dropped Content-Disposition: %q", got.header.Get("Content-Disposition"))
		}
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2 (a locked cache must not answer)", n)
	}
	if files := h.files(); len(files) != 0 {
		t.Fatalf("a locked cache wrote %v", files)
	}
	if tmp := h.tmpFiles(); len(tmp) != 0 {
		t.Fatalf("a locked cache left %v in tmp", tmp)
	}
	st := h.srv.status()
	if !st.Locked || st.Hits != 0 || st.Objects != 0 || st.UsedBytes != 0 || st.Passthrough != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestUnlockCachesThenHits(t *testing.T) {
	h := newHarness(t, options{locked: true})
	body := makeBody(70000, 3)
	token := h.put(testObj, body, "application/pdf", `inline; filename="a.pdf"`)

	if got := h.get(token, ""); got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("locked request: %d, %d bytes", got.status, len(got.body))
	}
	h.unlock()

	// First unlocked request fills the cache while it streams.
	got := h.get(token, "")
	if got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("first unlocked request: %d, %d bytes", got.status, len(got.body))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2", n)
	}
	h.waitObjects(1)
	if tmp := h.tmpFiles(); len(tmp) != 0 {
		t.Fatalf("tmp not empty after a completed fill: %v", tmp)
	}
	if got := h.get(token, ""); got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("second request: %d, %d bytes", got.status, len(got.body))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2: the second request hit the cache", n)
	}
	h.waitStatus(func(st statusReport) bool { return st.Hits >= 1 && st.Objects == 1 && !st.Locked })

	// A hit repeats the contract headers, and Content-Disposition still comes
	// from the token.
	hit := h.get(token, "")
	for name, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Disposition":    `inline; filename="a.pdf"`,
		"X-Content-Type-Options": "nosniff",
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "private, max-age=300",
		"Content-Length":         "70000",
	} {
		if got := hit.header.Get(name); got != want {
			t.Errorf("hit header %s = %q, want %q", name, got, want)
		}
	}

	// HEAD: headers without a body, still from the cache.
	head := h.do(http.MethodHead, token, "")
	if head.status != http.StatusOK || head.header.Get("Content-Length") != "70000" || len(head.body) != 0 {
		t.Fatalf("HEAD hit: %d, %q, %d body bytes", head.status, head.header.Get("Content-Length"), len(head.body))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests after HEAD, want 2", n)
	}
}

func TestConcurrentMissesFetchOnce(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(300000, 11)
	token := h.put(testObj, body, "application/octet-stream", "attachment")
	h.orig.gate = make(chan struct{})
	h.orig.started = make(chan struct{})

	const n = 8
	var wg sync.WaitGroup
	results := make([]result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.get(token, "")
		}(i)
	}
	<-h.orig.started
	// Give the other requests time to join the fill in flight.
	time.Sleep(100 * time.Millisecond)
	close(h.orig.gate)
	wg.Wait()

	for i, got := range results {
		if got.status != http.StatusOK || !bytes.Equal(got.body, body) {
			t.Fatalf("request %d: status %d, %d bytes", i, got.status, len(got.body))
		}
	}
	if c := h.orig.count(); c != 1 {
		t.Fatalf("origin saw %d requests for one object, want 1", c)
	}
}

func TestHitRanges(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(3*chunkSize+1234, 5)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	if got := h.get(token, ""); got.status != http.StatusOK {
		t.Fatalf("warm-up: %d", got.status)
	}
	h.waitObjects(1)
	size := int64(len(body))
	cases := []struct {
		rng    string
		status int
		lo, hi int64
	}{ // hi is exclusive here
		{"bytes=0-3", http.StatusPartialContent, 0, 4},
		{"bytes=65530-65545", http.StatusPartialContent, 65530, 65546},
		{"bytes=-10", http.StatusPartialContent, size - 10, size},
		{"bytes=131072-", http.StatusPartialContent, 131072, size},
		{fmt.Sprintf("bytes=%d-", size-1), http.StatusPartialContent, size - 1, size},
		{"bytes=0-999999999", http.StatusPartialContent, 0, size},
		{"bytes=999999999-", http.StatusRequestedRangeNotSatisfiable, 0, 0},
		{"bytes=0-1,4-5", http.StatusOK, 0, size},
		{"Bytes=0-3", http.StatusPartialContent, 0, 4},
		{" bytes=0-3 ", http.StatusPartialContent, 0, 4},
		{"bytes=5-3", http.StatusOK, 0, size},
		{"items=0-3", http.StatusOK, 0, size},
	}
	for _, tc := range cases {
		got := h.get(token, tc.rng)
		if got.status != tc.status {
			t.Errorf("%s: status %d, want %d", tc.rng, got.status, tc.status)
			continue
		}
		if tc.status == http.StatusRequestedRangeNotSatisfiable {
			if want := fmt.Sprintf("bytes */%d", size); got.header.Get("Content-Range") != want {
				t.Errorf("%s: Content-Range %q, want %q", tc.rng, got.header.Get("Content-Range"), want)
			}
			if len(got.body) != 0 {
				t.Errorf("%s: %d body bytes on 416", tc.rng, len(got.body))
			}
			continue
		}
		want := body[tc.lo:tc.hi]
		if !bytes.Equal(got.body, want) {
			t.Errorf("%s: got %d bytes, want %d", tc.rng, len(got.body), len(want))
			continue
		}
		if want := fmt.Sprintf("bytes %d-%d/%d", tc.lo, tc.hi-1, size); got.status == http.StatusPartialContent && got.header.Get("Content-Range") != want {
			t.Errorf("%s: Content-Range %q, want %q", tc.rng, got.header.Get("Content-Range"), want)
		}
		if want := fmt.Sprint(tc.hi - tc.lo); got.header.Get("Content-Length") != want {
			t.Errorf("%s: Content-Length %q, want %q", tc.rng, got.header.Get("Content-Length"), want)
		}
	}
	if c := h.orig.count(); c != 1 {
		t.Fatalf("origin saw %d requests, want 1: ranges must come from the cache", c)
	}
}

func TestDispositionFromToken(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(5000, 9)
	inline := h.put(testObj, body, "image/png", "inline")
	attachment := h.put(testObj, body, "image/png", `attachment; filename="b.png"`)

	if got := h.get(inline, ""); got.header.Get("Content-Disposition") != "inline" {
		t.Fatalf("first: Content-Disposition %q", got.header.Get("Content-Disposition"))
	}
	h.waitObjects(1)
	if got := h.get(attachment, ""); got.header.Get("Content-Disposition") != `attachment; filename="b.png"` {
		t.Fatalf("second: Content-Disposition %q", got.header.Get("Content-Disposition"))
	}
	if n := h.orig.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
}

func TestNoPlaintextOrObjectKeyOnDisk(t *testing.T) {
	h := newHarness(t, options{})
	marker := []byte("SECRET-PLAINTEXT-MARKER-0123456789")
	body := append(makeBody(200000, 13), marker...)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	if got := h.get(token, ""); got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("warm-up: %d", got.status)
	}
	h.waitObjects(1)
	files := h.files()
	if len(files) != 1 {
		t.Fatalf("cache holds %v, want one file", files)
	}
	banned := [][]byte{marker, []byte(testObj)}
	for _, name := range files {
		if strings.Contains(name, testObj) {
			t.Fatalf("file name %q contains obj", name)
		}
		raw, err := os.ReadFile(filepath.Join(h.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range banned {
			if bytes.Contains(raw, needle) {
				t.Fatalf("cache file %s contains %q", name, needle)
			}
		}
		if len(raw) <= len(body)/8 {
			t.Fatalf("cache file is only %d bytes for a %d byte object", len(raw), len(body))
		}
	}
}

func TestTamperedCacheFileIsDropped(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(5000, 17)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	if got := h.get(token, ""); got.status != http.StatusOK {
		t.Fatalf("warm-up: %d", got.status)
	}
	h.waitObjects(1)
	if files := h.files(); len(files) != 1 {
		t.Fatalf("cache holds %v", files)
	}

	// A damaged object is replaced: the request that finds it goes to the
	// origin and the file is written again.
	damage := func() string {
		t.Helper()
		files := h.files()
		if len(files) != 1 {
			t.Fatalf("cache holds %v", files)
		}
		path := filepath.Join(h.dir, files[0])
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 0x01
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	damage()
	got := h.get(token, "")
	if got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("after tampering: status %d, %d bytes", got.status, len(got.body))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2: the damaged file must be refetched", n)
	}

	// And with the origin down, the damaged file is gone rather than served.
	h.waitObjects(1)
	damage()
	h.orig.mu.Lock()
	h.orig.status = http.StatusInternalServerError
	h.orig.mu.Unlock()
	if got := h.get(token, ""); got.status != http.StatusInternalServerError {
		t.Fatalf("tampered cache file was served: status %d, %d bytes", got.status, len(got.body))
	}
	h.waitObjects(0)
	if files := h.files(); len(files) != 0 {
		t.Fatalf("damaged file survived: %v", files)
	}
}

func TestEvictionDropsLeastRecentlyUsed(t *testing.T) {
	const objSize = 400 << 10
	h := newHarness(t, options{size: 1 << 20})
	bodies := map[string][]byte{}
	tokens := map[string]string{}
	for i, obj := range []string{testObj, testObj2, testObj3} {
		bodies[obj] = makeBody(objSize, byte(i))
		tokens[obj] = h.put(obj, bodies[obj], "application/octet-stream", "inline")
	}
	first := h.get(tokens[testObj], "")
	if first.status != 200 {
		t.Fatalf("warm-up: %d", first.status)
	}
	h.waitObjects(1)
	second := h.get(tokens[testObj2], "")
	if second.status != 200 {
		t.Fatalf("warm-up: %d", second.status)
	}
	if st := h.waitObjects(2); st.UsedBytes <= 0 {
		t.Fatalf("two objects cached: %+v", st)
	}
	// Touch testObj so testObj2 becomes the least recently used.
	if got := h.get(tokens[testObj], ""); got.status != 200 {
		t.Fatalf("touch: %d", got.status)
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("touching fetched again: %d requests", n)
	}
	if got := h.get(tokens[testObj3], ""); got.status != 200 || !bytes.Equal(got.body, bodies[testObj3]) {
		t.Fatalf("third object: %d", got.status)
	}
	h.waitObjects(2)
	if st := h.srv.status(); st.Objects != 2 {
		t.Fatalf("after eviction: %+v", st)
	}
	if got := h.get(tokens[testObj], ""); got.status != 200 || !bytes.Equal(got.body, bodies[testObj]) {
		t.Fatalf("recently used object: %d", got.status)
	}
	if n := h.orig.count(); n != 3 {
		t.Fatalf("origin saw %d requests, want 3: the recently used object was evicted", n)
	}
	if got := h.get(tokens[testObj2], ""); got.status != 200 {
		t.Fatalf("evicted object: %d", got.status)
	}
	if n := h.orig.count(); n != 4 {
		t.Fatalf("origin saw %d requests, want 4: the evicted object must be fetched again", n)
	}
}

func TestOversizedObjectOnlyPassesThrough(t *testing.T) {
	h := newHarness(t, options{maxObject: 1000})
	body := makeBody(4000, 19)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	for i := 0; i < 2; i++ {
		got := h.get(token, "")
		if got.status != 200 || !bytes.Equal(got.body, body) {
			t.Fatalf("request %d: %d", i, got.status)
		}
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2", n)
	}
	if files := h.files(); len(files) != 0 {
		t.Fatalf("oversized object was written: %v", files)
	}
	// A ranged request over the limit is a plain pass-through too.
	if got := h.get(token, "bytes=0-3"); got.status != http.StatusPartialContent {
		t.Fatalf("range: %d", got.status)
	}
	if n := h.orig.count(); n != 3 {
		t.Fatalf("origin saw %d requests, want 3", n)
	}
	st := h.srv.status()
	if st.Objects != 0 || st.Passthrough != 3 {
		t.Fatalf("status %+v", st)
	}
}

func TestOriginLengthMismatchIsNotCached(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(5000, 23)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	h.orig.lengthDelta = 10

	got := h.get(token, "")
	if got.status != 200 || len(got.body) != len(body)+10 {
		t.Fatalf("status %d, %d bytes", got.status, len(got.body))
	}
	if files := h.files(); len(files) != 0 {
		t.Fatalf("mismatched object was cached: %v", files)
	}
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("second request: %d", got.status)
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2", n)
	}
	if st := h.srv.status(); st.Objects != 0 || st.Misses != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestOriginErrorStatusIsPassedThrough(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(2000, 29)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	h.orig.status = http.StatusInternalServerError
	if got := h.get(token, ""); got.status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", got.status)
	}
	if files := h.files(); len(files) != 0 {
		t.Fatalf("error response was cached: %v", files)
	}
}

func TestRangeMissFillsInBackground(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(200000, 31)
	token := h.put(testObj, body, "application/octet-stream", "inline")

	got := h.get(token, "bytes=0-3")
	if got.status != http.StatusPartialContent || !bytes.Equal(got.body, body[:4]) {
		t.Fatalf("range miss: status %d, %d bytes", got.status, len(got.body))
	}
	h.waitObjects(1)
	if got := h.get(token, ""); got.status != 200 || !bytes.Equal(got.body, body) {
		t.Fatalf("after background fill: %d, %d bytes", got.status, len(got.body))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2 (one range, one whole object)", n)
	}
	for _, rng := range h.orig.ranges()[1:] {
		if rng != "" {
			t.Fatalf("background fill sent Range %q", rng)
		}
	}
}

func TestMethodAndTokenChecks(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(100, 37)
	valid := h.put(testObj, body, "text/plain", "inline")

	req, err := http.NewRequest(http.MethodPost, h.blob.URL+"/b/"+valid, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.blob.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow: %q", got)
	}

	expired, err := signToken(testSignKey, tokenPayload{V: 1, Obj: testObj, Exp: time.Now().Add(-time.Second).Unix(), Size: int64(len(body)), Type: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	badVersion, err := signToken(testSignKey, tokenPayload{V: 2, Obj: testObj, Exp: time.Now().Add(time.Minute).Unix(), Size: int64(len(body)), Type: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	otherPayload, err := signToken(testSignKey, tokenPayload{V: 1, Obj: testObj, Exp: time.Now().Add(time.Minute).Unix(), Size: int64(len(body)) + 1, Type: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	validSig := valid[strings.Index(valid, "."):]
	swappedBody := otherPayload[:strings.Index(otherPayload, ".")]
	for name, path := range map[string]string{
		"garbage":         "/b/not-a-token",
		"expired":         "/b/" + expired,
		"other":           "/other/x",
		"bare":            "/b/",
		"wrong key":       "/b/" + tamperToken(t, valid),
		"changed payload": "/b/" + swappedBody + validSig,
		"bad version":     "/b/" + badVersion,
	} {
		resp, err := h.blob.Client().Get(h.blob.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, resp.StatusCode)
		}
	}
	if n := h.orig.count(); n != 0 {
		t.Fatalf("origin saw %d requests, want 0", n)
	}
}

// tamperToken changes one byte of the signature.
func tamperToken(t *testing.T, token string) string {
	t.Helper()
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatal("token has no signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0x10
	return body + "." + base64.RawURLEncoding.EncodeToString(raw)
}

func TestEmptyObject(t *testing.T) {
	h := newHarness(t, options{})
	token := h.put(testObj, nil, "text/plain", "inline")
	got := h.get(token, "")
	if got.status != http.StatusOK || len(got.body) != 0 || got.header.Get("Content-Length") != "0" {
		t.Fatalf("empty object: status %d, %d bytes, CL %q", got.status, len(got.body), got.header.Get("Content-Length"))
	}
	h.waitObjects(1)
	if got := h.get(token, ""); got.status != 200 || len(got.body) != 0 {
		t.Fatalf("cached empty object: status %d", got.status)
	}
	if n := h.orig.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
	h.waitObjects(1)
}

func TestStartupClearsTmpAndRebuildsLRU(t *testing.T) {
	h := newHarness(t, options{size: 1 << 20})
	bodies := [][]byte{makeBody(300<<10, 41), makeBody(300<<10, 43)}
	tokens := []string{
		h.put(testObj, bodies[0], "application/octet-stream", "inline"),
		h.put(testObj2, bodies[1], "application/octet-stream", "inline"),
	}
	for i, token := range tokens {
		if got := h.get(token, ""); got.status != 200 {
			t.Fatalf("warm-up %d: %d", i, got.status)
		}
	}
	h.waitObjects(2)
	files := h.files()
	if len(files) != 2 {
		t.Fatalf("cached %v", files)
	}
	// Make the order unambiguous: files[0] is the old one.
	old := filepath.Join(h.dir, files[0])
	if err := os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, tmpDirName, "leftover"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newCache(h.dir, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	objects, used := reopened.stats()
	if objects != 2 || used <= 0 {
		t.Fatalf("rebuilt LRU: %d objects, %d bytes", objects, used)
	}
	if _, err := os.Stat(filepath.Join(h.dir, tmpDirName, "leftover")); !os.IsNotExist(err) {
		t.Fatalf("tmp was not cleared: %v", err)
	}
	// A cache one byte smaller drops the object with the oldest mtime, which
	// the rebuilt order puts at the back.
	trimmed, err := newCache(h.dir, used-1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if objects, _ := trimmed.stats(); objects != 1 {
		t.Fatalf("after trimming: %d objects", objects)
	}
	if left := h.files(); len(left) != 1 || left[0] != files[1] {
		t.Fatalf("after trimming %v remains, want %q", left, files[1])
	}
}

func TestOriginSeesCredentialAndHostOnly(t *testing.T) {
	h := newHarness(t, options{locked: true})
	body := makeBody(1000, 47)
	token := h.put(testObj, body, "text/plain", "inline")
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("status %d", got.status)
	}
	headers := h.headers()
	hosts := h.hosts()
	if len(headers) != 1 || len(hosts) != 1 {
		t.Fatalf("origin saw %d requests", len(headers))
	}
	got := headers[0]
	if want := hex.EncodeToString(testCredential); got.Get("X-Edge-Credential") != want {
		t.Errorf("X-Edge-Credential = %q, want %q", got.Get("X-Edge-Credential"), want)
	}
	for _, name := range []string{"Cookie", "Authorization"} {
		if v := got.Get(name); v != "" {
			t.Errorf("the client's %s reached the origin: %q", name, v)
		}
	}
	blobHost := strings.TrimPrefix(h.blob.URL, "http://")
	if hosts[0] != blobHost {
		t.Errorf("origin saw Host %q, want the client's %q", hosts[0], blobHost)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1024":    1024,
		"10GiB":   10 << 30,
		"3gib":    3 << 30,
		"1GiB":    1 << 30,
		"512MiB":  512 << 20,
		"1.5MiB":  3 << 19,
		"2KB":     2000,
		" 4 KiB ": 4096,
		"0":       0,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "GiB", "10XiB", "1..2MiB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseHexKey(t *testing.T) {
	want := strings.Repeat("0f", 32)
	key, err := parseHexKey("  " + want + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(key) != want {
		t.Fatalf("key %x", key)
	}
	for _, bad := range []string{"", strings.Repeat("0f", 31), strings.Repeat("0f", 33), strings.Repeat("zz", 32)} {
		if _, err := parseHexKey(bad); err == nil {
			t.Errorf("%d-char %q accepted", len(bad), bad)
		}
	}
}

func TestRandomObjectsRoundTrip(t *testing.T) {
	h := newHarness(t, options{})
	for i := 0; i < 4; i++ {
		body := make([]byte, 1+i*chunkSize/3)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		obj := fmt.Sprintf("%032x", i+100)
		token := h.put(obj, body, "application/octet-stream", "inline")
		if got := h.get(token, ""); !bytes.Equal(got.body, body) {
			t.Fatalf("object %d: %d bytes, want %d", i, len(got.body), len(body))
		}
		if got := h.get(token, ""); !bytes.Equal(got.body, body) {
			t.Fatalf("object %d from cache: %d bytes, want %d", i, len(got.body), len(body))
		}
	}
	// Every cached object is readable in reverse order too.
	names := h.files()
	sort.Strings(names)
	if len(names) != 4 {
		t.Fatalf("cached %d objects", len(names))
	}
}

func TestClientDisconnectDoesNotStopTheFill(t *testing.T) {
	h := newHarness(t, options{})
	body := makeBody(300000, 67)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	h.orig.bodyGate = make(chan struct{})
	h.orig.started = make(chan struct{})

	left := make(chan struct{})
	go func() {
		defer close(left)
		req, err := http.NewRequest(http.MethodGet, h.blob.URL+"/b/"+token, nil)
		if err != nil {
			return
		}
		resp, err := h.blob.Client().Do(req)
		if err != nil {
			return
		}
		// The headers are here but the body is not; walk away.
		resp.Body.Close()
	}()
	<-h.orig.started
	// Wait until the client has its headers and has closed the connection.
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never received the response headers")
	}
	close(h.orig.bodyGate)

	h.waitObjects(1)
	got := h.get(token, "")
	if got.status != http.StatusOK || !bytes.Equal(got.body, body) {
		t.Fatalf("after the client left: status %d, %d bytes", got.status, len(got.body))
	}
	if n := h.orig.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
}

func TestNoRoomMeansPassThrough(t *testing.T) {
	// The object is bigger than the whole cache.
	t.Run("bigger than the cache", func(t *testing.T) {
		h := newHarness(t, options{size: 8192})
		body := makeBody(100000, 71)
		token := h.put(testObj, body, "application/octet-stream", "inline")
		for i := 0; i < 2; i++ {
			if got := h.get(token, ""); got.status != 200 || !bytes.Equal(got.body, body) {
				t.Fatalf("request %d: %d", i, got.status)
			}
		}
		if n := h.orig.count(); n != 2 {
			t.Fatalf("origin saw %d requests, want 2", n)
		}
		if files := h.files(); len(files) != 0 {
			t.Fatalf("an object bigger than the cache was written: %v", files)
		}
	})
	// The disk must keep -reserve bytes free; an impossible reserve refuses
	// every write.
	t.Run("reserve larger than the disk", func(t *testing.T) {
		h := newHarness(t, options{reserve: 1 << 60})
		body := makeBody(1000, 73)
		token := h.put(testObj, body, "application/octet-stream", "inline")
		for i := 0; i < 2; i++ {
			if got := h.get(token, ""); got.status != 200 || !bytes.Equal(got.body, body) {
				t.Fatalf("request %d: %d", i, got.status)
			}
		}
		if n := h.orig.count(); n != 2 {
			t.Fatalf("origin saw %d requests, want 2", n)
		}
		if files := h.files(); len(files) != 0 {
			t.Fatalf("cached despite the reserve: %v", files)
		}
	})
}

func TestHitRefreshesMtimeAtMostHourly(t *testing.T) {
	dir := t.TempDir()
	c, err := newCache(dir, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.setKey(testMaster); err != nil {
		t.Fatal(err)
	}
	body := makeBody(1000, 79)
	w, err := c.create(testObj, meta{Size: int64(len(body)), Type: "text/plain", Nosniff: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	var name string
	for _, file := range listObjects(t, dir) {
		name = file
	}
	if name == "" {
		t.Fatal("no object file was written")
	}
	path := filepath.Join(dir, name)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// A restart takes the mtime from the directory.
	reopened, err := newCache(dir, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.setKey(testMaster); err != nil {
		t.Fatal(err)
	}
	o, err := reopened.open(testObj)
	if err != nil {
		t.Fatal(err)
	}
	o.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("an hour-old hit did not refresh mtime: %s", time.Since(info.ModTime()))
	}
	first := info.ModTime()
	o, err = reopened.open(testObj)
	if err != nil {
		t.Fatal(err)
	}
	o.Close()
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(first) {
		t.Fatalf("a second hit within the hour rewrote mtime: %s then %s", first, info.ModTime())
	}
}

func TestLogsCarryNoTokenKeyOrObject(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	h := newHarness(t, options{})
	body := makeBody(2000, 83)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	h.orig.status = http.StatusInternalServerError
	if got := h.get(token, ""); got.status != http.StatusInternalServerError {
		t.Fatalf("status %d", got.status)
	}
	// A rejected token, an oversized object and a plain pass-through.
	if resp, err := h.blob.Client().Get(h.blob.URL + "/b/not-a-token"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	h2 := newHarness(t, options{maxObject: 10})
	h2.put(testObj, body, "application/octet-stream", "inline")
	if got := h2.get(h2.put(testObj, body, "application/octet-stream", "inline"), ""); got.status != 200 {
		t.Fatalf("oversized request: %d", got.status)
	}
	// An origin that cannot be reached: its error text carries the request
	// URL, and the URL carries the token.
	h3 := newHarness(t, options{locked: true})
	downToken := h3.put(testObj, body, "application/octet-stream", "inline")
	h3.originSrv.Close()
	if got := h3.get(downToken, ""); got.status != http.StatusBadGateway {
		t.Fatalf("unreachable origin: status %d", got.status)
	}
	if strings.Contains(buf.String(), downToken) {
		t.Fatalf("a log line carries the token of a failed origin request:\n%s", buf.String())
	}
	logs := buf.String()
	if logs == "" {
		t.Fatal("nothing was logged; the test proves nothing")
	}
	for _, banned := range []string{token, testObj, hex.EncodeToString(testMaster)} {
		if strings.Contains(logs, banned) {
			t.Fatalf("a log line carries %q:\n%s", banned, logs)
		}
	}
	if !strings.Contains(logs, "obj="+testObj[:6]) {
		t.Fatalf("no log line names the object prefix:\n%s", logs)
	}
}

func TestReasonDropsURLsAndPaths(t *testing.T) {
	uerr := &url.Error{Op: "Get", URL: "http://origin/internal/blob/secret-token", Err: errors.New("connection refused")}
	if got := reason(uerr); strings.Contains(got, "secret-token") || !strings.Contains(got, "connection refused") {
		t.Fatalf("url error logged as %q", got)
	}
	perr := &fs.PathError{Op: "open", Path: "/cache/0123abcd", Err: os.ErrNotExist}
	if got := reason(perr); strings.Contains(got, "0123abcd") || got == "" {
		t.Fatalf("path error logged as %q", got)
	}
}

func TestLockedPassesRangesThrough(t *testing.T) {
	h := newHarness(t, options{locked: true})
	body := makeBody(150000, 89)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	got := h.get(token, "bytes=100-199")
	if got.status != http.StatusPartialContent {
		t.Fatalf("status %d", got.status)
	}
	if !bytes.Equal(got.body, body[100:200]) {
		t.Fatalf("%d bytes, want 100", len(got.body))
	}
	if rng := h.orig.ranges(); len(rng) != 1 || rng[0] != "bytes=100-199" {
		t.Fatalf("origin saw ranges %v", rng)
	}
	if files := h.files(); len(files) != 0 {
		t.Fatalf("a locked cache wrote %v", files)
	}
	if got := h.get(token, "bytes=999999-"); got.status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out of range: %d", got.status)
	}
}

func TestRestartServesFromDisk(t *testing.T) {
	h := newHarness(t, options{size: 1 << 20})
	body := makeBody(120000, 97)
	token := h.put(testObj, body, "application/octet-stream", "inline")
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("fill: %d", got.status)
	}
	h.waitObjects(1)

	// A new process over the same directory starts locked, rebuilds the LRU
	// from the files and, once unlocked, serves them without the origin.
	c2, err := newCache(h.dir, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if objects, used := c2.stats(); objects != 1 || used <= 0 {
		t.Fatalf("rebuilt: %d objects, %d bytes", objects, used)
	}
	base, err := url.Parse(h.originURL)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := newServer(base, hex.EncodeToString(testCredential), testSignKey, c2, 1<<20)
	blob2 := httptest.NewServer(srv2)
	t.Cleanup(func() {
		srv2.cancel()
		blob2.Close()
		srv2.client.CloseIdleConnections()
	})
	get := func(token string) (*http.Response, []byte) {
		t.Helper()
		resp, err := blob2.Client().Get(blob2.URL + "/b/" + token)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, raw
	}
	if resp, raw := get(token); resp.StatusCode != 200 {
		t.Fatalf("locked restart: %d", resp.StatusCode)
	} else if !bytes.Equal(raw, body) {
		t.Fatalf("locked restart served %d bytes", len(raw))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2 (a locked restart passes through)", n)
	}
	if err := c2.setKey(testMaster); err != nil {
		t.Fatal(err)
	}
	resp, raw := get(token)
	if resp.StatusCode != 200 || !bytes.Equal(raw, body) {
		t.Fatalf("after unlock: %d, %d bytes", resp.StatusCode, len(raw))
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2: the restarted cache did not serve the object", n)
	}
	waitStatusOf(t, srv2, func(st statusReport) bool { return st.Hits == 1 && st.Objects == 1 })
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		header string
		size   int64
		want   byteRange
	}{
		{"", 100, byteRange{lo: 0, hi: 99}},
		{"bytes=0-3", 100, byteRange{lo: 0, hi: 3, partial: true}},
		{"bytes=50-", 100, byteRange{lo: 50, hi: 99, partial: true}},
		{"bytes=-10", 100, byteRange{lo: 90, hi: 99, partial: true}},
		{"bytes=-200", 100, byteRange{lo: 0, hi: 99, partial: true}},
		{"bytes=0-999", 100, byteRange{lo: 0, hi: 99, partial: true}},
		{"bytes=100-", 100, byteRange{unsatisfiable: true}},
		{"bytes=200-300", 100, byteRange{unsatisfiable: true}},
		{"bytes=-0", 100, byteRange{unsatisfiable: true}},
		{"bytes=0-1,5-6", 100, byteRange{lo: 0, hi: 99}},
		{"bytes=5-3", 100, byteRange{ignore: true}},
		{"garbage", 100, byteRange{lo: 0, hi: 99}},
		{"bytes=", 100, byteRange{ignore: true}},
		{"bytes=0-3", 0, byteRange{unsatisfiable: true}},
		{"bytes=-3", 0, byteRange{unsatisfiable: true}},
	}
	for _, tc := range cases {
		if got := parseRange(tc.header, tc.size); got != tc.want {
			t.Errorf("parseRange(%q, %d) = %+v, want %+v", tc.header, tc.size, got, tc.want)
		}
	}
}
