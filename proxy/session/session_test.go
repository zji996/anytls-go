package session

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"anytls/proxy/padding"

	"github.com/sagernet/sing/common/atomic"
)

func TestSessionRoundTripOverNetPipe(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	paddingFactory := newTestPaddingFactory("stop=0")
	serverReady := make(chan *Stream, 1)
	serverErr := make(chan error, 1)

	server := NewServerSession(serverConn, func(stream *Stream) {
		serverReady <- stream
		buf := make([]byte, len("ping"))
		if _, err := io.ReadFull(stream, buf); err != nil {
			serverErr <- err
			return
		}
		if string(buf) != "ping" {
			serverErr <- errors.New("server read unexpected payload")
			return
		}
		if _, err := stream.Write([]byte("pong")); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}, paddingFactory)
	go server.Run()
	defer server.Close()

	client := NewClientSession(clientConn, paddingFactory)
	client.Run()
	defer client.Close()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-serverReady:
	case <-time.After(time.Second):
		t.Fatal("server did not receive stream")
	}

	buf := make([]byte, len("pong"))
	if _, err = io.ReadFull(stream, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("client read %q, want pong", string(buf))
	}

	select {
	case err = <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server handler did not finish")
	}
}

func TestSessionUpdatePaddingSchemeIsClientScoped(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	clientPadding := newTestPaddingFactory("stop=0")
	serverPadding := newTestPaddingFactory("stop=1\n0=12-12")

	server := NewServerSession(serverConn, func(stream *Stream) {
		stream.Close()
	}, serverPadding)
	go server.Run()
	defer server.Close()

	client := NewClientSession(clientConn, clientPadding)
	client.Run()
	defer client.Close()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, time.Second, func() bool {
		return clientPadding.Load().Md5 == serverPadding.Load().Md5
	})
}

func TestSessionHandshakeFailurePropagates(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	paddingFactory := newTestPaddingFactory("stop=0")
	server := NewServerSession(serverConn, func(stream *Stream) {
		_ = stream.HandshakeFailure(errors.New("dial failed"))
		stream.Close()
	}, paddingFactory)
	server.peerVersion = 2
	go server.Run()
	defer server.Close()

	client := NewClientSession(clientConn, paddingFactory)
	client.peerVersion = 2
	client.Run()
	defer client.Close()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1)
	_, err = stream.Read(buf)
	if err == nil || !strings.Contains(err.Error(), "remote: dial failed") {
		t.Fatalf("stream.Read error = %v, want remote dial failure", err)
	}
}

func TestSlowStreamReaderDoesNotBlockOtherStreams(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	paddingFactory := newTestPaddingFactory("stop=0")
	s := NewClientSession(local, paddingFactory)
	defer s.Close()

	slow := newStream(1, s)
	fast := newStream(2, s)
	s.streamLock.Lock()
	s.streams[1] = slow
	s.streams[2] = fast
	s.streamLock.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.recvLoop()
	}()

	slowPayload := []byte("slow")
	for i := 0; i < streamReceiveQueueSize; i++ {
		if _, err := remote.Write(mustEncodeTestFrame(t, cmdPSH, 1, slowPayload)); err != nil {
			t.Fatal(err)
		}
	}

	fastPayload := []byte("fast")
	if _, err := remote.Write(mustEncodeTestFrame(t, cmdPSH, 2, fastPayload)); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len(fastPayload))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(fast, buf)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fast stream read was blocked by slow stream")
	}
	if string(buf) != string(fastPayload) {
		t.Fatalf("fast stream read %q, want %q", string(buf), string(fastPayload))
	}

	s.Close()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("recvLoop did not exit")
	}
}

func TestWriteDataFrameSplitsLargePayload(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	paddingFactory := newTestPaddingFactory("stop=0")

	s := NewClientSession(local, paddingFactory)
	payload := make([]byte, maxFrameDataLen+10)
	for i := range payload {
		payload[i] = byte(i)
	}

	errCh := make(chan error, 1)
	go func() {
		n, err := s.writeDataFrame(42, payload)
		if n != len(payload) {
			t.Errorf("writeDataFrame wrote %d bytes, want %d", n, len(payload))
		}
		errCh <- err
	}()

	got := readTestFrame(t, remote)
	if got.cmd != cmdPSH {
		t.Fatalf("first command = %d, want %d", got.cmd, cmdPSH)
	}
	if got.sid != 42 {
		t.Fatalf("first stream id = %d, want 42", got.sid)
	}
	if len(got.data) != maxFrameDataLen {
		t.Fatalf("first frame length = %d, want %d", len(got.data), maxFrameDataLen)
	}
	if string(got.data) != string(payload[:maxFrameDataLen]) {
		t.Fatal("first frame payload mismatch")
	}

	got = readTestFrame(t, remote)
	if got.cmd != cmdPSH {
		t.Fatalf("second command = %d, want %d", got.cmd, cmdPSH)
	}
	if got.sid != 42 {
		t.Fatalf("second stream id = %d, want 42", got.sid)
	}
	if len(got.data) != 10 {
		t.Fatalf("second frame length = %d, want 10", len(got.data))
	}
	if string(got.data) != string(payload[maxFrameDataLen:]) {
		t.Fatal("second frame payload mismatch")
	}

	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func mustEncodeTestFrame(t *testing.T, cmd byte, sid uint32, data []byte) []byte {
	t.Helper()

	buffer, err := encodeFrameRaw(cmd, sid, data)
	if err != nil {
		t.Fatal(err)
	}
	defer buffer.Release()

	encoded := make([]byte, len(buffer.Bytes()))
	copy(encoded, buffer.Bytes())
	return encoded
}

func newTestPaddingFactory(scheme string) *atomic.TypedValue[*padding.PaddingFactory] {
	factory := &atomic.TypedValue[*padding.PaddingFactory]{}
	factory.Store(padding.NewPaddingFactory([]byte(scheme)))
	return factory
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

type testFrame struct {
	cmd  byte
	sid  uint32
	data []byte
}

func readTestFrame(t *testing.T, r io.Reader) testFrame {
	t.Helper()

	var hdr rawHeader
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, hdr.Length())
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	return testFrame{
		cmd:  hdr.Cmd(),
		sid:  binary.BigEndian.Uint32(hdr[1:5]),
		data: data,
	}
}
