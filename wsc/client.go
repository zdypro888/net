package wsc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Client WebSocket 客户端（基于 Session）。
// 支持 Connect/Close 模式，Close 后可再次 Connect。
type Client[T any] struct {
	locker         sync.RWMutex
	serverURL      string
	session        *Session[T]
	handleChan     chan *Packet[T]
	codecs         *codecSet
	bufferSize     int
	maxMessageSize int64
}

// NewClient 创建客户端。可选 WithCodecs 配置支持的编码 (默认仅 JSON)。
func NewClient[T any](serverURL string, opts ...Option) *Client[T] {
	return NewClientWithBuffer[T](serverURL, DefaultBufferSize, opts...)
}

// NewClientWithBuffer 创建客户端并设置内部队列容量.
// 高吞吐通知流可按调用场景调大 bufferSize.
func NewClientWithBuffer[T any](serverURL string, bufferSize int, opts ...Option) *Client[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	client := &Client[T]{serverURL: serverURL, codecs: newCodecSet(o.codecs), bufferSize: bufferSize, maxMessageSize: o.resolvedMaxMessageSize()}
	client.resetSessionLocked()
	return client
}

func (c *Client[T]) Handle() <-chan *Packet[T] {
	c.locker.RLock()
	defer c.locker.RUnlock()
	return c.handleChan
}

func (c *Client[T]) resetSessionLocked() {
	session := createSessionWithBuffer[T](uuid.New().String(), c.bufferSize)
	handleChan := make(chan *Packet[T], c.bufferSize)
	c.session = session
	c.handleChan = handleChan
	go c.handleMessageGo(session, session.handchan, session.stopChan, handleChan)
}

func (c *Client[T]) sessionClosedLocked() bool {
	if c.session == nil {
		return true
	}
	c.session.locker.RLock()
	defer c.session.locker.RUnlock()
	return c.session.asyncChan == nil
}

// dial 建连并完成握手, 返回连接与本次协商出的 codec。握手始终走 JSON (一次性,
// 且需在 codec 确定前完成), 客户端把支持的 codec 名字按优先级带给服务端, 服务端在
// 响应里回选定的 codec。响应不带 codec (旧服务端) 时回退到默认 JSON。
func (c *Client[T]) dial(ctx context.Context, guid string) (*websocket.Conn, Codec, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, nil)
	if err != nil {
		return nil, nil, err
	}
	conn.SetReadLimit(c.maxMessageSize)
	// 握手
	if err := conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	if err := conn.WriteJSON(HandshakeRequest{
		GUID:    guid,
		Version: ProtocolVersion,
		Codecs:  c.codecs.names(),
	}); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	if err := conn.SetReadDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	var resp HandshakeResponse
	if err := conn.ReadJSON(&resp); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	if resp.Status != 200 {
		return nil, nil, errors.Join(fmt.Errorf("dial failed: %s", resp.Message), conn.Close())
	}
	// 解析协商结果: 空 = 旧服务端, 回退默认 JSON; 非空必须是本端支持的 codec。
	codec := defaultCodec
	if resp.Codec != "" {
		selected, ok := c.codecs.get(resp.Codec)
		if !ok {
			return nil, nil, errors.Join(fmt.Errorf("dial failed: server selected unsupported codec %q", resp.Codec), conn.Close())
		}
		codec = selected
	}
	// 清除 deadline
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	return conn, codec, nil
}

func (c *Client[T]) handleMessageGo(session *Session[T], msgchan <-chan *Packet[T], stopChan <-chan struct{}, handleChan chan<- *Packet[T]) {
	dialCtx, cancelDial := context.WithCancel(context.Background())
	defer cancelDial()
	go func() {
		<-stopChan
		cancelDial()
	}()
	running := true
	for running {
		select {
		case msg, ok := <-msgchan:
			if !ok {
				running = false
			} else if msg.Closed {
				session.closedSignal.Store(false)
				select {
				case <-stopChan:
					running = false
					continue
				case handleChan <- msg:
				default:
					slog.Warn("wsc client dropped closed notification because handle channel is full",
						slog.String("guid", session.guid), slog.Int("cap", cap(handleChan)))
				}
				if session.closed() {
					running = false
					continue
				}
				for running {
					if session.closed() {
						running = false
						break
					}
					conn, codec, err := c.dial(dialCtx, session.guid)
					if err != nil {
						select {
						case <-stopChan:
							running = false
						case <-time.After(3 * time.Second):
						}
						continue
					}
					// 重置连接. 用 Session.Reset 封装连接切换串行化, 不越界调用
					// 内部 helper 或操作 session.locker.
					if err := session.reset(context.Background(), conn, codec); err != nil {
						if closeErr := conn.Close(); closeErr != nil {
							slog.Warn("wsc client reconnect close failed",
								slog.Any("reset_err", err), slog.Any("close_err", closeErr))
						}
						running = false
					}
					break
				}
			} else {
				select {
				case <-stopChan:
					running = false
				case handleChan <- msg:
				}
			}
		case <-stopChan:
			running = false
		}
	}
	close(handleChan)
}

// Connect 执行连接
func (c *Client[T]) Connect(ctx context.Context) error {
	c.locker.Lock()
	if c.sessionClosedLocked() {
		c.resetSessionLocked()
	}
	session := c.session
	guid := session.guid
	c.locker.Unlock()

	conn, codec, err := c.dial(ctx, guid)
	if err != nil {
		return err
	}
	// 用 Session.Reset 而非越界拿 session.locker; Reset 内部负责连接切换串行化.
	if err := session.reset(ctx, conn, codec); err != nil {
		return errors.Join(err, conn.Close())
	}
	return nil
}

func (c *Client[T]) Write(ctx context.Context, data T) error {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return ErrSessionClosed
	}
	return session.Write(ctx, data)
}

func (c *Client[T]) Request(ctx context.Context, data T) (T, error) {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		var zero T
		return zero, ErrSessionClosed
	}
	return session.Request(ctx, data)
}

// Reply 回复服务端请求（用于双向通信场景）
func (c *Client[T]) Reply(ctx context.Context, id string, data T) error {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return ErrSessionClosed
	}
	return session.Reply(ctx, id, data)
}

// Close 关闭连接
func (c *Client[T]) Close() error {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return nil
	}
	return session.Close()
}
