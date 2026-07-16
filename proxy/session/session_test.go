package session

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"anytls/proxy/padding"

	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/buf"
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

func TestReceiveQueueBackpressurePreservesStreamData(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	paddingFactory := newTestPaddingFactory("stop=0")
	s := NewClientSession(local, paddingFactory)
	defer s.Close()

	stream := newStream(1, s)
	fast := newStream(2, s)
	s.streamLock.Lock()
	s.streams[1] = stream
	s.streams[2] = fast
	s.streamLock.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.recvLoop()
	}()

	streamPayload := []byte("data")
	fastPayload := []byte("fast")
	streamFrame := mustEncodeTestFrame(t, cmdPSH, stream.id, streamPayload)
	fastFrame := mustEncodeTestFrame(t, cmdPSH, fast.id, fastPayload)
	frameCount := streamReceiveQueueSize + 2
	writeDone := make(chan error, 1)
	go func() {
		for i := 0; i < frameCount; i++ {
			if _, err := remote.Write(streamFrame); err != nil {
				writeDone <- err
				return
			}
		}
		_, err := remote.Write(fastFrame)
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		t.Fatalf("writer was not backpressured by the full receive queue: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	want := strings.Repeat(string(streamPayload), frameCount)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("stream read %q, want %q", string(got), want)
	}

	buf := make([]byte, len(fastPayload))
	if _, err := io.ReadFull(fast, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(fastPayload) {
		t.Fatalf("fast stream read %q, want %q", string(buf), string(fastPayload))
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if s.IsClosed() {
		t.Fatal("session closed while applying receive backpressure")
	}

	s.Close()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("recvLoop did not exit")
	}
}

func TestStreamCloseUnblocksFullReceiveQueue(t *testing.T) {
	s := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	for i := 0; i < streamReceiveQueueSize; i++ {
		if !stream.queueIncoming([]byte("queued")) {
			t.Fatal("queueIncoming failed before queue reached capacity")
		}
	}

	queued := make(chan bool, 1)
	go func() {
		queued <- stream.queueIncoming([]byte("blocked"))
	}()
	select {
	case <-queued:
		t.Fatal("queueIncoming did not block on a full receive queue")
	case <-time.After(20 * time.Millisecond):
	}

	closed := make(chan struct{})
	go func() {
		stream.closeLocally()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream close was blocked by receive backpressure")
	}
	select {
	case ok := <-queued:
		if ok {
			t.Fatal("queueIncoming succeeded after stream close")
		}
	case <-time.After(time.Second):
		t.Fatal("queueIncoming remained blocked after stream close")
	}
}

func TestInvalidFrameHeaderClosesSession(t *testing.T) {
	tests := []struct {
		name   string
		cmd    byte
		length uint16
	}{
		{name: "unknown command", cmd: 255},
		{name: "FIN with data", cmd: cmdFIN, length: 1},
		{name: "SYN with data", cmd: cmdSYN, length: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()

			s := NewClientSession(local, newTestPaddingFactory("stop=0"))
			errCh := make(chan error, 1)
			go func() {
				errCh <- s.recvLoop()
			}()

			var hdr rawHeader
			hdr[0] = tt.cmd
			binary.BigEndian.PutUint16(hdr[5:], tt.length)
			if _, err := remote.Write(hdr[:]); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("recvLoop returned nil error")
				}
			case <-time.After(time.Second):
				t.Fatal("recvLoop did not reject invalid frame")
			}
			if !s.IsClosed() {
				t.Fatal("session remained open after invalid frame")
			}
		})
	}
}

func TestDataWriteErrorClosesSession(t *testing.T) {
	local, remote := net.Pipe()
	remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	if _, err := s.writeDataFrame(1, []byte("payload")); err == nil {
		t.Fatal("writeDataFrame succeeded on a closed connection")
	}
	if !s.IsClosed() {
		t.Fatal("session remained reusable after a data write error")
	}
}

func TestStreamWriteDeadlineInterruptsBlockedWrite(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	if err := stream.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := stream.Write([]byte("blocked"))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked Write was not interrupted promptly")
	}
	if !s.IsClosed() {
		t.Fatal("session remained open after an interrupted frame write")
	}
}

func TestSetWriteDeadlineInterruptsPendingWrite(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("blocked"))
		writeErr <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := stream.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeErr:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Write error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Write was not interrupted")
	}
}

