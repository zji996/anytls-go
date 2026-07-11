package session

import (
	"anytls/proxy/pipe"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
)

const streamReceiveQueueSize = 16

var errStreamReceiveQueueFull = errors.New("stream receive queue full")

type incomingChunk struct {
	data   []byte
	pooled bool
}

func (c *incomingChunk) release() {
	if c.pooled && c.data != nil {
		_ = buf.Put(c.data)
	}
	c.data = nil
}

type streamError struct {
	err error
}

// Stream implements net.Conn
type Stream struct {
	id uint32

	sess *Session

	readMu        sync.Mutex
	readChunk     incomingChunk
	readOffset    int
	recvMu        sync.Mutex
	recvCh        chan incomingChunk
	recvDone      chan struct{}
	recvCloseOnce sync.Once
	remoteDone    chan struct{}
	remoteOnce    sync.Once
	readDeadline  pipe.PipeDeadline
	writeDeadline pipe.PipeDeadline
	writesActive  atomic.Int32
	writeWatching atomic.Bool
	writeState    chan struct{}

	dieOnce sync.Once
	dieHook func()
	dieErr  atomic.Pointer[streamError]

	reportOnce sync.Once
}

// newStream initiates a Stream struct
func newStream(id uint32, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.sess = sess
	s.recvCh = make(chan incomingChunk, streamReceiveQueueSize)
	s.recvDone = make(chan struct{})
	s.remoteDone = make(chan struct{})
	s.readDeadline = pipe.MakePipeDeadline()
	s.writeDeadline = pipe.MakePipeDeadline()
	s.writeState = make(chan struct{}, 1)
	return s
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	s.readMu.Lock()
	defer s.readMu.Unlock()
	for {
		if s.readOffset < len(s.readChunk.data) {
			n = copy(b, s.readChunk.data[s.readOffset:])
			s.readOffset += n
			if s.readOffset == len(s.readChunk.data) {
				s.readChunk.release()
				s.readOffset = 0
			}
			return n, nil
		}
		select {
		case chunk := <-s.recvCh:
			s.readChunk = chunk
			s.readOffset = 0
			continue
		default:
		}

		select {
		case <-s.recvDone:
			return 0, s.readCloseError()
		case <-s.sess.die:
			return 0, s.readCloseError()
		case <-s.readDeadline.Wait():
			return 0, os.ErrDeadlineExceeded
		case <-s.remoteDone:
			return 0, io.EOF
		default:
		}

		select {
		case chunk := <-s.recvCh:
			s.readChunk = chunk
			s.readOffset = 0
		case <-s.recvDone:
			return 0, s.readCloseError()
		case <-s.sess.die:
			return 0, s.readCloseError()
		case <-s.readDeadline.Wait():
			return 0, os.ErrDeadlineExceeded
		case <-s.remoteDone:
			continue
		}
	}
}

// Write implements net.Conn
func (s *Stream) Write(b []byte) (n int, err error) {
	deadline := s.writeDeadline.Wait()
	select {
	case <-deadline:
		return 0, os.ErrDeadlineExceeded
	default:
	}
	if dieErr := s.loadDieErr(); dieErr != nil {
		return 0, dieErr
	}

	s.beginWrite()
	defer s.endWrite()

	n, err = s.sess.writeDataFrame(s.id, b)
	select {
	case <-deadline:
		return n, os.ErrDeadlineExceeded
	default:
	}
	return
}

func (s *Stream) ReadBuffer(buffer *buf.Buffer) error {
	if buffer.FreeLen() == 0 {
		return io.ErrShortBuffer
	}
	n, err := s.Read(buffer.FreeBytes())
	buffer.Extend(n)
	if n > 0 {
		return nil
	}
	return err
}

func (s *Stream) WriteBuffer(buffer *buf.Buffer) error {
	defer buffer.Release()
	if buffer.Len() == 0 {
		return nil
	}
	if buffer.Len() > maxFrameDataLen || buffer.Start() < headerOverHeadSize {
		_, err := s.Write(buffer.Bytes())
		return err
	}

	deadline := s.writeDeadline.Wait()
	select {
	case <-deadline:
		return os.ErrDeadlineExceeded
	default:
	}
	if dieErr := s.loadDieErr(); dieErr != nil {
		return dieErr
	}

	payloadLen := buffer.Len()
	header := buffer.ExtendHeader(headerOverHeadSize)
	header[0] = cmdPSH
	binary.BigEndian.PutUint32(header[1:5], s.id)
	binary.BigEndian.PutUint16(header[5:7], uint16(payloadLen))

	s.beginWrite()
	defer s.endWrite()
	err := s.sess.writeEncodedPayloadFrame(buffer.Bytes())
	select {
	case <-deadline:
		return os.ErrDeadlineExceeded
	default:
	}
	return err
}

