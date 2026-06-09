package wsc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server 管理多个 Session
type Server[T any] struct {
	locker             sync.Mutex
	sessions           map[string]*Session[T]
	cleanupTimers      map[string]cleanupTimer
	bufferSize         int
	codecs             *codecSet
	maxMessageSize     int64
	sessionIdleTimeout time.Duration
}

type cleanupTimer struct {
	generation uint64
	timer      *time.Timer
}

// NewServer 创建服务器。可选 WithCodecs 配置支持的编码 (默认仅 JSON)。
func NewServer[T any](opts ...Option) *Server[T] {
	return NewServerWithBuffer[T](DefaultBufferSize, opts...)
}

// NewServerWithBuffer 创建服务器并设置每个 Session 的内部队列容量.
func NewServerWithBuffer[T any](bufferSize int, opts ...Option) *Server[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	sessionIdleTimeout := DefaultSessionIdleTimeout
	if o.sessionIdleTimeout != nil {
		sessionIdleTimeout = *o.sessionIdleTimeout
	}
	return &Server[T]{
		sessions:           make(map[string]*Session[T]),
		cleanupTimers:      make(map[string]cleanupTimer),
		bufferSize:         bufferSize,
		codecs:             newCodecSet(o.codecs),
		maxMessageSize:     o.resolvedMaxMessageSize(),
		sessionIdleTimeout: sessionIdleTimeout,
	}
}

func (server *Server[T]) cancelSessionCleanupLocked(guid string) {
	if cleanup, ok := server.cleanupTimers[guid]; ok {
		delete(server.cleanupTimers, guid)
		// Stop 对已触发的 timer 返回 false 也无妨: expireSession 内部还会按
		// generation/cur 复查, 已触发的回调会因不匹配而成为 no-op。
		cleanup.timer.Stop()
	}
}

func (server *Server[T]) cancelSessionCleanup(guid string) {
	server.locker.Lock()
	server.cancelSessionCleanupLocked(guid)
	server.locker.Unlock()
}

// scheduleSessionCleanup 用运行时定时器堆 (time.AfterFunc) 挂一个到期回调, 等待期间
// 不占 goroutine; 到期才临时起一次性 goroutine 跑 expireSession。每个 guid 至多一个
// pending timer (新调度前先 Stop 旧的)。
func (server *Server[T]) scheduleSessionCleanup(session *Session[T], generation uint64) {
	if server.sessionIdleTimeout <= 0 || session.generation() != generation {
		return
	}
	server.locker.Lock()
	defer server.locker.Unlock()
	if cur := server.sessions[session.guid]; cur != session || session.generation() != generation {
		return
	}
	server.cancelSessionCleanupLocked(session.guid)
	timer := time.AfterFunc(server.sessionIdleTimeout, func() {
		server.expireSession(session, generation)
	})
	server.cleanupTimers[session.guid] = cleanupTimer{generation: generation, timer: timer}
}

// expireSession 在空闲定时器到期时执行: 仅当会话仍是同一代且未被替换/移除时删除并关闭。
func (server *Server[T]) expireSession(session *Session[T], generation uint64) {
	var expired *Session[T]
	server.locker.Lock()
	if cleanup, ok := server.cleanupTimers[session.guid]; ok && cleanup.generation == generation {
		delete(server.cleanupTimers, session.guid)
	}
	if cur := server.sessions[session.guid]; cur == session && session.generation() == generation {
		delete(server.sessions, session.guid)
		expired = session
	}
	server.locker.Unlock()
	if expired != nil {
		if err := expired.Close(); err != nil {
			slog.Warn("wsc server idle session close failed",
				slog.String("guid", expired.guid), slog.Any("err", err))
		}
	}
}

