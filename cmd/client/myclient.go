package main

import (
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"anytls/util"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	randv2 "math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
)

type myClient struct {
	dialOut        util.DialOutFunc
	sessionClient  *session.Client
	padding        *atomic.TypedValue[*padding.PaddingFactory]
	paddingRNG     randv2.ChaCha8
	paddingRNGLock sync.Mutex
	paddingBuf     [16]int
}

func NewMyClient(ctx context.Context, dialOut util.DialOutFunc, minIdleSession int) *myClient {
	var randomSeed [32]byte
	_, _ = rand.Read(randomSeed[:])
	s := &myClient{
		dialOut:    dialOut,
		padding:    padding.NewDefaultPaddingFactory(),
		paddingRNG: *randv2.NewChaCha8(randomSeed),
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

func (c *myClient) Prewarm(ctx context.Context, count int) error {
	return c.sessionClient.Prewarm(ctx, count)
}

func (c *myClient) createOutboundConnection(ctx context.Context) (net.Conn, error) {
	conn, err := c.dialOut(ctx)
	if err != nil {
		return nil, err
	}

	var paddingLen int
	c.paddingRNGLock.Lock()
	pad := c.padding.Load().GenerateRecordPayloadSizesWithRNGInto(0, &c.paddingRNG, c.paddingBuf[:])
	if len(pad) > 0 {
		paddingLen = pad[0]
	}
	c.paddingRNGLock.Unlock()
	if paddingLen < 0 || paddingLen > padding.MaxPaddingSize {
		conn.Close()
		return nil, fmt.Errorf("invalid authentication padding length: %d", paddingLen)
	}

	b := buf.NewSize(34 + paddingLen)
	defer b.Release()
	b.Write(passwordSha256)
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
