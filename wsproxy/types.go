package wsproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/gorilla/websocket"
)

const MaxMessageSize = 32 << 20

type MethodType int

// Method 常量每个只承担一个方向/一种含义 (A1 修复时确立的约定, wire 值不变):
const (
	// MethodRegisterSlaver: slaver → server. 注册一条可被 DialContext 取用的反向连接.
	MethodRegisterSlaver MethodType = iota
	// MethodSlaverDialout: server → slaver. 指令 slaver 向目标地址拨号.
	MethodSlaverDialout
	// MethodSlaverDialoutError: slaver → server. 拨号失败结果.
	MethodSlaverDialoutError
	// MethodSlaverDialoutSuccess: slaver → server. 拨号成功结果.
	MethodSlaverDialoutSuccess
	// MethodClientDialout: client → server. 请求经由 slaver 建立到目标地址的隧道.
	MethodClientDialout
	// MethodClientDialoutError: server → client. 隧道建立失败结果.
	MethodClientDialoutError
	// MethodClientDialoutSuccess: server → client. 隧道建立成功结果.
	MethodClientDialoutSuccess
)

type connPacket struct {
	// Id 全局唯一会话标识. 改为 string + UUID: 旧实现是进程内 atomic.Int64 自增,
	// 多副本/多进程下会碰撞. 用 github.com/google/uuid 保证跨进程唯一 (wsc 同款,
	// 不引入新依赖/不造轮子). wire tag "i" 不变, 仅类型 int64→string.
	Id      string     `json:"i"`
	Method  MethodType `json:"m"`
	Network string     `json:"n,omitempty"`
	Address string     `json:"a,omitempty"`
	Error   string     `json:"e,omitempty"`
	Token   string     `json:"t,omitempty"`
}

func closeWebSocketOnContextDone(ctx context.Context, conn *websocket.Conn) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			select {
			case <-done:
				return
			default:
				if err := conn.Close(); err != nil {
					slog.Debug("wsproxy context close websocket failed", slog.Any("err", err))
				}
			}
		case <-done:
		}
	}()
	return func() {
		once.Do(func() {
			close(done)
		})
		<-stopped
	}
}

// copyLoop 在 websocket 连接与 net.Conn 之间双向搬运数据, 任一方向出错或 ctx
// 取消时关闭两端并返回. server 与 slaver 的隧道桥接共用.
func copyLoop(ctx context.Context, wsConn *websocket.Conn, conn net.Conn) error {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			if err := errors.Join(wsConn.Close(), conn.Close()); err != nil {
				slog.Warn("wsproxy copyLoop close failed", slog.Any("err", err))
			}
		})
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-done:
		}
	}()

	defer closeBoth()

	// Go 1.25 WaitGroup.Go 自动 Add(1)/Done, 配套替换显式 Add(2)+defer Done.
	var waiter sync.WaitGroup
	errCh := make(chan error, 2)
	waiter.Go(func() {
		errCh <- normalizeCopyError("ws_to_conn", wsCopyToConn(closeBoth, wsConn, conn))
	})
	waiter.Go(func() {
		errCh <- normalizeCopyError("conn_to_ws", connCopyToWs(closeBoth, conn, wsConn))
	})
	waiter.Wait()
	close(done)
	close(errCh)

	var err error
	for copyErr := range errCh {
		err = errors.Join(err, copyErr)
	}
	return err
}

func normalizeCopyError(direction string, err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	if _, ok := errors.AsType[*websocket.CloseError](err); ok {
		return nil
	}
	return fmt.Errorf("%s: %w", direction, err)
}

func wsCopyToConn(closeBoth func(), wsConn *websocket.Conn, conn net.Conn) error {
	defer closeBoth()
	for {
		_, message, err := wsConn.ReadMessage()
		if err != nil {
			return err
		}
		if _, err := conn.Write(message); err != nil {
			return err
		}
	}
}

func connCopyToWs(closeBoth func(), conn net.Conn, wsConn *websocket.Conn) error {
	defer closeBoth()
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			return err
		}
	}
}
