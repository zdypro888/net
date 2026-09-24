package net

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotConnected 表示客户端尚未建立过连接。
var ErrNotConnected = errors.New("not connected")

// ErrConnectionClosed 表示已有连接被关闭或断开。
var ErrConnectionClosed = errors.New("connection closed")

// ErrDuplicateRequestID 表示同一连接中存在重复的在途请求 ID。
var ErrDuplicateRequestID = errors.New("duplicate request id")

// ErrPushQueueFull 表示业务处理积压；关闭当前连接，不能静默丢弃推送或阻塞控制响应。
var ErrPushQueueFull = errors.New("push queue capacity exceeded")

var errCanceledRequestIDLimit = errors.New("canceled request id retention limit reached")

type duplicateRequestIDError struct {
	id any
}

// Error 返回包含冲突请求 ID 的错误文本。
func (err duplicateRequestIDError) Error() string {
	return fmt.Sprintf("%s: %v", ErrDuplicateRequestID, err.id)
}

// Unwrap 允许调用方通过 errors.Is 识别 ErrDuplicateRequestID。
func (err duplicateRequestIDError) Unwrap() error {
	return ErrDuplicateRequestID
}

// IsDuplicateRequestID 为不依赖具体 net 版本的上层提供语义识别。
func (err duplicateRequestIDError) IsDuplicateRequestID() bool {
	return true
}

