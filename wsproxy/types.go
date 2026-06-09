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

const (
	MethodRegisterSlaver MethodType = iota
	MethodSlaverDialout
	MethodSlaverDialoutError
	MethodSlaverDialoutSuccess
	MethodClientDialout
	MethodClientDialoutError
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

type pump struct {
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

func (p *pump) copyLoop(ctx context.Context, wsConn *websocket.Conn, conn net.Conn) error {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			if err := errors.Join(wsConn.Close(), conn.Close()); err != nil {
				slog.Warn("wsproxy pump close failed", slog.Any("err", err))
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
		errCh <- p.normalizeCopyError("ws_to_conn", p.wsCopyToConn(closeBoth, wsConn, conn))
	})
	waiter.Go(func() {
		errCh <- p.normalizeCopyError("conn_to_ws", p.connCopyToWs(closeBoth, conn, wsConn))
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

func (p *pump) normalizeCopyError(direction string, err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return nil
	}
	return fmt.Errorf("%s: %w", direction, err)
}

func (p *pump) wsCopyToConn(closeBoth func(), wsConn *websocket.Conn, conn net.Conn) error {
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

func (p *pump) connCopyToWs(closeBoth func(), conn net.Conn, wsConn *websocket.Conn) error {
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
