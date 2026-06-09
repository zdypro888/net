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

type Client struct {
	Id     string
	WSAddr string
	Token  string
}

func NewClient(wsAddr string) *Client {
	return &Client{
		Id:     uuid.New().String(),
		WSAddr: wsAddr,
	}
}

func (client *Client) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, client.WSAddr, nil)
	if err != nil {
		return nil, err
	}
	stopContextClose := closeWebSocketOnContextDone(ctx, wsConn)
	defer stopContextClose()
	wsConn.SetReadLimit(MaxMessageSize)
	deadline := time.Now().Add(dialHandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline
	}
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