func (s *Stream) FrontHeadroom() int {
	return headerOverHeadSize
}

// Close implements net.Conn
func (s *Stream) Close() error {
	return s.closeWithError(io.ErrClosedPipe)
}

// closeLocally only closes Stream and don't notify remote peer
func (s *Stream) closeLocally() {
	s.closeLocallyWithError(net.ErrClosed)
}

func (s *Stream) closeRemotely() {
	var once bool
	s.dieOnce.Do(func() {
		s.storeDieErr(net.ErrClosed)
		s.remoteOnce.Do(func() {
			close(s.remoteDone)
		})
		once = true
	})
	if once && s.dieHook != nil {
		s.dieHook()
		s.dieHook = nil
	}
}

func (s *Stream) closeLocallyWithError(err error) bool {
	var once bool
	s.dieOnce.Do(func() {
		s.storeDieErr(err)
		s.closeReceiveQueue()
		once = true
	})
	if once {
		if s.dieHook != nil {
			s.dieHook()
			s.dieHook = nil
		}
	} else {
		s.closeReceiveQueue()
	}
	return once
}

func (s *Stream) closeWithError(err error) error {
	var once bool
	s.dieOnce.Do(func() {
		s.storeDieErr(err)
		s.closeReceiveQueue()
		once = true
	})
	if once {
		if s.dieHook != nil {
			s.dieHook()
			s.dieHook = nil
		}
		return s.sess.streamClosed(s.id)
	} else {
		s.closeReceiveQueue()
		return s.loadDieErr()
	}
}

func (s *Stream) queueIncoming(data []byte) bool {
	return s.queueIncomingChunk(incomingChunk{data: data})
}

func (s *Stream) queueIncomingPooled(data []byte) bool {
	return s.queueIncomingChunk(incomingChunk{data: data, pooled: true})
}

func (s *Stream) queueIncomingChunk(chunk incomingChunk) bool {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	select {
	case <-s.recvDone:
		return false
	case <-s.sess.die:
		return false
	default:
	}

	select {
	case s.recvCh <- chunk:
		return true
	case <-s.recvDone:
		return false
	case <-s.sess.die:
		return false
	default:
		return false
	}
}

func (s *Stream) closeReceiveQueue() {
	s.recvCloseOnce.Do(func() {
		s.recvMu.Lock()
		close(s.recvDone)
		s.recvMu.Unlock()
		s.readMu.Lock()
		s.readChunk.release()
		s.readOffset = 0
		for {
			select {
			case chunk := <-s.recvCh:
				chunk.release()
			default:
				s.readMu.Unlock()
				return
			}
		}
	})
}

func (s *Stream) loadDieErr() error {
	if state := s.dieErr.Load(); state != nil {
		return state.err
	}
	return nil
}

func (s *Stream) storeDieErr(err error) {
	s.dieErr.Store(&streamError{err: err})
}

func (s *Stream) readCloseError() error {
	if err := s.loadDieErr(); err != nil {
		return err
	}
	return net.ErrClosed
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	if s.loadDieErr() != nil {
		return io.ErrClosedPipe
	}
	s.readDeadline.Set(t)
	return nil
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Set(t)
	if !t.IsZero() && s.writesActive.Load() > 0 {
		s.ensureWriteDeadlineWatcher()
	}
	return nil
}

func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return s.SetReadDeadline(t)
}

func (s *Stream) beginWrite() {
	if s.writesActive.Add(1) == 1 {
		select {
		case <-s.writeState:
		default:
		}
	}
	if !s.writeDeadline.Deadline().IsZero() {
		s.ensureWriteDeadlineWatcher()
	}
}

func (s *Stream) endWrite() {
	if s.writesActive.Add(-1) == 0 {
		select {
		case s.writeState <- struct{}{}:
		default:
		}
	}
}

func (s *Stream) ensureWriteDeadlineWatcher() {
	if !s.writeWatching.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer func() {
			s.writeWatching.Store(false)
			if s.writesActive.Load() > 0 && !s.writeDeadline.Deadline().IsZero() {
				s.ensureWriteDeadlineWatcher()
			}
		}()
		for {
			if s.writeDeadline.Deadline().IsZero() {
				return
			}
			deadline := s.writeDeadline.Wait()
			select {
			case <-deadline:
				if s.writeDeadline.Wait() != deadline {
					continue
				}
				if s.writesActive.Load() > 0 {
					_ = s.sess.Close()
				}
				return
			case <-s.writeState:
				if s.writesActive.Load() == 0 {
					return
				}
			case <-s.sess.die:
				return
			}
		}
	}()
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
