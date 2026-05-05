package wsc

import (
	"errors"
	"time"
)

// 协议常量
const (
	ProtocolVersion   = "2.0.1"
	HeartbeatInterval = 30 * time.Second
	DefaultBufferSize = 0x10

	// MaxMissedHeartbeats = 5
	// SessionTimeout      = MaxMissedHeartbeats * HeartbeatInterval
	HandshakeTimeout = 10 * time.Second

// ReconnectInterval   = 2 * time.Second
)

// 错误定义
var (
	ErrSessionClosed = errors.New("session closed")

// ErrNotConnected    = errors.New("not connected")
// ErrReconnectDenied = errors.New("reconnect denied")
)

// HandshakeRequest 握手请求
type HandshakeRequest struct {
	GUID    string `json:"guid"`
	Version string `json:"version"`
}

// HandshakeResponse 握手响应
type HandshakeResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message,omitempty"`
}

// Message 内部消息格式
type Message[T any] struct {
	ID      string `json:"i,omitempty"` // 请求/响应 ID (UUID)
	Data    T      `json:"d,omitempty"` // 数据
	IsHeart bool   `json:"h,omitempty"` // 心跳标志
}

// Id 实现 net.Notify 接口
func (m *Message[T]) Id() (any, bool) {
	if m.ID != "" && !m.IsHeart {
		return m.ID, true
	}
	return nil, false
}
