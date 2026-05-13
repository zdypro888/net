package wsproxy

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

const dialHandshakeTimeout = 30 * time.Second

type Client struct {
	Id     int64
	WSAddr string
	Token  string
}

func NewClient(wsAddr string) *Client {
	return &Client{
		Id:     idCSeq.Add(1),
		WSAddr: wsAddr,
	}
}

func (client *Client) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, client.WSAddr, nil)
	if err != nil {
		return nil, err
	}
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
		wsConn.Close()
		return nil, err
	}
	if err := wsConn.SetReadDeadline(deadline); err != nil {
		wsConn.Close()
		return nil, err
	}
	outgoing := &connPacket{
		Id:      client.Id,
		Method:  MethodClientDialout,
		Network: network,
		Address: address,
		Token:   client.Token,
	}
	if err := wsConn.WriteJSON(outgoing); err != nil {
		wsConn.Close()
		return nil, normalizeHandshakeError(err)
	}
	var dialPacket connPacket
	if err := wsConn.ReadJSON(&dialPacket); err != nil {
		wsConn.Close()
		return nil, normalizeHandshakeError(err)
	}
	if dialPacket.Method != MethodClientDialoutSuccess {
		wsConn.Close()
		if dialPacket.Error != "" {
			return nil, errors.New(dialPacket.Error)
		}
		return nil, errors.New("dial failed")
	}
	wsConn.SetWriteDeadline(time.Time{})
	wsConn.SetReadDeadline(time.Time{})
	return &Session{Id: client.Id, Conn: wsConn}, nil
}