func TestClearedWriteDeadlineDoesNotInterruptWrite(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	if err := stream.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("payload"))
		writeErr <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if err := stream.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	go io.CopyN(io.Discard, remote, int64(len("payload")+headerOverHeadSize))
	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("Write failed after clearing deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not finish after clearing deadline")
	}
	if s.IsClosed() {
		t.Fatal("session closed after clearing write deadline")
	}
}

func TestStreamReadPartialChunks(t *testing.T) {
	s := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	defer stream.closeLocally()
	if !stream.queueIncoming([]byte("abcdef")) {
		t.Fatal("queueIncoming failed")
	}

	buffer := make([]byte, 2)
	var got string
	for range 3 {
		n, err := stream.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		got += string(buffer[:n])
	}
	if got != "abcdef" {
		t.Fatalf("partial reads = %q, want abcdef", got)
	}
}

func TestIdleSessionHeapReturnsHighestSequence(t *testing.T) {
	client := &Client{}
	for _, seq := range []uint64{2, 5, 1, 4, 3} {
		session := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
		session.seq = seq
		client.putIdleSession(session)
	}
	for want := uint64(5); want > 0; want-- {
		session := client.getIdleSession()
		if session == nil || session.seq != want {
			t.Fatalf("getIdleSession sequence = %v, want %d", session, want)
		}
	}
}

func TestIdleCleanupKeepsNewestMinimum(t *testing.T) {
	client := &Client{minIdleSession: 2}
	for seq := uint64(1); seq <= 5; seq++ {
		session := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
		session.seq = seq
		client.putIdleSession(session)
	}
	client.idleCleanupExpTime(time.Now().Add(time.Hour))

	for _, want := range []uint64{5, 4} {
		session := client.getIdleSession()
		if session == nil || session.seq != want {
			t.Fatalf("remaining session sequence = %v, want %d", session, want)
		}
	}
	if session := client.getIdleSession(); session != nil {
		t.Fatalf("unexpected extra idle session: %d", session.seq)
	}
}

func TestClientPrewarmCreatesIdleSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, func(context.Context) (net.Conn, error) {
		return newBlockingTestConn(), nil
	}, newTestPaddingFactory("stop=0"), time.Hour, time.Hour, 0)
	defer client.Close()

	if err := client.Prewarm(ctx, 3); err != nil {
		t.Fatal(err)
	}
	for want := uint64(3); want > 0; want-- {
		session := client.getIdleSession()
		if session == nil || session.seq != want {
			t.Fatalf("prewarmed session sequence = %v, want %d", session, want)
		}
	}
}

func TestStreamExtendedBufferIO(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	stream := newStream(7, s)
	payload := []byte("payload")
	writeBuffer := buf.NewSize(headerOverHeadSize + len(payload))
	writeBuffer.Resize(headerOverHeadSize, 0)
	_, _ = writeBuffer.Write(payload)
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- stream.WriteBuffer(writeBuffer)
	}()

	frame := readTestFrame(t, remote)
	if frame.cmd != cmdPSH || frame.sid != stream.id || string(frame.data) != string(payload) {
		t.Fatalf("WriteBuffer frame = command %d stream %d data %q", frame.cmd, frame.sid, frame.data)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}

	if !stream.queueIncoming([]byte("response")) {
		t.Fatal("queueIncoming failed")
	}
	readBuffer := buf.NewSize(32)
	defer readBuffer.Release()
	if err := stream.ReadBuffer(readBuffer); err != nil {
		t.Fatal(err)
	}
	if got := string(readBuffer.Bytes()); got != "response" {
		t.Fatalf("ReadBuffer data = %q, want response", got)
	}
}

func TestRemoteFINDrainsQueuedData(t *testing.T) {
	s := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	if !stream.queueIncoming([]byte("final payload")) {
		t.Fatal("queueIncoming failed")
	}
	stream.closeRemotely()

	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "final payload" {
		t.Fatalf("drained data = %q, want final payload", got)
	}
	_ = stream.Close()
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

type blockingTestConn struct {
	done chan struct{}
	once sync.Once
}

func newBlockingTestConn() *blockingTestConn {
	return &blockingTestConn{done: make(chan struct{})}
}

func (c *blockingTestConn) Read([]byte) (int, error) {
	<-c.done
	return 0, io.EOF
}

func (*blockingTestConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *blockingTestConn) Close() error                   { c.once.Do(func() { close(c.done) }); return nil }
func (*blockingTestConn) LocalAddr() net.Addr              { return nil }
func (*blockingTestConn) RemoteAddr() net.Addr             { return nil }
func (*blockingTestConn) SetDeadline(time.Time) error      { return nil }
func (*blockingTestConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingTestConn) SetWriteDeadline(time.Time) error { return nil }

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
