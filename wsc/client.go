package wsc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Client WebSocket 客户端（基于 Session）
// 支持 Start/Close 模式，Close 后可再次 Start
type Client[T any] struct {
	serverURL  string
	session    *Session[T]
	handleChan chan *Packet[T]
	codecs     *codecSet
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
	client := &Client[T]{serverURL: serverURL, codecs: newCodecSet(o.codecs)}
	client.session = createSessionWithBuffer[T](uuid.New().String(), bufferSize)
	client.handleChan = make(chan *Packet[T], bufferSize)
	// 启动消息处理协程
	go client.handleMessageGo(client.session.handchan, client.session.stopChan)
	return client
}

func (c *Client[T]) Handle() <-chan *Packet[T] {
	return c.handleChan
}

// dial 建连并完成握手, 返回连接与本次协商出的 codec。握手始终走 JSON (一次性,
// 且需在 codec 确定前完成), 客户端把支持的 codec 名字按优先级带给服务端, 服务端在
// 响应里回选定的 codec。响应不带 codec (旧服务端) 时回退到默认 JSON。
func (c *Client[T]) dial(ctx context.Context) (*websocket.Conn, Codec, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, nil)
	if err != nil {
		return nil, nil, err
	}
	// 握手
	if err := conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, nil, errors.Join(err, conn.Close())
	}
	if err := conn.WriteJSON(HandshakeRequest{
		GUID:    c.session.guid,
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

func (c *Client[T]) handleMessageGo(msgchan <-chan *Packet[T], stopChan <-chan struct{}) {
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
				select {
				case <-stopChan:
					running = false
					continue
				case c.handleChan <- msg:
				}
				for running {
					conn, codec, err := c.dial(dialCtx)
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
					if err := c.session.reset(context.Background(), conn, codec); err != nil {
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
				case c.handleChan <- msg:
				}
			}
		case <-stopChan:
			running = false
		}
	}
	close(c.handleChan)
}

// Connect 执行连接
func (c *Client[T]) Connect(ctx context.Context) error {
	conn, codec, err := c.dial(ctx)
	if err != nil {
		return err
	}
	// 用 Session.Reset 而非越界拿 session.locker; Reset 内部负责连接切换串行化.
	if err := c.session.reset(ctx, conn, codec); err != nil {
		return errors.Join(err, conn.Close())
	}
	return nil
}

func (c *Client[T]) Write(ctx context.Context, data T) error {
	return c.session.Write(ctx, data)
}

func (c *Client[T]) Request(ctx context.Context, data T) (T, error) {
	return c.session.Request(ctx, data)
}

// Reply 回复服务端请求（用于双向通信场景）
func (c *Client[T]) Reply(ctx context.Context, id string, data T) error {
	return c.session.Reply(ctx, id, data)
}

// Close 关闭连接
func (c *Client[T]) Close() error {
	return c.session.Close()
}
