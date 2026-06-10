package wsc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/zdypro888/net"
)

// DefaultAsyncTimeout — async() 写 asyncChan 的兜底超时. caller 传 ctx
// 无 deadline 时使用. 防上层 Reply/Write/Request 在 asyncChan 满 + 卡死
// 的 consumer 下永久阻塞.
const DefaultAsyncTimeout = 30 * time.Second

// HandchanBlockWarnThreshold — handleMessageGo 写 handchan 阻塞超过该窗口
// 时 log warn, 提示上层 consume goroutine 卡住或 handchan 容量不足.
// 不丢帧, 仅观察.
const HandchanBlockWarnThreshold = 5 * time.Second

type Packet[T any] struct {
	ID     string // 请求 ID (用于回复)
	Data   T
	Closed bool
}

// SessionStats 给运维查 wsc 健康度.
type SessionStats struct {
	AsyncChanLen    int    // 当前 asyncChan 队列长度 (高 = 上层 reply/write 堆积)
	AsyncChanCap    int    // asyncChan 容量
	HandchanLen     int    // 当前 handchan 队列长度 (高 = 上层 consume 慢)
	HandchanCap     int    // handchan 容量
	AsyncTimeouts   uint64 // async 写 asyncChan 触发兜底超时的累计次数
	HandchanWaitNS  uint64 // handleMessageGo 写 handchan 的累计阻塞纳秒数
	HandchanWarnCnt uint64 // handchan 阻塞超过 HandchanBlockWarnThreshold 的累计次数
	WriteErrors     uint64 // asyncGo 调 rawconn.WriteUnsafe / RequestCallbackUnsafe 返回 err 的累计次数 (BUG-3)
	LastWriteError  error  // 最近一次 write/request 错误 (BUG-3); 仅观察, 不保证全局排序
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
	// 观察性计数器, atomic 读写.
	asyncTimeouts   atomic.Uint64
	handchanWaitNS  atomic.Uint64
	handchanWarnCnt atomic.Uint64
	writeErrors     atomic.Uint64
	// lastWriteError: BUG-3 修复. asyncGo (单写) + Stats (多读) 真实并发,
	// atomic.Pointer 是单字段无锁方案; 不引入 mutex (更重, 且 Stats 不需要
	// 与其它字段同瞬一致).
	lastWriteError atomic.Pointer[error]
	wsconn         atomic.Pointer[wsconnection[T]]
	connGeneration atomic.Uint64
	onDisconnect   atomic.Pointer[func(session *Session[T], generation uint64)]
	closedSignal   atomic.Bool
}

