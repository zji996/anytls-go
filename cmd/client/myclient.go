package main

import (
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"anytls/util"
	"context"
	"encoding/binary"
	"net"
	"time"

	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
)

type myClient struct {
	dialOut       util.DialOutFunc
	sessionClient *session.Client
	padding       *atomic.TypedValue[*padding.PaddingFactory]
}

func NewMyClient(ctx context.Context, dialOut util.DialOutFunc, minIdleSession int) *myClient {
	s := &myClient{
		dialOut: dialOut,
		padding: padding.NewDefaultPaddingFactory(),
	}
	s.sessionClient = session.NewClient(ctx, s.createOutboundConnection, s.padding, time.Second*30, time.Second*30, minIdleSession)
	return s
}

func (c *myClient) CreateProxy(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	conn, err := c.sessionClient.CreateStream(ctx)
	if err != nil {
		return nil, err
	}
	err = M.SocksaddrSerializer.WriteAddrPort(conn, destination)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (c *myClient) createOutboundConnection(ctx context.Context) (net.Conn, error) {
	conn, err := c.dialOut(ctx)
	if err != nil {
		return nil, err
	}

	b := buf.NewPacket()
	defer b.Release()

	b.Write(passwordSha256)
	var paddingLen int
	if pad := c.padding.Load().GenerateRecordPayloadSizes(0); len(pad) > 0 {
		paddingLen = pad[0]
	}
	binary.BigEndian.PutUint16(b.Extend(2), uint16(paddingLen))
	if paddingLen > 0 {
		b.WriteZeroN(paddingLen)
	}

	_, err = b.WriteTo(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}
