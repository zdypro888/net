package net

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotConnected: Client 从未通过 Reset/ResetUnsafe 建立过连接.
// ErrConnectionClosed: 曾经连接过, 但已被 Close 或底层连接断开.
// 同一可观察状态只返回同一 sentinel; 下游用 errors.Is 区分"还没连"与"连过又断".
var ErrNotConnected = fmt.Errorf("not connected")
var ErrConnectionClosed = fmt.Errorf("connection closed")

const DefaultBufferSize = 0x10

// DefaultAsyncTimeout — async() 写 asynchan 的兜底超时. caller 传 ctx 无 deadline
// 时使用. RUN-6 修复: 防止 Write/Request 用 context.Background() + asynchan 满 +
// asyncGo 卡死 = 永久阻塞.
const DefaultAsyncTimeout = 30 * time.Second

// ClientStats 给运维查 net.Client 健康度.
type ClientStats struct {
	AsynchanLen   int    // 当前 asynchan 队列长度 (高 = caller 投递快/asyncGo 处理慢)
	AsynchanCap   int    // asynchan 容量
	HeartCount    uint64 // 心跳累计次数 (跨 Reset 归零)
	AsyncTimeouts uint64 // async 入队等待超时 (DefaultAsyncTimeout 兜底或 caller deadline 到期) 的累计次数; 不含 caller 主动取消
	LastError     error  // 最近一次 lastError 快照
	Closed        bool   // 是否已 Close
}

// Client 是一个支持请求-响应模式的多路复用网络客户端。
// 它封装了底层连接，实现了并发安全的读写操作，支持心跳和自动重连等功能。
// M 是消息类型，T 是底层连接类型，必须实现 Conn[M] 接口。
// T 注意需要指针类型才能修改连接状态。
// 原则上M, T都应该是指针类型，以避免数据拷贝开销。
//
// 关于 *Unsafe API 的并发契约 (R2 重新明确):
//   - Reset / Close / Write / Request / AsyncCall / RequestCallback (Lock 版本)
//     线程安全, 可并发调用.
//   - ResetUnsafe / CloseUnsafe / WriteUnsafe / RequestUnsafe / AsyncCallUnsafe /
//     RequestCallbackUnsafe 不持锁. 当且仅当 caller 自己是 single owner goroutine
//     (例如 apn.asyncGo, wsc.Session.asyncGo) 时可用 —— 该 goroutine 必须独占
//     Reset/Close/Write 系列调用. 与上面 Lock 版本混用会 race.
//   - async() 内部用 sync.Once 保证 stopChan 只 close 一次, 即便 Close 与 asyncGo
//     退出竞速也只触发一次 close. 它是 *Unsafe 与 Lock 版本之间的同步桥.
type Client[M any, T Conn[M]] struct {
	locker sync.RWMutex   // 保护 Client 状态的读写锁
	waiter sync.WaitGroup // 等待 goroutine 退出
	errMu  sync.RWMutex

	conn      T     // 底层连接实现
	lastError error // 最后发生的错误

	heartCount atomic.Uint64
	heartTime  time.Time // 下次心跳时间
	bufferSize int

	asynchan chan *asynRequest[M, T]
	stopChan chan struct{} // 停止信号; 由 stopOnce 保证只 close 一次

	// handleCancel 取消 *仅* 传给 conn.Handle 的 handleCtx (cctx 的子 ctx). D-P1-1 修复:
	// asyncGo 主循环里 conn.Handle 是同步调用, 用户 Handle 若长时间阻塞 (例如往满通道写)
	// 会卡死 asyncGo, 让 Close 的 waiter.Wait() 永久挂起. CloseUnsafe 立即 cancel
	// handleCtx, 把取消信号透传给遵守 ctx 的 Handle 实现 (如 wsc.wsconnection.Handle
	// select ctx.Done()), 使其解阻塞 → asyncGo 回到循环顶看到 asynchan 已关闭 → 走正常
	// 退出尾段 → Close 返回.
	//
	// 为什么不直接 cancel cctx: cctx 同时驱动 asyncGo 的 <-ctx.Done() 分支 (会把
	// lastError 置为 context.Canceled). 若 Close 直接 cancel cctx, Close()/pending
	// Request 拿到的错误会从 ErrConnectionClosed 变成 context.Canceled — 改变了对外
	// 可观察语义. 用独立 handleCtx 只解阻塞 Handle, 不触碰 asyncGo 的错误归一逻辑.
	// 在 Lock 下读写, 与 ResetUnsafe/CloseUnsafe 串行.
	handleCancel context.CancelFunc

	// stopOnce: BUG-1/BUG-2 修复. 旧实现要求 stopChan "只能在 asyncGo 中关闭",
	// 这是一个隐式契约 — 一旦 Close 路径也想关 stopChan 让 async() 立即解锁就会
	// double close panic. sync.Once 让 Close/Reset 与 asyncGo 退出竞速时只触发一次.
	// 必须加: 多 goroutine 关闭同一 chan 是经典 Go 反模式, sync.Once 是唯一 idiomatic 方案.
	stopOnce sync.Once

	// 观察性 (OPS-3 Stats).
	asyncTimeouts atomic.Uint64
	closed        atomic.Bool
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
	client := &Client[M, T]{bufferSize: DefaultBufferSize}
	return client
}

