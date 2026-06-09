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
	locker         sync.Mutex
	sessions       map[string]*Session[T]
	bufferSize     int
	codecs         *codecSet
	maxMessageSize int64
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
	return &Server[T]{
		sessions:       make(map[string]*Session[T]),
		bufferSize:     bufferSize,
		codecs:         newCodecSet(o.codecs),
		maxMessageSize: o.resolvedMaxMessageSize(),
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
	// 协商 codec: 取客户端偏好里服务端也支持的第一个; 客户端未带偏好 (旧客户端) 回退 JSON。
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
	if existing, exists := server.sessions[req.GUID]; exists {
		session = existing
	} else {
		session = createSessionWithBuffer[T](req.GUID, server.bufferSize)
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
			}
			server.locker.Unlock()
			return nil, errors.Join(err, closeErr, session.Close())
		}
		return nil, errors.Join(err, closeErr)
	}
	return session, nil
}

// Close 关闭服务器，断开所有会话
func (server *Server[T]) Close() error {
	server.locker.Lock()
	defer server.locker.Unlock()

	var err error
	for guid, session := range server.sessions {
		err = errors.Join(err, session.Close())
		delete(server.sessions, guid)
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
	defer server.locker.Unlock()
	if session, ok := server.sessions[guid]; ok {
		if err := session.Close(); err != nil {
			slog.Warn("wsc server remove session close failed",
				slog.String("guid", guid), slog.Any("err", err))
		}
		delete(server.sessions, guid)
	}
}
