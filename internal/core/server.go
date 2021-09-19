package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/yomorun/yomo/internal/frame"
	"github.com/yomorun/yomo/pkg/logger"
	"github.com/yomorun/yomo/pkg/tracing"
)

// Server is the underlining server of Zipper
type Server struct {
	token              string
	stream             quic.Stream
	state              string
	funcs              *ConcurrentMap // connected stream functions
	funcBuckets        map[int]string // user config stream functions
	counterOfDataFrame int64
	downstreams        map[string]*Client
}

// NewServer create a Server instance.
func NewServer(name string) *Server {
	s := &Server{
		token:       name,
		funcs:       NewConcurrentMap(),
		funcBuckets: make(map[int]string),
		downstreams: make(map[string]*Client),
	}
	once.Do(func() {
		s.init()
	})

	return s
}

// ListenAndServe starts the server.
func (s *Server) ListenAndServe(ctx context.Context, endpoint string) error {
	qconf := &quic.Config{
		Versions:                       []quic.VersionNumber{quic.Version1, quic.VersionDraft29},
		MaxIdleTimeout:                 time.Second * 3,
		KeepAlive:                      true,
		MaxIncomingStreams:             10000,
		MaxIncomingUniStreams:          10000,
		HandshakeIdleTimeout:           time.Second * 3,
		InitialStreamReceiveWindow:     1024 * 1024 * 2,
		InitialConnectionReceiveWindow: 1024 * 1024 * 2,
		DisablePathMTUDiscovery:        true,
		// Tracer:                         getQlogConfig("server"),
	}

	// if os.Getenv("YOMO_QLOG") != "" {
	// 	s.logger.Debugf("YOMO_QLOG=%s", os.Getenv("YOMO_QLOG"))
	// 	qconf.Tracer = getQlogConfig("server")
	// }

	// listen the address
	listener, err := quic.ListenAddr(endpoint, generateTLSConfig(endpoint), qconf)
	if err != nil {
		logger.Errorf("%squic.ListenAddr on: %s, err=%v", ServerLogPrefix, endpoint, err)
		return err
	}
	defer listener.Close()
	logger.Printf("%s✅ (name:%s) Listening on: %s, QUIC: %v", ServerLogPrefix, s.token, listener.Addr(), qconf.Versions)

	s.state = ConnStateConnected
	for {
		// create a new session when new yomo-client connected
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()

		session, err := listener.Accept(sctx)
		if err != nil {
			logger.Errorf("%screate session error: %v", ServerLogPrefix, err)
			sctx.Done()
			return err
		}

		connID := getConnID(session)
		logger.Infof("%s❤️1/ new connection: %s", ServerLogPrefix, connID)

		go func(ctx context.Context, sess quic.Session) {
			for {
				logger.Infof("%s❤️2/ waiting for new stream", ServerLogPrefix)
				stream, err := sess.AcceptStream(ctx)
				if err != nil {
					// if client close the connection, then we should close the session
					logger.Errorf("%s❤️3/ %T on [stream] %v, deleting from s.funcs if this stream is [sfn]", ServerLogPrefix, err, err)
					if name, ok := s.funcs.GetSfn(connID); ok {
						s.funcs.Remove(name, connID)
					}
					break
				}
				defer stream.Close()
				// defer ctx.Done()
				logger.Infof("%s❤️4/ [stream:%d] created, connID=%s", ServerLogPrefix, stream.StreamID(), connID)
				// 监听 stream 并做处理
				s.handleSession(session, stream)
				logger.Infof("%s❤️5/ [stream:%d] handleSession DONE", ServerLogPrefix, stream.StreamID())
			}
		}(sctx, session)
	}
}

// Close will shutdown the server.
func (s *Server) Close() error {
	if s.stream != nil {
		if err := s.stream.Close(); err != nil {
			logger.Errorf("%sClose(): %v", ServerLogPrefix, err)
			return err
		}
	}
	return nil
}

