package main

import (
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sirupsen/logrus"
)

const initialConnectionTimeout = 10 * time.Second

func handleTcpConnection(ctx context.Context, c net.Conn, s *myServer) {
	defer func() {
		if r := recover(); r != nil {
			logrus.Errorln("[BUG]", r, string(debug.Stack()))
		}
	}()
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	if err := c.SetDeadline(time.Now().Add(initialConnectionTimeout)); err != nil {
		logrus.Debugln("set initial deadline:", err)
		return
	}

	var ok bool
	c, ok = routeInitialConnection(ctx, c, s.fallbackAddr)
	if !ok {
		return
	}
	c = tls.Server(c, s.tlsConfig)

	var authenticated, canFallback bool
	var err error
	c, authenticated, canFallback, err = authenticateConnection(c)
	if !authenticated {
		if err != nil {
			logrus.Debugln("authenticate:", err)
		}
		if canFallback {
			_ = c.SetDeadline(time.Time{})
			fallback(ctx, c, s.fallbackAddr)
		}
		return
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		logrus.Debugln("clear initial deadline:", err)
		return
	}

	session := session.NewServerSession(c, func(stream *session.Stream) {
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorln("[BUG]", r, string(debug.Stack()))
			}
		}()
		defer stream.Close()

		destination, err := M.SocksaddrSerializer.ReadAddrPort(stream)
		if err != nil {
			logrus.Debugln("ReadAddrPort:", err)
			return
		}

		if strings.Contains(destination.String(), "udp-over-tcp.arpa") {
			proxyOutboundUoT(ctx, stream, destination)
		} else {
			proxyOutboundTCP(ctx, stream, destination)
		}
	}, &padding.DefaultPaddingFactory)
	session.Run()
	session.Close()
}

func authenticateConnection(c net.Conn) (net.Conn, bool, bool, error) {
	request := make([]byte, 34)
	n, err := io.ReadFull(c, request)
	if err != nil {
		return replayConnection(c, request[:n]), false, n > 0, err
	}
	if subtle.ConstantTimeCompare(request[:32], passwordSha256) != 1 {
		return replayConnection(c, request), false, true, nil
	}

	paddingLen := int(binary.BigEndian.Uint16(request[32:]))
	if paddingLen == 0 {
		return c, true, false, nil
	}
	request = append(request, make([]byte, paddingLen)...)
	n, err = io.ReadFull(c, request[34:])
	if err != nil {
		return replayConnection(c, request[:34+n]), false, true, err
	}
	return c, true, false, nil
}

func replayConnection(c net.Conn, data []byte) net.Conn {
	return bufio.NewCachedConn(c, buf.As(data))
}

func routeInitialConnection(ctx context.Context, c net.Conn, fallbackAddr string) (net.Conn, bool) {
	var firstByte [1]byte
	n, err := c.Read(firstByte[:])
	if err != nil {
		logrus.Debugln("initial read:", err)
		return nil, false
	}
	if n == 0 {
		return nil, false
	}

	cachedConn := bufio.NewCachedConn(c, buf.As(firstByte[:n]))
	if firstByte[0] != 0x16 {
		_ = cachedConn.SetDeadline(time.Time{})
		fallback(ctx, cachedConn, fallbackAddr)
		return cachedConn, false
	}
	return cachedConn, true
}

func fallback(ctx context.Context, c net.Conn, fallbackAddr string) {
	if fallbackAddr == "" {
		logrus.Debugln("fallback disabled:", c.RemoteAddr())
		return
	}
	logrus.Debugln("fallback:", c.RemoteAddr(), "=>", fallbackAddr)

	dialer := net.Dialer{Timeout: time.Second * 5}
	fallbackConn, err := dialer.DialContext(ctx, "tcp", fallbackAddr)
	if err != nil {
		logrus.Debugln("fallback dial:", err)
		return
	}
	defer fallbackConn.Close()

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(fallbackConn, c)
		if tcpConn, ok := fallbackConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(c, fallbackConn)
		if tcpConn, ok := c.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
	case <-errCh:
	}
}
