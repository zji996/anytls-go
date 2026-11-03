package main

import (
	std_bufio "bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// reverseOrigin is the HTTPS upstream the reverse entry reaches: it records
// what arrives and can stream a reply or accept a WebSocket.
type reverseOrigin struct {
	srv *httptest.Server
	mu  sync.Mutex
	// bodies, paths and hosts are what arrived, in order.
	bodies [][]byte
	paths  []string
	hosts  []string
}

func newReverseOrigin(t *testing.T) *reverseOrigin {
	t.Helper()
	o := &reverseOrigin{}
	o.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies = append(o.bodies, body)
		o.paths = append(o.paths, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		o.hosts = append(o.hosts, r.Host)
		o.mu.Unlock()
		switch {
		case r.URL.Path == "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "data: second\n\n")
		case r.URL.Path == "/ws" && r.Header.Get("Upgrade") == "websocket":
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			if line, err := rw.ReadString('\n'); err == nil {
				rw.WriteString("echo " + line)
				rw.Flush()
			}
		default:
			fmt.Fprintf(w, "ok %d", len(body))
		}
	}))
	o.srv.EnableHTTP2 = true
	o.srv.StartTLS()
	t.Cleanup(o.srv.Close)
	return o
}

func (o *reverseOrigin) last() ([]byte, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.bodies) == 0 {
		return nil, ""
	}
	return o.bodies[len(o.bodies)-1], o.paths[len(o.paths)-1]
}

// reverseEntryChain is a front with its reverse entry, a decoder behind it,
// and an HTTPS origin both the decoder and the front's direct proxy reach, as
// Sub2API, the router and DMIT see it.
type reverseEntryChain struct {
	origin  *reverseOrigin
	ca      *authority
	front   *front
	back    *httptest.Server
	dec     *decoder
	reverse *reverseFront
	lanes   *laneSet
	cfg     reverseConfig
	// reverseServer serves the entry; reverseDone is what start returned.
	reverseServer *http.Server
	reverseDone   <-chan error
	// reverseURL is the plain HTTP entry; direct is a client that reaches it.
	reverseURL *url.URL
	client     *http.Client
	// originsFile and authFile are the files SIGHUP reloads.
	originsFile, authFile string
}

const reverseTestHost = "example.com"

// newReverseEntryChain builds the chain; advertise is the list the decoder
// puts in /health, and nil stands for an older decoder that advertises none.
func newReverseEntryChain(t *testing.T, advertise domainList) *reverseEntryChain {
	t.Helper()
	c := &reverseEntryChain{origin: newReverseOrigin(t)}
	originAddr := c.origin.srv.Listener.Addr().String()
	originRoots := x509.NewCertPool()
	originRoots.AddCert(c.origin.srv.Certificate())
	toOrigin := route{
		tls:     &tls.Config{RootCAs: originRoots},
		resolve: map[string]string{reverseTestHost + ":443": originAddr},
	}

	c.dec = newDecoder(toOrigin, 64<<20, 64<<20)
	c.dec.hosts = advertise
	c.back = httptest.NewUnstartedServer(c.dec)
	c.back.Config = newDecoderServer(c.dec)
	c.back.Start()
	t.Cleanup(c.back.Close)
	backURL := mustURL(t, c.back.URL)

	dir := t.TempDir()
	if err := createCA(dir, domainList{reverseTestHost}); err != nil {
		t.Fatal(err)
	}
	var err error
	if c.ca, err = loadAuthority(dir); err != nil {
		t.Fatal(err)
	}
	c.originsFile = filepath.Join(dir, "reverse-origins")
	c.authFile = filepath.Join(dir, "reverse.auth")
	writeFile(t, c.originsFile, "# upstreams\n"+reverseTestHost+"\nsub."+reverseTestHost+"\n")
	writeFile(t, c.authFile, "revuser:revpass\n")

	dial := func(ctx context.Context, dest M.Socksaddr) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", originAddr)
	}
	newTestLane := func(name string) *lane {
		enc := newEncoder(route{url: backURL}, 32<<20, 64<<20)
		direct := newProxy(toOrigin)
		enc.origin = direct.Transport
		enc.probe(context.Background(), backURL)
		if !enc.healthy() {
			t.Fatal("the probe did not find the decoder")
		}
		return &lane{name: name, enc: enc, direct: direct, dial: dial}
	}
	def := newTestLane("")
	c.lanes = newLaneSet(maxLanes, func(user, _ string) *lane {
		if user == "" {
			return def
		}
		return newTestLane(user)
	})
	c.front = newFront(domainList{reverseTestHost}, c.ca, c.lanes)
	c.cfg = reverseConfig{origins: c.originsFile, auth: c.authFile, allow: "127.0.0.0/8,::1"}
	if c.reverse, err = newReverseFront(c.front, c.cfg); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); c.front.conns.Close() })
	// The entry as runFront serves it.
	c.reverseServer, c.reverseDone = c.reverse.start(ln)
	t.Cleanup(func() { c.reverseServer.Close() })
	c.reverseURL = mustURL(t, "http://"+ln.Addr().String())
	c.client = &http.Client{}
	return c
}

