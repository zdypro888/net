package wsproxy

import (
	"context"
	"errors"
	"net"

	"github.com/gorilla/websocket"
)

type Client struct {
	Id     int64
	WSAddr string
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
	outgoing := &connPacket{
		Id:      client.Id,
		Method:  MethodClientDialout,
		Network: network,
		Address: address,
	}
	if err := wsConn.WriteJSON(outgoing); err != nil {
		wsConn.Close()
		return nil, err
	}
	var dialPacket connPacket
	if err := wsConn.ReadJSON(&dialPacket); err != nil {
		wsConn.Close()
		return nil, err
	}
	if dialPacket.Method != MethodClientDialoutSuccess {
		wsConn.Close()
		if dialPacket.Error != "" {
			return nil, errors.New(dialPacket.Error)
		}
		return nil, errors.New("dial failed")
	}
	return &Session{Id: client.Id, Conn: wsConn}, nil
}
