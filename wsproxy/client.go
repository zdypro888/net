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
	wsConn.SetReadLimit(MaxMessageSize)
	deadline := time.Now().Add(dialHandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline
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
		return nil, errors.Join(err, wsConn.Close())
	}
	if err := wsConn.SetReadDeadline(deadline); err != nil {
		return nil, errors.Join(err, wsConn.Close())
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
		return nil, errors.Join(err, wsConn.Close())
	}
	if err := wsConn.SetReadDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, wsConn.Close())
	}
	return &Session{Id: client.Id, Conn: wsConn}, nil
}
