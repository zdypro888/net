package wsproxy

import (
	"context"
	"net"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

var idCSeq atomic.Int64

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
	Id      int64      `json:"i"`
	Method  MethodType `json:"m"`
	Network string     `json:"n,omitempty"`
	Address string     `json:"a,omitempty"`
	Error   string     `json:"e,omitempty"`
	Token   string     `json:"t,omitempty"`
}

type pump struct {
}

func (p *pump) copyLoop(ctx context.Context, wsConn *websocket.Conn, conn net.Conn) error {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			wsConn.Close()
			conn.Close()
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
	waiter.Go(func() { p.wsCopyToConn(closeBoth, wsConn, conn) })
	waiter.Go(func() { p.connCopyToWs(closeBoth, conn, wsConn) })
	waiter.Wait()
	close(done)
	return nil
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
