package wsproxy

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Slaver struct {
	Id    string
	Token string
}

func NewSlaver() *Slaver {
	return &Slaver{
		Id: uuid.New().String(),
	}
}

func (slaver *Slaver) Start(ctx context.Context, serverAddr string) {
	go slaver.Run(ctx, serverAddr)
}

func (slaver *Slaver) Run(ctx context.Context, addr string) error {
	// backoff 复用与 dial-fail 相同的 3s 窗口, 避免握手期失败 (WriteJSON / ReadJSON /
	// 非预期 Method) 退化成 hot reconnect loop 把对端 server / 本地 CPU 打死.
	backoff := func() bool {
		select {
		case <-time.After(3 * time.Second):
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, addr, nil)
		if err != nil {
			slog.Warn("wsproxy slaver dial failed; backoff before retry",
				slog.String("addr", addr), slog.Any("err", err))
			if !backoff() {
				return ctx.Err()
			}
			continue
		}
		incoming := &connPacket{
			Id:     slaver.Id,
			Method: MethodRegisterSlaver, // 注册连接
			Token:  slaver.Token,
		}
		if err := wsConn.WriteJSON(incoming); err != nil {
			slog.Warn("wsproxy slaver register write failed",
				slog.String("addr", addr), slog.Any("err", err))
			wsConn.Close()
			if !backoff() {
				return ctx.Err()
			}
			continue
		}
		var outgoing connPacket
		if err := wsConn.ReadJSON(&outgoing); err != nil {
			slog.Warn("wsproxy slaver read dial-request failed",
				slog.String("addr", addr), slog.Any("err", err))
			wsConn.Close()
			if !backoff() {
				return ctx.Err()
			}
			continue
		}
		if outgoing.Method != MethodSlaverDialout {
			slog.Warn("wsproxy slaver unexpected method from server",
				slog.String("addr", addr), slog.Int("method", int(outgoing.Method)))
			wsConn.Close()
			if !backoff() {
				return ctx.Err()
			}
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