// Stats 拍 session 当前队列占用 + 观察性计数器. 不持锁 — Len/Cap 是 chan
// builtin, atomic.Uint64.Load 也无锁; 单条调用返回的几个字段不严格一致,
// 但用作监控足够.
func (s *Session[T]) Stats() SessionStats {
	if s == nil {
		return SessionStats{}
	}
	s.locker.RLock()
	asyncChan := s.asyncChan
	handchan := s.handchan
	s.locker.RUnlock()
	stats := SessionStats{
		AsyncTimeouts:   s.asyncTimeouts.Load(),
		HandchanWaitNS:  s.handchanWaitNS.Load(),
		HandchanWarnCnt: s.handchanWarnCnt.Load(),
		WriteErrors:     s.writeErrors.Load(),
	}
	if asyncChan != nil {
		stats.AsyncChanLen = len(asyncChan)
		stats.AsyncChanCap = cap(asyncChan)
	}
	if handchan != nil {
		stats.HandchanLen = len(handchan)
		stats.HandchanCap = cap(handchan)
	}
	if errp := s.lastWriteError.Load(); errp != nil {
		stats.LastWriteError = *errp
	}
	return stats
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

func (s *Session[T]) closed() bool {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.asyncChan == nil
}

func (s *Session[T]) generation() uint64 {
	return s.connGeneration.Load()
}

func (s *Session[T]) advanceGeneration() uint64 {
	return s.connGeneration.Add(1)
}

func (s *Session[T]) setOnDisconnect(fn func(session *Session[T], generation uint64)) {
	s.onDisconnect.Store(&fn)
}

func (s *Session[T]) notifyDisconnect(generation uint64) {
	if fn := s.onDisconnect.Load(); fn != nil {
		(*fn)(s, generation)
	}
}

func createSessionWithBuffer[T any](guid string, bufferSize int) *Session[T] {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	handchan := make(chan *Packet[T], bufferSize)
	stopChan := make(chan struct{})
	asyncChan := make(chan *asyncInfo[T], bufferSize)
	s := &Session[T]{
		guid:       guid,
		bufferSize: bufferSize,
		handchan:   handchan,
		stopChan:   stopChan,
		asyncChan:  asyncChan,
	}
	// Go 1.25 WaitGroup.Go 自动 Add(1)/Done, 比显式 Add+defer Done 少一个易错点.
	s.waiter.Go(func() { s.asyncGo(asyncChan, handchan, stopChan) })
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
	Codec   Codec // 仅 asyncCommandConn 使用: 本次连接握手协商出的 codec
	Request *Message[T]

	response chan *asyncMsgErr[T]
	ready    chan error
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

func (s *Session[T]) handleMessageGo(handchan chan *Packet[T], msgchan <-chan *messagechannel[T], stopChan chan struct{}, generation uint64) {
	running := true
	disconnected := false
	notifiedDisconnect := false
	notifyDisconnect := func() {
		if disconnected && !notifiedDisconnect {
			notifiedDisconnect = true
			s.notifyDisconnect(generation)
		}
	}
	// forwardClosed 用 closedSignal 把"正在补发的 Closed 包"合并为最多一个: client 的重连
	// 循环依赖 handchan 上的 Closed 包触发重连, 但 server 端若禁用 idle cleanup 且上层永久不
	// 消费, 每个断线 generation 都阻塞补发会无界堆 goroutine。CAS 抢到信号者才阻塞投递, 其余
	// 直接返回(被合并, 不 park), 因此任一时刻至多一个 goroutine 卡在投递上。
	// 投递成功(或整体关闭走 stopChan)后必须无条件清掉信号: 否则 server 端没有"消费即复位"的
	// 钩子(只有 client.go 在读到 Closed 时复位), 一旦投递完成时 handchan 恰满, 信号会永久卡在
	// true → 该 session 后续所有断线的 Closed 都被合并丢弃(小 buffer 下首次断线即触发)。复位后
	// 后续断线可再次投递, 与上面注释"至少给恢复后的 consumer 留一个 Closed"的本意一致。
	forwardClosed := func() {
		if !s.closedSignal.CompareAndSwap(false, true) {
			return
		}
		select {
		case handchan <- &Packet[T]{Closed: true}:
		case <-stopChan:
		}
		s.closedSignal.Store(false)
	}
	defer notifyDisconnect()
	for running {
		select {
		case <-stopChan:
			running = false
		case msg, ok := <-msgchan:
			if !ok {
				disconnected = true
				running = false
				notifyDisconnect()
				forwardClosed()
			} else {
				if msg.Closed {
					disconnected = true
					notifyDisconnect()
					running = false
					forwardClosed()
					continue
				}
				packet := msg.ToPacket()
				// 快路径: handchan 有空位立即写入.
				select {
				case handchan <- packet:
					continue
				case <-stopChan:
					running = false
					continue
				default:
				}
				// 慢路径: handchan 满, 上层 consume goroutine 没及时 read.
				// 加观察性计数 + 阻塞写; 不丢帧 (协议帧丢一条可能让 wire 状态
				// 不一致). 超过阈值 log warn 提示运维: 要么上层 consume 卡了,
				// 要么 bufferSize 配小了.
				start := time.Now()
				warnTimer := time.NewTimer(HandchanBlockWarnThreshold)
				warned := false
			blockLoop:
				for {
					select {
					case handchan <- packet:
						break blockLoop
					case <-stopChan:
						running = false
						break blockLoop
					case <-warnTimer.C:
						if !warned {
							warned = true
							s.handchanWarnCnt.Add(1)
							slog.Warn("wsc handchan blocked; upstream consume slow",
								slog.String("guid", s.guid),
								slog.Duration("waited", time.Since(start)),
								slog.Int("cap", cap(handchan)))
						}
						warnTimer.Reset(HandchanBlockWarnThreshold)
					}
				}
				warnTimer.Stop()
				s.handchanWaitNS.Add(uint64(time.Since(start)))
			}
		}
	}
}

func (s *Session[T]) asyncGo(asyncChan <-chan *asyncInfo[T], handchan chan *Packet[T], stopChan chan struct{}) {
	rawconn := net.NewClient[*Message[T], *wsconnection[T]]()
	rawconn.SetBufferSize(s.bufferSize)
	var recvWaiter sync.WaitGroup
	for info := range asyncChan {
		switch info.Command {
		case asyncCommandConn: // 设置连接
			// wsConn 所有权转移到 rawconn, rawconn.Close 时会关闭连接
			wsConn := createWSConnection[T](info.Conn, s.bufferSize, info.Codec)
			generation := s.advanceGeneration()
			s.wsconn.Store(wsConn)
			// Go 1.25 WaitGroup.Go.
			recvWaiter.Go(func() { s.handleMessageGo(handchan, wsConn.channel(), stopChan, generation) })
			rawconn.ResetUnsafe(context.Background(), wsConn)
			if info.ready != nil {
				info.ready <- nil
				close(info.ready)
			}
		case asyncCommandWrite: // 发送通知
			// BUG-3: 旧实现吞掉 err, 上层 Reply/Write 拿到 nil 误以为消息已上线.
			// 现在保留 fire-and-forget 语义 (Reply/Write 早返回), 但把错误记到
			// Session.lastWriteError + writeErrors 计数 + slog.Warn 提示一次,
			// 让运维通过 Stats() 看到累计写失败.
			if err := rawconn.WriteUnsafe(context.Background(), info.Request); err != nil {
				s.writeErrors.Add(1)
				s.lastWriteError.Store(&err)
				slog.Warn("wsc Session async write failed",
					slog.String("guid", s.guid), slog.Any("err", err))
			}
		case asyncCommandRequest: // 发送请求并等待响应; 断线由 info.Response 直接通知 caller, 不在此处重试
			// RequestCallbackUnsafe 内部 async() 失败时会同步通过 info.Response
			// 通知 caller, 这里仍然记 lastWriteError 用作运维观察 (不重复通知 caller).
			if err := rawconn.RequestCallbackUnsafe(context.Background(), info.Request, info.Response); err != nil {
				s.writeErrors.Add(1)
				s.lastWriteError.Store(&err)
			}
		}
	}
	if err := rawconn.Close(); err != nil {
		if !errors.Is(err, net.ErrConnectionClosed) {
			slog.Warn("wsc Session async raw connection close failed",
				slog.String("guid", s.guid), slog.Any("err", err))
		}
	}
	s.wsconn.Store(nil)
	// stopChan 关闭（特意设计,其它地方不会关闭 stopChan）
	close(stopChan)
	recvWaiter.Wait()
	close(handchan)
}

// async 把 asyncInfo 投入 asyncChan 给 asyncGo 处理. asyncChan 满时阻塞写入.
// caller 传 ctx 无 deadline 时自动加 DefaultAsyncTimeout 兜底 — 防止上层调
// Reply/Write/Request 时使用 context.Background() (大量 sess.Reply 路径) +
// 下游 consume 卡死 + asyncChan 满 = 永久阻塞.
func (s *Session[T]) async(ctx context.Context, info *asyncInfo[T]) error {
	if s.asyncChan == nil {
		return ErrSessionClosed
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultAsyncTimeout)
		defer cancel()
	}
	select {
	case s.asyncChan <- info:
		return nil
	case <-ctx.Done():
		s.asyncTimeouts.Add(1)
		return ctx.Err()
	case <-s.stopChan:
		return ErrSessionClosed
	}
}

// reset 切换 session 底层 ws 连接 (典型场景: client 断线重连). 入队 asyncCommandConn,
// 由 asyncGo 处理. 持 WLock 是为了让连接切换命令先于后续 Write/Reply/Request
// 入队; 否则并发调用可能把业务消息排到 reset 前面, 导致写到旧连接或未连接状态.
func (s *Session[T]) reset(ctx context.Context, conn *websocket.Conn, codec Codec) error {
	info := &asyncInfo[T]{
		Command: asyncCommandConn,
		Conn:    conn,
		Codec:   codec,
		ready:   make(chan error, 1),
	}
	// 仅入队阶段持锁: 保证 conn 切换命令先于后续并发 Write/Reply/Request 入队。随后
	// 释放锁再等 ready —— asyncGo 处理本命令期间(可能被卡死的旧连接写拖到 WriteTimeout)
	// 不应阻塞其它操作。ready 缓冲为 1 故 asyncGo 发送不阻塞; stopChan 创建后不变, 锁外读安全。
	s.locker.Lock()
	err := s.async(ctx, info)
	s.locker.Unlock()
	if err != nil {
		return err
	}
	select {
	case err := <-info.ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopChan:
		return ErrSessionClosed
	}
}

// Reset 切换 session 底层 ws 连接, 使用默认 JSON codec。保留旧签名以兼容直接管理
// websocket.Conn 的调用方; 握手协商路径内部使用 reset 传入选定 codec。
//
// 注意: 标准入口 (Client.Connect / Server.OnConnection) 会在握手前对 conn 调用
// SetReadLimit(读上限持久生效于该 conn 全生命周期)。直接调用本方法的高级调用方若需
// 限制入站消息大小防 OOM, 应自行在传入的 conn 上调用 SetReadLimit。
func (s *Session[T]) Reset(ctx context.Context, conn *websocket.Conn) error {
	return s.reset(ctx, conn, defaultCodec)
}

// enqueueWrite 发送通知, id 非空表示是对请求的 Reply. Write / Reply 共用底层
// 实现, 仅 id 不同. 调用方必须在入队阶段持有 RLock.
func (s *Session[T]) enqueueWrite(ctx context.Context, id string, data T) error {
	return s.async(ctx, &asyncInfo[T]{
		Command: asyncCommandWrite,
		Request: &Message[T]{ID: id, Data: data},
	})
}

// Write 发送通知 (不等待响应). 持 RLock — 跟其它 Write/Reply/Request/Reset 并发安全,
// Close 拿 WLock 时互斥.
func (s *Session[T]) Write(ctx context.Context, data T) error {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.enqueueWrite(ctx, "", data)
}

// Reply 回复请求 (服务端响应客户端 Request 用). id 必须是收到的 Packet.ID, 跟
// 客户端 Request 的 message.ID 匹配.
func (s *Session[T]) Reply(ctx context.Context, id string, data T) error {
	s.locker.RLock()
	defer s.locker.RUnlock()
	return s.enqueueWrite(ctx, id, data)
}

func (s *Session[T]) enqueueRequest(ctx context.Context, data T) (*asyncInfo[T], error) {
	request := &asyncInfo[T]{
		Command:  asyncCommandRequest,
		Request:  &Message[T]{ID: uuid.New().String(), Data: data},
		response: make(chan *asyncMsgErr[T], 0x01),
	}
	if err := s.async(ctx, request); err != nil {
		close(request.response)
		return nil, err
	}
	return request, nil
}

func (s *Session[T]) waitRequest(ctx context.Context, request *asyncInfo[T]) (T, error) {
	var zero T
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

// Request 入队阶段持 RLock (保证 asyncChan 不被并发 close), wait 阶段不持锁
// (避免 Close 拿 Wlock 时 RLock 永远不释放 → 死锁).
func (s *Session[T]) Request(ctx context.Context, data T) (T, error) {
	s.locker.RLock()
	request, err := s.enqueueRequest(ctx, data)
	s.locker.RUnlock()
	if err != nil {
		var zero T
		return zero, err
	}
	return s.waitRequest(ctx, request)
}

// Close 关闭 session. 持 WLock 跟所有 in-flight 调用 (Reset/Write/Reply/Request
// 入队阶段持 RLock) 互斥序列化. close(asyncChan) 后 asyncGo 退出走清理路径,
// 后续 RLock 调用会看到 s.asyncChan == nil 返 ErrSessionClosed.
func (s *Session[T]) Close() error {
	s.locker.Lock()
	defer s.locker.Unlock()
	if s.asyncChan != nil {
		close(s.asyncChan)
		s.asyncChan = nil
	}
	if wsConn := s.wsconn.Load(); wsConn != nil {
		if err := wsConn.Close(context.Background()); err != nil && !errors.Is(err, net.ErrConnectionClosed) {
			slog.Warn("wsc Session close websocket failed",
				slog.String("guid", s.guid), slog.Any("err", err))
		}
	}
	s.waiter.Wait()
	return nil
}
