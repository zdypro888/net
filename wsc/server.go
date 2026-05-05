package wsc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server 管理多个 Session
type Server[T any] struct {
	locker     sync.Mutex
	sessions   map[string]*Session[T]
	bufferSize int
}

// NewServer 创建服务器
// onSession 回调在新会话建立时调用，返回 handler 用于处理消息
func NewServer[T any]() *Server[T] {
	return NewServerWithBuffer[T](DefaultBufferSize)
}

// NewServerWithBuffer 创建服务器并设置每个 Session 的内部队列容量.
func NewServerWithBuffer[T any](bufferSize int) *Server[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	return &Server[T]{
		sessions:   make(map[string]*Session[T]),
		bufferSize: bufferSize,
	}
}

// OnConnection 处理新连接
// args 是可选的额外参数
// 返回 Session，通过 session.Handle() 获取消息通道
func (server *Server[T]) OnConnection(conn *websocket.Conn, args any) (*Session[T], error) {
	conn.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	var req HandshakeRequest
	if err := conn.ReadJSON(&req); err != nil {
		conn.Close()
		return nil, err
	}
	// 校验握手请求
	if req.GUID == "" || req.Version != ProtocolVersion {
		conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout))
		conn.WriteJSON(HandshakeResponse{Status: 500, Message: "invalid request"})
		conn.Close()
		return nil, fmt.Errorf("client[%s] handshake failed. version=%s", req.GUID, req.Version)
	}
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout))
	if err := conn.WriteJSON(HandshakeResponse{Status: 200}); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
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
	if err := session.Reset(context.Background(), conn); err != nil {
		conn.Close()
		if created {
			server.locker.Lock()
			if cur, ok := server.sessions[req.GUID]; ok && cur == session {
				delete(server.sessions, req.GUID)
			}
			server.locker.Unlock()
			session.Close()
		}
		return nil, err
	}
	return session, nil
}

// Close 关闭服务器，断开所有会话
func (server *Server[T]) Close() error {
	server.locker.Lock()
	defer server.locker.Unlock()

	for guid, session := range server.sessions {
		session.Close()
		delete(server.sessions, guid)
	}
	return nil
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
		session.Close()
		delete(server.sessions, guid)
	}
}
