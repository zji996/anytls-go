package main

import (
	"anytls/proxy"
	"context"
	"net"
	"strings"
	"time"

	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/sirupsen/logrus"
)

func proxyOutboundTCP(ctx context.Context, conn net.Conn, destination M.Socksaddr) error {
	c, err := proxy.SystemDialer.DialContext(ctx, "tcp", destination.String())
	if err != nil {
		logrus.Debugln("proxyOutboundTCP DialContext:", err)
		err = E.Errors(err, N.ReportHandshakeFailure(conn, err))
		return err
	}

	err = N.ReportHandshakeSuccess(conn)
	if err != nil {
		return err
	}

	return bufio.CopyConn(ctx, conn, c)
}

// dedupDestination is a reserved stream destination: clients reach the
// server's deduplicating decoder through it, and servers without one refuse it
// instead of resolving the name.
const dedupDestination = "dedup.anytls.arpa"

// proxyOutboundDedup relays a stream to the local decoder at backend, a
// host:port or unix:/path address.
func proxyOutboundDedup(ctx context.Context, conn net.Conn, backend string) error {
	if backend == "" {
		err := E.New("no dedup backend configured")
		return E.Errors(err, N.ReportHandshakeFailure(conn, err))
	}
	network, address := "tcp", backend
	if path, ok := strings.CutPrefix(backend, "unix:"); ok {
		network, address = "unix", path
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	c, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		logrus.Debugln("proxyOutboundDedup DialContext:", err)
		return E.Errors(err, N.ReportHandshakeFailure(conn, err))
	}
	if err = N.ReportHandshakeSuccess(conn); err != nil {
		c.Close()
		return err
	}
	return bufio.CopyConn(ctx, conn, c)
}

func proxyOutboundUoT(ctx context.Context, conn net.Conn, destination M.Socksaddr) error {
	request, err := uot.ReadRequest(conn)
	if err != nil {
		logrus.Debugln("proxyOutboundUoT ReadRequest:", err)
		return err
	}

	c, err := net.ListenPacket("udp", "")
	if err != nil {
		logrus.Debugln("proxyOutboundUoT ListenPacket:", err)
		err = E.Errors(err, N.ReportHandshakeFailure(conn, err))
		return err
	}

	err = N.ReportHandshakeSuccess(conn)
	if err != nil {
		return err
	}

	return bufio.CopyPacketConn(ctx, uot.NewConn(conn, *request), bufio.NewPacketConn(c))
}