// handle streams on a session
func (s *Server) handleSession(session quic.Session, mainStream quic.Stream) {
	fs := NewFrameStream(mainStream)
	// check update for stream
	for {
		logger.Infof("%shandleSession 💚 waiting read next...", ServerLogPrefix)
		f, err := fs.ReadFrame()
		if err != nil {
			logger.Errorf("%s%T %v", ServerLogPrefix, err, err)
			if errors.Is(err, net.ErrClosed) {
				// if client close the connection, net.ErrClosed will be raise
				// by quic-go IdleTimeoutError after connection's KeepAlive config.
				// logger.Infof("[ERR] on [ParseFrame] %v", net.ErrClosed)
				break
			}
			// any error occurred, we should close the session
			// after this, session.AcceptStream() will raise the error
			// which specific in session.CloseWithError()
			mainStream.Close()
			session.CloseWithError(0xCC, err.Error())
			logger.Warnf("%ssession.Close()", ServerLogPrefix)
			break
		}

		frameType := f.Type()
		logger.Debugf("%stype=%s, frame=%# x", ServerLogPrefix, frameType, f.Encode())
		switch frameType {
		case frame.TagOfHandshakeFrame:
			s.handleHandShakeFrame(mainStream, session, f.(*frame.HandshakeFrame))
		case frame.TagOfPingFrame:
			s.handlePingFrame(mainStream, session, f.(*frame.PingFrame))
		case frame.TagOfDataFrame:
			s.handleDataFrame(mainStream, session, f.(*frame.DataFrame))
			s.dispatchToDownstreams(f.(*frame.DataFrame))
		default:
			logger.Errorf("%serr=%v, frame=%v", ServerLogPrefix, err, f.Encode())
		}
	}
}

// StatsFunctions returns the sfn stats of server.
func (s *Server) StatsFunctions() map[string][]*quic.Stream {
	return s.funcs.GetCurrentSnapshot()
}

// StatsCounter returns how many DataFrames pass through server.
func (s *Server) StatsCounter() int64 {
	return s.counterOfDataFrame
}

// Downstreams return all the downstream servers.
func (s *Server) Downstreams() map[string]*Client {
	return s.downstreams
}

func (s *Server) handleHandShakeFrame(stream quic.Stream, session quic.Session, f *frame.HandshakeFrame) error {
	logger.Infof("%s ------> GOT ❤️ HandshakeFrame : %# x", ServerLogPrefix, f)
	logger.Infof("%sClientType=%# x, is %s", ServerLogPrefix, f.ClientType, ClientType(f.ClientType))
	// client type
	clientType := ClientType(f.ClientType)
	switch clientType {
	case ClientTypeSource:
	case ClientTypeStreamFunction:
		// 检查 name 是否有效，如果无效则需要关闭连接。
		if !s.validateHandshake(f) {
			// unexpected client connected. close the connection.
			stream.Close()
			session.CloseWithError(0xCC, "handshake validation faild, illegal sfn")
			// break
			return errors.New("core.server: handshake validation faild, illegal sfn")
		}

		// 校验成功，注册 sfn 给 SfnManager
		s.funcs.Set(f.Name, getConnID(session), &stream)

	case ClientTypeUpstreamZipper:
	default:
		// Step 1-4: 错误，不认识该 client-type，关闭连接
		logger.Errorf("%sClientType=%# x, ilegal!", ServerLogPrefix, f.ClientType)
		// stream.Close()
		// session.CloseWithError(0xCC, "Unknown ClientType, ilegal!")
		return errors.New("core.server: Unknown ClientType, illegal")
	}
	return nil
}

func (s *Server) handlePingFrame(stream quic.Stream, session quic.Session, f *frame.PingFrame) error {
	logger.Infof("%s------> GOT ❤️ PingFrame : %# x", ServerLogPrefix, f)
	return nil
}

