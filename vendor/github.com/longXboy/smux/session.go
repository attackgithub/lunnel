package smux

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

const (
	defaultAcceptBacklog = 1024
)

const (
	errBrokenPipe      = "broken pipe"
	errInvalidProtocol = "invalid protocol version"
	errGoAway          = "stream id overflows, should start a new connection"
)

type writeRequest struct {
	frame  Frame
	result chan writeResult
}

type writeResult struct {
	n   int
	err error
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn io.ReadWriteCloser

	config           *Config
	nextStreamID     uint32 // next stream identifier
	nextStreamIDLock sync.Mutex

	bucket       int32         // token bucket
	bucketNotify chan struct{} // used for waiting for tokens

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die       chan struct{} // flag session has died
	sendDie   chan struct{}
	dieLock   sync.Mutex
	chAccepts chan *Stream

	dataReady int32 // flag data has arrived

	goAway int32 // flag id exhausted

	deadline atomic.Value

	writes chan writeRequest
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.sendDie = make(chan struct{})
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketNotify = make(chan struct{}, 1)
	s.writes = make(chan writeRequest)

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}
	go s.recvLoop()
	go s.sendLoop()
	go s.keepalive()
	if int64(s.config.IdleStreamTimeout) > 0 {
		go s.idleManage()
	}
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream(tunnelName string) (*Stream, error) {
	if s.IsClosed() {
		return nil, errors.New(errBrokenPipe)
	}

	// generate stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}

	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid%2 { // stream-id overflows
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}
	s.nextStreamIDLock.Unlock()

	stream := newStream(sid, s.config.MaxFrameSize, s, tunnelName)

	f := newFrame(cmdSYN, sid)
	f.data = []byte(tunnelName)
	if _, err := s.writeFrame(f); err != nil {
		return nil, errors.Wrap(err, "writeFrame")
	}

	s.streamLock.Lock()
	s.streams[sid] = stream
	s.streamLock.Unlock()
	return stream, nil
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(d.Sub(time.Now()))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-deadline:
		return nil, errTimeout
	case <-s.die:
		return nil, errors.New(errBrokenPipe)
	}
}

func (s *Session) finish() (err error) {
	s.dieLock.Lock()

	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe)
	default:
		s.writeFrame(newFrame(cmdNOP, 0))
		close(s.die)
		s.dieLock.Unlock()
		close(s.sendDie)
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		s.notifyBucket()
		return s.conn.Close()
	}

}

// Close is used to close the session and all streams.
func (s *Session) Close() (err error) {
	s.dieLock.Lock()

	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe)
	default:
		s.writeFrame(newFrame(cmdNOP, 0))
		close(s.die)
		s.dieLock.Unlock()
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		s.notifyBucket()

		go func() {
			defer func() {
				close(s.sendDie)
				s.conn.Close()
			}()

			deadLine := time.NewTimer(time.Second * 60)
			defer deadLine.Stop()

			f := newFrame(cmdSESSFIN, 0)
			req := writeRequest{
				frame:  f,
				result: make(chan writeResult, 1),
			}
			select {
			case <-deadLine.C:
				return
			case s.writes <- req:
			}

			<-deadLine.C
		}()
		return
	}
}

// notifyBucket notifies recvLoop that bucket is available
func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:
	}
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.deadline.Store(t)
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	delete(s.streams, sid)
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// session read a frame from underlying connection
// it's data is pointed to the input buffer
func (s *Session) readFrame(buffer []byte) (f Frame, err error) {
	if _, err := io.ReadFull(s.conn, buffer[:headerSize]); err != nil {
		return f, errors.Wrap(err, "readFrame")
	}

	dec := rawHeader(buffer)
	if dec.Version() != version {
		return f, errors.New(errInvalidProtocol)
	}

	f.ver = dec.Version()
	f.cmd = dec.Cmd()
	f.sid = dec.StreamID()
	if length := dec.Length(); length > 0 {
		if _, err := io.ReadFull(s.conn, buffer[headerSize:headerSize+length]); err != nil {
			return f, errors.Wrap(err, "readFrame")
		}
		f.data = buffer[headerSize : headerSize+length]
	}
	return f, nil
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	buffer := make([]byte, (1<<16)+headerSize)
	for {
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			<-s.bucketNotify
		}

		if f, err := s.readFrame(buffer); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)

			switch f.cmd {
			case cmdNOP:
			case cmdSESSFIN:
				s.finish()
				return
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[f.sid]; !ok {
					stream := newStream(f.sid, s.config.MaxFrameSize, s, string(f.data))
					s.streams[f.sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()

			case cmdFIN:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.markRST()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					atomic.AddInt32(&s.bucket, -int32(len(f.data)))
					stream.pushBytes(f.data)
					stream.notifyReadEvent()
					atomic.StoreUint32(&stream.activeTs, uint32(time.Now().Unix()))
				}
				s.streamLock.Unlock()
			default:
				s.finish()
				return
			}
		} else {
			s.finish()
			return
		}
	}
}

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

func (s *Session) idleManage() {
	timeOut := int32(int64(s.config.IdleStreamTimeout) / 1000000000)

	ticker := time.NewTicker(s.config.IdleStreamTimeout / 30)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var streams []*Stream
			s.streamLock.Lock()
			for _, stream := range s.streams {
				streams = append(streams, stream)
			}
			s.streamLock.Unlock()
			now := uint32(time.Now().Unix())
			for i := range streams {
				ts := atomic.LoadUint32(&streams[i].activeTs)
				if _itimediff(now, ts) > timeOut {
					streams[i].Close()
				}
			}
		case <-s.die:
			return
		}
	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)

	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <-tickerPing.C:
			s.writeFrame(newFrame(cmdNOP, 0))
			s.notifyBucket() // force a signal to the recvLoop
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				s.finish()
				return
			}
		case <-s.die:
			return
		}
	}
}

func (s *Session) sendLoop() {
	buf := make([]byte, (1<<16)+headerSize)
	for {
		select {
		case <-s.sendDie:
			return
		case request, ok := <-s.writes:
			if !ok {
				continue
			}
			buf[0] = request.frame.ver
			buf[1] = request.frame.cmd
			binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)
			copy(buf[headerSize:], request.frame.data)
			n, err := s.conn.Write(buf[:headerSize+len(request.frame.data)])

			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n:   n,
				err: err,
			}

			request.result <- result
			close(request.result)
		}
	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	req := writeRequest{
		frame:  f,
		result: make(chan writeResult, 1),
	}
	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	case s.writes <- req:
	}

	result := <-req.result
	return result.n, result.err
}