// DefaultBufferSize 是客户端异步请求与接收队列的默认容量。
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
// ResetUnsafe / CloseUnsafe 仅供独占生命周期的单 owner 调用，不得与 Reset/Close 混用。
// 其它 Unsafe 名称为兼容旧调用保留，入队与对应普通方法使用同一快照和停止门控。
type Client[M any, T Conn[M]] struct {
	locker    sync.RWMutex   // 保护 Client 会话状态的读写锁
	lifecycle sync.Mutex     // 串行化公开 Reset/Close；等待 worker 时不占状态锁
	waiter    sync.WaitGroup // 等待 goroutine 退出
	errMu     sync.RWMutex

	conn      T     // 底层连接实现
	lastError error // 最后发生的错误

	heartCount atomic.Uint64
	heartTime  time.Time // 下次心跳时间
	bufferSize int

	asynchan chan *asynRequest[M, T]
	// 每代独立的入队锁；Close 先关 stopChan，再等待发送者退出，避免队列背压挡住关闭。
	sendMu   *sync.RWMutex
	stopChan chan struct{} // 停止信号; 由 stopOnce 保证只 close 一次
	active   atomic.Bool

	// Close 必须先中断底层 I/O，再等待 worker 退出。
	closeCurrent func() error

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

// AsyncCommand 表示网络客户端异步工作队列中的操作类型。
type AsyncCommand int

const (
	// AsyncCommandSend 表示发送消息并按需等待响应。
	AsyncCommandSend AsyncCommand = iota + 1
	// AsyncCommandCallback 表示在连接所属工作协程中执行回调。
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

// Conn 返回当前底层连接的并发安全快照。
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
	client.startConnection(ctx, conn)
}

// startConnection 在旧代 worker 全部退出后初始化新代；调用方独占生命周期和状态写入。
func (client *Client[M, T]) startConnection(ctx context.Context, conn T) {
	client.conn = conn
	var closeOnce sync.Once
	var closeErr error
	closeCurrent := func() error {
		closeOnce.Do(func() {
			closeErr = conn.Close(context.Background())
		})
		return closeErr
	}
	client.closeCurrent = closeCurrent

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
	client.sendMu = &sync.RWMutex{}
	client.active.Store(true)
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

	// 推送按序独立执行；Handle 内部即使再次发送请求，也不阻塞响应匹配循环。
	pushes := make(chan M, bufSize)
	client.waiter.Go(func() {
		for {
			select {
			case <-handleCtx.Done():
				return
			case msg := <-pushes:
				if handleCtx.Err() != nil {
					return
				}
				conn.Handle(handleCtx, msg)
			}
		}
	})
	// 启动工作协程. 用 Go 1.25 WaitGroup.Go 自动 Add(1)/Done, 避免显式
	// Add/Done 配对错位的经典坑.
	client.waiter.Go(func() {
		client.asyncGo(cctx, handleCtx, cancel, handleCancel, conn, closeCurrent, asynchan, recvchan, pushes)
	})
	client.waiter.Go(func() { client.receiveGo(cctx, conn, recvchan) })
}

// Reset 重置 Client 使用新的连接。如果存在则关闭旧连接。
func (client *Client[M, T]) Reset(ctx context.Context, conn T) {
	client.lifecycle.Lock()
	defer client.lifecycle.Unlock()
	client.locker.Lock()
	client.CloseUnsafe()
	client.locker.Unlock()
	// 回调收到取消后可能读取 Stats/Conn；等待退出时不能持状态写锁。
	client.waiter.Wait()
	client.locker.Lock()
	client.startConnection(ctx, conn)
	client.locker.Unlock()
}

// CloseUnsafe 中断当前代连接并停止入队；调用方须独占 Reset/Close。
// stopChan 必须先于 sendMu 关闭，否则满队列中的发送者无法释放入队读锁。
func (client *Client[M, T]) CloseUnsafe() {
	client.closed.Store(true)
	client.signalStop()
	if client.handleCancel != nil {
		client.handleCancel()
	}
	if client.closeCurrent != nil {
		if err := client.closeCurrent(); err != nil {
			slog.Debug("net.Client interrupt close failed", slog.Any("err", err))
		}
	}
	client.active.Store(false)
	if client.asynchan != nil {
		client.sendMu.Lock()
		close(client.asynchan)
		client.asynchan = nil
		client.sendMu.Unlock()
	}
}

// Close 关闭 Client 并释放所有资源。
// 会等待所有 goroutine 退出后返回。
// 返回连接期间最后发生的错误（如果有）。
func (client *Client[M, T]) Close() error {
	client.lifecycle.Lock()
	defer client.lifecycle.Unlock()
	client.locker.Lock()
	client.CloseUnsafe()
	client.locker.Unlock()
	client.waiter.Wait()
	return client.getLastError()
}

// Stats 返回 net.Client 运行时计数，队列快照在读锁内取得。
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
func (client *Client[M, T]) asyncGo(ctx context.Context, handleCtx context.Context, cancel context.CancelFunc, handleCancel context.CancelFunc, conn T, closeConn func() error, asynchan chan *asynRequest[M, T], recvchan <-chan M, pushes chan<- M) {
	requests := newRequestTracker[M]()
	var zeroM M
	running := true
	heartTimer := time.NewTimer(time.Until(client.heartTime))
	defer heartTimer.Stop()
	pendingTicker := time.NewTicker(time.Second)
	defer pendingTicker.Stop()
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
					if notifyId, ok := notify.Id(); ok && notifyId != nil && reflect.ValueOf(notifyId).Comparable() {
						foundNotify = requests.handleResponse(notifyId, recv)
					}
				}
				if !foundNotify {
					select {
					case pushes <- recv:
					default:
						client.setLastError(ErrPushQueueFull)
						running = false
					}
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
					writeCtx := ctx
					var cancelWrite context.CancelFunc
					var stopConnectionCancel func() bool
					var notifyID any
					if asyncRequest.Notify {
						if err := asyncRequest.contextErr(); err != nil {
							asyncRequest.Response(zeroM, err)
							continue
						}
						notify, ok := any(asyncRequest.Data).(NotifyMessage)
						if !ok {
							asyncRequest.Response(zeroM, fmt.Errorf("message does not implement Notify interface"))
							continue
						}
						var hasID bool
						notifyID, hasID = notify.Id()
						if !hasID || notifyID == nil {
							asyncRequest.Response(zeroM, fmt.Errorf("message does not provide a request id"))
							continue
						}
						if !reflect.ValueOf(notifyID).Comparable() {
							asyncRequest.Response(zeroM, fmt.Errorf("request id type %T is not comparable", notifyID))
							continue
						}
						if existing, exists := requests.request(notifyID); exists {
							if err := existing.contextErr(); err != nil {
								if !requests.cancel(notifyID, existing, zeroM, err) {
									client.setLastError(errCanceledRequestIDLimit)
									running = false
								}
							}
							asyncRequest.Response(zeroM, duplicateRequestIDError{id: notifyID})
							continue
						}
						if requests.containsCanceled(notifyID) {
							asyncRequest.Response(zeroM, duplicateRequestIDError{id: notifyID})
							continue
						}
						// 请求写入既受调用方控制，也必须在本次连接结束时退出。
						writeCtx, cancelWrite = context.WithCancel(asyncRequest.ctx)
						stopConnectionCancel = context.AfterFunc(ctx, cancelWrite)
					}

					err := conn.Write(writeCtx, asyncRequest.Data)
					if stopConnectionCancel != nil {
						stopConnectionCancel()
						cancelWrite()
					}
					if err != nil {
						asyncRequest.Response(zeroM, err)
					} else if asyncRequest.Notify {
						requests.add(notifyID, asyncRequest)
					} else {
						asyncRequest.Response(zeroM, nil)
					}
					if err != nil {
						client.setLastError(err)
						running = false
					}
				case AsyncCommandCallback:
					if asyncall.Callback != nil {
						asyncall.Callback(handleCtx, conn)
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
		case <-pendingTicker.C:
			if !requests.removeCanceled(zeroM) {
				client.setLastError(errCanceledRequestIDLimit)
				running = false
			}
		}
	}
	client.signalStop()
	// 停止信号之后等待已取得本代队列的发送者退出，随后排空才不会遗漏迟到入队的请求。
	// [审计修复 2026-09-24] 仅新增本注释与下方 lint:ignore，代码未变。
	// 有意的空临界区：写锁作为屏障，等待持有读锁的 async 调用全部返回。
	client.sendMu.Lock()
	//lint:ignore SA2001 见上：空临界区是屏障而非遗漏。
	client.sendMu.Unlock()
	client.active.Store(false)

	// Close 可能等待 Handle 退出，必须先取消业务处理。
	handleCancel()
	// 关闭底层连接，让 receiveGo 退出. close 错误只记日志, 不写入 lastError:
	// 保持 Close/pending Request 的返回错误归一为 ErrConnectionClosed(与 receiveGo
	// 及其它 Close 错误处理一致), 避免 conn.Close 偶发错误污染对外错误契约.
	if err := closeConn(); err != nil {
		slog.Warn("net.Client teardown close failed", slog.Any("err", err))
	}
	// 底层连接已关闭，再取消接收协程的父上下文。
	cancel()

	// 处理 recvchan 中残留的数据，尝试匹配响应
	for recv := range recvchan {
		if notify, ok := any(recv).(NotifyMessage); ok {
			if notifyId, ok := notify.Id(); ok && notifyId != nil && reflect.ValueOf(notifyId).Comparable() {
				// 清理阶段也须遵守取消隔离，不能把已取消请求报告为成功。
				requests.handleResponse(notifyId, recv)
			}
		}
	}
	lastErr := client.getLastError()
	if lastErr == nil {
		lastErr = ErrConnectionClosed
		client.setLastError(lastErr)
	}

	// 排空 asynchan 中已入队但未处理的请求，通知 caller 失败。
	// 注意: asynchan close 责任在 CloseUnsafe (Wlock 内), 这里只 drain 已入队的;
	// 若 asynchan 未 close (Reset 路径 / ctx-cancel 路径), 用 select+default 拉空,
	// caller 后续 send 会走 stopChan 分支拿到 ErrConnectionClosed.
	failQueuedRequests(asynchan, zeroM, lastErr)

	// 通知所有未匹配的请求：连接已关闭. 跳过已 canceled 的 (caller 已经从 ctx.Done 走了,
	// 通知反而是 double-touch).
	requests.finish(zeroM, lastErr)
	notifyRemaining := requests.count()

	// 通知 Write/Request 连接已关闭. signalStop 用 sync.Once, 与 CloseUnsafe 已经
	// signalStop 过的场景幂等. 主动关闭属于正常生命周期，只把意外断线记为 warn.
	if client.closed.Load() {
		slog.Debug("net.Client closed",
			slog.Any("lastError", lastErr),
			slog.Int("notifyRemaining", notifyRemaining))
	} else {
		slog.Warn("net.Client closed unexpectedly",
			slog.Any("lastError", lastErr),
			slog.Int("notifyRemaining", notifyRemaining))
	}
	client.signalStop()
}

