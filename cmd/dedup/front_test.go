package main

import (
	std_bufio "bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
)

// tlsOrigin is an HTTPS origin for example.com that records what it receives.
type tlsOrigin struct {
	srv       *httptest.Server
	mu        sync.Mutex
	bodies    [][]byte
	protos    []string
	encodings []string
}

func newTLSOrigin(t *testing.T) *tlsOrigin {
	o := &tlsOrigin{}
	o.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies = append(o.bodies, body)
		o.protos = append(o.protos, r.Proto)
		o.encodings = append(o.encodings, r.Header.Get("Accept-Encoding"))
		o.mu.Unlock()
		if r.URL.Path == "/gz" && r.Header.Get("Accept-Encoding") == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			fmt.Fprint(zw, "compressed hello")
			zw.Close()
			return
		}
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
	roots  *x509.CertPool
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
	dial := func(ctx context.Context, dest M.Socksaddr) (net.Conn, error) {
		// Every name leads to the origin, as the exit would resolve it.
		return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
	}
	newTestLane := func(name string) *lane {
		enc := newEncoder(route{url: backURL}, 32<<20, 64<<20)
		direct := newProxy(toOrigin)
		enc.origin = direct.Transport
		if probe {
			enc.probe(context.Background(), backURL)
		}
		return &lane{name: name, enc: enc, direct: direct, dial: dial}
	}
	def := newTestLane("")
	c.enc = def.enc
	if probe && !c.enc.healthy() {
		t.Fatal("probe did not find the decoder")
	}
	c.front = newFront(domains, c.ca, newLaneSet(3, func(user, _ string) *lane {
		if user == "" {
			return def
		}
		return newTestLane(user)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); c.front.conns.Close() })
	go c.front.serve(ln)
	c.proxy = mustURL(t, "socks5://"+ln.Addr().String())
	c.roots = x509.NewCertPool()
	c.roots.AddCert(c.ca.cert)
	c.client = c.clientTrusting(c.roots)
	return c
}