// SetBufferSize 设置内部发送/接收队列容量.
// 必须在 Reset 前调用; 已连接后修改不会影响当前连接的队列.
func (client *Client[M, T]) SetBufferSize(size int) {
	client.locker.Lock()
	defer client.locker.Unlock()
	if size <= 0 {
		size = DefaultBufferSize
	}
	client.bufferSize = size
}

func (client *Client[M, T]) Conn() T {
	client.locker.RLock()
	defer client.locker.RUnlock()
	return client.conn
}

func (client *Client[M, T]) setLastError(err error) {
	client.errMu.Lock()
	client.lastError = err
	client.errMu.Unlock()
}

func (client *Client[M, T]) getLastError() error {
	client.errMu.RLock()
	defer client.errMu.RUnlock()
	return client.lastError
}

// signalStop 关闭 stopChan, 通知所有 select 在 stopChan 上的 goroutine.
// sync.Once 保证幂等 — Close / Reset / asyncGo 退出三处都可能 trigger.
func (client *Client[M, T]) signalStop() {
	if client.stopChan != nil {
		client.stopOnce.Do(func() { close(client.stopChan) })
	}
}

// ResetUnsafe 初始化连接状态并启动工作协程。
// 注意：此方法不持有锁，调用方需自行确保并发安全。
func (client *Client[M, T]) ResetUnsafe(ctx context.Context, conn T) {
	// 关闭旧的发送通道，通知旧的 asyncGo 退出
	client.CloseUnsafe()
	// 等待旧的 goroutine 完全退出
	client.waiter.Wait()
	client.conn = conn

	// 重置 stopOnce: 新的 session 用新的 stopChan, sync.Once 也要重置.
	client.stopOnce = sync.Once{}
	client.closed.Store(false)
	client.setLastError(nil)

	// 创建新的通道
	stopChan := make(chan struct{})
	client.stopChan = stopChan
	bufSize := client.bufferSize
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	asynchan := make(chan *asynRequest[M, T], bufSize)
	client.asynchan = asynchan
	recvchan := make(chan M, bufSize)
	cctx, cancel := context.WithCancel(ctx)
	// handleCtx 是 cctx 的子 ctx, 仅用于 conn.Handle (D-P1-1). CloseUnsafe cancel 它
	// 让阻塞的用户 Handle 解锁; 不触碰 cctx, 保证 asyncGo 的错误归一 (ErrConnectionClosed)
	// 不被 context.Canceled 覆盖. cctx cancel 时 handleCtx 也随之 cancel (父子关系).
	handleCtx, handleCancel := context.WithCancel(cctx)
	client.handleCancel = handleCancel

	// 心跳计数随会话归零, 避免新连接拿到上一次会话累计的 count.
	client.heartCount.Store(0)
	// 初始化心跳时间
	if heartConn, ok := any(conn).(ConnHeart[M]); ok {
		_, heartTime, _ := heartConn.Heart(true, 0)
		client.heartTime = heartTime
	} else {
		client.heartTime = time.Now().Add(60 * time.Second)
	}

	// 启动工作协程. 用 Go 1.25 WaitGroup.Go 自动 Add(1)/Done, 避免显式
	// Add/Done 配对错位的经典坑.
	client.waiter.Go(func() {
		client.asyncGo(cctx, handleCtx, cancel, handleCancel, conn, asynchan, recvchan)
	})
	client.waiter.Go(func() { client.receiveGo(cctx, conn, recvchan) })
}

