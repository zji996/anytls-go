package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	M "github.com/sagernet/sing/common/metadata"
)

// tlsOrigin is an HTTPS origin for example.com that records what it receives.
type tlsOrigin struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	protos []string
}

func newTLSOrigin(t *testing.T) *tlsOrigin {
	o := &tlsOrigin{}
	o.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies = append(o.bodies, body)
		o.protos = append(o.protos, r.Proto)
		o.mu.Unlock()
		fmt.Fprintf(w, "ok %d", len(body))
	}))
	o.srv.EnableHTTP2 = true
	o.srv.StartTLS()
	t.Cleanup(o.srv.Close)
	return o
}

func (o *tlsOrigin) last() ([]byte, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.bodies[len(o.bodies)-1], o.protos[len(o.protos)-1]
}

type frontChain struct {
	origin *tlsOrigin
	ca     *authority
	enc    *encoder
	front  *front
	back   *httptest.Server
	proxy  *url.URL
	// client trusts the private CA only, like Sub2API with the CA added.
	client *http.Client
}

func newFrontChain(t *testing.T, probe bool) *frontChain {
	t.Helper()
	c := &frontChain{origin: newTLSOrigin(t)}
	originAddr := c.origin.srv.Listener.Addr().String()
	originRoots := x509.NewCertPool()
	originRoots.AddCert(c.origin.srv.Certificate())
	toOrigin := route{
		tls:     &tls.Config{RootCAs: originRoots},
		resolve: map[string]string{"example.com:443": originAddr},
	}

	dec := newDecoder(toOrigin, 64<<20, 64<<20)
	dec.hosts = domainList{"example.com"}
	c.back = httptest.NewServer(dec)
	t.Cleanup(c.back.Close)

	dir := t.TempDir()
	domains := domainList{"example.com"}
	if err := createCA(dir, domains); err != nil {
		t.Fatal(err)
	}
	var err error
	if c.ca, err = loadAuthority(dir); err != nil {
		t.Fatal(err)
	}
	backURL := mustURL(t, c.back.URL)
	c.enc = newEncoder(route{url: backURL}, 32<<20, 64<<20)
	direct := newProxy(toOrigin)
	c.enc.origin = direct.Transport
	dial := func(ctx context.Context, dest M.Socksaddr) (net.Conn, error) {
		// Every name leads to the origin, as the exit would resolve it.
		return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
	}
	c.front = newFront(domains, c.ca, dial, c.enc, direct)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); c.front.conns.Close() })
	go c.front.serve(ln)
	c.proxy = mustURL(t, "socks5://"+ln.Addr().String())
	if probe {
		c.enc.probe(context.Background(), backURL)
		if !c.enc.healthy() {
			t.Fatal("probe did not find the decoder")
		}
	}
	roots := x509.NewCertPool()
	roots.AddCert(c.ca.cert)
	c.client = c.clientTrusting(roots)
	return c
}

func (c *frontChain) clientTrusting(roots *x509.CertPool) *http.Client {
	tr := &http.Transport{
		Proxy:             http.ProxyURL(c.proxy),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: true,
	}
	return &http.Client{Transport: tr}
}

func (c *frontChain) post(t *testing.T, client *http.Client, body []byte) *http.Response {
	t.Helper()
	resp, err := client.Post("https://example.com/backend-api/codex/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != fmt.Sprintf("ok %d", len(body)) {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if last, _ := c.origin.last(); !bytes.Equal(last, body) {
		t.Fatalf("origin got %d bytes, want %d identical", len(last), len(body))
	}
	return resp
}

func issuedByCA(resp *http.Response, ca *authority) bool {
	return bytes.Equal(resp.TLS.PeerCertificates[0].RawIssuer, ca.cert.RawSubject)
}

func TestFrontDeduplicatesInterceptedHTTP2(t *testing.T) {
	c := newFrontChain(t, true)
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	var total int
	for turn := 1; turn <= 3; turn++ {
		body := conversation(turn, images)
		total += len(body)
		resp := c.post(t, c.client, body)
		if resp.ProtoMajor != 2 || !issuedByCA(resp, c.ca) {
			t.Fatalf("client saw %s from %s, want HTTP/2 from the private CA", resp.Proto, resp.TLS.PeerCertificates[0].Issuer)
		}
		if _, proto := c.origin.last(); proto != "HTTP/2.0" {
			t.Fatalf("origin saw %s, want HTTP/2.0", proto)
		}
	}
	s := c.front.snapshot()
	if ratio := float64(total) / float64(s["sent_bytes"].(int64)); ratio < 1.8 {
		t.Fatalf("ratio %.2f too low: %v", ratio, s)
	}
	if s["intercepted_conns"].(int64) != 1 || s["passed_conns"].(int64) != 0 {
		t.Fatalf("want one reused intercepted connection: %v", s)
	}
}

func TestFrontPassesThroughOtherNames(t *testing.T) {
	c := newFrontChain(t, true)
	client := c.clientTrusting(nil)
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	resp, err := client.Get("https://other.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if issuedByCA(resp, c.ca) {
		t.Fatal("a name outside the allowlist was intercepted")
	}
	if n := c.front.passed.Load(); n != 1 {
		t.Fatalf("passed %d connections, want 1", n)
	}
}

func TestFrontPassesThroughWithoutDecoder(t *testing.T) {
	c := newFrontChain(t, false)
	roots := x509.NewCertPool()
	roots.AddCert(c.ca.cert)
	roots.AddCert(c.origin.srv.Certificate())
	resp := c.post(t, c.clientTrusting(roots), conversation(1, []string{image(100 << 10)}))
	if issuedByCA(resp, c.ca) {
		t.Fatal("intercepted although no decoder answered")
	}
}

func TestFrontSendsToOriginWhenDecoderDies(t *testing.T) {
	c := newFrontChain(t, true)
	images := []string{image(300 << 10), image(300 << 10)}
	c.post(t, c.client, conversation(1, images))
	c.back.Close()
	// Same intercepted connection; the framed request must still arrive.
	resp := c.post(t, c.client, conversation(2, images))
	if !issuedByCA(resp, c.ca) {
		t.Fatal("expected the existing intercepted connection")
	}
	s := c.front.snapshot()
	if s["fallbacks"].(int64) != 1 || c.enc.healthy() {
		t.Fatalf("want one fallback and framing paused: %v", s)
	}
	c.post(t, c.client, conversation(2, images))
}

func TestCARefusesNamesOutsideItsConstraints(t *testing.T) {
	dir := t.TempDir()
	if err := createCA(dir, domainList{"example.com"}); err != nil {
		t.Fatal(err)
	}
	ca, err := loadAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.check(domainList{"example.com", "sub.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := ca.check(domainList{"example.org"}); err == nil {
		t.Fatal("CA certified a name outside its constraints")
	}
}

func TestDecoderRefusesOtherOrigins(t *testing.T) {
	dec := newDecoder(route{}, 1<<20, 1<<20)
	dec.hosts = domainList{"example.com"}
	srv := httptest.NewServer(dec)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "internal.lan"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}
