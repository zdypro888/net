package net

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Client 是一个支持请求-响应模式的多路复用网络客户端。
//
// 特性：
//   - 支持异步请求-响应匹配（通过 Notify 接口的 Id 匹配）
//   - 支持单向写入（Write）和双向请求（Request）
//   - 线程安全，支持并发调用 Write/Request
//   - 支持优雅关闭和连接重置
//
// 架构：
//   - receiveGo: 接收协程，从 Conn 读取数据并发送到 recvchan
//   - asyncGo: 异步处理协程，处理发送队列和接收队列，匹配请求-响应
//
// 使用示例：
//
//	client := NewClient(ctx, conn)
//	defer client.Close()
//
//	// 单向写入
//	err := client.Write(ctx, message)
//
//	// 请求-响应（message 需实现 Notify 接口）
//	resp, err := client.Request(ctx, request)
type Client struct {
	locker sync.RWMutex       // 保护 Client 状态的读写锁
	waiter sync.WaitGroup     // 等待 goroutine 退出
	cancel context.CancelFunc // 取消内部 context

	conn      Conn        // 底层连接实现
	running   atomic.Bool // 运行状态标志
	lastError error       // 最后发生的错误

	heartCount atomic.Uint64
	heartTime  time.Time // 下次心跳时间

	sendchan chan *sendEvent // 发送队列
	stopChan chan struct{}   // 停止信号，通知 Write/Request 连接已关闭
}

// receiveEvent 封装从连接接收到的数据
type receiveEvent struct {
	Data  any   // 接收到的数据
	Error error // 接收时发生的错误
}

// NewClient 创建并启动一个新的 Client。
// ctx 用于控制 Client 的生命周期，取消 ctx 会导致连接关闭。
// conn 是底层的网络连接实现。
func NewClient(ctx context.Context, conn Conn) *Client {
	client := &Client{}
	client.onConnected(ctx, conn)
	return client
}

// Reset 重置 Client 使用新的连接。
// 仅在 Client 未运行时可调用（连接已断开或已关闭）。
// 如果 Client 正在运行，返回错误。
func (client *Client) Reset(ctx context.Context, conn Conn) error {
	client.locker.Lock()
	defer client.locker.Unlock()
	if client.running.Load() {
		return fmt.Errorf("already connected")
	}
	client.onConnected(ctx, conn)
	return nil
}

func (client *Client) Conn() Conn {
	client.locker.Lock()
	defer client.locker.Unlock()
	return client.conn
}

// onConnected 初始化连接状态并启动工作协程。
// 必须在持有锁或初始化时调用。
func (client *Client) onConnected(ctx context.Context, conn Conn) {
	// 关闭旧的发送通道，通知旧的 asyncGo 退出
	client.closeSendChan()
	// 等待旧的 goroutine 完全退出
	client.waiter.Wait()
	client.running.Store(true)

	// 创建新的通道
	client.sendchan = make(chan *sendEvent, 16)
	client.waiter.Add(2)
	recvchan := make(chan *receiveEvent, 16)
	cctx, cancel := context.WithCancel(ctx)
	client.cancel = cancel

	client.conn = conn

	// 初始化心跳时间
	if heartConn, ok := conn.(ConnHeart); ok {
		_, heartTime := heartConn.Heart(true, 0)
		client.heartTime = heartTime
	} else {
		client.heartTime = time.Now().Add(60 * time.Second)
	}

	// 启动工作协程
	client.stopChan = make(chan struct{})
	go client.asyncGo(cctx, conn, client.sendchan, recvchan, client.stopChan)
	go client.receiveGo(cctx, conn, recvchan)
}

// closeSendChan 关闭发送通道并处理残留的发送请求。
// 残留的请求会收到 "connection closed" 错误。
func (client *Client) closeSendChan() {
	if client.sendchan != nil {
		close(client.sendchan)
		// 处理缓冲区中残留的发送请求
		for send := range client.sendchan {
			send.Response <- &dataOrErr{Error: fmt.Errorf("connection closed")}
			close(send.Response)
		}
		client.sendchan = nil
	}
}

// Close 关闭 Client 并释放所有资源。
// 会等待所有 goroutine 退出后返回。
// 返回连接期间最后发生的错误（如果有）。
func (client *Client) Close() error {
	client.locker.Lock()
	defer client.locker.Unlock()

	client.running.Store(false)

	// 取消内部 context，通知 goroutine 退出
	if client.cancel != nil {
		client.cancel()
		client.cancel = nil
	}

	// 关闭发送通道，触发 asyncGo 退出
	client.closeSendChan()

	// 等待所有 goroutine 退出
	client.waiter.Wait()

	err := client.lastError
	return err
}

// receiveGo 是接收协程，负责从连接读取数据。
// 读取到的数据发送到 recvchan 供 asyncGo 处理。
// 当连接断开或发生错误时退出。
func (client *Client) receiveGo(ctx context.Context, conn Conn, recvchan chan *receiveEvent) {
	for client.running.Load() {
		data, err := conn.Read(ctx)
		if err != nil {
			client.running.Store(false)
			break
		}
		event := &receiveEvent{Data: data, Error: err}
		recvchan <- event
	}
	close(recvchan)
	client.waiter.Done()
}

