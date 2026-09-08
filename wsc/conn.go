package wsc

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type messagechannel[T any] struct {
	Closed  bool
	Message *Message[T]
}

func (mc *messagechannel[T]) ToPacket() *Packet[T] {
	packet := &Packet[T]{Closed: mc.Closed}
	if mc.Message != nil {
		packet.ID = mc.Message.ID
		packet.Data = mc.Message.Data
	}
	return packet
}

// wsconnection 封装 WebSocket 连接，实现 net.Conn 接口
type wsconnection[T any] struct {
	conn    *websocket.Conn
	msgchan chan *messagechannel[T]
	// stop 先解除 Handle 的背压，sendMu 再隔离 close(msgchan) 与仍在发送的 Handle。
	stop     chan struct{}
	sendMu   sync.RWMutex
	closeMux sync.Once
	closeErr error
	// codec 决定信封的 wire 编码 (JSON 文本帧 / proto 二进制帧), 握手协商得到。
	codec Codec
}

// createWSConnection 创建 WebSocket 连接封装, msgchan 由 Conn 管理。codec 为 nil 时
// 回退到默认 JSON codec。
func createWSConnection[T any](conn *websocket.Conn, bufferSize int, codec Codec) *wsconnection[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	if codec == nil {
		codec = defaultCodec
	}
	// 读上限已由 Client.dial / Server.OnConnection 按各自配置在握手前设于同一 conn,
	// 此处不再重复 SetReadLimit, 以免用 const 覆盖掉调用方配置的 WithMaxMessageSize。
	return &wsconnection[T]{
		conn:    conn,
		msgchan: make(chan *messagechannel[T], bufferSize),
		stop:    make(chan struct{}),
		codec:   codec,
	}
}

func (c *wsconnection[T]) channel() <-chan *messagechannel[T] {
	return c.msgchan
}

// Close 关闭连接(实现 net.Conn 接口, 不可以外部调用)
func (c *wsconnection[T]) Close(ctx context.Context) error {
	c.closeMux.Do(func() {
		close(c.stop)
		c.closeErr = c.conn.Close()
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
		// 队列关闭本身即可通知断线；有空位时保留显式 Closed 包以兼容接收方。
		select {
		case <-ctx.Done():
		case c.msgchan <- &messagechannel[T]{Closed: true}:
		default:
		}
		close(c.msgchan)
	})
	return c.closeErr
}

// Read 读取消息(实现 net.Conn 接口, 不可以外部调用)
//
// 每次进入设 ReadDeadline=ReadIdleTimeout 做对端死检测: 这段时间内对方必须
// 发出任何一条消息 (心跳也算), 否则视为僵死返 net.ErrDeadlineExceeded
// 让上层 net.Client 走 lastError → 关连接 → 重连. 心跳本身 30s 一次, 60s 上限
// 容忍单次心跳抖动 + 时序漂移. 详见 ReadIdleTimeout 注释.
func (c *wsconnection[T]) Read(ctx context.Context) (*Message[T], error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(ReadIdleTimeout)); err != nil {
		return nil, err
	}
	messageType, data, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var msg Message[T]
	if err := c.codec.Decode(messageType, data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Write 写入消息(实现 net.Conn 接口, 不可以外部调用)
func (c *wsconnection[T]) Write(ctx context.Context, data *Message[T]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	messageType, payload, err := c.codec.Encode(data)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(WriteTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// Gorilla 写超时/取消后不能继续复用；取消时关闭连接以中断正在阻塞的写。
	stopCancel := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	err = c.conn.WriteMessage(messageType, payload)
	if !stopCancel() || ctx.Err() != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if clearErr := c.conn.SetWriteDeadline(time.Time{}); err == nil {
		err = clearErr
	}
	return err
}

// Heart 返回心跳消息(实现 net.Conn 接口, 不可以外部调用)
func (c *wsconnection[T]) Heart(connect bool, count uint64) (*Message[T], time.Time, bool) {
	if connect {
		return nil, time.Now().Add(HeartbeatInterval), false
	}
	// 心跳消息：ID=0 的空消息
	return &Message[T]{IsHeart: true}, time.Now().Add(HeartbeatInterval), true
}

// Handle 处理对方发来的消息（请求或通知），返回响应数据（实现 net.Conn 接口, 不可以外部调用）
func (c *wsconnection[T]) Handle(ctx context.Context, data *Message[T]) {
	if data == nil || data.IsHeart {
		return
	}
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	select {
	case <-c.stop:
		return
	default:
	}
	select {
	case <-ctx.Done():
	case <-c.stop:
	case c.msgchan <- &messagechannel[T]{Message: data}:
	}
}