// clientAs reaches the front with a SOCKS5 username, as a router names a node.
func (c *frontChain) clientAs(user string) *http.Client {
	client := c.clientTrusting(c.roots)
	proxy := *c.proxy
	proxy.User = url.UserPassword(user, "pw")
	client.Transport.(*http.Transport).Proxy = http.ProxyURL(&proxy)
	return client
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

func TestFrontGivesEachSOCKSUserItsOwnLane(t *testing.T) {
	c := newFrontChain(t, true)
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	// Clients without a username keep the default lane.
	c.post(t, c.client, conversation(1, images))
	for turn, user := range []string{"alice", "bob", "alice"} {
		resp := c.post(t, c.clientAs(user), conversation(turn+1, images))
		if !issuedByCA(resp, c.ca) {
			t.Fatalf("%s was not intercepted", user)
		}
	}
	lanes := c.front.snapshot()["lanes"].(map[string]any)
	framed := func(name string) int64 { return lanes[name].(map[string]any)["framed"].(int64) }
	if len(lanes) != 3 || framed("") != 1 || framed("alice") != 2 || framed("bob") != 1 {
		t.Fatalf("want lanes default=1 alice=2 bob=1 framed: %v", lanes)
	}
	if _, err := c.clientAs("carol").Get("https://example.com/"); err == nil {
		t.Fatal("a fourth lane was accepted past the limit of 3")
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

func TestGoGzipHeaderReachesOriginAndResponseDecodes(t *testing.T) {
	c := newFrontChain(t, true)
	resp, err := c.client.Get("https://example.com/gz")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "compressed hello" {
		t.Fatalf("client got %q", got)
	}
	c.origin.mu.Lock()
	defer c.origin.mu.Unlock()
	if ae := c.origin.encodings[len(c.origin.encodings)-1]; ae != "gzip" {
		t.Fatalf("origin saw Accept-Encoding %q, want gzip", ae)
	}
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

// authSOCKS stands in for the router's SOCKS inbound that picks a node by
// username. It records each connection's user and sends every name to origin.
type authSOCKS struct {
	origin string
	mu     sync.Mutex
	users  []string
}

func (s *authSOCKS) NewConnection(ctx context.Context, conn net.Conn, md M.Metadata) error {
	user, _ := auth.UserFromContext[string](ctx)
	s.mu.Lock()
	s.users = append(s.users, user)
	s.mu.Unlock()
	target := md.Destination.String()
	if md.Destination.IsFqdn() {
		target = s.origin
	}
	remote, err := net.Dial("tcp", target)
	if err != nil {
		return err
	}
	defer remote.Close()
	return bufio.CopyConn(ctx, conn, remote)
}

func (s *authSOCKS) NewPacketConnection(context.Context, N.PacketConn, M.Metadata) error {
	return errors.New("UDP is not supported")
}

func (s *authSOCKS) count(user string) (n int, others []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u == user {
			n++
		} else {
			others = append(others, u)
		}
	}
	return n, others
}

// A lane that has found its decoder intercepts new connections while the
// decoder is down, and sends their requests to the origin.
func TestFrontKeepsInterceptingWhileDecoderPauses(t *testing.T) {
	c := newFrontChain(t, true)
	c.enc.pause("test", errStatus(503))
	resp := c.post(t, c.client, conversation(1, []string{image(100 << 10)}))
	if !issuedByCA(resp, c.ca) {
		t.Fatal("passed through although the lane had found its decoder")
	}
	if s := c.front.snapshot(); s["framed"].(int64) != 0 || s["passed_conns"].(int64) != 0 {
		t.Fatalf("want the request sent plain on an intercepted connection: %v", s)
	}
}

func TestLaneDialsViaWithItsUser(t *testing.T) {
	origin := newTLSOrigin(t)
	originAddr := origin.srv.Listener.Addr().String()
	originRoots := x509.NewCertPool()
	originRoots.AddCert(origin.srv.Certificate())
	dec := newDecoder(route{tls: &tls.Config{RootCAs: originRoots}, resolve: map[string]string{"example.com:443": originAddr}}, 64<<20, 64<<20)
	dec.hosts = domainList{"example.com"}
	back := httptest.NewServer(dec)
	t.Cleanup(back.Close)

	via := &authSOCKS{origin: originAddr}
	viaLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { viaLn.Close() })
	users := auth.NewAuthenticator([]auth.User{{Username: "bob", Password: "pw"}})
	go func() {
		for {
			c, err := viaLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				socks.HandleConnection0(context.Background(), c, std_bufio.NewReader(c), users, via, M.Metadata{})
			}()
		}
	}()

	dir := t.TempDir()
	if err := createCA(dir, domainList{"example.com"}); err != nil {
		t.Fatal(err)
	}
	ca, err := loadAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	lc := laneConfig{
		via: mustURL(t, "socks5://"+viaLn.Addr().String()), decoder: mustURL(t, back.URL),
		indexBytes: 32 << 20, maxBody: 64 << 20, inflight: &budget{}, probe: time.Hour,
	}
	f := newFront(domainList{"example.com"}, ca, newLaneSet(maxLanes, lc.newLane))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); f.conns.Close() })
	go f.serve(ln)

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	roots.AddCert(origin.srv.Certificate())
	client := func(user string) *http.Client {
		return &http.Client{Transport: &http.Transport{
			Proxy:             http.ProxyURL(&url.URL{Scheme: "socks5", Host: ln.Addr().String(), User: url.UserPassword(user, "pw")}),
			TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: "example.com"},
			ForceAttemptHTTP2: true,
		}}
	}
	bob := client("bob")
	// The first connection makes the lane. It waits for the lane's first probe:
	// a client may keep this connection for hours.
	body := conversation(1, []string{image(300 << 10)})
	resp, err := bob.Post("https://example.com/backend-api/codex/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	l := f.lanes.get("bob", "pw")
	if !issuedByCA(resp, ca) || l.enc.stats.framed.Load() != 1 {
		t.Fatalf("want bob's first request intercepted and framed: %v", l.enc.snapshot())
	}
	// The probe, whose connection the framed request reuses.
	if n, others := via.count("bob"); n < 1 || len(others) != 0 {
		t.Fatalf("via saw bob %d times and also %v", n, others)
	}
	// An exit the router does not know must not be reached some other way.
	if _, err := client("mallory").Get("https://other.test/"); err == nil {
		t.Fatal("mallory got through a via that only knows bob")
	}
}