// Reset 重置 Client 使用新的连接。如果存在则关闭旧连接。
func (client *Client[M, T]) Reset(ctx context.Context, conn T) {
	client.locker.Lock()
	defer client.locker.Unlock()
	client.ResetUnsafe(ctx, conn)
}

// CloseUnsafe 关闭异步通道，通知 asyncGo 退出。
// 注意：此方法不持有锁，调用方需自行确保并发安全。
// 安全: signalStop 用 sync.Once 保护, 与 asyncGo 退出尾段并发关 stopChan 不会 panic.
// asynchan 仍然由 CloseUnsafe (在 Lock 内) 单点关闭, 与 async() 的 RLock 互斥保证
// 不会 send-after-close.
func (client *Client[M, T]) CloseUnsafe() {
	client.closed.Store(true)
	// 先 signalStop 让 async() 与 RequestUnsafe 卡在 select 上的 caller 立即解锁,
	// 避免它们继续等 asynchan 空位 / response. asyncGo 在 ctx-cancel + asynchan
	// 关闭后会走到清理路径, 也会幂等地再 signalStop 一次.
	client.signalStop()
	// D-P1-1: cancel handleCtx, 把取消透传给可能正卡在 conn.Handle 内的用户回调
	// (遵守 ctx 的 Handle 会因 ctx.Done() 解阻塞), 从而 asyncGo 能回到循环顶看到
	// asynchan 已关闭并走正常退出尾段, waiter.Wait() 不被永久阻塞. 只 cancel
	// handleCtx 不 cancel cctx — 不改变 asyncGo 的 lastError 归一 (仍为 ErrConnectionClosed).
	// cancel 幂等; 若 asyncGo 已退出该字段仍有效, 调用无副作用.
	if client.handleCancel != nil {
		client.handleCancel()
	}
	if client.asynchan != nil {
		// asynchan 只可以在 locker 保护下关闭
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
	err := client.getLastError()
	return err
}

// Stats 返回 net.Client 运行时计数. 不持锁 — 各字段 atomic 或 chan-Len,
// 不保证瞬间一致性, 监控用足够.
func (client *Client[M, T]) Stats() ClientStats {
	stats := ClientStats{
		HeartCount:    client.heartCount.Load(),
		AsyncTimeouts: client.asyncTimeouts.Load(),
		Closed:        client.closed.Load(),
	}
	client.locker.RLock()
	if client.asynchan != nil {
		stats.AsynchanLen = len(client.asynchan)
		stats.AsynchanCap = cap(client.asynchan)
	}
	stats.LastError = client.getLastError()
	client.locker.RUnlock()
	return stats
}

// receiveGo 是接收协程，负责从连接读取数据。
// 读取到的数据发送到 recvchan 供 asyncGo 处理。
// 当连接断开或发生错误时退出。
func (client *Client[M, T]) receiveGo(ctx context.Context, conn T, recvchan chan M) {
	for {
		data, err := conn.Read(ctx)
		if err != nil {
			// C-P2-2: 非主动 Close 导致的读失败, 记录真实断连原因便于运维诊断;
			// 主动 Close (closed 先置位, teardown 再关 conn) 属预期, 不刷日志.
			if !client.closed.Load() {
				slog.Warn("net client receive loop exited on read error", slog.Any("err", err))
			}
			break
		}
		select {
		case recvchan <- data:
		case <-ctx.Done():
			close(recvchan)
			return
		}
	}
	close(recvchan)
}

// asyncGo 是异步处理协程，负责：
// 1. 处理发送队列（sendchan）中的请求
// 2. 处理接收队列（recvchan）中的响应
// 3. 匹配请求和响应（通过 Notify.Id）
// 4. 分发未匹配的消息到 Handle
func (client *Client[M, T]) asyncGo(ctx context.Context, handleCtx context.Context, cancel context.CancelFunc, handleCancel context.CancelFunc, conn T, asynchan <-chan *asynRequest[M, T], recvchan <-chan M) {
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
			client.setLastError(ctx.Err())
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
					// 无匹配请求，作为服务端推送处理. 用 handleCtx (Close 时被 cancel)
					// 而非 ctx: 让阻塞的用户 Handle 在 Close 发起时能解阻塞 (D-P1-1).
					conn.Handle(handleCtx, recv)
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
						client.setLastError(err)
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
						client.setLastError(err)
						running = false
					}
				}
			} else {
				// 无心跳支持，设置为较远的时间点
				client.heartTime = time.Now().Add(60 * time.Second)
			}
			// 防御性下限: Heart 实现若返回过去或零值时间会让 timer.Reset(<=0) 立即触发,
			// 形成 hot loop 卷死 CPU. 兜底 1s 保证 worst-case 仍是低频心跳.
			delay := time.Until(client.heartTime)
			if delay <= 0 {
				delay = time.Second
			}
			heartTimer.Reset(delay)
		}
		if len(asyncNotifys) > 100 {
			// 清理已取消的请求，防止内存泄漏.
			// BUG-7: 旧实现调 asyncRequest.Canceled() 内有 side-effect (close waiter + go callback),
			// 与下方退出尾段的 Response 路径会 double-close panic. 改为纯查询 + 单点清理:
			// canceled 的 message 直接 delete, 不通知 (caller 已经从 ctx.Done 路径走了).
			for id, asyncRequest := range asyncNotifys {
				if asyncRequest.canceled.Load() {
					delete(asyncNotifys, id)
				}
			}
		}
	}

	// 关闭底层连接，让 receiveGo 退出. close 错误只记日志, 不写入 lastError:
	// 保持 Close/pending Request 的返回错误归一为 ErrConnectionClosed(与 receiveGo
	// 及其它 Close 错误处理一致), 避免 conn.Close 偶发错误污染对外错误契约.
	if err := conn.Close(ctx); err != nil {
		slog.Warn("net.Client teardown close failed", slog.Any("err", err))
	}
	// 先 cancel handleCtx 再 cancel cctx (cctx cancel 会级联 cancel handleCtx,
	// 显式调用是为了清晰 + 释放 context 资源, 幂等).
	handleCancel()
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
	lastErr := client.getLastError()
	if lastErr == nil {
		lastErr = ErrConnectionClosed
		client.setLastError(lastErr)
	}

	// BUG-2 修复: 排空 asynchan 中已入队但未处理的 notify request, 通知 caller "失败".
	// Write(notify=false) 是 fire-and-forget, 入队成功即返回; 这里无法 retroactively
	// 改变它的返回值, 但可以保证 Request/RequestCallback 不会永久等待.
	// 注意: asynchan close 责任在 CloseUnsafe (Wlock 内), 这里只 drain 已入队的;
	// 若 asynchan 未 close (Reset 路径 / ctx-cancel 路径), 用 select+default 拉空,
	// caller 后续 send 会走 stopChan 分支拿到 ErrConnectionClosed.
