package wsc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
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
	// closed 用 atomic.Bool 而非"赋 nil 字段". Close 会 close(msgchan),
	// Handle 必须先看 closed 决定是否 send (send 到 closed chan 会 panic).
	// atomic.Bool 必须加: Close (closeMux 单写) 与 Handle (任意 goroutine 读)
	// 跨 goroutine 访问同一字段, 不加同步会 race; 单字段 atomic.Bool 是最轻方案.
	closed   atomic.Bool
	closeMux sync.Once
}

// createWSConnection 创建 WebSocket 连接封装, msgchan 由 Conn 管理
func createWSConnection[T any](conn *websocket.Conn, bufferSize int) *wsconnection[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	return &wsconnection[T]{
		conn:    conn,
		msgchan: make(chan *messagechannel[T], bufferSize),
	}
}

func (c *wsconnection[T]) channel() <-chan *messagechannel[T] {
	return c.msgchan
}

// Close 关闭连接(实现 net.Conn 接口, 不可以外部调用)
func (c *wsconnection[T]) Close(ctx context.Context) error {
	var err error
	c.closeMux.Do(func() {
		err = c.conn.Close()
		// 先置 closed 标志: Handle 在见到 closed=true 后不再 send, 避免与
		// close(msgchan) 之后的 send 撞上 panic.
		c.closed.Store(true)
		// best-effort: 在 close 前尝试投递一个 Closed=true 包给 reader 做
		// reconnect 提示, 拥塞或 ctx 取消时就跳过.
		select {
		case <-ctx.Done():
		case c.msgchan <- &messagechannel[T]{Closed: true}:
		default:
		}
		close(c.msgchan)
	})
	return err
}

// Read 读取消息(实现 net.Conn 接口, 不可以外部调用)
//
// 每次进入设 ReadDeadline=ReadIdleTimeout 做对端死检测: 这段时间内对方必须
// 发出任何一条消息 (心跳也算), 否则视为僵死返 net.ErrDeadlineExceeded
// 让上层 net.Client 走 lastError → 关连接 → 重连. 心跳本身 30s 一次, 60s 上限
// 容忍单次心跳抖动 + 时序漂移. 详见 ReadIdleTimeout 注释.
func (c *wsconnection[T]) Read(ctx context.Context) (*Message[T], error) {
	c.conn.SetReadDeadline(time.Now().Add(ReadIdleTimeout))
	var msg Message[T]
	if err := c.conn.ReadJSON(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Write 写入消息(实现 net.Conn 接口, 不可以外部调用)
func (c *wsconnection[T]) Write(ctx context.Context, data *Message[T]) error {
	c.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	err := c.conn.WriteJSON(data)
	c.conn.SetWriteDeadline(time.Time{})
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
	// 心跳消息不处理; closed 后不再 send 到已 close 的 msgchan (send 到 closed 会 panic).
	if data.IsHeart || c.closed.Load() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("wsc dropped message while connection was closing", slog.Any("panic", r))
		}
	}()
	msgchannel := &messagechannel[T]{
		Message: data,
	}
	select {
	case <-ctx.Done():
	case c.msgchan <- msgchannel:
	}
}
