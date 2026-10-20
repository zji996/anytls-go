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
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	shttp "github.com/sagernet/sing/protocol/http"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

// front is the router side. It takes SOCKS5 and HTTP proxy connections and
// terminates TLS to allowlisted names on port 443 with leaves from the private
// CA, so the encoder sees their requests. Every other connection, and every
// connection while no decoder answers, is relayed unchanged through its lane.
type front struct {
	domains domainList
	ca      *authority
	lanes   *laneSet

	tlsConfig *tls.Config
	server    *http.Server
	conns     *connListener
	closed    sync.Map // *tls.Conn -> chan struct{}
	connLanes sync.Map // *tls.Conn -> *lane

	// maxConns bounds open proxy connections; 0 is no limit.
	maxConns                     int
	open                         sync.WaitGroup
	intercepted, passed, refused atomic.Int64
}

const peekTimeout = 10 * time.Second

// lane is the way to one exit. Every exit's decoder holds different chunks, so
// each lane has its own encoder.
type lane struct {
	name string
	enc  *encoder
	// direct serves intercepted requests while the decoder is down.
	direct *httputil.ReverseProxy
	dial   func(ctx context.Context, dest M.Socksaddr) (net.Conn, error)
}

// laneSet makes a lane on first use for each SOCKS5 username and password,
// which name the exit. A router steering traffic of several nodes to one front
// gives each node's connections its own username, and the front dials -via
// with the same one, so the node and with it the exit IP stay the same.
type laneSet struct {
	mu    sync.Mutex
	lanes map[[2]string]*lane
	max   int
	make  func(user, pass string) *lane
}

func newLaneSet(max int, make func(user, pass string) *lane) *laneSet {
	return &laneSet{lanes: map[[2]string]*lane{}, max: max, make: make}
}

// get returns nil past max lanes.
func (s *laneSet) get(user, pass string) *lane {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{user, pass}
	if l, ok := s.lanes[key]; ok {
		return l
	}
	if len(s.lanes) >= s.max {
		return nil
	}
	l := s.make(user, pass)
	s.lanes[key] = l
	return l
}

func (s *laneSet) all() []*lane {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]*lane, 0, len(s.lanes))
	for _, l := range s.lanes {
		all = append(all, l)
	}
	return all
}

type laneKey struct{}

func newFront(domains domainList, ca *authority, lanes *laneSet) *front {
	f := &front{domains: domains, ca: ca, lanes: lanes, conns: newConnListener()}
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
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			l, _ := f.connLanes.Load(c)
			return context.WithValue(ctx, laneKey{}, l)
		},
		ErrorLog: log.Default(),
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
	if first[0] == socks5.Version {
		err = f.serveSOCKS5(ctx, c, reader)
	} else {
		md := M.Metadata{Source: M.SocksaddrFromNet(c.RemoteAddr()), Destination: M.SocksaddrFromNet(c.LocalAddr())}
		err = shttp.HandleConnection(ctx, c, reader, nil, f, md)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		log.Printf("proxy client %s: %v", c.RemoteAddr(), err)
	}
}

var errTooManyLanes = errors.New("too many lanes")

