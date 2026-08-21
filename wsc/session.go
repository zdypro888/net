package wsc

import (
	"context"
	"errors"
	"log/slog"
	stdnet "net"
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

// Packet 表示会话收到的数据及其请求 ID、连接代次和关闭状态。
type Packet[T any] struct {
	ID         string // 请求 ID (用于回复)
	Data       T
	Generation uint64 // 收到该包的底层连接代次
	Closed     bool
}

type connectionArgsState struct {
	value     any
	pending   int
	receiving bool
}

// SessionStats 给运维查 wsc 健康度.
type SessionStats struct {
	AsyncChanLen    int    // 当前 asyncChan 队列长度 (高 = 上层 reply/write 堆积)
	AsyncChanCap    int    // asyncChan 容量
	HandchanLen     int    // 当前 handchan 队列长度 (高 = 上层 consume 慢)
	HandchanCap     int    // handchan 容量
	AsyncTimeouts   uint64 // async 写 asyncChan 等待超时 (兜底或 caller deadline 到期) 的累计次数; 不含 caller 主动取消
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
	stopOnce   sync.Once
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
	// onClose: D4 修复. server 创建 session 时注册"从 sessions 表摘除自己", 让调用方
	// 直接 session.Close() (不经 server.RemoveSession) 时表项也被移除 —— 否则在
	// WithSessionIdleTimeout<=0 (禁用空闲清理) 配置下僵尸条目永不回收.
	onClose          atomic.Pointer[func(session *Session[T])]
	closedSignal     atomic.Bool
	closedGeneration atomic.Uint64
	closedDelivered  atomic.Uint64
	connectionMu     sync.Mutex
	connectionArgs   map[uint64]*connectionArgsState
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

// Handle 返回会话的入站数据通道；会话结束时该通道会关闭。
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

func (s *Session[T]) setConnectionArgs(generation uint64, args any) {
	if args == nil {
		return
	}
	s.connectionMu.Lock()
	if s.connectionArgs == nil {
		s.connectionArgs = make(map[uint64]*connectionArgsState)
	}
	s.connectionArgs[generation] = &connectionArgsState{value: args, receiving: true}
	s.connectionMu.Unlock()
}

func (s *Session[T]) retainConnectionArgs(generation uint64) {
	s.connectionMu.Lock()
	if state := s.connectionArgs[generation]; state != nil {
		state.pending++
	}
	s.connectionMu.Unlock()
}

func (s *Session[T]) releaseConnectionArgs(generation uint64) any {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	state := s.connectionArgs[generation]
	if state == nil {
		return nil
	}
	value := state.value
	if state.pending > 0 {
		state.pending--
	}
	if !state.receiving && state.pending == 0 {
		delete(s.connectionArgs, generation)
	}
	return value
}

func (s *Session[T]) finishConnectionArgs(generation uint64) {
	s.connectionMu.Lock()
	if state := s.connectionArgs[generation]; state != nil {
		state.receiving = false
		if state.pending == 0 {
			delete(s.connectionArgs, generation)
		}
	}
	s.connectionMu.Unlock()
}

// TakeConnectionArgs 返回指定连接代次绑定的参数，并标记一个已收到的数据包已领取。
// 每个非关闭 Packet 应调用一次；最后一个包领取且接收协程退出后，对应参数会被释放。
func (s *Session[T]) TakeConnectionArgs(generation uint64) any {
	return s.releaseConnectionArgs(generation)
}

func (s *Session[T]) setOnDisconnect(fn func(session *Session[T], generation uint64)) {
	s.onDisconnect.Store(&fn)
}

func (s *Session[T]) setOnClose(fn func(session *Session[T])) {
	s.onClose.Store(&fn)
}

func (s *Session[T]) notifyClose() {
	if fn := s.onClose.Load(); fn != nil {
		(*fn)(s)
	}
}

func (s *Session[T]) notifyDisconnect(generation uint64) {
	if fn := s.onDisconnect.Load(); fn != nil {
		(*fn)(s, generation)
	}
}

func (s *Session[T]) signalStop() {
	s.stopOnce.Do(func() { close(s.stopChan) })
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
	Command    asyncCommand
	Conn       *websocket.Conn
	Codec      Codec // 仅 asyncCommandConn 使用: 本次连接握手协商出的 codec
	Args       any   // 仅 asyncCommandConn 使用: 随该连接收到的 Packet 传递
	Generation uint64
	Request    *Message[T]
	Context    context.Context

	response     chan *asyncMsgErr[T]
	responseOnce sync.Once
	ready        chan error
}

// Response 处理响应数据或错误
func (info *asyncInfo[T]) Response(msg *Message[T], err error) {
	info.responseOnce.Do(func() {
		if info.response != nil {
			select {
			case info.response <- &asyncMsgErr[T]{Message: msg, Error: err}:
			default:
			}
			close(info.response)
		}
	})
}

func (s *Session[T]) handleMessageGo(handchan chan *Packet[T], msgchan <-chan *messagechannel[T], stopChan chan struct{}, generation uint64, hasConnectionArgs bool) {
	running := true
	disconnected := false
	notifiedDisconnect := false
	notifyDisconnect := func() {
		if disconnected && !notifiedDisconnect {
			notifiedDisconnect = true
			s.notifyDisconnect(generation)
		}
	}
	// forwardClosed 保证任一时刻至多一个 goroutine 阻塞投递 Closed。若阻塞期间又有连接断开，
	// closedGeneration 会保留最新代次，投递者恢复后继续补发；中间代次可合并，因为最新代次的
	// Closed 同时代表更早连接均已断开。代次加一编码，使尚未建连时的 generation=0 也可记录。
	forwardClosed := func() {
		encodedGeneration := generation + 1
		for {
			if encodedGeneration <= s.closedDelivered.Load() {
				return
			}
			pending := s.closedGeneration.Load()
			if encodedGeneration <= pending || s.closedGeneration.CompareAndSwap(pending, encodedGeneration) {
				break
			}
		}
		if !s.closedSignal.CompareAndSwap(false, true) {
			return
		}
		for {
			pending := s.closedGeneration.Load()
			delivered := s.closedDelivered.Load()
			if pending <= delivered {
				s.closedSignal.Store(false)
				if s.closedGeneration.Load() <= s.closedDelivered.Load() || !s.closedSignal.CompareAndSwap(false, true) {
					return
				}
				continue
			}
			select {
			case handchan <- &Packet[T]{Generation: pending - 1, Closed: true}:
			case <-stopChan:
				s.closedSignal.Store(false)
				return
			}
			for {
				delivered = s.closedDelivered.Load()
				if pending <= delivered || s.closedDelivered.CompareAndSwap(delivered, pending) {
					break
				}
			}
		}
	}
	defer notifyDisconnect()
	if hasConnectionArgs {
		defer s.finishConnectionArgs(generation)
	}
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
				packet.Generation = generation
				if hasConnectionArgs {
					s.retainConnectionArgs(generation)
				}
				if !s.forwardPacket(handchan, packet, stopChan) {
					if hasConnectionArgs {
						s.releaseConnectionArgs(generation)
					}
					running = false
				}
			}
		}
	}
}

