package wsproxy

import (
	"context"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

type Slaver struct {
	Id int64
}

func NewSlaver() *Slaver {
	return &Slaver{
		Id: idCSeq.Add(1),
	}
}

func (slaver *Slaver) Start(ctx context.Context, serverAddr string) {
	go slaver.Run(ctx, serverAddr)
}

func (slaver *Slaver) Run(ctx context.Context, addr string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, addr, nil)
		if err != nil {
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		incoming := &connPacket{
			Id:     slaver.Id,
			Method: MethodRegisterSlaver, // 注册连接
		}
		if err := wsConn.WriteJSON(incoming); err != nil {
			wsConn.Close()
			continue
		}
		var outgoing connPacket
		if err := wsConn.ReadJSON(&outgoing); err != nil {
			wsConn.Close()
			continue
		}
		if outgoing.Method != MethodSlaverDialout {
			wsConn.Close()
			continue
		}
		go slaver.dialContext(ctx, wsConn, outgoing.Network, outgoing.Address)
	}
}

func (slaver *Slaver) dialContext(ctx context.Context, wsConn *websocket.Conn, network, address string) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		// 发送连接错误响应，忽略写入错误（连接可能已断开）
		wsConn.WriteJSON(&connPacket{
			Id:     slaver.Id,
			Method: MethodSlaverDialoutError, // 连接错误
			Error:  err.Error(),
		})
		wsConn.Close()
		return
	}

	// 发送连接成功响应
	if err := wsConn.WriteJSON(&connPacket{
		Id:     slaver.Id,
		Method: MethodSlaverDialoutSuccess, // 连接成功
	}); err != nil {
		// WebSocket 写入失败，关闭两端连接
		wsConn.Close()
		conn.Close()
		return
	}
	p := &pump{}
	p.copyLoop(ctx, wsConn, conn)
}