// reverseLane is the lane -reverse-auth names.
func (c *reverseEntryChain) reverseLane() *lane { return c.lanes.get("revuser", "revpass") }

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// post sends "<origin>/<path>" through the reverse entry and returns the
// answer's status after draining it.
func (c *reverseEntryChain) post(t *testing.T, origin, path string, body []byte) int {
	t.Helper()
	resp, err := c.client.Post(c.reverseURL.String()+"/"+origin+path, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// The reverse entry frames through a real encoder and decoder to an HTTPS
// origin: the body arrives byte for byte, later turns are mostly references,
// and the origin sees the rewritten path and Host.
func TestFrontReverseFramesThroughTheDecoder(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	var total int
	for turn := 1; turn <= 3; turn++ {
		body := conversation(turn, images)
		total += len(body)
		if status := c.post(t, reverseTestHost, "/v1/chat/completions?x=1", body); status != http.StatusOK {
			t.Fatalf("turn %d: status %d", turn, status)
		}
		got, path := c.origin.last()
		if string(got) != string(body) {
			t.Fatalf("turn %d: origin got %d bytes, want %d identical", turn, len(got), len(body))
		}
		if path != "/v1/chat/completions?x=1" {
			t.Fatalf("turn %d: origin got path %q", turn, path)
		}
	}
	c.origin.mu.Lock()
	host := c.origin.hosts[len(c.origin.hosts)-1]
	c.origin.mu.Unlock()
	if host != reverseTestHost {
		t.Fatalf("origin got Host %q, want %q", host, reverseTestHost)
	}
	l := c.reverseLane()
	if l == nil {
		t.Fatal("the reverse request did not use its -reverse-auth lane")
	}
	snap := l.enc.snapshot()
	if snap["framed"].(int64) != 3 {
		t.Fatalf("want three frames through the decoder: %v", snap)
	}
	if ratio := float64(total) / float64(snap["sent_bytes"].(int64)); ratio < 1.8 {
		t.Fatalf("ratio %.2f too low: %v", ratio, snap)
	}
	if s := c.front.snapshot(); s["reverse_requests"].(int64) != 3 || s["reverse_direct"].(int64) != 0 || s["reverse_refused"].(int64) != 0 {
		t.Fatalf("want three framed reverse requests and nothing else: %v", s)
	}
}

// An origin outside -reverse-origins answers 421 and reaches nothing.
func TestFrontReverseRefusesOriginsOutsideTheList(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	if status := c.post(t, "other.test", "/v1/models", []byte("{}")); status != http.StatusMisdirectedRequest {
		t.Fatalf("status %d, want 421", status)
	}
	if !c.reverse.origins.match("sub."+reverseTestHost) || c.reverse.origins.match("other.test") {
		t.Fatalf("origin list %v does not match suffixes the way x509 constraints do", c.reverse.origins)
	}
	if s := c.front.snapshot(); s["reverse_refused"].(int64) != 1 || s["reverse_requests"].(int64) != 1 {
		t.Fatalf("want one refused reverse request: %v", s)
	}
}

// A client outside -reverse-allow is refused and counted.
func TestFrontReverseRefusesSourcesOutsideTheAllowlist(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	nets, err := parseCIDRs("10.0.0.0/8,192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	c.reverse.allow = nets
	if status := c.post(t, reverseTestHost, "/v1/models", []byte("{}")); status != http.StatusForbidden {
		t.Fatalf("status %d, want 403", status)
	}
	if s := c.front.snapshot(); s["reverse_refused"].(int64) != 1 || s["reverse_direct"].(int64) != 0 {
		t.Fatalf("want one refusal and nothing forwarded: %v", s)
	}
	// The test's loopback client is served once its network is allowed.
	c.reverse.allow = nets[:1]
	if !c.reverse.allows("10.1.2.3:1234") || c.reverse.allows("127.0.0.1:1234") {
		t.Fatal("the allowlist does not decide by source address")
	}
	c.reverse.allow = append(nets, mustNet(t, "127.0.0.0/8"))
	if status := c.post(t, reverseTestHost, "/v1/models", []byte("{}")); status != http.StatusOK {
		t.Fatalf("allowed source answered %d", status)
	}
}

func mustNet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	nets, err := parseCIDRs(cidr)
	if err != nil || len(nets) != 1 {
		t.Fatalf("bad network %q", cidr)
	}
	return nets[0]
}

// With no decoder answering, the request still completes through the lane's
// direct proxy: through the anytls exit, without dedup.
func TestFrontReverseGoesDirectWhenDecoderIsDown(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	// Make the lane in this goroutine, before the decoder goes away: the
	// probe of a lane made later would find nothing.
	l := c.reverseLane()
	c.back.Close()
	l.enc.pause("test", errStatus(503))
	body := conversation(1, []string{image(100 << 10)})
	if status := c.post(t, reverseTestHost, "/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("status %d with no decoder", status)
	}
	if got, _ := c.origin.last(); string(got) != string(body) {
		t.Fatal("origin got a different body")
	}
	s := c.front.snapshot()
	if s["reverse_direct"].(int64) != 1 || s["reverse_requests"].(int64) != 1 {
		t.Fatalf("want one direct reverse request: %v", s)
	}
	if snap := c.reverseLane().enc.snapshot(); snap["framed"].(int64) != 0 {
		t.Fatalf("nothing should have been framed: %v", snap)
	}
}

// An origin the decoder does not advertise goes direct, unframed, so a link
// works as soon as the front lists it.
func TestFrontReverseGoesDirectWhenDecoderDoesNotAdvertiseTheOrigin(t *testing.T) {
	c := newReverseEntryChain(t, domainList{"chatgpt.com"})
	if c.reverseLane().enc.accepts(reverseTestHost, c.front.domains) {
		t.Fatal("the encoder accepted an origin the decoder did not advertise")
	}
	body := conversation(1, []string{image(300 << 10)})
	if status := c.post(t, reverseTestHost, "/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if got, _ := c.origin.last(); string(got) != string(body) {
		t.Fatal("origin got a different body")
	}
	if snap := c.reverseLane().enc.snapshot(); snap["framed"].(int64) != 0 || snap["plain"].(int64) != 0 {
		t.Fatalf("the decoder did not advertise the origin, yet the encoder saw it: %v", snap)
	}
	if s := c.front.snapshot(); s["reverse_direct"].(int64) != 1 {
		t.Fatalf("want one direct reverse request: %v", s)
	}
}

// An older decoder that advertises no list keeps today's behaviour: the
// front's own intercept list decides.
func TestFrontReverseUsesItsOwnListWhenDecoderAdvertisesNone(t *testing.T) {
	c := newReverseEntryChain(t, nil)
	if c.reverseLane().enc.decoderDomains.Load() != nil {
		t.Fatal("a decoder that advertised nothing left a list behind")
	}
	body := conversation(1, []string{image(300 << 10)})
	if status := c.post(t, reverseTestHost, "/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if snap := c.reverseLane().enc.snapshot(); snap["framed"].(int64) != 1 {
		t.Fatalf("a decoder without a list should fall back to -domains: %v", snap)
	}
	if c.reverseLane().enc.accepts("elsewhere.test", c.front.domains) {
		t.Fatal("an origin outside -domains was accepted")
	}
}

// SIGHUP adds and removes origins without a restart, and a file that does not
// parse keeps the list in use.
func TestFrontReverseReloadsOriginsOnHangup(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	writeFile(t, c.originsFile, "sub."+reverseTestHost+"\n")
	if err := c.reverse.reload(c.cfg); err != nil {
		t.Fatal(err)
	}
	if status := c.post(t, reverseTestHost, "/v1/models", []byte("{}")); status != http.StatusMisdirectedRequest {
		t.Fatalf("a removed origin answered %d, want 421", status)
	}
	writeFile(t, c.originsFile, reverseTestHost+"\n")
	if err := c.reverse.reload(c.cfg); err != nil {
		t.Fatal(err)
	}
	if status := c.post(t, reverseTestHost, "/v1/models", []byte("{}")); status != http.StatusOK {
		t.Fatalf("an origin added without a restart answered %d", status)
	}
	writeFile(t, c.originsFile, "*.bad.test\n")
	if err := c.reverse.reload(c.cfg); err == nil {
		t.Fatal("a wildcard origin was accepted")
	}
	if status := c.post(t, reverseTestHost, "/v1/models", []byte("{}")); status != http.StatusOK {
		t.Fatalf("a broken file dropped the origins in use: %d", status)
	}
	// The auth file reloads too, and a broken one changes nothing.
	writeFile(t, c.originsFile, reverseTestHost+"\n")
	writeFile(t, c.authFile, "second:secret\n")
	if err := c.reverse.reload(c.cfg); err != nil {
		t.Fatal(err)
	}
	if a := *c.reverse.auth.Load(); a != [2]string{"second", "secret"} {
		t.Fatalf("auth after the reload is %q", a)
	}
	writeFile(t, c.authFile, "no-colon\n")
	if err := c.reverse.reload(c.cfg); err == nil {
		t.Fatal("an auth file without a colon was accepted")
	}
	if a := *c.reverse.auth.Load(); a != [2]string{"second", "secret"} {
		t.Fatalf("a failed auth reload changed the lane to %q", a)
	}
	if c.lanes.get("second", "secret") == nil {
		t.Fatal("the reloaded credentials do not name a lane")
	}
}

// A streamed answer is not buffered: the first event arrives long before the
// origin finishes the reply.
func TestFrontReverseStreamsRepliesUnbuffered(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	body := conversation(1, []string{image(300 << 10)})
	resp, err := c.client.Post(c.reverseURL.String()+"/"+reverseTestHost+"/stream", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		first <- string(buf[:n])
	}()
	select {
	case got := <-first:
		if !strings.Contains(got, "data: first") {
			t.Fatalf("first bytes %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event was held back until the reply ended")
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("the rest of the reply is %q", rest)
	}
}

// A WebSocket upgrade always takes the direct proxy.
func TestFrontReverseUpgradeGoesDirect(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	conn, err := net.Dial("tcp", c.reverseURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	body := conversation(1, []string{image(300 << 10)})
	fmt.Fprintf(conn, "POST /%s/ws HTTP/1.1\r\nHost: 192.0.2.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nContent-Length: %d\r\n\r\n%s",
		reverseTestHost, len(body), body)
	br := std_bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %d, want 101", resp.StatusCode)
	}
	fmt.Fprint(conn, "hello\n")
	if line, err := br.ReadString('\n'); err != nil || line != "echo hello\n" {
		t.Fatalf("after the upgrade read %q, %v", line, err)
	}
	if s := c.front.snapshot(); s["reverse_direct"].(int64) != 1 {
		t.Fatalf("an upgrade must go direct: %v", s)
	}
	if snap := c.reverseLane().enc.snapshot(); snap["framed"].(int64) != 0 {
		t.Fatalf("an upgrade must not be framed: %v", snap)
	}
}

// Reverse and intercepted traffic of the same user:password share one lane:
// the same encoder, index and epoch, so references stay valid across them.
func TestFrontReverseSharesTheLaneWithInterceptedTraffic(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	body := conversation(1, []string{image(300 << 10)})
	if status := c.post(t, reverseTestHost, "/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	l := c.reverseLane()
	if l == nil {
		t.Fatal("no lane for the reverse credentials")
	}
	// Intercepted traffic carrying the same credentials picks this lane.
	if same := c.lanes.get("revuser", "revpass"); same != l || same.enc != l.enc {
		t.Fatal("intercepted traffic with the same credentials got another lane")
	}
	if l.enc.lastEpoch.Load().(string) == "" || l.enc.epoch.Load().(string) != l.enc.lastEpoch.Load().(string) {
		t.Fatalf("the shared lane has no stable epoch: %v", l.enc.snapshot())
	}
	if snap := l.enc.snapshot(); snap["framed"].(int64) != 1 {
		t.Fatalf("want the reverse request framed once: %v", snap)
	}
	// The default lane keeps its own index.
	if def := c.lanes.get("", ""); def == l {
		t.Fatal("the reverse request used the default lane")
	}
}

// The auth file is required: a front asked for -reverse without one refuses
// to start.
func TestFrontReverseRequiresAuthAndValidOrigins(t *testing.T) {
	if _, _, err := readAuth(""); err == nil {
		t.Fatal("an empty -reverse-auth was accepted")
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if _, _, err := readAuth(missing); err == nil {
		t.Fatal("a missing auth file was accepted")
	}
	bad := filepath.Join(dir, "bad")
	writeFile(t, bad, "nocolon\n")
	if _, _, err := readAuth(bad); err == nil {
		t.Fatal("an auth file without a colon was accepted")
	}
	if _, err := newReverseFront(nil, reverseConfig{auth: bad}); err == nil {
		t.Fatal("a front with only a bad auth file was built")
	}
}

// The client's escaping reaches the origin unchanged, framed or direct: an
// encoded slash stays one path segment.
func TestFrontReverseKeepsEscapedPaths(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	body := conversation(1, []string{image(100 << 10)})
	if status := c.post(t, reverseTestHost, "/v1/files/a%2Fb?x=1", body); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	c.origin.mu.Lock()
	got := c.origin.paths[len(c.origin.paths)-1]
	c.origin.mu.Unlock()
	if got != "/v1/files/a%2Fb?x=1" {
		t.Fatalf("origin saw %q, want /v1/files/a%%2Fb?x=1", got)
	}
}

// Plain HTTP carrying API keys must name its clients: no -reverse-allow, no
// reverse entry.
func TestFrontReverseRequiresAnAllowlist(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	cfg := c.cfg
	cfg.allow = ""
	if _, err := newReverseFront(c.front, cfg); err == nil {
		t.Fatal("a reverse entry without -reverse-allow was built")
	}
}

// Stopping the front lets a reply in flight through the reverse entry finish,
// and ends the entry cleanly rather than as an error, which would exit the
// program and cut every open request.
func TestFrontReverseDrainsOnStop(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	body := conversation(1, []string{image(100 << 10)})
	resp, err := c.client.Post(c.reverseURL.String()+"/"+reverseTestHost+"/stream", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if n, _ := resp.Body.Read(buf); !strings.Contains(string(buf[:n]), "data: first") {
		t.Fatalf("first bytes %q", buf[:n])
	}
	main, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopFront(ctx, main, c.reverseServer, c.front)
		close(stopped)
	}()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(rest), "data: second") {
		t.Fatalf("the reply in flight was cut: %q, %v", rest, err)
	}
	select {
	case err := <-c.reverseDone:
		if err != nil {
			t.Fatalf("the entry ended with %v, which runFront treats as fatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the entry did not end after the drain")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopFront did not return")
	}
}

// The entry forwards to exactly the hosts it lists, not to every name under
// them.
func TestFrontReverseMatchesOriginsExactly(t *testing.T) {
	c := newReverseEntryChain(t, domainList{reverseTestHost})
	body := conversation(1, []string{image(10 << 10)})
	if status := c.post(t, "other."+reverseTestHost, "/v1/chat/completions", body); status != http.StatusMisdirectedRequest {
		t.Fatalf("an unlisted name under a listed one answered %d, want 421", status)
	}
	if status := c.post(t, "SUB."+reverseTestHost, "/v1/models", nil); status == http.StatusMisdirectedRequest {
		t.Fatal("a listed host in other case was refused")
	}
}
