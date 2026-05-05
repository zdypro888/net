package wsc

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/zdypro888/net"
)

type Packet[T any] struct {
	ID     string // 请求 ID (用于回复)
	Data   T
	Closed bool
}

// Session 通用会话（客户端和服务端共用）
type Session[T any] struct {
	guid       string
	handchan   chan *Packet[T]
	bufferSize int
	locker     sync.RWMutex
	stopChan   chan struct{}
	asyncChan  chan *asyncInfo[T]
	waiter     sync.WaitGroup
}

// GUID 返回标识
func (s *Session[T]) GUID() string {
	return s.guid
}

func (s *Session[T]) Handle() <-chan *Packet[T] {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.handchan
}

func createSessionWithBuffer[T any](guid string, bufferSize int) *Session[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	s := &Session[T]{
		guid:       guid,
		bufferSize: bufferSize,
		handchan:   make(chan *Packet[T], bufferSize),
		stopChan:   make(chan struct{}),
	}
	s.asyncChan = make(chan *asyncInfo[T], bufferSize)
	s.waiter.Add(1)
	go s.asyncGo(&s.waiter, s.asyncChan, s.handchan, s.stopChan)
	return s
}

type asyncCommand int

const (
	asyncCommandConn asyncCommand = iota + 1
	asyncCommandWrite
	asyncCommandRequest
)

type asyncMsgErr[T any] struct {
	Message *Message[T]
	Error   error
}

type asyncInfo[T any] struct {
	Command asyncCommand
	Conn    *websocket.Conn
	Request *Message[T]

	response chan *asyncMsgErr[T]
}

// Response 处理响应数据或错误
func (info *asyncInfo[T]) Response(msg *Message[T], err error) {
	if info.response != nil {
		select {
		case info.response <- &asyncMsgErr[T]{Message: msg, Error: err}:
		default:
		}
		close(info.response)
	}
}

func (s *Session[T]) handleMessageGo(waiter *sync.WaitGroup, handchan chan *Packet[T], msgchan <-chan *messagechannel[T], stopChan chan struct{}) {
	defer waiter.Done()
	running := true
	for running {
		select {
		case <-stopChan:
			running = false
		case msg, ok := <-msgchan:
			if !ok {
				running = false
			} else {
				packet := msg.ToPacket()
				select {
				case handchan <- packet:
				case <-stopChan:
					running = false
				}
			}
		}
	}
}

func (s *Session[T]) asyncGo(waiter *sync.WaitGroup, asyncChan <-chan *asyncInfo[T], handchan chan *Packet[T], stopChan chan struct{}) {
	defer waiter.Done()
	rawconn := net.NewClient[*Message[T], *wsconnection[T]]()
	rawconn.SetBufferSize(s.bufferSize)
	var recvWaiter sync.WaitGroup
	for info := range asyncChan {
		switch info.Command {
		case asyncCommandConn: // 设置连接
			// wsConn 所有权转移到 rawconn, rawconn.Close 时会关闭连接
			wsConn := createWSConnection[T](info.Conn, s.bufferSize)
			recvWaiter.Add(1)
			go s.handleMessageGo(&recvWaiter, handchan, wsConn.channel(), stopChan)
			rawconn.ResetUnsafe(context.Background(), wsConn)
		case asyncCommandWrite: // 发送通知
			rawconn.WriteUnsafe(context.Background(), info.Request)
		case asyncCommandRequest: // 发送请求并等待响应; 断线由 info.Response 直接通知 caller, 不在此处重试
			rawconn.RequestCallbackUnsafe(context.Background(), info.Request, info.Response)
		}
	}
	rawconn.Close()
	// stopChan 关闭（特意设计,其它地方不会关闭 stopChan）
	close(stopChan)
	recvWaiter.Wait()
	close(handchan)
}

// resetUnsafe 设置连接并通知等待者
func (s *Session[T]) async(ctx context.Context, info *asyncInfo[T]) error {
	if s.asyncChan == nil {
		return ErrSessionClosed
	}
	select {
	case s.asyncChan <- info:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopChan:
		return ErrSessionClosed
	}
}

// resetUnsafe 设置连接并通知等待者
func (s *Session[T]) resetUnsafe(ctx context.Context, conn *websocket.Conn) error {
	asyncall := &asyncInfo[T]{
		Command: asyncCommandConn,
		Conn:    conn,
	}
	return s.async(ctx, asyncall)
}

func (s *Session[T]) Reset(ctx context.Context, conn *websocket.Conn) error {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.resetUnsafe(ctx, conn)
}

// writeWithId 发送消息（内部方法，可选 ID）
func (s *Session[T]) asyncWriteUnsafe(ctx context.Context, id string, data T) error {
	msg := &asyncInfo[T]{
		Command: asyncCommandWrite,
		Request: &Message[T]{ID: id, Data: data},
	}
	if err := s.async(ctx, msg); err != nil {
		return err
	}
	return nil
}

// writeUnsafe 发送通知（不等待响应，无连接直接失败）
func (s *Session[T]) writeUnsafe(ctx context.Context, data T) error {
	return s.asyncWriteUnsafe(ctx, "", data)
}

func (s *Session[T]) Write(ctx context.Context, data T) error {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.writeUnsafe(ctx, data)
}

// replyUnsafe 回复请求（用于服务端响应客户端请求）
func (s *Session[T]) replyUnsafe(ctx context.Context, id string, data T) error {
	return s.asyncWriteUnsafe(ctx, id, data)
}

// Reply 回复请求（用于服务端响应客户端请求）
func (s *Session[T]) Reply(ctx context.Context, id string, data T) error {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.replyUnsafe(ctx, id, data)
}

// requestUnsafe 发送请求并等待响应（自动管理 ID，断线返回错误）
func (s *Session[T]) requestUnsafe(ctx context.Context, data T) (T, error) {
	var zero T
	request := &asyncInfo[T]{
		Command:  asyncCommandRequest,
		Request:  &Message[T]{ID: uuid.New().String(), Data: data},
		response: make(chan *asyncMsgErr[T], 0x01),
	}
	if err := s.async(ctx, request); err != nil {
		close(request.response)
		return zero, err
	}
	select {
	case resp, ok := <-request.response:
		if !ok {
			return zero, ErrSessionClosed
		} else if resp == nil {
			return zero, nil
		} else if resp.Error != nil {
			return zero, resp.Error
		} else if resp.Message == nil {
			return zero, nil
		}
		return resp.Message.Data, nil
	case <-ctx.Done():
		// caller 离开后, asyncGo 仍可能调 Response; chan 缓冲为 1, 不会阻塞后台回调.
		return zero, ctx.Err()
	case <-s.stopChan:
		return zero, ErrSessionClosed
	}
}

func (s *Session[T]) Request(ctx context.Context, data T) (T, error) {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.requestUnsafe(ctx, data)
}

// closeUnsafe 关闭
func (s *Session[T]) closeUnsafe() error {
	if s.asyncChan != nil {
		close(s.asyncChan)
		s.asyncChan = nil
	}
	s.waiter.Wait()
	return nil
}

// Close 关闭
func (s *Session[T]) Close() error {
	s.locker.Lock()
	defer s.locker.Unlock()
	return s.closeUnsafe()
}