drainLoop:
	for {
		select {
		case req, ok := <-asynchan:
			if !ok {
				break drainLoop
			}
			if req.Command == AsyncCommandSend && req.Message != nil {
				if !req.Message.canceled.Load() {
					req.Message.Response(zeroM, lastErr)
				}
			}
		default:
			break drainLoop
		}
	}

	// 通知所有未匹配的请求：连接已关闭. 跳过已 canceled 的 (caller 已经从 ctx.Done 走了,
	// 通知反而是 double-touch).
	for _, asyncRequest := range asyncNotifys {
		if !asyncRequest.canceled.Load() {
			asyncRequest.Response(zeroM, lastErr)
		}
	}
	notifyRemaining := len(asyncNotifys)

	// 通知 Write/Request 连接已关闭. signalStop 用 sync.Once, 与 CloseUnsafe 已经
	// signalStop 过的场景幂等. OPS-2: 记一行 warn 让运维知道 client 因什么退出.
	slog.Warn("net.Client closed",
		slog.Any("lastError", lastErr),
		slog.Int("notifyRemaining", notifyRemaining))
	client.signalStop()
}

func (client *Client[M, T]) async(ctx context.Context, request *asynRequest[M, T]) error {
	if client.asynchan == nil {
		// stopChan 在首次 ResetUnsafe 时创建: 仍为 nil 说明从未连接过;
		// 否则是连接后被 Close (CloseUnsafe 置 asynchan=nil) — 与下方
		// stopChan 竞速分支同语义, 同一"已关"状态只暴露 ErrConnectionClosed.
		if client.stopChan == nil {
			return ErrNotConnected
		}
		return ErrConnectionClosed
	}
	// RUN-6: caller 传 ctx 无 deadline 时强加 DefaultAsyncTimeout 兜底, 防止
	// asynchan 满 + asyncGo 卡死 = 永久阻塞. 不影响有 deadline 的 caller.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultAsyncTimeout)
		defer cancel()
	}
	select {
	case <-ctx.Done():
		// 只把真正的超时 (内部 DefaultAsyncTimeout 兜底或 caller deadline 到期)
		// 计入 asyncTimeouts; caller 主动取消 (context.Canceled) 不是拥塞信号.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			client.asyncTimeouts.Add(1)
		}
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
	canceled atomic.Bool             // 是否已取消 (RequestUnsafe ctx.Done 时 store true)
	waiter   chan *messageError[M]
}

