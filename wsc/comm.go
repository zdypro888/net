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

	HandshakeTimeout = 10 * time.Second
	WriteTimeout     = 10 * time.Second

	// ReadIdleTimeout 读端静默上限. 这段时间内若没收到对方任何消息 (含心跳)
	// 视为对端僵死, ReadJSON 立即返 net.ErrDeadlineExceeded → 上层关连接 + 重连.
	//
	// 为什么不能只靠"心跳 write 失败"做死检测: TCP 写入 OS send buffer 即返回成功,
	// NAT/防火墙静默切断后 OS 重传 ~15min (tcp_retries2) 才报错, 期间发出去的心跳
	// 全部"成功"沉默. 必须在 read 端用 deadline 才能秒级感知对端僵死.
	//
	// 取 2×HeartbeatInterval: 容忍单次心跳丢失 + 时序抖动, 又远小于 OS TCP 重传上限.
	ReadIdleTimeout = 2 * HeartbeatInterval
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
