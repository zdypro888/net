package wsproxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
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

// Run 维持待命连接，接到拨号命令后补充下一条；取消时等待已派发的隧道全部退出。
func (slaver *Slaver) Run(ctx context.Context, addr string) error {
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, packet, err := slaver.waitDialRequest(ctx, addr)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("wsproxy slaver registration failed; backoff before retry", slog.Any("err", err))
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		workers.Go(func() { slaver.dialContext(ctx, conn, packet.Network, packet.Address) })
	}
}

// waitDialRequest 完成有界注册，再用 Ping/Pong 维持待命连接；失败时始终收回连接所有权。
func (slaver *Slaver) waitDialRequest(ctx context.Context, addr string) (conn *websocket.Conn, packet connPacket, err error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, dialHandshakeTimeout)
	conn, response, err := websocket.DefaultDialer.DialContext(dialCtx, addr, nil)
	cancelDial()
	if err != nil {
		if response != nil && response.Body != nil {
			err = errors.Join(err, response.Body.Close())
		}
		return nil, packet, err
	}
	stopContextClose := closeWebSocketOnContextDone(ctx, conn)
	defer func() {
		stopContextClose()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			err = errors.Join(err, conn.Close())
			conn = nil
		}
	}()
	conn.SetReadLimit(MaxMessageSize)
	if err = conn.SetWriteDeadline(handshakeDeadline(ctx)); err != nil {
		return conn, packet, err
	}
	if err = conn.WriteJSON(&connPacket{Id: slaver.Id, Method: MethodRegisterSlaver, Token: slaver.Token}); err != nil {
		return conn, packet, err
	}
	if err = conn.SetWriteDeadline(time.Time{}); err != nil {
		return conn, packet, err
	}
	if err = conn.SetReadDeadline(time.Now().Add(slaverHeartbeatTimeout)); err != nil {
		return conn, packet, err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(slaverHeartbeatTimeout))
	})
	stopHeartbeat := keepSlaverConnectionAlive(ctx, conn)
	err = conn.ReadJSON(&packet)
	stopHeartbeat()
	if err != nil {
		return conn, packet, err
	}
	if packet.Method != MethodSlaverDialout {
		return conn, packet, errors.New("unexpected slaver dial-request method")
	}
	if err = validateTarget(packet.Network, packet.Address); err != nil {
		return conn, packet, err
	}
	// 待命期结束，迟到的 Pong 不得再次为已经投入使用的隧道设置空闲超时。
	conn.SetPongHandler(nil)
	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		return conn, packet, err
	}
	return conn, packet, nil
}

// dialContext 的 TCP 建连和结果写出均有上限；进入 copyLoop 后仅由上层 ctx 和连接 IO 结束。
func (slaver *Slaver) dialContext(ctx context.Context, wsConn *websocket.Conn, network, address string) {
	defer wsConn.Close()
	stopContextClose := closeWebSocketOnContextDone(ctx, wsConn)
	defer stopContextClose()
	dialCtx, cancelDial := context.WithTimeout(ctx, dialHandshakeTimeout)
	conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, network, address)
	cancelDial()
	packet := &connPacket{Id: slaver.Id, Method: MethodSlaverDialoutSuccess}
	if dialErr != nil {
		packet.Method = MethodSlaverDialoutError
		packet.Error = dialErr.Error()
	} else {
		defer conn.Close()
	}
	if err := wsConn.SetWriteDeadline(handshakeDeadline(ctx)); err != nil {
		return
	}
	if err := wsConn.WriteJSON(packet); err != nil {
		if ctx.Err() == nil {
			slog.Warn("wsproxy slaver write dial result failed", slog.Any("err", err))
		}
		return
	}
	if dialErr != nil {
		return
	}
	if err := wsConn.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	stopContextClose()
	if err := ctx.Err(); err != nil {
		return
	}
	if err := copyLoop(ctx, wsConn, conn); err != nil {
		slog.Warn("wsproxy slaver copy loop failed", slog.Any("err", err))
	}
}