// Response 由 asyncGo 单 goroutine 调用. waiter chan 在 asyncMessage(notify=true)
// 路径创建, buffer=1; select+default 保证 caller 已离开 (从 ctx.Done 走) 时也不
// 阻塞. close(waiter) 让 RequestUnsafe 内 select 解锁.
// BUG-7: 旧 Canceled() 有 close(waiter) side-effect, 已删除; 现在 Response 是
// "唯一关闭 waiter 的入口", 不会与其它路径 double close.
func (request *asyncMessage[M]) Response(resp M, err error) {
	if request.callback != nil {
		go request.callback(resp, err)
	}
	if request.waiter != nil {
		select {
		case request.waiter <- &messageError[M]{Response: resp, Error: err}:
		default:
		}
		close(request.waiter)
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

// asyncMessage 发送数据到连接(设置是否需要响应)
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
	message, err := client.asyncMessage(ctx, data, true)
	if err != nil {
		var zeroM M
		return zeroM, err
	}
	// 等待逻辑与 Request 完全一致, 复用 waitResponse 避免两份 select 漂移
	// (canceled 标记 / waiter close 语义二者必须一致, 单点维护更安全).
	return client.waitResponse(ctx, message)
}

// Request 发送数据到连接, 等待响应.
// *注意* 次方法会阻塞直到收到响应或发生错误, 所以不可以在 Handle 回调中调用此方法.
// 线程安全，可并发调用。
//
// 注意 RLock 释放时机: 入队 (asyncMessage → async) 在 RLock 内, 等待响应 (waiter)
// 在 RLock 外. 原因: 等响应可能阻塞数秒~数分钟, 持 RLock 会让并发的 Close (Wlock)
// 一直等不到 RLock 释放, 造成 Close 永远 hang. 而入队 / waiter 之间用 stopChan
// 与 message.canceled 的内置同步, 不需要锁保护.
func (client *Client[M, T]) Request(ctx context.Context, data M) (M, error) {
	client.locker.RLock()
	message, err := client.asyncMessage(ctx, data, true)
	client.locker.RUnlock()
	if err != nil {
		return *new(M), err
	}
	return client.waitResponse(ctx, message)
}

// waitResponse 等 asyncGo 通过 Response 或 stopChan 关闭通知; 不持锁.
func (client *Client[M, T]) waitResponse(ctx context.Context, message *asyncMessage[M]) (M, error) {
	var zeroM M
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
