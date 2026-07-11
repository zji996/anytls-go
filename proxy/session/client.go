package session

import (
	"anytls/proxy/padding"
	"anytls/util"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/sagernet/sing/common/atomic"
	"github.com/sirupsen/logrus"
)

var clientDebugSessionPool = os.Getenv("CLIENT_DEBUG_SESSION_POOL") == "1"
var clientStreamCounter atomic.Uint64

type Client struct {
	die       context.Context
	dieCancel context.CancelFunc

	dialOut util.DialOutFunc

	sessionCounter atomic.Uint64

	idleSessions    []*Session
	idleSessionLock sync.Mutex

	sessions     map[uint64]*Session
	sessionsLock sync.Mutex

	padding *atomic.TypedValue[*padding.PaddingFactory]

	idleSessionTimeout time.Duration
	minIdleSession     int
}

func NewClient(ctx context.Context, dialOut util.DialOutFunc,
	_padding *atomic.TypedValue[*padding.PaddingFactory], idleSessionCheckInterval, idleSessionTimeout time.Duration, minIdleSession int,
) *Client {
	c := &Client{
		sessions:           make(map[uint64]*Session),
		dialOut:            dialOut,
		padding:            _padding,
		idleSessionTimeout: idleSessionTimeout,
		minIdleSession:     minIdleSession,
	}
	if idleSessionCheckInterval <= time.Second*5 {
		idleSessionCheckInterval = time.Second * 30
	}
	if c.idleSessionTimeout <= time.Second*5 {
		c.idleSessionTimeout = time.Second * 30
	}
	c.die, c.dieCancel = context.WithCancel(ctx)
	util.StartRoutine(c.die, idleSessionCheckInterval, c.idleCleanup)
	return c
}

func (c *Client) CreateStream(ctx context.Context) (net.Conn, error) {
	select {
	case <-c.die.Done():
		return nil, io.ErrClosedPipe
	default:
	}

	var session *Session
	var stream *Stream
	var err error

	session = c.getIdleSession()
	if session == nil {
		session, err = c.createSession(ctx)
		if session != nil && clientDebugSessionPool {
			logrus.Infoln("create session:", session.seq)
		}
	} else {
		if clientDebugSessionPool {
			logrus.Infoln("get session:", session.seq)
		}
	}
	if session == nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	stream, err = session.OpenStream()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	if clientDebugSessionPool {
		cn := clientStreamCounter.Add(1)
		s := c.sessionCounter.Load()
		logrus.Infoln("cumulative session:", s, "cumulative stream:", cn, "avg:", float64(cn)/float64(s))
	}

	stream.dieHook = func() {
		// If Session is not closed, put this Stream to pool
		if !session.IsClosed() {
			if clientDebugSessionPool {
				logrus.Infoln("put session:", session.seq, stream.id)
			}
			select {
			case <-c.die.Done():
				// Now client has been closed
				go session.Close()
			default:
				c.putIdleSession(session)
			}
		} else {
			if clientDebugSessionPool {
				logrus.Infoln("discard session stream:", session.seq, stream.id)
			}
		}
	}

	return stream, nil
}

func (c *Client) Prewarm(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}
	count = min(count, 64)
	workerCount := min(count, 4)
	jobs := make(chan struct{}, count)
	for range count {
		jobs <- struct{}{}
	}
	close(jobs)

	var waitGroup sync.WaitGroup
	var errorLock sync.Mutex
	var warmupErrors []error
	for range workerCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for range jobs {
				session, err := c.createSession(ctx)
				if err != nil {
					errorLock.Lock()
					warmupErrors = append(warmupErrors, err)
					errorLock.Unlock()
					continue
				}
				select {
				case <-c.die.Done():
					_ = session.Close()
				case <-ctx.Done():
					_ = session.Close()
				default:
					c.putIdleSession(session)
				}
			}
		}()
	}
	waitGroup.Wait()
	return errors.Join(warmupErrors...)
}

func (c *Client) getIdleSession() (idle *Session) {
	c.idleSessionLock.Lock()
	for len(c.idleSessions) > 0 {
		idle = c.idleSessions[0]
		c.removeIdleSessionLocked(0)
		if !idle.IsClosed() {
			break
		}
		idle = nil
	}
	c.idleSessionLock.Unlock()
	return
}

func (c *Client) putIdleSession(session *Session) {
	c.idleSessionLock.Lock()
	defer c.idleSessionLock.Unlock()
	if session.IsClosed() {
		return
	}
	if session.idleIndex >= 0 {
		return
	}
	session.idleSince = time.Now()
	session.idleIndex = len(c.idleSessions)
	c.idleSessions = append(c.idleSessions, session)
	c.idleSessionBubbleUp(session.idleIndex)
}