// serveSOCKS5 answers a SOCKS5 CONNECT. Unlike sing's server it takes any
// username and password: they name the lane, and -via is the one to check them.
func (f *front) serveSOCKS5(ctx context.Context, c net.Conn, r *std_bufio.Reader) error {
	if _, err := r.ReadByte(); err != nil {
		return err
	}
	methods, err := socks5.ReadAuthRequest0(r)
	if err != nil {
		return err
	}
	method := socks5.AuthTypeNotRequired
	if slices.Contains(methods.Methods, socks5.AuthTypeUsernamePassword) {
		method = socks5.AuthTypeUsernamePassword
	} else if !slices.Contains(methods.Methods, socks5.AuthTypeNotRequired) {
		socks5.WriteAuthResponse(c, socks5.AuthResponse{Method: socks5.AuthTypeNoAcceptedMethods})
		return errors.New("socks5: no acceptable auth method")
	}
	if err := socks5.WriteAuthResponse(c, socks5.AuthResponse{Method: method}); err != nil {
		return err
	}
	var user, pass string
	if method == socks5.AuthTypeUsernamePassword {
		up, err := socks5.ReadUsernamePasswordAuthRequest(r)
		if err != nil {
			return err
		}
		user, pass = up.Username, up.Password
	}
	l := f.lanes.get(user, pass)
	if method == socks5.AuthTypeUsernamePassword {
		status := socks5.UsernamePasswordStatusSuccess
		if l == nil {
			status = socks5.UsernamePasswordStatusFailure
		}
		if err := socks5.WriteUsernamePasswordAuthResponse(c, socks5.UsernamePasswordAuthResponse{Status: status}); err != nil {
			return err
		}
	}
	if l == nil {
		return errTooManyLanes
	}
	req, err := socks5.ReadRequest(r)
	if err != nil {
		return err
	}
	if req.Command != socks5.CommandConnect {
		socks5.WriteResponse(c, socks5.Response{ReplyCode: socks5.ReplyCodeUnsupported})
		return errors.New("socks5: only CONNECT is supported")
	}
	if err := socks5.WriteResponse(c, socks5.Response{ReplyCode: socks5.ReplyCodeSuccess, Bind: M.SocksaddrFromNet(c.LocalAddr())}); err != nil {
		return err
	}
	return f.connect(ctx, &prefixConn{Conn: c, r: r}, req.Destination, l)
}

// NewConnection takes HTTP proxy connections, which go to the default lane.
func (f *front) NewConnection(ctx context.Context, conn net.Conn, md M.Metadata) error {
	l := f.lanes.get("", "")
	if l == nil {
		return errTooManyLanes
	}
	return f.connect(ctx, conn, md.Destination, l)
}

// connect is called with the proxy handshake done; conn now carries the
// client's bytes for dest.
func (f *front) connect(ctx context.Context, conn net.Conn, dest M.Socksaddr, l *lane) error {
	if dest.Port == 443 {
		conn.SetReadDeadline(time.Now().Add(peekTimeout))
		peeked, name := peekServerName(conn)
		conn.SetReadDeadline(time.Time{})
		conn = &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(peeked), conn)}
		if name != "" && f.domains.match(name) && l.enc.healthy() {
			f.intercepted.Add(1)
			return f.intercept(conn, l)
		}
	}
	f.passed.Add(1)
	remote, err := l.dial(ctx, dest)
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
func (f *front) intercept(conn net.Conn, l *lane) error {
	tlsConn := tls.Server(conn, f.tlsConfig)
	done := make(chan struct{})
	f.closed.Store(tlsConn, done)
	f.connLanes.Store(tlsConn, l)
	if !f.conns.push(tlsConn) {
		f.closed.Delete(tlsConn)
		f.connLanes.Delete(tlsConn)
		return net.ErrClosed
	}
	<-done
	return nil
}

func (f *front) connState(c net.Conn, state http.ConnState) {
	if state != http.StateClosed && state != http.StateHijacked {
		return
	}
	f.connLanes.Delete(c)
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
	l := r.Context().Value(laneKey{}).(*lane)
	if l.enc.healthy() {
		l.enc.ServeHTTP(w, r)
		return
	}
	l.direct.ServeHTTP(w, r)
}

// snapshot sums the lanes' counters and lists each lane, the default one as "".
func (f *front) snapshot() map[string]any {
	s := map[string]any{}
	lanes := map[string]any{}
	for _, l := range f.lanes.all() {
		ls := l.enc.snapshot()
		delete(ls, "role")
		lanes[l.name] = ls
		for k, v := range ls {
			if n, ok := v.(int64); ok {
				total, _ := s[k].(int64)
				s[k] = total + n
			}
		}
	}
	orig, _ := s["original_bytes"].(int64)
	sent, _ := s["sent_bytes"].(int64)
	if sent > 0 {
		s["ratio"] = float64(orig) / float64(sent)
	}
	s["inflight_bytes"] = int64(0)
	if all := f.lanes.all(); len(all) > 0 {
		s["inflight_bytes"] = all[0].enc.inflight.used.Load()
	}
	s["role"] = "front"
	s["lanes"] = lanes
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
