package wsc

import (
	"context"
	"fmt"
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
}

// NewClient 创建客户端
func NewClient[T any](serverURL string) *Client[T] {
	return NewClientWithBuffer[T](serverURL, DefaultBufferSize)
}

// NewClientWithBuffer 创建客户端并设置内部队列容量.
// 高吞吐通知流可按调用场景调大 bufferSize.
func NewClientWithBuffer[T any](serverURL string, bufferSize int) *Client[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	client := &Client[T]{serverURL: serverURL}
	client.session = createSessionWithBuffer[T](uuid.New().String(), bufferSize)
	client.handleChan = make(chan *Packet[T], bufferSize)
	// 启动消息处理协程
	go client.handleMessageGo(client.session.handchan, client.session.stopChan)
	return client
}

func (c *Client[T]) Handle() <-chan *Packet[T] {
	return c.handleChan
}

func (c *Client[T]) dial(ctx context.Context) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, nil)
	if err != nil {
		return nil, err
	}
	// 握手
	conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout))
	if err := conn.WriteJSON(HandshakeRequest{
		GUID:    c.session.guid,
		Version: ProtocolVersion,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	var resp HandshakeResponse
	if err := conn.ReadJSON(&resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp.Status != 200 {
		conn.Close()
		return nil, fmt.Errorf("dial failed: %s", resp.Message)
	}
	// 清除 deadline
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})
	return conn, nil
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
					conn, err := c.dial(dialCtx)
					if err != nil {
						select {
						case <-stopChan:
							running = false
						case <-time.After(3 * time.Second):
						}
						continue
					}
					// 重置连接
					c.session.locker.Lock()
					if err := c.session.resetUnsafe(context.Background(), conn); err != nil {
						conn.Close()
						running = false
					}
					c.session.locker.Unlock()
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
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	// 重置连接
	c.session.locker.Lock()
	defer c.session.locker.Unlock()
	if err := c.session.resetUnsafe(ctx, conn); err != nil {
		conn.Close()
		return err
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
