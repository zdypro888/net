package net

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNotConnected = fmt.Errorf("not connected")
var ErrConnectionClosed = fmt.Errorf("connection closed")

// Client 是一个支持请求-响应模式的多路复用网络客户端。
// 它封装了底层连接，实现了并发安全的读写操作，支持心跳和自动重连等功能。
// M 是消息类型，T 是底层连接类型，必须实现 Conn[M] 接口。
// T 注意需要指针类型才能修改连接状态。
// 原则上M, T都应该是指针类型，以避免数据拷贝开销。
type Client[M any, T Conn[M]] struct {
	locker sync.RWMutex   // 保护 Client 状态的读写锁
	waiter sync.WaitGroup // 等待 goroutine 退出

	conn      T     // 底层连接实现
	lastError error // 最后发生的错误

	heartCount atomic.Uint64
	heartTime  time.Time // 下次心跳时间

	asynchan chan *asynRequest[M, T]
	stopChan chan struct{} // 停止信号，通知 Write/Request 连接已关闭(只可以再asyncGo中关闭)
}

type AsyncCommand int

const (
	AsyncCommandSend AsyncCommand = iota + 1
	AsyncCommandCallback
)

type asynRequest[M any, T Conn[M]] struct {
	Command  AsyncCommand
	Callback func(ctx context.Context, conn T)
	Message  *asyncMessage[M]
}

// NewClient 创建并启动一个新的 Client。
func NewClient[M any, T Conn[M]]() *Client[M, T] {
	client := &Client[M, T]{}
	return client
}

func (client *Client[M, T]) Conn() T {
	client.locker.RLock()
	defer client.locker.RUnlock()
	return client.conn
}

// ResetUnsafe 初始化连接状态并启动工作协程。
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) ResetUnsafe(ctx context.Context, conn T) {
	// 关闭旧的发送通道，通知旧的 asyncGo 退出
	client.CloseUnsafe()
	// 等待旧的 goroutine 完全退出
	client.waiter.Wait()
	client.conn = conn

	// 创建新的通道
	client.stopChan = make(chan struct{})
	client.asynchan = make(chan *asynRequest[M, T], 0x10)
	recvchan := make(chan M, 0x10)
	cctx, cancel := context.WithCancel(ctx)

	// 初始化心跳时间
	if heartConn, ok := any(conn).(ConnHeart[M]); ok {
		_, heartTime, _ := heartConn.Heart(true, 0)
		client.heartTime = heartTime
	} else {
		client.heartTime = time.Now().Add(60 * time.Second)
	}

	// 启动工作协程
	client.waiter.Add(2)
	go client.asyncGo(cctx, cancel, conn, client.asynchan, recvchan, client.stopChan)
	go client.receiveGo(cctx, conn, recvchan)
}

// Reset 重置 Client 使用新的连接。如果存在则关闭旧连接。
func (client *Client[M, T]) Reset(ctx context.Context, conn T) {
	client.locker.Lock()
	defer client.locker.Unlock()
	client.ResetUnsafe(ctx, conn)
}

// CloseUnsafe 关闭异步通道，通知 asyncGo 退出。
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) CloseUnsafe() {
	if client.asynchan != nil {
		// asynchan 只可以再 locker 保护下关闭
		close(client.asynchan)
		client.asynchan = nil
	}
}

// Close 关闭 Client 并释放所有资源。
// 会等待所有 goroutine 退出后返回。
// 返回连接期间最后发生的错误（如果有）。
func (client *Client[M, T]) Close() error {
	client.locker.Lock()
	defer client.locker.Unlock()
	client.CloseUnsafe()
	// 等待所有 goroutine 退出
	client.waiter.Wait()
	err := client.lastError
	return err
}

// receiveGo 是接收协程，负责从连接读取数据。
// 读取到的数据发送到 recvchan 供 asyncGo 处理。
// 当连接断开或发生错误时退出。
func (client *Client[M, T]) receiveGo(ctx context.Context, conn T, recvchan chan M) {
	for {
		data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		recvchan <- data
	}
	close(recvchan)
	client.waiter.Done()
}