// failQueuedRequests 完成连接停止前已入队、但尚未由 asyncGo 写出的请求。
func failQueuedRequests[M any, T Conn[M]](asynchan <-chan *asynRequest[M, T], zeroM M, connectionErr error) {
	for {
		select {
		case req, ok := <-asynchan:
			if !ok {
				return
			}
			if req.Command == AsyncCommandSend && req.Message != nil {
				requestErr := connectionErr
				if contextErr := req.Message.contextErr(); contextErr != nil {
					requestErr = contextErr
				}
				req.Message.Response(zeroM, requestErr)
			}
		default:
			return
		}
	}
}

func (client *Client[M, T]) async(ctx context.Context, request *asynRequest[M, T]) error {
	// 状态锁只用于取得本代快照；任何背压等待均在锁外，Close/Reset 可立即发出停止信号。
	client.locker.RLock()
	queue, stop, sendMu := client.asynchan, client.stopChan, client.sendMu
	active := client.active.Load()
	client.locker.RUnlock()
	if !active || queue == nil {
		if stop == nil {
			return ErrNotConnected
		}
		return ErrConnectionClosed
	}
	sendMu.RLock()
	defer sendMu.RUnlock()
	select {
	case <-stop:
		return ErrConnectionClosed
	default:
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultAsyncTimeout)
		defer cancel()
	}
	// 已取消的调用不能因发送分支恰好可用而成功入队。
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			client.asyncTimeouts.Add(1)
		}
		return err
	}
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			client.asyncTimeouts.Add(1)
		}
		return ctx.Err()
	case queue <- request:
		return nil
	case <-stop:
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
	ctx      context.Context
	once     sync.Once
}

