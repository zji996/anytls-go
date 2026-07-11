package session

import (
	"io"
	"net"
	"testing"
	"time"
)

func BenchmarkStreamWritePersistentPipe(b *testing.B) {
	payload := make([]byte, 16*1024)
	paddingFactory := newTestPaddingFactory("stop=0")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	s := NewClientSession(local, paddingFactory)
	stream := newStream(1, s)
	done := make(chan struct{})
	go func() {
		_, _ = io.CopyN(io.Discard, remote, int64(b.N*(len(payload)+headerOverHeadSize)))
		close(done)
	}()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, err := stream.Write(payload); err != nil {
			b.Fatal(err)
		} else if n != len(payload) {
			b.Fatalf("Stream.Write wrote %d bytes, want %d", n, len(payload))
		}
	}
	<-done
}

func BenchmarkStreamReadQueued(b *testing.B) {
	payload := make([]byte, 16*1024)
	readBuffer := make([]byte, len(payload))
	s := NewClientSession(&discardConn{}, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	defer stream.closeLocally()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !stream.queueIncoming(payload) {
			b.Fatal("queueIncoming failed")
		}
		if _, err := io.ReadFull(stream, readBuffer); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionReceiveFrame(b *testing.B) {
	payload := make([]byte, 16*1024)
	frame := mustEncodeBenchmarkFrame(b, cmdPSH, 1, payload)
	local, remote := net.Pipe()
	defer remote.Close()

	s := NewClientSession(local, newTestPaddingFactory("stop=0"))
	stream := newStream(1, s)
	s.streams[1] = stream
	recvDone := make(chan struct{})
	go func() {
		_ = s.recvLoop()
		close(recvDone)
	}()
	writeDone := make(chan error, 1)
	frameRead := make(chan struct{})
	go func() {
		for i := 0; i < b.N; i++ {
			if _, err := remote.Write(frame); err != nil {
				writeDone <- err
				return
			}
			<-frameRead
		}
		writeDone <- nil
	}()

	readBuffer := make([]byte, len(payload))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := io.ReadFull(stream, readBuffer); err != nil {
			b.Fatal(err)
		}
		frameRead <- struct{}{}
	}
	b.StopTimer()
	if err := <-writeDone; err != nil {
		b.Fatal(err)
	}
	_ = s.Close()
	<-recvDone
}

func mustEncodeBenchmarkFrame(b *testing.B, cmd byte, sid uint32, data []byte) []byte {
	b.Helper()
	buffer, err := encodeFrameRaw(cmd, sid, data)
	if err != nil {
		b.Fatal(err)
	}
	defer buffer.Release()
	encoded := make([]byte, len(buffer.Bytes()))
	copy(encoded, buffer.Bytes())
	return encoded
}

type discardConn struct{}

func (*discardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*discardConn) Close() error                     { return nil }
func (*discardConn) LocalAddr() net.Addr              { return nil }
func (*discardConn) RemoteAddr() net.Addr             { return nil }
func (*discardConn) SetDeadline(time.Time) error      { return nil }
func (*discardConn) SetReadDeadline(time.Time) error  { return nil }
func (*discardConn) SetWriteDeadline(time.Time) error { return nil }
