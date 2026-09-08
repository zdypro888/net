package wsproxy

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const dialHandshakeTimeout = 30 * time.Second

// Client 保存 WebSocket 代理客户端的会话标识、服务地址和鉴权令牌。
type Client struct {
	Id     string
	WSAddr string
	Token  string
}

// NewClient 创建使用指定 WebSocket 服务地址的代理客户端。
func NewClient(wsAddr string) *Client {
	return &Client{
		Id:     uuid.New().String(),
		WSAddr: wsAddr,
	}
}

// Dial 通过 WebSocket 代理建立到目标地址的连接。
func (client *Client) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if err := validateTarget(network, address); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dialHandshakeTimeout)
	defer cancel()
	wsConn, response, err := websocket.DefaultDialer.DialContext(ctx, client.WSAddr, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			err = errors.Join(err, response.Body.Close())
		}
		return nil, err
	}
	stopContextClose := closeWebSocketOnContextDone(ctx, wsConn)
	defer stopContextClose()
	wsConn.SetReadLimit(MaxMessageSize)
	deadline := handshakeDeadline(ctx)
	closeWithContextError := func(err error) error {
		closeErr := wsConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, closeErr)
		}
		return errors.Join(err, closeErr)
	}
	normalizeHandshakeError := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		return err
	}
	if err := wsConn.SetWriteDeadline(deadline); err != nil {
		return nil, closeWithContextError(err)
	}
	if err := wsConn.SetReadDeadline(deadline); err != nil {
		return nil, closeWithContextError(err)
	}
	outgoing := &connPacket{
		Id:      client.Id,
		Method:  MethodClientDialout,
		Network: network,
		Address: address,
		Token:   client.Token,
	}
	if err := wsConn.WriteJSON(outgoing); err != nil {
		return nil, errors.Join(normalizeHandshakeError(err), wsConn.Close())
	}
	var dialPacket connPacket
	if err := wsConn.ReadJSON(&dialPacket); err != nil {
		return nil, errors.Join(normalizeHandshakeError(err), wsConn.Close())
	}
	if dialPacket.Method != MethodClientDialoutSuccess {
		closeErr := wsConn.Close()
		if dialPacket.Error != "" {
			return nil, errors.Join(errors.New(dialPacket.Error), closeErr)
		}
		return nil, errors.Join(errors.New("dial failed"), closeErr)
	}
	if err := wsConn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, closeWithContextError(err)
	}
	if err := wsConn.SetReadDeadline(time.Time{}); err != nil {
		return nil, closeWithContextError(err)
	}
	// 先停掉 ctx watcher 并 join(stopContextClose 内部 <-stopped 会等 watcher 退出),
	// 再复查 ctx。否则 watcher 可能在本函数把 wsConn 交还给调用方之后才触发 Close,
	// 迟到关闭已返回的连接, 让调用方拿到 (已关闭conn, nil)。与 proxy.go 目的相同但机制不同:
	// proxy.go 用 context.AfterFunc(stop 不 join) 故靠末尾复查 ctx 兜底, 此处 helper 自带 join。
	stopContextClose()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, wsConn.Close())
	}
	return &Session{Id: client.Id, Conn: wsConn}, nil
}