// asyncGo 是异步处理协程，负责：
// 1. 处理发送队列（sendchan）中的请求
// 2. 处理接收队列（recvchan）中的响应
// 3. 匹配请求和响应（通过 Notify.Id）
// 4. 分发未匹配的消息到 Handle
func (client *Client[M, T]) asyncGo(ctx context.Context, cancel context.CancelFunc, conn T, asynchan <-chan *asynRequest[M, T], recvchan <-chan M, stopchan chan struct{}) {
	// asyncNotifys 存储等待响应的请求，key 是 Notify.Id()
	asyncNotifys := make(map[any]*asyncMessage[M])
	var zeroM M
	running := true
	heartTimer := time.NewTimer(time.Until(client.heartTime))
	defer heartTimer.Stop()
	for running {
		select {
		case <-ctx.Done():
			// context 被取消
			client.lastError = ctx.Err()
			running = false
		case recv, ok := <-recvchan:
			// 处理接收到的数据
			if !ok {
				// recvchan 已关闭，receiveGo 已退出
				running = false
			} else {
				// 尝试匹配请求
				foundNotify := false
				if notify, ok := any(recv).(NotifyMessage); ok {
					if notifyId, ok := notify.Id(); ok {
						if asyncRequest, ok := asyncNotifys[notifyId]; ok {
							// 找到匹配的请求，发送响应
							asyncRequest.Response(recv, nil)
							delete(asyncNotifys, notifyId)
							foundNotify = true
						}
					}
				}
				if !foundNotify {
					// 无匹配请求，作为服务端推送处理
					conn.Handle(ctx, recv)
				}
			}
		case asyncall, ok := <-asynchan:
			// 处理发送请求
			if !ok {
				// sendchan 已关闭，Client 正在关闭
				running = false
			} else {
				switch asyncall.Command {
				case AsyncCommandSend:
					asyncRequest := asyncall.Message
					// 写入数据到连接
					err := conn.Write(ctx, asyncRequest.Data)
					if asyncRequest.Notify {
						if err != nil {
							asyncRequest.Response(zeroM, err)
						} else {
							// 注册到等待队列
							if notify, ok := any(asyncRequest.Data).(NotifyMessage); ok {
								if notifyId, ok := notify.Id(); ok {
									// 注册到等待队列
									asyncNotifys[notifyId] = asyncRequest
									asyncRequest = nil
								}
							}
							if asyncRequest != nil {
								// 没有实现 Notify 接口，无法匹配响应
								asyncRequest.Response(zeroM, fmt.Errorf("message does not implement Notify interface"))
							}
						}
					}
					if err != nil {
						client.lastError = err
						running = false
					}
				case AsyncCommandCallback:
					if asyncall.Callback != nil {
						asyncall.Callback(ctx, conn)
					}
				}

			}
		case <-heartTimer.C:
			// 处理心跳
			if heartConn, ok := any(conn).(ConnHeart[M]); ok {
				var heartData M
				var heartHasData bool
				if heartData, client.heartTime, heartHasData = heartConn.Heart(false, client.heartCount.Add(1)); heartHasData {
					if err := conn.Write(ctx, heartData); err != nil {
						client.lastError = err
						running = false
					}
				}
			} else {
				// 无心跳支持，设置为较远的时间点
				client.heartTime = time.Now().Add(60 * time.Second)
			}
			heartTimer.Reset(time.Until(client.heartTime))
		}
		if len(asyncNotifys) > 100 {
			// 清理已取消的请求，防止内存泄漏
			for id, asyncRequest := range asyncNotifys {
				if asyncRequest.Canceled() {
					delete(asyncNotifys, id)
				}
			}
		}
	}

	// 关闭底层连接，让 receiveGo 退出
	conn.Close(ctx)
	cancel()

	// 处理 recvchan 中残留的数据，尝试匹配响应
	for recv := range recvchan {
		if notify, ok := any(recv).(NotifyMessage); ok {
			if notifyId, ok := notify.Id(); ok {
				if asyncRequest, found := asyncNotifys[notifyId]; found {
					asyncRequest.Response(recv, nil)
					delete(asyncNotifys, notifyId)
				}
			}
		}
	}
	if client.lastError == nil {
		client.lastError = ErrConnectionClosed
	}
	// 通知所有未匹配的请求：连接已关闭
	for _, asyncRequest := range asyncNotifys {
		asyncRequest.Response(zeroM, client.lastError)
	}
	asyncNotifys = nil
	// 通知 Write/Request 连接已关闭
	close(stopchan)
	client.waiter.Done()
}

func (client *Client[M, T]) async(ctx context.Context, request *asynRequest[M, T]) error {
	if client.asynchan == nil {
		return ErrNotConnected
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case client.asynchan <- request:
		return nil
	case <-client.stopChan:
		return ErrConnectionClosed
	}
}

type messageError[M any] struct {
	Error    error
	Response M
}

