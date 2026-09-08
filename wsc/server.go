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
	handshakes         map[*websocket.Conn]struct{}
	cleanupTimers      map[string]*cleanupTimer
	attaching          map[*Session[T]]*sessionAttach
	bufferSize         int
	codecs             *codecSet
	maxMessageSize     int64
	sessionIdleTimeout time.Duration
	closed             bool
}

// sessionAttach 暂存连接交接期间的断线事件，避免旧连接清理与新连接安装互相抢占。
type sessionAttach struct {
	count        int
	disconnected bool
	generation   uint64
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
		handshakes:         make(map[*websocket.Conn]struct{}),
		cleanupTimers:      make(map[string]*cleanupTimer),
		attaching:          make(map[*Session[T]]*sessionAttach),
		bufferSize:         bufferSize,
		codecs:             newCodecSet(o.codecs),
		maxMessageSize:     o.resolvedMaxMessageSize(),
		sessionIdleTimeout: sessionIdleTimeout,
	}
}

func (server *Server[T]) cancelSessionCleanupLocked(guid string) bool {
	if cleanup, ok := server.cleanupTimers[guid]; ok {
		delete(server.cleanupTimers, guid)
		// Stop 对已触发的 timer 返回 false 也无妨: 本函数已先删掉表项,
		// 已触发但稍后才执行的回调会在 expireSession 的表项门控处 no-op。
		cleanup.timer.Stop()
		return true
	}
	return false
}

func (server *Server[T]) cancelSessionCleanup(guid string) bool {
	server.locker.Lock()
	canceled := server.cancelSessionCleanupLocked(guid)
	server.locker.Unlock()
	return canceled
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
	if attach := server.attaching[session]; attach != nil {
		attach.disconnected, attach.generation = true, generation
		return
	}
	server.cancelSessionCleanupLocked(session.guid)
	cleanup := &cleanupTimer{generation: generation}
	cleanup.timer = time.AfterFunc(server.sessionIdleTimeout, func() {
		server.expireSession(session, cleanup)
	})
	server.cleanupTimers[session.guid] = cleanup
}

// expireSession 同时匹配定时器对象、session 和代次；同代重排也不能被已触发的旧回调删除。
func (server *Server[T]) expireSession(session *Session[T], expected *cleanupTimer) {
	var expired *Session[T]
	server.locker.Lock()
	if cleanup := server.cleanupTimers[session.guid]; cleanup != nil && cleanup == expected {
		delete(server.cleanupTimers, session.guid)
		if cur := server.sessions[session.guid]; cur == session && session.generation() == cleanup.generation {
			delete(server.sessions, session.guid)
			expired = session
		}
	}
	server.locker.Unlock()
	if expired != nil {
		if err := expired.Close(); err != nil {
			slog.Warn("wsc server idle session close failed", slog.Any("err", err))
		}
	}
}

// finishSessionAttach 在最后一个并发交接结束后恢复仍适用于当前代次的断线清理。
func (server *Server[T]) finishSessionAttach(session *Session[T]) {
	server.locker.Lock()
	attach := server.attaching[session]
	attach.count--
	finished := attach.count == 0
	if finished {
		delete(server.attaching, session)
	}
	disconnected, generation := attach.disconnected, attach.generation
	server.locker.Unlock()
	if finished && disconnected {
		server.scheduleSessionCleanup(session, generation)
	}
}

