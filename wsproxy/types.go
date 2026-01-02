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
}

type pump struct {
}

func (p *pump) copyLoop(ctx context.Context, wsConn *websocket.Conn, conn net.Conn) error {
	defer wsConn.Close()
	defer conn.Close()

	var waiter sync.WaitGroup
	waiter.Add(2)
	go p.wsCopyToConn(ctx, &waiter, wsConn, conn)
	go p.connCopyToWs(ctx, &waiter, conn, wsConn)
	waiter.Wait()
	return nil
}

func (p *pump) wsCopyToConn(ctx context.Context, waiter *sync.WaitGroup, wsConn *websocket.Conn, conn net.Conn) error {
	defer waiter.Done()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, message, err := wsConn.ReadMessage()
		if err != nil {
			return err
		}
		if _, err := conn.Write(message); err != nil {
			return err
		}
	}
}

func (p *pump) connCopyToWs(ctx context.Context, waiter *sync.WaitGroup, conn net.Conn, wsConn *websocket.Conn) error {
	defer waiter.Done()
	buf := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			return err
		}
	}
}
