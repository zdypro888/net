package wsproxy

import (
	"container/list"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNoConnection = errors.New("no available connection")

// Server 表示一个代理服务器
type Server struct {
	locker   sync.Mutex
	sessions *list.List // 使用 list 保持顺序，FIFO 方式使用连接
}

// NewServer 创建新的代理服务器
func NewServer() *Server {
	return &Server{
		sessions: list.New(),
	}
}

// OnConnection 处理新连接
func (server *Server) OnConnection(conn *websocket.Conn) {
	// 设置读取超时，防止恶意连接
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var incoming connPacket
	if err := conn.ReadJSON(&incoming); err != nil {
		conn.Close()
		return
	}
	// 清除读取超时
	conn.SetReadDeadline(time.Time{})
	switch incoming.Method {
	case MethodRegisterSlaver:
		// 注册连接
		session := &Session{Id: incoming.Id, Conn: conn}
		server.locker.Lock()
		server.sessions.PushBack(session)
		server.locker.Unlock()
	case MethodSlaverDialout:
		// 处理 Dialout 请求
		go server.onClientDialout(context.Background(), conn, &incoming)
	default:
		conn.Close()
	}
}

// popSession 从连接池中取出第一个会话（FIFO）
func (server *Server) popSession() *Session {
	server.locker.Lock()
	defer server.locker.Unlock()
	if server.sessions.Len() == 0 {
		return nil
	}
	front := server.sessions.Front()
	server.sessions.Remove(front)
	return front.Value.(*Session)
}

// DialContext 通过代理连接到目标地址
func (server *Server) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// popSession 已经从池中移除了会话，每个连接只用一次
	session := server.popSession()
	if session == nil {
		return nil, ErrNoConnection
	}

	// 设置超时，防止阻塞
	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	session.Conn.SetWriteDeadline(time.Now().Add(timeout))
	session.Conn.SetReadDeadline(time.Now().Add(timeout))

	outgoing := &connPacket{
		Id:      session.Id,
		Method:  MethodSlaverDialout, // Dial
		Network: network,
		Address: address,
	}
	if err := session.Conn.WriteJSON(outgoing); err != nil {
		session.Close()
		return nil, err
	}
	var incoming connPacket
	if err := session.Conn.ReadJSON(&incoming); err != nil {
		session.Close()
		return nil, err
	}

	// 清除超时，后续由 copyLoop 管理
	session.Conn.SetWriteDeadline(time.Time{})
	session.Conn.SetReadDeadline(time.Time{})

	if incoming.Method != MethodSlaverDialoutSuccess {
		session.Close()
		if incoming.Error != "" {
			return nil, errors.New(incoming.Error)
		}
		return nil, errors.New("dial failed")
	}

	return session, nil
}

func (server *Server) onClientDialout(ctx context.Context, conn *websocket.Conn, packet *connPacket) {
	session, err := server.DialContext(ctx, packet.Network, packet.Address)
	if err != nil {
		// 发送连接错误响应，忽略写入错误（连接可能已断开）
		conn.WriteJSON(&connPacket{
			Id:     packet.Id,
			Method: MethodClientDialoutError, // 连接错误
			Error:  err.Error(),
		})
		conn.Close()
		return
	}
	// 发送连接成功响应
	if err := conn.WriteJSON(&connPacket{
		Id:     packet.Id,
		Method: MethodClientDialoutSuccess, // 连接成功
	}); err != nil {
		// WebSocket 写入失败，关闭两端连接
		conn.Close()
		session.Close()
		return
	}
	p := &pump{}
	p.copyLoop(ctx, conn, session)
}

// ConnectionCount 返回当前连接数
func (server *Server) ConnectionCount() int {
	server.locker.Lock()
	defer server.locker.Unlock()
	return server.sessions.Len()
}

// CloseAll 关闭所有连接
func (server *Server) CloseAll() {
	server.locker.Lock()
	defer server.locker.Unlock()

	for server.sessions.Len() > 0 {
		front := server.sessions.Front()
		server.sessions.Remove(front)
		front.Value.(*Session).Close()
	}
}

var DefaultServer = NewServer()
