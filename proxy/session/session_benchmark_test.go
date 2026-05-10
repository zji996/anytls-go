package session

import (
	"io"
	"net"
	"testing"
)

func BenchmarkEncodeFrameRaw(b *testing.B) {
	payload := make([]byte, 16*1024)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buffer, err := encodeFrameRaw(cmdPSH, 1, payload)
		if err != nil {
			b.Fatal(err)
		}
		buffer.Release()
	}
}

func BenchmarkWriteDataFrame(b *testing.B) {
	payload := make([]byte, 16*1024)
	paddingFactory := newTestPaddingFactory("stop=0")

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		local, remote := net.Pipe()
		s := NewClientSession(local, paddingFactory)
		done := make(chan struct{})
		go func() {
			_, _ = io.CopyN(io.Discard, remote, int64(len(payload)+headerOverHeadSize))
			close(done)
		}()
		if n, err := s.writeDataFrame(1, payload); err != nil {
			b.Fatal(err)
		} else if n != len(payload) {
			b.Fatalf("writeDataFrame wrote %d bytes, want %d", n, len(payload))
		}
		<-done
		local.Close()
		remote.Close()
	}
}

func BenchmarkWriteDataFramePersistentPipe(b *testing.B) {
	payload := make([]byte, 16*1024)
	paddingFactory := newTestPaddingFactory("stop=0")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	s := NewClientSession(local, paddingFactory)
	done := make(chan struct{})
	go func() {
		_, _ = io.CopyN(io.Discard, remote, int64(b.N*(len(payload)+headerOverHeadSize)))
		close(done)
	}()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, err := s.writeDataFrame(1, payload); err != nil {
			b.Fatal(err)
		} else if n != len(payload) {
			b.Fatalf("writeDataFrame wrote %d bytes, want %d", n, len(payload))
		}
	}
	<-done
}

func BenchmarkPaddingSizes(b *testing.B) {
	paddingFactory := newTestPaddingFactory("stop=8\n0=30-30\n1=100-400\n2=400-500,c,500-1000")
	factory := paddingFactory.Load()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = factory.GenerateRecordPayloadSizes(uint32(i % 8))
	}
}

func BenchmarkPaddingSizesFixed(b *testing.B) {
	paddingFactory := newTestPaddingFactory("stop=3\n0=30-30\n1=100-100,c,200-200")
	factory := paddingFactory.Load()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = factory.GenerateRecordPayloadSizes(uint32(i % 2))
	}
}

func BenchmarkPaddingSizesRandom(b *testing.B) {
	paddingFactory := newTestPaddingFactory("stop=3\n0=30-400\n1=100-400,c,500-1000")
	factory := paddingFactory.Load()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = factory.GenerateRecordPayloadSizes(uint32(i % 2))
	}
}
