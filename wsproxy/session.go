package wsproxy

import (
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// Session 表示一个代理会话, 需要实现 net.Conn 接口
type Session struct {
	Id   int64
	Conn *websocket.Conn
}

func (s *Session) Read(b []byte) (n int, err error) {
	_, message, err := s.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	n = copy(b, message)
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
