package wsproxy

import (
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Session 表示一个代理会话, 需要实现 net.Conn 接口
type Session struct {
	Id            string
	Conn          *websocket.Conn
	buffer        []byte // 缓存未读完的数据
	readMu        sync.Mutex
	writeMu       sync.Mutex
	deadlineMu    sync.Mutex
	writeDeadline time.Time
	close         sync.Once
	closeErr      error
	onClose       func(*Session)
}

// Read 从 WebSocket 二进制消息中读取字节，并缓存本次未消费的剩余数据。
func (s *Session) Read(b []byte) (n int, err error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

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

// Write 将数据作为一个二进制 WebSocket 消息写入代理会话。
func (s *Session) Write(b []byte) (n int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// SetWriteDeadline 可并发中断阻塞写；在同一把锁下同步 Gorilla 缓存的
	// 截止时间，保证最近一次设置生效。
	s.deadlineMu.Lock()
	err = s.Conn.SetWriteDeadline(s.writeDeadline)
	s.deadlineMu.Unlock()
	if err != nil {
		return 0, err
	}
	err = s.Conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close 关闭代理会话，并执行一次所属服务器的注销回调。
func (s *Session) Close() error {
	s.close.Do(func() {
		s.closeErr = s.Conn.Close()
		if s.onClose != nil {
			s.onClose(s)
		}
	})
	return s.closeErr
}

func (s *Session) setOnClose(onClose func(*Session)) {
	s.onClose = onClose
}

// LocalAddr 返回底层 WebSocket 连接的本地地址。
func (s *Session) LocalAddr() net.Addr {
	return s.Conn.LocalAddr()
}

// RemoteAddr 返回底层 WebSocket 连接的远端地址。
func (s *Session) RemoteAddr() net.Addr {
	return s.Conn.RemoteAddr()
}

// SetDeadline 同时设置代理会话的读取和写入截止时间。
func (s *Session) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	return s.SetWriteDeadline(t)
}

// SetReadDeadline 设置读取截止时间，并可中断正在阻塞的读取。
func (s *Session) SetReadDeadline(t time.Time) error {
	// Gorilla 的 SetReadDeadline 只转发到底层 net.Conn；直接调用底层可安全地
	// 中断正在阻塞的 Read，同时不与 Gorilla 的解析状态并发。
	return s.Conn.UnderlyingConn().SetReadDeadline(t)
}

// SetWriteDeadline 设置写入截止时间，并可中断正在阻塞的写入。
func (s *Session) SetWriteDeadline(t time.Time) error {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	if err := s.Conn.UnderlyingConn().SetWriteDeadline(t); err != nil {
		return err
	}
	s.writeDeadline = t
	return nil
}