func (s *Session[T]) forwardPacket(handchan chan<- *Packet[T], packet *Packet[T], stopChan <-chan struct{}) bool {
	select {
	case handchan <- packet:
		return true
	case <-stopChan:
		return false
	default:
	}

	// handchan 满时必须施加背压，丢失协议帧会造成连接状态不一致。
	start := time.Now()
	warnTimer := time.NewTimer(HandchanBlockWarnThreshold)
	defer warnTimer.Stop()
	defer func() { s.handchanWaitNS.Add(uint64(time.Since(start))) }()
	warn := warnTimer.C
	for {
		select {
		case handchan <- packet:
			return true
		case <-stopChan:
			return false
		case <-warn:
			warn = nil
			s.handchanWarnCnt.Add(1)
			slog.Warn("wsc handchan blocked; upstream consume slow",
				slog.String("guid", s.guid),
				slog.Duration("waited", time.Since(start)),
				slog.Int("cap", cap(handchan)))
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
			s.setConnectionArgs(generation, info.Args)
			hasConnectionArgs := info.Args != nil
			s.wsconn.Store(wsConn)
			// Go 1.25 WaitGroup.Go.
			recvWaiter.Go(func() {
				s.handleMessageGo(handchan, wsConn.channel(), stopChan, generation, hasConnectionArgs)
			})
			rawconn.ResetUnsafe(context.Background(), wsConn)
			if info.ready != nil {
				info.ready <- nil
				close(info.ready)
			}
		case asyncCommandWrite: // 发送通知
			if info.Generation != 0 && info.Generation != s.generation() {
				if info.ready != nil {
					info.ready <- net.ErrConnectionClosed
					close(info.ready)
				}
				continue
			}
			if info.Context != nil && info.Context.Err() != nil {
				if info.ready != nil {
					info.ready <- info.Context.Err()
					close(info.ready)
				}
				continue
			}
			// BUG-3: 旧实现吞掉 err, 上层 Reply/Write 拿到 nil 误以为消息已上线.
			// 现在保留 fire-and-forget 语义 (Reply/Write 早返回), 但把错误记到
			// Session.lastWriteError + writeErrors 计数 + slog.Warn 提示一次,
			// 让运维通过 Stats() 看到累计写失败.
			if err := rawconn.WriteUnsafe(context.Background(), info.Request); err != nil {
				s.writeErrors.Add(1)
				s.lastWriteError.Store(&err)
				slog.Warn("wsc Session async write failed",
					slog.String("guid", s.guid), slog.Any("err", err))
				if info.ready != nil {
					info.ready <- err
					close(info.ready)
				}
			} else if info.ready != nil {
				info.ready <- nil
				close(info.ready)
			}
		case asyncCommandRequest: // 发送请求并等待响应; 断线由 info.Response 直接通知 caller, 不在此处重试
			// RequestCallbackUnsafe 内部 async() 失败时会同步通过 info.Response
			// 通知 caller, 这里仍然记 lastWriteError 用作运维观察 (不重复通知 caller).
			requestCtx := info.Context
			if requestCtx == nil {
				requestCtx = context.Background()
			}
			if err := rawconn.RequestCallbackUnsafe(requestCtx, info.Request, info.Response); err != nil {
				s.writeErrors.Add(1)
				s.lastWriteError.Store(&err)
			}
		}
	}
	if err := rawconn.Close(); err != nil {
		if !errors.Is(err, net.ErrConnectionClosed) && !errors.Is(err, stdnet.ErrClosed) {
			slog.Warn("wsc Session async raw connection close failed",
				slog.String("guid", s.guid), slog.Any("err", err))
		}
	}
	s.wsconn.Store(nil)
	s.signalStop()
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
		// 与 net.Client.async 一致: 只把真正的超时 (内部兜底或 caller deadline 到期)
		// 计入 asyncTimeouts; caller 主动取消不是拥塞信号.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.asyncTimeouts.Add(1)
		}
		return ctx.Err()
	case <-s.stopChan:
		return ErrSessionClosed
	}
}

