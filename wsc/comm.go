package wsc

import (
	"errors"
	"fmt"
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
	// 视为对端僵死, ReadMessage 立即返 net.ErrDeadlineExceeded → 上层关连接 + 重连.
	//
	// 为什么不能只靠"心跳 write 失败"做死检测: TCP 写入 OS send buffer 即返回成功,
	// NAT/防火墙静默切断后 OS 重传 ~15min (tcp_retries2) 才报错, 期间发出去的心跳
	// 全部"成功"沉默. 必须在 read 端用 deadline 才能秒级感知对端僵死.
	//
	// 取 2×HeartbeatInterval: 容忍单次心跳丢失 + 时序抖动, 又远小于 OS TCP 重传上限.
	ReadIdleTimeout = 2 * HeartbeatInterval
)

// 错误定义
var ErrSessionClosed = errors.New("session closed")

// HandshakeRequest 握手请求
type HandshakeRequest struct {
	GUID    string `json:"guid"`
	Version string `json:"version"`
	// Codecs 客户端支持的 codec 名字, 按优先级从高到低排列. 旧客户端不带此字段,
	// 服务端按 JSON 处理 (向后兼容). 协商逻辑见 codec.go。
	Codecs []string `json:"codecs,omitempty"`
}

// HandshakeResponse 握手响应
type HandshakeResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message,omitempty"`
	// Codec 服务端最终选定的 codec 名字. 为空表示对端是旧服务端 (不支持协商),
	// 客户端回退到 JSON。
	Codec string `json:"codec,omitempty"`
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

// Envelope 把消息信封以非泛型形式暴露给 Codec, 使二进制 codec (如 protocodec)
// 能在不知道具体载荷类型 T 的情况下读写信封字段。*Message[T] 实现该接口。
// JSON codec 直接序列化整个结构体, 不经过本接口。
type Envelope interface {
	EnvelopeID() string
	EnvelopeHeart() bool
	// EnvelopePayload 返回载荷值 (类型 T)。对零值 Message 调用可得到类型模板
	// (指针类型为 typed-nil), 供 codec 反射分配同类型实例用于解码。
	EnvelopePayload() any
	SetEnvelopeID(id string)
	SetEnvelopeHeart(heart bool)
	// SetEnvelopePayload 把 payload 赋给 Data, 类型不匹配返回错误。
	SetEnvelopePayload(payload any) error
}

func (m *Message[T]) EnvelopeID() string          { return m.ID }
func (m *Message[T]) EnvelopeHeart() bool         { return m.IsHeart }
func (m *Message[T]) EnvelopePayload() any        { return m.Data }
func (m *Message[T]) SetEnvelopeID(id string)     { m.ID = id }
func (m *Message[T]) SetEnvelopeHeart(heart bool) { m.IsHeart = heart }

func (m *Message[T]) SetEnvelopePayload(payload any) error {
	data, ok := payload.(T)
	if !ok {
		return fmt.Errorf("wsc: payload type %T not assignable to %T", payload, m.Data)
	}
	m.Data = data
	return nil
}
