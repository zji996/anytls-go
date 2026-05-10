package main

import (
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"bytes"
	"context"
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

func handleTcpConnection(ctx context.Context, c net.Conn, s *myServer) {
	defer func() {
		if r := recover(); r != nil {
			logrus.Errorln("[BUG]", r, string(debug.Stack()))
		}
	}()

	c = tls.Server(c, s.tlsConfig)
	defer c.Close()

	b := buf.NewPacket()
	defer b.Release()

	n, err := b.ReadOnceFrom(c)
	if err != nil {
		logrus.Debugln("ReadOnceFrom:", err)
		return
	}
	c = bufio.NewCachedConn(c, b)

	by, err := b.ReadBytes(32)
	if err != nil || !bytes.Equal(by, passwordSha256) {
		b.Resize(0, n)
		fallback(ctx, c, s.fallbackAddr)
		return
	}
	by, err = b.ReadBytes(2)
	if err != nil {
		b.Resize(0, n)
		fallback(ctx, c, s.fallbackAddr)
		return
	}
	paddingLen := binary.BigEndian.Uint16(by)
	if paddingLen > 0 {
		_, err = b.ReadBytes(int(paddingLen))
		if err != nil {
			b.Resize(0, n)
			fallback(ctx, c, s.fallbackAddr)
			return
		}
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