func (request *asyncMessage[M]) contextErr() error {
	if request.ctx != nil {
		if err := request.ctx.Err(); err != nil {
			return err
		}
	}
	if request.canceled.Load() {
		return context.Canceled
	}
	return nil
}

// Response 由 asyncGo 单 goroutine 调用. waiter chan 在 asyncMessage(notify=true)
// 路径创建, buffer=1; select+default 保证 caller 已离开 (从 ctx.Done 走) 时也不
// 阻塞. close(waiter) 让 RequestUnsafe 内 select 解锁.
// BUG-7: 旧 Canceled() 有 close(waiter) side-effect, 已删除; 现在 Response 是
// "唯一关闭 waiter 的入口", 不会与其它路径 double close.
func (request *asyncMessage[M]) Response(resp M, err error) {
	request.once.Do(func() {
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
	})
}

// RequestCallbackUnsafe 发送数据到连接(设置是否需要响应)
// 保留旧方法名；入队会短暂加锁，回调在独立 goroutine 中执行。
func (client *Client[M, T]) RequestCallbackUnsafe(ctx context.Context, data M, callback func(resp M, err error)) error {
	message := &asyncMessage[M]{Data: data, Notify: true, callback: callback, ctx: ctx}
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
// 入队只在取得当前连接快照时短暂加锁。
func (client *Client[M, T]) asyncMessage(ctx context.Context, data M, notify bool) (*asyncMessage[M], error) {
	message := &asyncMessage[M]{Data: data, Notify: notify}
	if notify {
		message.ctx = ctx
		message.waiter = make(chan *messageError[M], 1)
	}
	request := &asynRequest[M, T]{Command: AsyncCommandSend, Message: message}
	if err := client.async(ctx, request); err != nil {
		if notify {
			var zero M
			message.Response(zero, err)
		}
		return nil, err
	}
	return message, nil
}

// WriteUnsafe 发送数据到连接(不需要响应)
// 保留旧方法名；ctx 仅控制入队，已入队通知按连接生命周期发送。
func (client *Client[M, T]) WriteUnsafe(ctx context.Context, data M) error {
	_, err := client.asyncMessage(ctx, data, false)
	return err
}

// Write 发送数据到连接(不需要响应)
// 线程安全，可并发调用。
func (client *Client[M, T]) Write(ctx context.Context, data M) error {
	return client.WriteUnsafe(ctx, data)
}

// RequestUnsafe 发送数据到连接, 等待响应.
// *注意* 次方法会阻塞直到收到响应或发生错误, 所以不可以在 Handle 回调中调用此方法.
// 保留旧方法名；行为与 Request 相同。
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
// 入队和等待响应均不持会话状态锁；async 只在取得队列快照时短暂加锁。
func (client *Client[M, T]) Request(ctx context.Context, data M) (M, error) {
	message, err := client.asyncMessage(ctx, data, true)
	if err != nil {
		var zero M
		return zero, err
	}
	return client.waitResponse(ctx, message)
}

// waitResponse 等 asyncGo 完成该请求或调用方取消，不持会话锁。
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
// 保留旧方法名；回调须观察传入 ctx，且不能在自身 worker 中同步 Close/Reset/Request。
func (client *Client[M, T]) AsyncCallUnsafe(ctx context.Context, callback func(ctx context.Context, conn T)) error {
	request := &asynRequest[M, T]{Command: AsyncCommandCallback, Callback: callback}
	return client.async(ctx, request)
}

// AsyncCall 异步执行回调函数，传入当前连接。
// 回调在 asyncGo 协程中执行，保证线程安全。
// 如果客户端未连接，返回错误。
func (client *Client[M, T]) AsyncCall(ctx context.Context, callback func(ctx context.Context, conn T)) error {
	return client.AsyncCallUnsafe(ctx, callback)
}