// reset 切换 session 底层 ws 连接 (典型场景: client 断线重连). 入队 asyncCommandConn,
// 由 asyncGo 处理.
func (s *Session[T]) reset(ctx context.Context, conn *websocket.Conn, codec Codec, args any) error {
	info := &asyncInfo[T]{
		Command: asyncCommandConn,
		Conn:    conn,
		Codec:   codec,
		Args:    args,
		ready:   make(chan error, 1),
	}
	// 入队阶段持 RLock (与 Write/Reply/Request 同款): 锁的职责只是 asyncChan 生命周期
	// 保护 (防 Close 并发 close(asyncChan) 导致 send-on-closed panic), 不是命令排序 ——
	// 与 reset 并发的业务消息本就无确定顺序, 排在 conn 切换命令前后都是合法时序; reset
	// 返回后的消息则由 ready 等待保证落在新连接上。改 RLock 修 D2: asyncChan 满时 async
	// 可阻塞至 DefaultAsyncTimeout(30s), 旧实现此间持 WLock 会把所有并发 Write/Reply/
	// Request (RLock) 挡住 30s。等 ready 阶段不持锁 —— asyncGo 处理本命令期间 (可能被
	// 卡死的旧连接写拖到 WriteTimeout) 不应阻塞其它操作。ready 缓冲为 1 故 asyncGo 发送
	// 不阻塞; stopChan 创建后不变, 锁外读安全。
	s.locker.RLock()
	err := s.async(ctx, info)
	s.locker.RUnlock()
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
	return s.reset(ctx, conn, defaultCodec, nil)
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

// ReplyGeneration 仅在请求来源连接仍为当前代次时回复。代次校验在 asyncGo
// 实际写入前执行，确保已经入队的 Reset 不会把旧请求 ID 带到新连接。
func (s *Session[T]) ReplyGeneration(ctx context.Context, generation uint64, id string, data T) error {
	info := &asyncInfo[T]{
		Command:    asyncCommandWrite,
		Generation: generation,
		Request:    &Message[T]{ID: id, Data: data},
		Context:    ctx,
		ready:      make(chan error, 1),
	}
	s.locker.RLock()
	err := s.async(ctx, info)
	s.locker.RUnlock()
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

// Generation 返回当前底层连接代次。
func (s *Session[T]) Generation() uint64 {
	return s.generation()
}

func (s *Session[T]) enqueueRequest(ctx context.Context, data T) (*asyncInfo[T], error) {
	request := &asyncInfo[T]{
		Command:  asyncCommandRequest,
		Request:  &Message[T]{ID: uuid.New().String(), Data: data},
		Context:  ctx,
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
// 退出前触发 onClose 回调 (server 注册的表项摘除, 幂等), 保证直接 Close 的 session
// 不会作为僵尸条目滞留在 server.sessions 中. 锁序安全: 回调内只取 server.locker,
// 而所有持 server.locker 的路径都不会反向获取 s.locker.
func (s *Session[T]) Close() error {
	s.locker.Lock()
	defer s.locker.Unlock()
	if s.asyncChan != nil {
		close(s.asyncChan)
		s.asyncChan = nil
	}
	// 先解除所有接收转发对 handchan 的阻塞，再等待 asyncGo。否则业务消费者
	// 在调用 Close 时若 handchan 已满，会与 recvWaiter 形成环形等待。
	s.signalStop()
	if wsConn := s.wsconn.Load(); wsConn != nil {
		if err := wsConn.Close(context.Background()); err != nil && !errors.Is(err, net.ErrConnectionClosed) {
			slog.Warn("wsc Session close websocket failed",
				slog.String("guid", s.guid), slog.Any("err", err))
		}
	}
	s.waiter.Wait()
	s.notifyClose()
	return nil
}