// asyncGo 是异步处理协程，负责：
// 1. 处理发送队列（sendchan）中的请求
// 2. 处理接收队列（recvchan）中的响应
// 3. 匹配请求和响应（通过 Notify.Id）
// 4. 分发未匹配的消息到 Handle
func (client *Client) asyncGo(ctx context.Context, conn Conn, sendchan <-chan *sendEvent, recvchan <-chan *receiveEvent, stopchan chan struct{}) {
	// notifys 存储等待响应的请求，key 是 Notify.Id()
	notifys := make(map[any]chan *dataOrErr)

	// 主循环：处理发送和接收
	for client.running.Load() {
		select {
		case <-ctx.Done():
			// context 被取消
			client.running.Store(false)
			client.lastError = ctx.Err()

		case <-stopchan:
			// 收到停止信号
			client.running.Store(false)

		case recv, ok := <-recvchan:
			// 处理接收到的数据
			if !ok {
				// recvchan 已关闭，receiveGo 已退出
				client.running.Store(false)
			} else if recv.Error != nil {
				// 接收时发生错误
				client.lastError = recv.Error
				client.running.Store(false)
			} else {
				// 尝试匹配请求
				foundNotify := false
				if notify, ok := recv.Data.(Notify); ok {
					if notifyId, ok := notify.Id(); ok {
						if respChan, ok := notifys[notifyId]; ok {
							// 找到匹配的请求，发送响应
							respChan <- &dataOrErr{Data: recv.Data}
							close(respChan)
							delete(notifys, notifyId)
							foundNotify = true
						}
					}
				}
				if !foundNotify {
					// 无匹配请求，作为服务端推送处理
					if data := conn.Handle(ctx, recv.Data); data != nil {
						// 处理返回的数据（如果有）
						if err := conn.Write(ctx, data); err != nil {
							client.lastError = err
							client.running.Store(false)
						}
					}
				}
			}

		case send, ok := <-sendchan:
			// 处理发送请求
			if !ok {
				// sendchan 已关闭，Client 正在关闭
				client.running.Store(false)
			} else {
				// 写入数据到连接
				err := conn.Write(ctx, send.Data)
				isNotifySuccess := false
				if err == nil {
					if send.Notify {
						// 写入成功且需要等待响应
						if notify, ok := send.Data.(Notify); ok {
							if notifyId, ok := notify.Id(); ok {
								// 注册到等待队列
								notifys[notifyId] = send.Response
								isNotifySuccess = true
							}
						}
					}
				} else {
					// 写入时发生错误
					client.lastError = err
					client.running.Store(false)
				}
				if !isNotifySuccess {
					// 不需要等待响应，或数据未实现 Notify 接口
					// 立即返回结果
					send.Response <- &dataOrErr{Error: err}
					close(send.Response)
				}
			}

		case <-time.After(time.Until(client.heartTime)):
			// 处理心跳
			if heartConn, ok := conn.(ConnHeart); ok {
				var heartData any
				if heartData, client.heartTime = heartConn.Heart(false, client.heartCount.Add(1)); heartData != nil {
					if err := conn.Write(ctx, heartData); err != nil {
						client.lastError = err
						client.running.Store(false)
					}
				}
			} else {
				// 无心跳支持，设置为较远的时间点
				client.heartTime = time.Now().Add(60 * time.Second)
			}
		}
	}

	// 退出清理

	// 关闭底层连接，让 receiveGo 退出
	conn.Close(ctx)

	// 处理 recvchan 中残留的数据，尝试匹配响应
	for recv := range recvchan {
		if notify, ok := recv.Data.(Notify); ok {
			if notifyId, ok := notify.Id(); ok {
				if respChan, found := notifys[notifyId]; found {
					respChan <- &dataOrErr{Data: recv.Data, Error: recv.Error}
					close(respChan)
					delete(notifys, notifyId)
				}
			}
		}
	}

	// 通知所有未匹配的请求：连接已关闭
	for _, respChan := range notifys {
		respChan <- &dataOrErr{Error: fmt.Errorf("connection closed")}
		close(respChan)
	}

	// 通知 Write/Request 连接已关闭
	close(stopchan)
	client.waiter.Done()
}

// dataOrErr 封装响应数据或错误
type dataOrErr struct {
	Data  any   // 响应数据
	Error error // 错误信息
}

// sendEvent 封装发送请求
type sendEvent struct {
	Data     any             // 要发送的数据
	Notify   bool            // 是否需要等待响应
	Response chan *dataOrErr // 响应通道
}

// Write 发送数据到连接，不等待响应。
// 阻塞直到数据被写入或发生错误。
// 线程安全，可并发调用。
func (client *Client) Write(ctx context.Context, data any) error {
	if data == nil {
		return fmt.Errorf("data is nil")
	}
	client.locker.RLock()
	defer client.locker.RUnlock()

	if !client.running.Load() {
		return fmt.Errorf("not connected")
	}

	send := &sendEvent{Data: data, Notify: false, Response: make(chan *dataOrErr, 1)}

	// 发送到队列
	select {
	case <-ctx.Done():
		return ctx.Err()
	case client.sendchan <- send:
	case <-client.stopChan:
		return fmt.Errorf("connection closed")
	}

	// 等待写入完成
	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-send.Response:
		return resp.Error
	case <-client.stopChan:
		return fmt.Errorf("connection closed")
	}
}

// Request 发送请求并等待响应。
// data 必须实现 Notify 接口，否则行为与 Write 相同。
// 响应通过 Notify.Id() 匹配。
// 阻塞直到收到响应或发生错误。
// 线程安全，可并发调用。
func (client *Client) Request(ctx context.Context, data any) (any, error) {
	if data == nil {
		return nil, fmt.Errorf("data is nil")
	}
	client.locker.RLock()
	defer client.locker.RUnlock()

	if !client.running.Load() {
		return nil, fmt.Errorf("not connected")
	}

	send := &sendEvent{Data: data, Notify: true, Response: make(chan *dataOrErr, 1)}

	// 发送到队列
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case client.sendchan <- send:
	case <-client.stopChan:
		return nil, fmt.Errorf("connection closed")
	}

	// 等待响应
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-send.Response:
		return resp.Data, resp.Error
	case <-client.stopChan:
		return nil, fmt.Errorf("connection closed")
	}
}
