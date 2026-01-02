package wsproxy

import (
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// Session 表示一个代理会话, 需要实现 net.Conn 接口
type Session struct {
	Id     int64
	Conn   *websocket.Conn
	buffer []byte // 缓存未读完的数据
}

func (s *Session) Read(b []byte) (n int, err error) {
	// 如果缓存中有数据，先返回缓存的数据
	if len(s.buffer) > 0 {
		n = copy(b, s.buffer)
		s.buffer = s.buffer[n:]
		return n, nil
	}

	// 读取新的 WebSocket 消息
	_, message, err := s.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}

	// 如果消息长度小于等于 buffer 大小，直接复制
	if len(message) <= len(b) {
		return copy(b, message), nil
	}

	// 消息长度大于 buffer，复制部分数据，剩余存入缓存
	n = copy(b, message)
	s.buffer = make([]byte, len(message)-n)
	copy(s.buffer, message[n:])
	return n, nil
}

func (s *Session) Write(b []byte) (n int, err error) {
	err = s.Conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (s *Session) Close() error {
	return s.Conn.Close()
}

func (s *Session) LocalAddr() net.Addr {
	return s.Conn.LocalAddr()
}

func (s *Session) RemoteAddr() net.Addr {
	return s.Conn.RemoteAddr()
}

func (s *Session) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	return s.SetWriteDeadline(t)
}

func (s *Session) SetReadDeadline(t time.Time) error {
	return s.Conn.SetReadDeadline(t)
}

func (s *Session) SetWriteDeadline(t time.Time) error {
	return s.Conn.SetWriteDeadline(t)
}
