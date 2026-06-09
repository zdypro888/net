package wsc

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// CodecJSON 是默认 JSON codec 的 wire 标识符。
const CodecJSON = "json"

// Codec 把消息信封序列化为单个 WebSocket 帧, 以及反向解析。实现必须并发安全。
//
// 默认实现是 JSONCodec; Protocol Buffers codec 在 protocodec 子包中。codec 在握手
// 阶段协商 (见 HandshakeRequest.Codecs), 因此一个服务端可同时服务同协议版本的
// JSON 与 proto 客户端。
type Codec interface {
	// Name 是握手协商时交换的标识符。
	Name() string
	// Encode 把信封值 (恒为 *Message[T]) 编码为 WebSocket 消息类型
	// (websocket.TextMessage / BinaryMessage) 与载荷字节。
	Encode(v any) (messageType int, data []byte, err error)
	// Decode 把收到的 WebSocket 帧解析进信封值 (恒为 *Message[T])。
	Decode(messageType int, data []byte, v any) error
}

// JSONCodec 是默认 codec: 把整个信封编码为 JSON 文本帧, 保持历史 wire 格式不变,
// 适用于任意可 json 序列化的 T。
type JSONCodec struct{}

// Name 实现 Codec。
func (JSONCodec) Name() string { return CodecJSON }

// Encode 实现 Codec。
func (JSONCodec) Encode(v any) (int, []byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, nil, err
	}
	return websocket.TextMessage, data, nil
}

// Decode 实现 Codec。不校验 messageType, 与历史 ReadJSON 的宽松行为一致。
func (JSONCodec) Decode(_ int, data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// defaultCodec 在未配置任何 codec 时使用, 也作为对端不支持协商时的回退。
var defaultCodec Codec = JSONCodec{}

// Option 配置 Client 或 Server。
type Option func(*options)

type options struct {
	codecs             []Codec
	maxMessageSize     int64
	sessionIdleTimeout *time.Duration
}

// resolvedMaxMessageSize 返回配置的入站消息上限, 未配置(<=0)时回退默认 MaxMessageSize。
func (o options) resolvedMaxMessageSize() int64 {
	if o.maxMessageSize > 0 {
		return o.maxMessageSize
	}
	return MaxMessageSize
}

// WithCodecs 设置 Client 提供 / Server 支持的 codec, 按优先级从高到低排列。
// 协商时取双方都支持的、按请求方优先级最高的那个; 不设置则只用 JSON。
// 同时支持两者可传 WithCodecs(wsc.JSONCodec{}, protocodec.New())。
func WithCodecs(codecs ...Codec) Option {
	return func(o *options) { o.codecs = append(o.codecs, codecs...) }
}

// WithMaxMessageSize 设置单条入站消息的最大字节数, 防恶意/异常超大帧 OOM。
// <=0 使用默认 MaxMessageSize。Client 生效于拨号建立的连接, Server 生效于接受的连接。
func WithMaxMessageSize(n int64) Option {
	return func(o *options) { o.maxMessageSize = n }
}

// WithSessionIdleTimeout 设置 Server 在底层连接断开后保留 session 的时间.
// timeout <= 0 表示不自动清理断线 session. Client 会忽略该选项。
func WithSessionIdleTimeout(timeout time.Duration) Option {
	return func(o *options) { o.sessionIdleTimeout = &timeout }
}

// codecSet 按优先级保存已配置的 codec, 并提供按名查找与协商。
type codecSet struct {
	order  []Codec
	byName map[string]Codec
}

func newCodecSet(codecs []Codec) *codecSet {
	cs := &codecSet{byName: make(map[string]Codec)}
	for _, c := range codecs {
		if c == nil {
			continue
		}
		name := c.Name()
		if name == "" {
			continue
		}
		if _, dup := cs.byName[name]; dup {
			continue
		}
		cs.order = append(cs.order, c)
		cs.byName[name] = c
	}
	if len(cs.order) == 0 {
		cs.order = []Codec{defaultCodec}
		cs.byName[defaultCodec.Name()] = defaultCodec
	}
	return cs
}

// names 按优先级返回已配置的 codec 名字。
func (cs *codecSet) names() []string {
	names := make([]string, len(cs.order))
	for i, c := range cs.order {
		names[i] = c.Name()
	}
	return names
}

func (cs *codecSet) get(name string) (Codec, bool) {
	c, ok := cs.byName[name]
	return c, ok
}

// negotiate 按请求方优先级顺序, 选出本集合也支持的第一个 codec。请求方未带任何
// 偏好 (对端早于协商版本) 且本集合支持 JSON 时, 回退到 JSON。
func (cs *codecSet) negotiate(requested []string) (Codec, bool) {
	for _, name := range requested {
		if c, ok := cs.byName[name]; ok {
			return c, true
		}
	}
	if len(requested) == 0 {
		if c, ok := cs.byName[defaultCodec.Name()]; ok {
			return c, true
		}
	}
	return nil, false
}
