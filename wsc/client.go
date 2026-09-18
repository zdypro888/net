package wsc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxHandshakeErrorBody = 4 << 10

// HTTPHandshakeError 表示 WebSocket 在 HTTP 升级阶段被服务端拒绝。
// StatusCode 可供上层区分鉴权失败与临时服务故障，Body 仅保留有限长度用于诊断。
type HTTPHandshakeError struct {
	StatusCode int
	Status     string
	Body       string
	err        error
}

// Error 返回包含 HTTP 状态和服务端响应的握手错误。
func (err *HTTPHandshakeError) Error() string {
	if body := strings.TrimSpace(err.Body); body != "" {
		return fmt.Sprintf("websocket handshake failed: %s: %s", err.Status, body)
	}
	return fmt.Sprintf("websocket handshake failed: %s", err.Status)
}

// Unwrap 返回 Gorilla WebSocket 的原始握手错误。
func (err *HTTPHandshakeError) Unwrap() error {
	return err.err
}

func captureHTTPHandshakeError(dialErr error, response *http.Response) error {
	if response == nil {
		return dialErr
	}
	if response.Body == nil {
		return &HTTPHandshakeError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			err:        dialErr,
		}
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxHandshakeErrorBody))
	closeErr := response.Body.Close()
	handshakeErr := &HTTPHandshakeError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Body:       string(body),
		err:        dialErr,
	}
	return errors.Join(handshakeErr, readErr, closeErr)
}

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

// Handle 返回当前会话的入站数据通道；会话结束时该通道会关闭。
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
	// 同一握手上限覆盖 DNS/TCP/TLS、HTTP Upgrade 和应用握手。
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, nil)
	if err != nil {
		return nil, nil, captureHTTPHandshakeError(err, response)
	}
	conn.SetReadLimit(c.maxMessageSize)
	stopContextClose := context.AfterFunc(ctx, func() {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Debug("wsc client close canceled handshake failed", slog.Any("err", closeErr))
		}
	})
	defer stopContextClose()
	closeWithContextError := func(err error) error {
		closeErr := conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, closeErr)
		}
		return errors.Join(err, closeErr)
	}
	deadline := time.Now().Add(HandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	// 握手
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	if err := conn.WriteJSON(HandshakeRequest{
		GUID:    guid,
		Version: ProtocolVersion,
		Codecs:  c.codecs.names(),
	}); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	var resp HandshakeResponse
	if err := conn.ReadJSON(&resp); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	if resp.Status != 200 {
		return nil, nil, closeWithContextError(fmt.Errorf("dial failed: %s", resp.Message))
	}
	// 旧服务端未返回 codec 时只可回退到本端也允许的 JSON，不能越过显式 codec 限制。
	selectedName := resp.Codec
	if selectedName == "" {
		selectedName = CodecJSON
	}
	codec, ok := c.codecs.get(selectedName)
	if !ok {
		return nil, nil, closeWithContextError(fmt.Errorf("dial failed: server selected unsupported codec %q", selectedName))
	}
	// 清除 deadline
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, nil, closeWithContextError(err)
	}
	if !stopContextClose() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, errors.Join(ctxErr, conn.Close())
		}
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
				if msg.Generation < session.generation() {
					continue
				}
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
					if err := session.reset(context.Background(), conn, codec, nil); err != nil {
						if closeErr := conn.Close(); closeErr != nil {
							slog.Warn("wsc client reconnect close failed",
								slog.Any("reset_err", err), slog.Any("close_err", closeErr))
						}
						// 只有会话真的关闭才退出。reset 还可能因入队超时失败，此时 Session 仍存活；
						// 若在这里退出，handleChan 被关闭而后续 Connect 会复用旧 session，接收链永久卡死。
						if errors.Is(err, ErrSessionClosed) || session.closed() {
							running = false
							break
						}
						select {
						case <-stopChan:
							running = false
						case <-time.After(3 * time.Second):
						}
						continue
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

	// Close 同时中断本次 Connect 的 HTTP/应用握手，避免会话关闭后还继续等待网络超时。
	ctx, cancelConnect := context.WithCancel(ctx)
	stopSessionClose := context.AfterFunc(session.ctx, cancelConnect)
	defer func() { stopSessionClose(); cancelConnect() }()
	conn, codec, err := c.dial(ctx, guid)
	if err != nil {
		return err
	}
	// 用 Session.Reset 而非越界拿 session.locker; Reset 内部负责连接切换串行化.
	if err := session.reset(ctx, conn, codec, nil); err != nil {
		return errors.Join(err, conn.Close())
	}
	return nil
}

// Write 向当前 WebSocket 会话发送一条无需响应的数据。
func (c *Client[T]) Write(ctx context.Context, data T) error {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return ErrSessionClosed
	}
	return session.Write(ctx, data)
}

// Request 向当前 WebSocket 会话发送请求并等待对应响应。
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

// ReplyGeneration 仅在请求来源连接仍为当前代次时回复，避免重连后把旧请求 ID
// 写入新连接。
func (c *Client[T]) ReplyGeneration(ctx context.Context, generation uint64, id string, data T) error {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return ErrSessionClosed
	}
	return session.ReplyGeneration(ctx, generation, id, data)
}

// Generation 返回当前底层连接代次。
func (c *Client[T]) Generation() uint64 {
	c.locker.RLock()
	session := c.session
	c.locker.RUnlock()
	if session == nil {
		return 0
	}
	return session.Generation()
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