// OnConnection 处理新连接
// args 是可选的连接参数，会原样附加到该连接产生的每个非关闭 Packet。
// 返回 Session，通过 session.Handle() 获取消息通道
func (server *Server[T]) OnConnection(conn *websocket.Conn, args any) (*Session[T], error) {
	server.locker.Lock()
	if server.closed {
		server.locker.Unlock()
		return nil, errors.Join(ErrServerClosed, conn.Close())
	}
	server.handshakes[conn] = struct{}{}
	server.locker.Unlock()
	defer func() {
		server.locker.Lock()
		delete(server.handshakes, conn)
		server.locker.Unlock()
	}()
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
	server.locker.Lock()
	hadIdleCleanup := server.cancelSessionCleanupLocked(req.GUID)
	if server.closed {
		server.locker.Unlock()
		return nil, errors.Join(ErrServerClosed, conn.Close())
	}
	delete(server.handshakes, conn)
	if existing, exists := server.sessions[req.GUID]; exists {
		// 复用同 GUID 的已存在 session 重连。真正的 generation 推进只发生在 reset 成功后的
		// asyncGo 里, 这里不预推进 —— 已触发但尚未执行的旧 idle-cleanup 回调由 expireSession
		// 的 timer 存在性门控作废 (上面 cancelSessionCleanup 已把 timer 从表中删除)。
		session = existing
		session.setOnDisconnect(server.scheduleSessionCleanup)
	} else {
		session = createSessionWithBuffer[T](req.GUID, server.bufferSize)
		session.setOnDisconnect(server.scheduleSessionCleanup)
		// D4: 调用方直接 session.Close() (不经 RemoveSession) 时也要把表项摘除,
		// 否则禁用 idle cleanup (WithSessionIdleTimeout<=0) 的配置下僵尸条目永不回收.
		session.setOnClose(server.detachSession)
		server.sessions[req.GUID] = session
		created = true
	}
	attach := server.attaching[session]
	if attach == nil {
		attach = &sessionAttach{}
		server.attaching[session] = attach
	}
	attach.count++
	if hadIdleCleanup {
		attach.disconnected, attach.generation = true, session.generation()
	}
	server.locker.Unlock()
	defer server.finishSessionAttach(session)
	if err := session.reset(context.Background(), conn, codec, args); err != nil {
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
		// 已取消的旧清理及交接中收到的断线事件，由 finishSessionAttach 统一恢复。
		return nil, errors.Join(err, closeErr)
	}
	return session, nil
}

// Close 关闭服务器，断开所有会话
func (server *Server[T]) Close() error {
	server.locker.Lock()
	server.closed = true
	handshakes := make([]*websocket.Conn, 0, len(server.handshakes))
	for conn := range server.handshakes {
		delete(server.handshakes, conn)
		handshakes = append(handshakes, conn)
	}
	sessions := make([]*Session[T], 0, len(server.sessions))
	for guid, session := range server.sessions {
		server.cancelSessionCleanupLocked(guid)
		delete(server.sessions, guid)
		sessions = append(sessions, session)
	}
	server.locker.Unlock()
	var err error
	for _, conn := range handshakes {
		err = errors.Join(err, conn.Close())
	}
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

// detachSession 是 session.Close 触发的回调 (D4): 把自己从 sessions 表摘除并取消
// pending 的 idle-cleanup timer. 只摘表不再调 Close (调用方正在 Close 中).
// 与 RemoveSession / expireSession / OnConnection 错误路径并发安全: 各方都以
// "表内身份匹配才删除"为门控, 后到者 no-op, 不会 double-remove.
func (server *Server[T]) detachSession(session *Session[T]) {
	server.locker.Lock()
	if cur, ok := server.sessions[session.guid]; ok && cur == session {
		delete(server.sessions, session.guid)
		server.cancelSessionCleanupLocked(session.guid)
	}
	server.locker.Unlock()
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

// RemoveSessionIf 仅在表内仍是 expected 实例时移除会话。用于同一 GUID 的旧消费者
// 延迟退出时，避免误删已经替换的新 Session。
func (server *Server[T]) RemoveSessionIf(guid string, expected *Session[T]) bool {
	if expected == nil {
		return false
	}
	server.locker.Lock()
	session, ok := server.sessions[guid]
	if !ok || session != expected {
		server.locker.Unlock()
		return false
	}
	server.cancelSessionCleanupLocked(guid)
	delete(server.sessions, guid)
	server.locker.Unlock()
	if err := session.Close(); err != nil {
		slog.Warn("wsc server conditional session close failed",
			slog.String("guid", guid), slog.Any("err", err))
	}
	return true
}
