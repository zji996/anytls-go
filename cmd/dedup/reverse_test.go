package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reverseChain is client -> encode -> decode (routed) -> two services, as
// Caddy, the frp tunnel and the LAN services see it.
type reverseChain struct {
	a, b    *origin
	enc     *encoder
	dec     *decoder
	routes  string
	encSrv  *httptest.Server
	decAddr string
}

func newReverseChain(t *testing.T) *reverseChain {
	t.Helper()
	c := &reverseChain{a: &origin{}, b: &origin{}}
	aSrv, bSrv := httptest.NewServer(c.a), httptest.NewServer(c.b)
	c.routes = filepath.Join(t.TempDir(), "routes")
	writeRoutes(t, c.routes, "# services\na.example "+aSrv.URL+"\nb.example "+bSrv.URL+"\n")
	table := &routeTable{}
	if err := table.load(c.routes); err != nil {
		t.Fatal(err)
	}
	c.dec = newDecoder(route{table: table}, 64<<20, 64<<20)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	decSrv := newDecoderServer(c.dec)
	go decSrv.Serve(ln)
	c.decAddr = "http://" + ln.Addr().String()
	c.enc = newEncoder(direct(t, c.decAddr), 60<<20, 64<<20)
	c.enc.probe(context.Background(), mustURL(t, c.decAddr))
	c.encSrv = httptest.NewServer(c.enc)
	t.Cleanup(func() { c.encSrv.Close(); decSrv.Close(); aSrv.Close(); bSrv.Close() })
	return c
}

func writeRoutes(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (c *reverseChain) post(t *testing.T, host string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.encSrv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(got)
}

func TestReverseRoutesEachHostToItsService(t *testing.T) {
	c := newReverseChain(t)
	if !c.enc.healthy() || !c.enc.useH2.Load() {
		t.Fatal("the encoder did not find the decoder over HTTP/2 through its upstream")
	}
	images := []string{image(300 << 10), image(300 << 10), image(300 << 10)}
	var total int
	for turn := 1; turn <= 3; turn++ {
		for _, host := range []string{"a.example", "b.example:443"} {
			body := conversation(turn, images)
			total += len(body)
			if status, got := c.post(t, host, body); status != 200 || got != fmt.Sprintf("ok %d", len(body)) {
				t.Fatalf("%s: status %d body %q", host, status, got)
			}
		}
	}
	if len(c.a.bodies) != 3 || len(c.b.bodies) != 3 {
		t.Fatalf("services got %d and %d requests, want 3 each", len(c.a.bodies), len(c.b.bodies))
	}
	if !bytes.Equal(c.a.last(), conversation(3, images)) || c.a.hosts[2] != "a.example" {
		t.Fatalf("service a got a different body or Host %q", c.a.hosts[2])
	}
	s := c.enc.snapshot()
	if ratio := float64(total) / float64(s["sent_bytes"].(int64)); s["framed"].(int64) != 6 || ratio < 3 {
		t.Fatalf("want 6 frames at a ratio over 3, got %.2f: %v", ratio, s)
	}
}

func TestReverseRefusesUnroutedHostsAndReloads(t *testing.T) {
	c := newReverseChain(t)
	body := conversation(1, []string{image(100 << 10)})
	if status, _ := c.post(t, "c.example", body); status != http.StatusForbidden {
		t.Fatalf("unrouted host answered %d, want 403", status)
	}
	if c.enc.snapshot()["retries"].(int64) != 0 || !c.enc.healthy() {
		t.Fatal("a refused host should neither be resent nor pause framing")
	}
	other := &origin{}
	srv := httptest.NewServer(other)
	defer srv.Close()
	writeRoutes(t, c.routes, "a.example http://127.0.0.1:1\nc.example "+srv.URL+"\n")
	if err := c.dec.routes.load(c.routes); err != nil {
		t.Fatal(err)
	}
	if status, _ := c.post(t, "c.example", body); status != 200 || len(other.bodies) != 1 {
		t.Fatalf("reloaded route: status %d, service got %d requests", status, len(other.bodies))
	}
	if status, _ := c.post(t, "b.example", body); status != http.StatusForbidden {
		t.Fatalf("removed route answered %d, want 403", status)
	}
	// A file that does not parse keeps the routes in use.
	writeRoutes(t, c.routes, "c.example https://elsewhere\n")
	if err := c.dec.routes.load(c.routes); err == nil {
		t.Fatal("an https service was accepted")
	}
	if c.dec.routes.lookup("C.example:8443") == nil {
		t.Fatal("a failed reload dropped the routes in use")
	}
}

func TestParseRoutesRejectsMistakes(t *testing.T) {
	for _, text := range []string{
		"a.example",
		"a.example http://x:1 extra",
		"a.example https://x:1",
		"a.example http://x:1/path",
		"a.example http://x:1\nA.example http://y:1",
		"*.example http://x:1",
	} {
		if _, err := parseRoutes(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
	m, err := parseRoutes("\n# none yet\n")
	if err != nil || len(m) != 0 {
		t.Fatalf("an empty table: %v %v", m, err)
	}
}

// A WebSocket crosses on an HTTP/1.1 connection of its own while frames share
// HTTP/2, and framing stays on.
func TestReverseCarriesWebSocketBesideHTTP2(t *testing.T) {
	c := newReverseChain(t)
	if !c.enc.useH2.Load() {
		t.Fatal("the encoder did not find the decoder over HTTP/2")
	}
	conn, err := net.Dial("tcp", c.encSrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: a.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(conn)
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
	if !c.enc.healthy() || !c.enc.useH2.Load() {
		t.Fatal("a WebSocket paused framing or turned HTTP/2 off")
	}
	body := conversation(1, []string{image(100 << 10)})
	if status, _ := c.post(t, "a.example", body); status != 200 || c.enc.snapshot()["framed"].(int64) != 1 {
		t.Fatalf("after the WebSocket: status %d, %v", status, c.enc.snapshot())
	}
}

// A client that gives up on a slow answer does not pause framing.
func TestReverseClientCancelKeepsFraming(t *testing.T) {
	c := newReverseChain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	body := conversation(1, []string{image(100 << 10)})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.encSrv.URL+"/hang", bytes.NewReader(body))
	req.Host = "a.example"
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the hanging request answered")
	}
	// The encoder sees the cancel after the client does.
	deadline := time.Now().Add(2 * time.Second)
	for c.enc.snapshot()["framed"].(int64) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if !c.enc.healthy() {
		t.Fatal("a client cancel paused framing")
	}
}