// asyncMessage 封装发送请求
type asyncMessage[M any] struct {
	Data     M                       // 要发送的数据
	Notify   bool                    // 是否需要等待响应
	callback func(resp M, err error) // 可选的回调函数
	canceled atomic.Bool             // 是否已取消
	waiter   chan *messageError[M]
}

func (request *asyncMessage[M]) Canceled() bool {
	if request.canceled.Load() {
		if request.waiter != nil {
			close(request.waiter)
			request.waiter = nil
		}
		if request.callback != nil {
			var zeroM M
			go request.callback(zeroM, fmt.Errorf("request canceled"))
			request.callback = nil
		}
		return true
	}
	return false
}

// Response 处理响应数据或错误
func (request *asyncMessage[M]) Response(resp M, err error) {
	if request.callback != nil {
		go request.callback(resp, err)
		request.callback = nil
	}
	if request.waiter != nil {
		select {
		case request.waiter <- &messageError[M]{Response: resp, Error: err}:
		default:
		}
		close(request.waiter)
		request.waiter = nil
	}
}

// RequestCallbackUnsafe 发送数据到连接(设置是否需要响应)
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) RequestCallbackUnsafe(ctx context.Context, data M, callback func(resp M, err error)) error {
	message := &asyncMessage[M]{Data: data, Notify: true, callback: callback}
	request := &asynRequest[M, T]{Command: AsyncCommandSend, Message: message}
	if err := client.async(ctx, request); err != nil {
		if callback != nil {
			var zeroM M
			go callback(zeroM, err)
		}
		return err
	}
	return nil
}

// asyncMessageUnsafe 发送数据到连接(设置是否需要响应)
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) asyncMessage(ctx context.Context, data M, notify bool) (*asyncMessage[M], error) {
	message := &asyncMessage[M]{Data: data, Notify: notify}
	if notify {
		message.waiter = make(chan *messageError[M], 1)
	}
	request := &asynRequest[M, T]{Command: AsyncCommandSend, Message: message}
	if err := client.async(ctx, request); err != nil {
		if notify {
			close(message.waiter)
		}
		return nil, err
	}
	return message, nil
}

// WriteUnsafe 发送数据到连接(不需要响应)
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) WriteUnsafe(ctx context.Context, data M) error {
	_, err := client.asyncMessage(ctx, data, false)
	return err
}

// Write 发送数据到连接(不需要响应)
// 线程安全，可并发调用。
func (client *Client[M, T]) Write(ctx context.Context, data M) error {
	client.locker.RLock()
	defer client.locker.RUnlock()
	return client.WriteUnsafe(ctx, data)
}

// RequestUnsafe 发送数据到连接, 等待响应.
// *注意* 次方法会阻塞直到收到响应或发生错误, 所以不可以在 Handle 回调中调用此方法.
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) RequestUnsafe(ctx context.Context, data M) (M, error) {
	var zeroM M
	message, err := client.asyncMessage(ctx, data, true)
	if err != nil {
		return zeroM, err
	}
	select {
	case <-ctx.Done():
		message.canceled.Store(true)
		return zeroM, ctx.Err()
	case resp, ok := <-message.waiter:
		if !ok {
			return zeroM, ErrConnectionClosed
		}
		return resp.Response, resp.Error
	}
}

// Request 发送数据到连接, 等待响应.
// *注意* 次方法会阻塞直到收到响应或发生错误, 所以不可以在 Handle 回调中调用此方法.
// 线程安全，可并发调用。
func (client *Client[M, T]) Request(ctx context.Context, data M) (M, error) {
	client.locker.RLock()
	defer client.locker.RUnlock()
	return client.RequestUnsafe(ctx, data)
}

// AsyncCallUnsafe 异步执行回调函数，传入当前连接。
// 回调在 asyncGo 协程中执行，保证回调内部线程安全。
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) AsyncCallUnsafe(ctx context.Context, callback func(ctx context.Context, conn T)) error {
	request := &asynRequest[M, T]{Command: AsyncCommandCallback, Callback: callback}
	return client.async(ctx, request)
}

// AsyncCall 异步执行回调函数，传入当前连接。
// 回调在 asyncGo 协程中执行，保证线程安全。
// 如果客户端未连接，返回错误。
func (client *Client[M, T]) AsyncCall(ctx context.Context, callback func(ctx context.Context, conn T)) error {
	client.locker.RLock()
	defer client.locker.RUnlock()
	return client.AsyncCallUnsafe(ctx, callback)
}