func (c *Client) createSession(ctx context.Context) (*Session, error) {
	underlying, err := c.dialOut(ctx)
	if err != nil {
		return nil, err
	}
	select {
	case <-c.die.Done():
		_ = underlying.Close()
		return nil, io.ErrClosedPipe
	default:
	}

	session := NewClientSession(underlying, c.padding)
	session.seq = c.sessionCounter.Add(1)
	session.dieHook = func() {
		if clientDebugSessionPool {
			logrus.Infoln("session died:", session.seq, session.streamId.Load(), session.pktCounter.Load())
		}

		c.idleSessionLock.Lock()
		if session.idleIndex >= 0 {
			c.removeIdleSessionLocked(session.idleIndex)
		}
		c.idleSessionLock.Unlock()

		c.sessionsLock.Lock()
		delete(c.sessions, session.seq)
		c.sessionsLock.Unlock()
	}

	c.sessionsLock.Lock()
	select {
	case <-c.die.Done():
		c.sessionsLock.Unlock()
		_ = session.Close()
		return nil, io.ErrClosedPipe
	default:
		c.sessions[session.seq] = session
		c.sessionsLock.Unlock()
	}

	session.Run()
	return session, nil
}

func (c *Client) Close() error {
	c.dieCancel()

	c.sessionsLock.Lock()
	sessionToClose := make([]*Session, 0, len(c.sessions))
	for _, session := range c.sessions {
		sessionToClose = append(sessionToClose, session)
	}
	clear(c.sessions)
	c.sessionsLock.Unlock()

	for _, session := range sessionToClose {
		session.Close()
	}

	return nil
}

func (c *Client) idleCleanup() {
	c.idleCleanupExpTime(time.Now().Add(-c.idleSessionTimeout))
}

func (c *Client) idleCleanupExpTime(expTime time.Time) {
	var sessionToClose []*Session

	c.idleSessionLock.Lock()
	expired := make([]*Session, 0, len(c.idleSessions))
	for _, session := range c.idleSessions {
		if clientDebugSessionPool {
			logrus.Debugln("check session:", session.seq, expTime, session.idleSince)
		}
		if session.idleSince.Before(expTime) {
			expired = append(expired, session)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		return expired[i].seq < expired[j].seq
	})
	closeCount := min(len(expired), max(0, len(c.idleSessions)-c.minIdleSession))
	for _, session := range expired[:closeCount] {
		sessionToClose = append(sessionToClose, session)
		c.removeIdleSessionLocked(session.idleIndex)
	}
	c.idleSessionLock.Unlock()

	for _, session := range sessionToClose {
		if clientDebugSessionPool {
			logrus.Infoln("local cleanup session:", session.seq)
		}
		session.Close()
	}
}

func (c *Client) removeIdleSessionLocked(index int) *Session {
	removed := c.idleSessions[index]
	last := len(c.idleSessions) - 1
	if index != last {
		c.idleSessions[index] = c.idleSessions[last]
		c.idleSessions[index].idleIndex = index
	}
	c.idleSessions[last] = nil
	c.idleSessions = c.idleSessions[:last]
	removed.idleIndex = -1
	if index < len(c.idleSessions) {
		if !c.idleSessionBubbleUp(index) {
			c.idleSessionBubbleDown(index)
		}
	}
	return removed
}

func (c *Client) idleSessionBubbleUp(index int) bool {
	moved := false
	for index > 0 {
		parent := (index - 1) / 2
		if c.idleSessions[parent].seq >= c.idleSessions[index].seq {
			break
		}
		c.swapIdleSessions(parent, index)
		index = parent
		moved = true
	}
	return moved
}

func (c *Client) idleSessionBubbleDown(index int) {
	for {
		left := index*2 + 1
		if left >= len(c.idleSessions) {
			return
		}
		largest := left
		right := left + 1
		if right < len(c.idleSessions) && c.idleSessions[right].seq > c.idleSessions[left].seq {
			largest = right
		}
		if c.idleSessions[index].seq >= c.idleSessions[largest].seq {
			return
		}
		c.swapIdleSessions(index, largest)
		index = largest
	}
}

func (c *Client) swapIdleSessions(i, j int) {
	c.idleSessions[i], c.idleSessions[j] = c.idleSessions[j], c.idleSessions[i]
	c.idleSessions[i].idleIndex = i
	c.idleSessions[j].idleIndex = j
}
