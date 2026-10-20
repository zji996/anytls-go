package main

import (
	std_bufio "bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	shttp "github.com/sagernet/sing/protocol/http"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

// front is the router side. It takes SOCKS5 and HTTP proxy connections and
// terminates TLS to allowlisted names on port 443 with leaves from the private
// CA, so the encoder sees their requests. Every other connection, and every
// connection while no decoder answers, is relayed unchanged through dial.
type front struct {
	domains domainList
	ca      *authority
	dial    func(ctx context.Context, dest M.Socksaddr) (net.Conn, error)
	enc     *encoder
	// direct serves intercepted requests while the decoder is down.
	direct *httputil.ReverseProxy

	tlsConfig *tls.Config
	server    *http.Server
	conns     *connListener
	closed    sync.Map // *tls.Conn -> chan struct{}

	// maxConns bounds open proxy connections; 0 is no limit.
	maxConns                     int
	open                         sync.WaitGroup
	intercepted, passed, refused atomic.Int64
}

const peekTimeout = 10 * time.Second

func newFront(domains domainList, ca *authority, dial func(context.Context, M.Socksaddr) (net.Conn, error), enc *encoder, direct *httputil.ReverseProxy) *front {
	f := &front{domains: domains, ca: ca, dial: dial, enc: enc, direct: direct, conns: newConnListener()}
	f.tlsConfig = &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return ca.leaf(hello.ServerName)
		},
	}
	f.server = &http.Server{
		Handler:           f,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ConnState:         f.connState,
		ErrorLog:          log.Default(),
		// Clients are on the LAN, where 256 KiB windows still run at hundreds
		// of MB/s; the 1 MiB default would let each connection buffer 1 MiB.
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerConnection: 256 << 10,
			MaxReceiveBufferPerStream:     256 << 10,
		},
	}
	// Serve speaks HTTP/2 on *tls.Conn connections that negotiated h2.
	go f.server.Serve(f.conns)
	return f
}

// serve accepts proxy clients until ln fails.
func (f *front) serve(ln net.Listener) error {
	var slots chan struct{}
	if f.maxConns > 0 {
		slots = make(chan struct{}, f.maxConns)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		if slots == nil {
			f.open.Add(1)
			go f.handle(c)
			continue
		}
		select {
		case slots <- struct{}{}:
			f.open.Add(1)
			go func() {
				defer func() { <-slots }()
				f.handle(c)
			}()
		default:
			if n := f.refused.Add(1); n&(n-1) == 0 {
				log.Printf("%d connections open; refused %d so far, now %s", f.maxConns, n, c.RemoteAddr())
			}
			c.Close()
		}
	}
}

// shutdown stops serving intercepted connections once their requests end and
// waits, until ctx is done, for those and the relayed ones to close.
func (f *front) shutdown(ctx context.Context) {
	f.server.Shutdown(ctx)
	closed := make(chan struct{})
	go func() {
		f.open.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
	}
}

func (f *front) handle(c net.Conn) {
	defer f.open.Done()
	defer c.Close()
	// One client's connection must not take the proxy down for all others.
	defer func() {
		if p := recover(); p != nil {
			log.Printf("proxy client %s: panic: %v\n%s", c.RemoteAddr(), p, debug.Stack())
		}
	}()
	reader := std_bufio.NewReader(c)
	first, err := reader.Peek(1)
	if err != nil {
		return
	}
	ctx := context.Background()
	md := M.Metadata{Source: M.SocksaddrFromNet(c.RemoteAddr()), Destination: M.SocksaddrFromNet(c.LocalAddr())}
	if first[0] == socks5.Version {
		err = socks.HandleConnection0(ctx, c, reader, nil, f, md)
	} else {
		err = shttp.HandleConnection(ctx, c, reader, nil, f, md)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		log.Printf("proxy client %s: %v", c.RemoteAddr(), err)
	}
}

// NewConnection is called with the proxy handshake done; conn now carries the
// client's bytes for dest.
func (f *front) NewConnection(ctx context.Context, conn net.Conn, md M.Metadata) error {
	dest := md.Destination
	if dest.Port == 443 {
		conn.SetReadDeadline(time.Now().Add(peekTimeout))
		peeked, name := peekServerName(conn)
		conn.SetReadDeadline(time.Time{})
		conn = &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(peeked), conn)}
		if name != "" && f.domains.match(name) && f.enc.healthy() {
			f.intercepted.Add(1)
			return f.intercept(conn)
		}
	}
	f.passed.Add(1)
	remote, err := f.dial(ctx, dest)
	if err != nil {
		return err
	}
	defer remote.Close()
	return bufio.CopyConn(ctx, conn, remote)
}

func (f *front) NewPacketConnection(context.Context, N.PacketConn, M.Metadata) error {
	return errors.New("UDP is not supported")
}

// intercept hands conn to the HTTP server as TLS and waits until the server
// is done with it, because the proxy handler closes conn on return.
func (f *front) intercept(conn net.Conn) error {
	tlsConn := tls.Server(conn, f.tlsConfig)
	done := make(chan struct{})
	f.closed.Store(tlsConn, done)
	if !f.conns.push(tlsConn) {
		f.closed.Delete(tlsConn)
		return net.ErrClosed
	}
	<-done
	return nil
}

func (f *front) connState(c net.Conn, state http.ConnState) {
	if state != http.StateClosed && state != http.StateHijacked {
		return
	}
	if done, ok := f.closed.LoadAndDelete(c); ok {
		close(done.(chan struct{}))
	}
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The SNI was allowlisted; refuse a Host that tries to reach elsewhere.
	if !f.domains.match(hostname(r.Host)) {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	if f.enc.healthy() {
		f.enc.ServeHTTP(w, r)
		return
	}
	f.direct.ServeHTTP(w, r)
}

func (f *front) snapshot() map[string]any {
	s := f.enc.snapshot()
	s["role"] = "front"
	s["intercepted_conns"] = f.intercepted.Load()
	s["passed_conns"] = f.passed.Load()
	s["refused_conns"] = f.refused.Load()
	return s
}

var errPeeked = errors.New("client hello peeked")

// peekServerName reads the client's first TLS record far enough to learn the
// SNI, returning the bytes consumed so they can be replayed.
func peekServerName(c net.Conn) (peeked []byte, name string) {
	rec := &recordingConn{Conn: c}
	tls.Server(rec, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name = hello.ServerName
			return nil, errPeeked
		},
	}).Handshake()
	return rec.buf.Bytes(), name
}

// recordingConn keeps what is read and discards what is written, so a TLS
// server can parse a ClientHello without answering it.
type recordingConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.buf.Write(p[:n])
	return n, err
}

func (c *recordingConn) Write(p []byte) (int, error) { return len(p), nil }

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// connListener feeds intercepted connections to an http.Server.
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnListener() *connListener {
	return &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *connListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
