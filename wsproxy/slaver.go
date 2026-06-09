package wsproxy

import (
	"context"
	"errors"
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
	go func() {
		// Run 仅在 ctx 取消时返回(内部对每次握手/拨号失败已 backoff+slog), 故正常停机
		// (context.Canceled/DeadlineExceeded)不告警, 仅意外退出才 Warn.
		if err := slaver.Run(ctx, serverAddr); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("wsproxy slaver stopped unexpectedly",
				slog.String("addr", serverAddr), slog.Any("err", err))
		}
	}()
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
		wsConn.SetReadLimit(MaxMessageSize)
		incoming := &connPacket{
			Id:     slaver.Id,
			Method: MethodRegisterSlaver, // 注册连接
			Token:  slaver.Token,
		}
		if err := wsConn.WriteJSON(incoming); err != nil {
			slog.Warn("wsproxy slaver register write failed",
				slog.String("addr", addr), slog.Any("err", err))
			if closeErr := wsConn.Close(); closeErr != nil {
				slog.Warn("wsproxy slaver close after register write failure failed",
					slog.String("addr", addr), slog.Any("err", closeErr))
			}
			if !backoff() {
				return ctx.Err()
			}
			continue
		}
		var outgoing connPacket
		if err := wsConn.ReadJSON(&outgoing); err != nil {
			slog.Warn("wsproxy slaver read dial-request failed",
				slog.String("addr", addr), slog.Any("err", err))
			if closeErr := wsConn.Close(); closeErr != nil {
				slog.Warn("wsproxy slaver close after read dial-request failure failed",
					slog.String("addr", addr), slog.Any("err", closeErr))
			}
			if !backoff() {
				return ctx.Err()
			}
			continue
		}
		if outgoing.Method != MethodSlaverDialout {
			slog.Warn("wsproxy slaver unexpected method from server",
				slog.String("addr", addr), slog.Int("method", int(outgoing.Method)))
			if closeErr := wsConn.Close(); closeErr != nil {
				slog.Warn("wsproxy slaver close after unexpected method failed",
					slog.String("addr", addr), slog.Any("err", closeErr))
			}
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
		if writeErr := wsConn.WriteJSON(&connPacket{
			Id:     slaver.Id,
			Method: MethodSlaverDialoutError, // 连接错误
			Error:  err.Error(),
		}); writeErr != nil {
			slog.Warn("wsproxy slaver write dial error response failed",
				slog.Any("dial_err", err), slog.Any("write_err", writeErr))
		}
		if closeErr := wsConn.Close(); closeErr != nil {
			slog.Warn("wsproxy slaver close after dial failure failed", slog.Any("err", closeErr))
		}
		return
	}

	// 发送连接成功响应
	if err := wsConn.WriteJSON(&connPacket{
		Id:     slaver.Id,
		Method: MethodSlaverDialoutSuccess, // 连接成功
	}); err != nil {
		// WebSocket 写入失败，关闭两端连接
		if closeErr := errors.Join(wsConn.Close(), conn.Close()); closeErr != nil {
			slog.Warn("wsproxy slaver close after success response failure failed",
				slog.Any("write_err", err), slog.Any("close_err", closeErr))
		}
		return
	}
	p := &pump{}
	if err := p.copyLoop(ctx, wsConn, conn); err != nil {
		slog.Warn("wsproxy slaver copy loop failed", slog.Any("err", err))
	}
}
