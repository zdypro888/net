package wsproxy

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Session 表示一个代理会话, 需要实现 net.Conn 接口
type Session struct {
	Id            string
	Conn          *websocket.Conn
	reader        io.Reader // 当前二进制帧，按调用方缓冲区流式读取
	readMu        sync.Mutex
	writeMu       sync.Mutex
	deadlineMu    sync.Mutex
	writeDeadline time.Time
	writeTimer    *time.Timer
	writeActive   bool
	writeTimedOut bool
	closed        bool
	close         sync.Once
	closeErr      error
	onClose       func(*Session)
}

// Read 从 WebSocket 二进制消息中读取字节，并缓存本次未消费的剩余数据。
func (s *Session) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for {
		if s.reader == nil {
			kind, reader, err := s.Conn.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				return 0, fmt.Errorf("wsproxy: expected binary stream, got message type %d", kind)
			}
			s.reader = reader
		}
		n, err := s.reader.Read(b)
		if err == io.EOF {
			// 帧边界不是 TCP 流的 EOF；空帧不向上游返回 (0, nil)。
			s.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// Write 将数据作为一个二进制 WebSocket 消息写入代理会话。
func (s *Session) Write(b []byte) (n int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// 已过期且尚未开始的写直接返回，不进入 Gorilla 使整条写流永久失效。
	// 一旦帧写入途中超时，Gorilla 连接不可恢复，调用方须重新拨号。
	s.deadlineMu.Lock()
	if s.closed {
		s.deadlineMu.Unlock()
		return 0, net.ErrClosed
	}
	if !s.writeDeadline.IsZero() && !time.Now().Before(s.writeDeadline) {
		s.deadlineMu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	// Gorilla 每次写帧都会应用缓存的期限；禁止它覆盖并发修改后的期限。
	// 由会话定时器中断在途写，过期的 WebSocket 帧不能继续复用。
	err = s.Conn.SetWriteDeadline(time.Time{})
	s.writeActive = err == nil
	s.armWriteTimerLocked()
	s.deadlineMu.Unlock()
	if err != nil {
		return 0, err
	}
	err = s.Conn.WriteMessage(websocket.BinaryMessage, b)
	s.deadlineMu.Lock()
	s.writeActive = false
	s.armWriteTimerLocked()
	if s.writeTimedOut {
		err = os.ErrDeadlineExceeded
	}
	s.deadlineMu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close 关闭代理会话，并执行一次所属服务器的注销回调。
func (s *Session) Close() error {
	s.close.Do(func() {
		s.deadlineMu.Lock()
		s.closed = true
		s.armWriteTimerLocked()
		s.deadlineMu.Unlock()
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
	if s.closed {
		return net.ErrClosed
	}
	s.writeDeadline = t
	s.armWriteTimerLocked()
	return nil
}

// armWriteTimerLocked 统一管理在途写期限；持 deadlineMu 调用。
func (s *Session) armWriteTimerLocked() {
	if s.writeTimer != nil {
		s.writeTimer.Stop()
		s.writeTimer = nil
	}
	if s.closed || !s.writeActive || s.writeDeadline.IsZero() {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(time.Until(s.writeDeadline), func() {
		s.deadlineMu.Lock()
		// Stop 不保证回调未启动，身份检查阻止旧期限关闭新一轮写。
		if s.writeTimer != timer || s.closed || !s.writeActive {
			s.deadlineMu.Unlock()
			return
		}
		s.writeTimedOut = true
		s.closed = true
		s.deadlineMu.Unlock()
		_ = s.Close()
	})
	s.writeTimer = timer
}
