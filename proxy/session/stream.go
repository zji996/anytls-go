package session

import (
	"anytls/proxy/pipe"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const streamReceiveQueueSize = 16

// Stream implements net.Conn
type Stream struct {
	id uint32

	sess *Session

	pipeR         *pipe.PipeReader
	pipeW         *pipe.PipeWriter
	recvCh        chan []byte
	recvDone      chan struct{}
	recvCloseOnce sync.Once
	writeDeadline pipe.PipeDeadline

	dieOnce sync.Once
	dieHook func()
	dieErr  error

	reportOnce sync.Once
}

// newStream initiates a Stream struct
func newStream(id uint32, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.sess = sess
	s.pipeR, s.pipeW = pipe.Pipe()
	s.recvCh = make(chan []byte, streamReceiveQueueSize)
	s.recvDone = make(chan struct{})
	s.writeDeadline = pipe.MakePipeDeadline()
	go s.recvLoop()
	return s
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	n, err = s.pipeR.Read(b)
	if n == 0 && s.dieErr != nil {
		err = s.dieErr
	}
	return
}

// Write implements net.Conn
func (s *Stream) Write(b []byte) (n int, err error) {
	select {
	case <-s.writeDeadline.Wait():
		return 0, os.ErrDeadlineExceeded
	default:
	}
	if s.dieErr != nil {
		return 0, s.dieErr
	}
	n, err = s.sess.writeDataFrame(s.id, b)
	return
}

// Close implements net.Conn
func (s *Stream) Close() error {
	return s.closeWithError(io.ErrClosedPipe)
}

// closeLocally only closes Stream and don't notify remote peer
func (s *Stream) closeLocally() {
	var once bool
	s.dieOnce.Do(func() {
		s.dieErr = net.ErrClosed
		s.closeReceiveQueue()
		s.pipeR.Close()
		once = true
	})
	if once {
		if s.dieHook != nil {
			s.dieHook()
			s.dieHook = nil
		}
	}
}

func (s *Stream) closeWithError(err error) error {
	var once bool
	s.dieOnce.Do(func() {
		s.dieErr = err
		s.closeReceiveQueue()
		s.pipeR.Close()
		once = true
	})
	if once {
		if s.dieHook != nil {
			s.dieHook()
			s.dieHook = nil
		}
		return s.sess.streamClosed(s.id)
	} else {
		return s.dieErr
	}
}

func (s *Stream) queueIncoming(data []byte) bool {
	select {
	case s.recvCh <- data:
		return true
	case <-s.recvDone:
		return false
	case <-s.sess.die:
		return false
	}
}

func (s *Stream) recvLoop() {
	defer s.pipeW.Close()
	for {
		select {
		case data := <-s.recvCh:
			if len(data) > 0 {
				_, _ = s.pipeW.Write(data)
			}
		case <-s.recvDone:
			return
		case <-s.sess.die:
			return
		}
	}
}

func (s *Stream) closeReceiveQueue() {
	s.recvCloseOnce.Do(func() {
		close(s.recvDone)
	})
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	return s.pipeR.SetReadDeadline(t)
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Set(t)
	return nil
}

func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return s.SetReadDeadline(t)
}

// LocalAddr satisfies net.Conn interface
func (s *Stream) LocalAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Stream) RemoteAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// HandshakeFailure should be called when Server fail to create outbound proxy
func (s *Stream) HandshakeFailure(err error) error {
	var once bool
	s.reportOnce.Do(func() {
		once = true
	})
	if once && err != nil && s.sess.peerVersion >= 2 {
		f := newFrame(cmdSYNACK, s.id)
		f.data = []byte(err.Error())
		if _, err := s.sess.writeControlFrame(f); err != nil {
			return err
		}
	}
	return nil
}

// HandshakeSuccess should be called when Server success to create outbound proxy
func (s *Stream) HandshakeSuccess() error {
	var once bool
	s.reportOnce.Do(func() {
		once = true
	})
	if once && s.sess.peerVersion >= 2 {
		if _, err := s.sess.writeControlFrame(newFrame(cmdSYNACK, s.id)); err != nil {
			return err
		}
	}
	return nil
}