func (s *Server) handleDataFrame(mainStream quic.Stream, session quic.Session, f *frame.DataFrame) error {
	currentIssuer := f.GetIssuer()

	// tracing
	span, err := tracing.NewRemoteTraceSpan(f.GetMetadata("TraceID"), f.GetMetadata("SpanID"), "server", fmt.Sprintf("handleDataFrame <-[%s]", currentIssuer))
	if err == nil {
		defer span.End()
	}
	// counter +1
	atomic.AddInt64(&s.counterOfDataFrame, 1)
	// 收到数据帧
	logger.Infof("%sframeType=%s, metadata=%s, issuer=%s, session.RemoteAddr()=%s, counter=%d", ServerLogPrefix, f.Type(), f.GetMetadatas(), currentIssuer, session.RemoteAddr(), s.counterOfDataFrame)
	// 因为是Immutable Stream，按照规则发送给 sfn
	var j int
	for i, fn := range s.funcBuckets {
		// 发送给 currentIssuer 的下一个 sfn
		if fn == currentIssuer {
			j = i + 1
		}
	}
	// 表示要执行第一个 sfn
	if j == 0 {
		logger.Infof("%s1st sfn write to [%s] -> [%s]:", ServerLogPrefix, currentIssuer, s.funcBuckets[0])
		targetStream := s.funcs.Get(s.funcBuckets[0])
		if targetStream == nil {
			logger.Debugf("%ssfn[%s] stream is nil", ServerLogPrefix, s.funcBuckets[0])
			err := fmt.Errorf("sfn[%s] stream is nil", s.funcBuckets[0])
			return err
		}
		(*targetStream).Write(f.Encode())
		return nil
	}

	if len(s.funcBuckets[j]) == 0 {
		logger.Debugf("%sno sfn found, drop this data frame", ServerLogPrefix)
		err := errors.New("no sfn found, drop this data frame")
		return err
	}

	targetStream := s.funcs.Get(s.funcBuckets[j])
	logger.Infof("%swill write to: [%s] -> [%s], target is nil:%v", ServerLogPrefix, currentIssuer, s.funcBuckets[j], targetStream == nil)
	if targetStream != nil {
		(*targetStream).Write(f.Encode())
	}

	return nil
}

// AddWorkflow add sfn to this server.
func (s *Server) AddWorkflow(wfs ...Workflow) error {
	for _, wf := range wfs {
		s.funcBuckets[wf.Seq] = wf.Token
	}
	return nil
}

// validateHandshake validates if the handshake frame is valid.
func (s *Server) validateHandshake(f *frame.HandshakeFrame) bool {
	isValid := false
	for _, k := range s.funcBuckets {
		logger.Debugf("%s>>> validateHandshake: (f)=%s, (list)=%s", ServerLogPrefix, f.Name, k)
		if k == f.Name {
			isValid = true
			break
		}
	}

	logger.Warnf("%svalidateHandshake(%v), result: %v", ServerLogPrefix, *f, isValid)
	return isValid
}

func (s *Server) init() {
	// tracing
	_, _, err := tracing.NewTracerProvider(s.token)
	if err != nil {
		logger.Errorf("tracing: %v", err)
	}
}

// AddDownstreamServer add a downstream server to this server. all the DataFrames will be
// dispatch to all the downstreams.
func (s *Server) AddDownstreamServer(addr string, c *Client) {
	s.downstreams[addr] = c
}

// dispatch every DataFrames to all downstreams
func (s *Server) dispatchToDownstreams(df *frame.DataFrame) {
	for addr, ds := range s.downstreams {
		logger.Debugf("%sdispatching to [%s]: %# x", ServerLogPrefix, addr, df.SeqID())
		ds.WriteFrame(df)
	}
}

// getConnID get quic session connection id
func getConnID(sess quic.Session) string {
	return sess.RemoteAddr().String()
}