// OnConnection 处理新连接
// args 是可选的额外参数
// 返回 Session，通过 session.Handle() 获取消息通道
func (server *Server[T]) OnConnection(conn *websocket.Conn, args any) (*Session[T], error) {
	conn.SetReadLimit(server.maxMessageSize)
	if err := conn.SetReadDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	var req HandshakeRequest
	if err := conn.ReadJSON(&req); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	// 校验握手请求
	if req.GUID == "" || req.Version != ProtocolVersion {
		handshakeErr := fmt.Errorf("client[%s] handshake failed. version=%s", req.GUID, req.Version)
		if err := conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
			return nil, errors.Join(handshakeErr, err, conn.Close())
		}
		if err := conn.WriteJSON(HandshakeResponse{Status: 500, Message: "invalid request"}); err != nil {
			return nil, errors.Join(handshakeErr, err, conn.Close())
		}
		return nil, errors.Join(handshakeErr, conn.Close())
	}
	// 协商 codec: 取客户端偏好里服务端也支持的第一个; 同协议版本客户端未带偏好时回退 JSON。
	codec, ok := server.codecs.negotiate(req.Codecs)
	if !ok {
		handshakeErr := fmt.Errorf("client[%s] handshake failed: no common codec, client offered %v", req.GUID, req.Codecs)
		if err := conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
			return nil, errors.Join(handshakeErr, err, conn.Close())
		}
		if err := conn.WriteJSON(HandshakeResponse{Status: 500, Message: "no common codec"}); err != nil {
			return nil, errors.Join(handshakeErr, err, conn.Close())
		}
		return nil, errors.Join(handshakeErr, conn.Close())
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := conn.WriteJSON(HandshakeResponse{Status: 200, Codec: codec.Name()}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	var session *Session[T]
	var created bool
	server.cancelSessionCleanup(req.GUID)
	server.locker.Lock()
	if existing, exists := server.sessions[req.GUID]; exists {
		session = existing
		// 同 GUID 重连已开始: 先推进 generation, 让任何已触发但尚未执行的
		// 旧 idle-cleanup 回调失效。真正 reset 成功后 asyncGo 会再推进到
		// 本次新连接的 generation。
		session.advanceGeneration()
		session.setOnDisconnect(server.scheduleSessionCleanup)
	} else {
		session = createSessionWithBuffer[T](req.GUID, server.bufferSize)
		session.setOnDisconnect(server.scheduleSessionCleanup)
		server.sessions[req.GUID] = session
		created = true
	}
	server.locker.Unlock()
	if err := session.reset(context.Background(), conn, codec); err != nil {
		closeErr := conn.Close()
		if created {
			server.locker.Lock()
			if cur, ok := server.sessions[req.GUID]; ok && cur == session {
				delete(server.sessions, req.GUID)
				server.cancelSessionCleanupLocked(req.GUID)
			}
			server.locker.Unlock()
			return nil, errors.Join(err, closeErr, session.Close())
		}
		server.scheduleSessionCleanup(session, session.generation())
		return nil, errors.Join(err, closeErr)
	}
	return session, nil
}

// Close 关闭服务器，断开所有会话
func (server *Server[T]) Close() error {
	server.locker.Lock()
	sessions := make([]*Session[T], 0, len(server.sessions))
	for guid, session := range server.sessions {
		server.cancelSessionCleanupLocked(guid)
		delete(server.sessions, guid)
		sessions = append(sessions, session)
	}
	server.locker.Unlock()
	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.Close())
	}
	return err
}

// GetSession 获取指定 GUID 的会话
func (server *Server[T]) GetSession(guid string) *Session[T] {
	server.locker.Lock()
	defer server.locker.Unlock()
	return server.sessions[guid]
}

// RemoveSession 移除并关闭指定会话
func (server *Server[T]) RemoveSession(guid string) {
	server.locker.Lock()
	session, ok := server.sessions[guid]
	if ok {
		server.cancelSessionCleanupLocked(guid)
		delete(server.sessions, guid)
	}
	server.locker.Unlock()
	if ok {
		if err := session.Close(); err != nil {
			slog.Warn("wsc server remove session close failed",
				slog.String("guid", guid), slog.Any("err", err))
		}
	}
}
