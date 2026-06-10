package wsproxy

import (
	"container/list"
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNoConnection = errors.New("no available connection")

// DefaultMaxSessions 是注册池的默认容量上限 (Server.MaxSessions<=0 时生效).
// 每个池条目占一条 TCP + 一个 watcher goroutine, 上限防止异常 slaver 无界堆积.
const DefaultMaxSessions = 1024

// ServerStats 给 oncall 看 wsproxy 池健康度.
type ServerStats struct {
	Sessions         int    // 当前池中可用 session 数
	ActiveDialouts   int32  // 正在跑 copyLoop 的 dialout 数
	TokenAuthRejects uint64 // 累计 token 校验失败次数 (Slaver 注册 / Dialout 入口)
	BadHandshake     uint64 // 累计握手帧解析失败次数 (ReadJSON 失败 / unknown Method)
	NoSessionDials   uint64 // DialContext 时池空 (ErrNoConnection) 的累计次数
	StaleSessions    uint64 // 检测到的死 slaver 连接累计次数 (池内断开 / 取出后拨号握手失败); 与 NoSessionDials 一起区分"池空"与"池里是僵尸"
}

// slaverRead 是 watchSlaver 交付的一次读结果: 池内挂起的 ReadJSON 在 slaver 断开时
// 立即返回错误 (死连接检测), 在 dialout 握手时返回 slaver 的首条响应 (读权交接).
type slaverRead struct {
	packet connPacket
	err    error
}

// slaverEntry 是注册池条目. watcher goroutine 在入池时启动并挂起在 ReadJSON 上:
//   - 条目仍在池内时读到任何结果 (读错误 = slaver 断开; 读到包 = 违反协议主动发包)
//     → 立即出池 + 关闭 + 计 StaleSessions (D1: 死/坏连接不滞留到使用时才暴露);
//   - 已被 pop (dialout 握手中) → 读到的是 slaver 的响应, 经 reply 交付后退出
//     (gorilla conn 不支持并发读, 读权始终归 watcher 直到交接完成).
type slaverEntry struct {
	session *Session
	reply   chan slaverRead // cap 1; watcher 交付一次后退出, 投递永不阻塞
	done    chan struct{}   // watcher 退出时 close; CloseAll 以此 join 池内 watcher
	popped  bool            // 由 server.locker 保护: 已出池 (popSession/CloseAll), watcher 不再负责回收与计数
}

// Server 表示一个代理服务器
type Server struct {
	locker   sync.Mutex
	sessions *list.List // 使用 list 保持顺序，FIFO 方式使用连接; 元素类型 *slaverEntry
	Token    string

	// MaxSessions 限制注册池容量, <=0 时用 DefaultMaxSessions. 超限的注册被拒绝
	// (关闭连接, slaver 侧按既有 backoff 重试), 防止死注册无界堆积.
	MaxSessions int

	// 生命周期管理: ctx 用于让 in-flight onClientDialout / copyLoop 在 CloseAll
	// 时被 cancel. activeWG 等待所有在飞 dialout goroutine 退出.
	// ctx/cancel 一次性创建, 之后只读 — 不是同步原语, 是 cancel 协议.
	// activeWG 必须加: CloseAll 要 wait 所有 onClientDialout 退出, sync.WaitGroup
	// 是经典语义无替代.
	ctx            context.Context
	cancel         context.CancelFunc
	activeWG       sync.WaitGroup
	activeDialouts atomic.Int32 // 正在执行 copyLoop 的 dialout 数; 多 goroutine 增减, atomic 最轻.
	tokenRejects   atomic.Uint64
	badHandshakes  atomic.Uint64
	noSessionDials atomic.Uint64
	staleSessions  atomic.Uint64

	closed atomic.Bool // CloseAll 触发后置 true, 阻止后续 OnConnection 接新 session
}

// NewServer 创建新的代理服务器
func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		sessions: list.New(),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// OnConnection 处理新连接
func (server *Server) OnConnection(conn *websocket.Conn) {
	conn.SetReadLimit(MaxMessageSize)
	if server.closed.Load() {
		// CloseAll 已调用, 不接新连接.
		if err := conn.Close(); err != nil {
			slog.Warn("wsproxy OnConnection close rejected connection failed", slog.Any("err", err))
		}
		return
	}
	// 设置读取超时，防止恶意连接
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Warn("wsproxy OnConnection close after deadline setup failure failed",
				slog.Any("deadline_err", err), slog.Any("close_err", closeErr))
		}
		return
	}
	var incoming connPacket
	if err := conn.ReadJSON(&incoming); err != nil {
		server.badHandshakes.Add(1)
		slog.Warn("wsproxy OnConnection ReadJSON failed",
			slog.String("remote", conn.RemoteAddr().String()),
			slog.Any("err", err))
		if closeErr := conn.Close(); closeErr != nil {
			slog.Warn("wsproxy OnConnection close after bad handshake failed", slog.Any("err", closeErr))
		}
		return
	}
	if server.Token != "" && subtle.ConstantTimeCompare([]byte(incoming.Token), []byte(server.Token)) != 1 {
		// 常数时间比较防止远端攻击者按 Token 字符位 timing 爆破.
		server.tokenRejects.Add(1)
		slog.Warn("wsproxy OnConnection token mismatch",
			slog.String("remote", conn.RemoteAddr().String()),
			slog.Int("method", int(incoming.Method)))
		if err := conn.Close(); err != nil {
			slog.Warn("wsproxy OnConnection close after token mismatch failed", slog.Any("err", err))
		}
		return
	}
	// 清除读取超时
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			slog.Warn("wsproxy OnConnection close after deadline clear failure failed",
				slog.Any("deadline_err", err), slog.Any("close_err", closeErr))
		}
		return
	}
	switch incoming.Method {
	case MethodRegisterSlaver:
		// 注册连接 (入池 + 启动 watcher); 池满或服务器已关时拒绝.
		if !server.registerSlaverSession(&Session{Id: incoming.Id, Conn: conn}) {
			if err := conn.Close(); err != nil {
				slog.Warn("wsproxy OnConnection close rejected slaver failed", slog.Any("err", err))
			}
			return
		}
	case MethodClientDialout:
		// A1 修复: client → server 的拨号请求方法是 MethodClientDialout (Client.Dial
		// 发送的就是它); 旧实现错写成 MethodSlaverDialout (那是 server → slaver 的
		// 指令方向), 导致 Client.Dial 对自家 Server 必然落入 default 被关闭.
		// 用 server.ctx 取代 context.Background(): CloseAll 取消所有在飞 dialout.
		// activeWG 让 CloseAll 能等到所有 copyLoop 退出 (drain 语义).
		server.locker.Lock()
		if server.closed.Load() {
			server.locker.Unlock()
			if err := conn.Close(); err != nil {
				slog.Warn("wsproxy OnConnection close late rejected dialout failed", slog.Any("err", err))
			}
			return
		}
		server.activeWG.Go(func() {
			server.onClientDialout(server.ctx, conn, &incoming)
		})
		server.locker.Unlock()
	default:
		server.badHandshakes.Add(1)
		slog.Warn("wsproxy OnConnection unknown method",
			slog.String("remote", conn.RemoteAddr().String()),
			slog.Int("method", int(incoming.Method)))
		if err := conn.Close(); err != nil {
			slog.Warn("wsproxy OnConnection close unknown method failed", slog.Any("err", err))
		}
	}
}

// registerSlaverSession 把注册成功的 slaver 连接入池并启动 watcher.
// OnConnection 注册分支与测试共用. 返回 false 表示服务器已关闭或池已满 (调用方负责关连接).
func (server *Server) registerSlaverSession(session *Session) bool {
	limit := server.MaxSessions
	if limit <= 0 {
		limit = DefaultMaxSessions
	}
	server.locker.Lock()
	if server.closed.Load() {
		server.locker.Unlock()
		return false
	}
	if server.sessions.Len() >= limit {
		server.locker.Unlock()
		slog.Warn("wsproxy slaver pool full; rejecting registration",
			slog.String("session", session.Id),
			slog.String("remote", session.Conn.RemoteAddr().String()),
			slog.Int("limit", limit))
		return false
	}
	entry := &slaverEntry{session: session, reply: make(chan slaverRead, 1), done: make(chan struct{})}
	elem := server.sessions.PushBack(entry)
	server.locker.Unlock()
	go server.watchSlaver(entry, elem)
	return true
}

// watchSlaver 挂起在池内连接的 ReadJSON 上, 读到一次结果即交付并退出:
//   - 条目仍在池内: 读错误 (slaver 在注册侧断开) 和读到包 (违反协议主动发包)
//     一律立即出池 + 关闭 + 计 StaleSessions —— 后者若留在池中将失去看守,
//     之后断开无人检测, 退化回"滞留到 pop 时才暴露";
//   - 已被 pop (dialout 握手中): 读到的是 slaver 响应, 经 reply 交接给
//     DialContext, 回收与计数归 DialContext (closeWithContextError).
//
// list.Remove 对已移除元素是 no-op, popped 标记保证出池责任(回收+计数)只归一方.
func (server *Server) watchSlaver(entry *slaverEntry, elem *list.Element) {
	defer close(entry.done)
	var packet connPacket
	err := entry.session.Conn.ReadJSON(&packet)
	entry.reply <- slaverRead{packet: packet, err: err}
	server.locker.Lock()
	inPool := !entry.popped
	if inPool {
		entry.popped = true
		server.sessions.Remove(elem)
	}
	server.locker.Unlock()
	if !inPool {
		return
	}
	server.staleSessions.Add(1)
	if err != nil {
		slog.Warn("wsproxy slaver disconnected while pooled",
			slog.String("session", entry.session.Id), slog.Any("err", err))
	} else {
		slog.Warn("wsproxy slaver sent unsolicited packet while pooled; evicting",
			slog.String("session", entry.session.Id), slog.Int("method", int(packet.Method)))
	}
	if closeErr := entry.session.Close(); closeErr != nil {
		slog.Debug("wsproxy watchSlaver close evicted slaver failed", slog.Any("err", closeErr))
	}
}

// popSession 从连接池中取出第一个可用会话（FIFO）, 跳过 watcher 已判死的僵尸条目.
func (server *Server) popSession() *slaverEntry {
	server.locker.Lock()
	defer server.locker.Unlock()
	for server.sessions.Len() > 0 {
		front := server.sessions.Front()
		server.sessions.Remove(front)
		entry := front.Value.(*slaverEntry)
		entry.popped = true
		select {
		case read := <-entry.reply:
			// watcher 在池内就交付了结果: 要么读错误 (死连接), 要么 slaver 违反
			// 协议主动发包. 两者都按僵尸丢弃, 继续取下一个.
			server.staleSessions.Add(1)
			slog.Warn("wsproxy popSession discarded stale slaver",
				slog.String("session", entry.session.Id), slog.Any("err", read.err))
			if err := entry.session.Close(); err != nil {
				slog.Debug("wsproxy popSession close stale slaver failed", slog.Any("err", err))
			}
		default:
			return entry
		}
	}
	return nil
}

// DialContext 通过代理连接到目标地址
func (server *Server) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// 已过期的 ctx 不应消耗池中会话: popSession 取出的连接只用一次, 直接早返回。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// popSession 已经从池中移除了会话，每个连接只用一次
	entry := server.popSession()
	if entry == nil {
		server.noSessionDials.Add(1)
		return nil, ErrNoConnection
	}
	session := entry.session
	stopContextClose := closeWebSocketOnContextDone(ctx, session.Conn)
	defer stopContextClose()
	closeWithContextError := func(err error) error {
		closeErr := session.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, closeErr)
		}
		// 非 ctx 取消的拨号握手失败 = pop 出的连接已不可用, 计入 StaleSessions
		// 让 oncall 区分"池空"与"池里是僵尸".
		server.staleSessions.Add(1)
		slog.Warn("wsproxy DialContext stale slaver session",
			slog.String("session", session.Id), slog.Any("err", err))
		return errors.Join(err, closeErr)
	}

	// 设置写超时，防止阻塞。与 client.Dial 一致直接用 deadline，不经 now+Until —
	// 否则 ctx 即将到期时算出的负 timeout 会把可用连接的 deadline 设成过去 → 立即超时。
	deadline := time.Now().Add(dialHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	if err := session.Conn.SetWriteDeadline(deadline); err != nil {
		return nil, closeWithContextError(err)
	}
	// 握手读超时不用 SetReadDeadline: gorilla 把它归类为 read 方法 (要求与其它
	// read 方法串行), 而 watcher 的 ReadJSON 此刻在飞 —— v1.5.3 实现是 net.Conn
	// 透传所以碰巧安全, 但那是实现细节不是契约. Close 是 gorilla 明文允许与读写
	// 并发的方法, 到期关连接同样让在飞 ReadJSON 立即出错返回. deadline 已过期时
	// AfterFunc 立即触发, 与 SetReadDeadline 的"过去时刻 → 立即超时"语义一致.
	handshakeTimer := time.AfterFunc(time.Until(deadline), func() {
		if err := session.Conn.Close(); err != nil {
			slog.Debug("wsproxy DialContext handshake deadline close failed", slog.Any("err", err))
		}
	})
	defer handshakeTimer.Stop()

	outgoing := &connPacket{
		Id:      session.Id,
		Method:  MethodSlaverDialout, // server → slaver 拨号指令
		Network: network,
		Address: address,
		Token:   server.Token,
	}
	if err := session.Conn.WriteJSON(outgoing); err != nil {
		return nil, closeWithContextError(err)
	}
	// slaver 的响应由 watchSlaver 读取并经 reply 交付 (读权交接, 见 slaverEntry).
	// 阻塞有界: handshakeTimer 到期关连接, ctx 取消时 watcher 的 ReadJSON 也会因连接被关而返回.
	read := <-entry.reply
	if !handshakeTimer.Stop() {
		// timer 已触发 (连接已被关闭): 即使响应恰好在 deadline 边缘到达, 连接也
		// 已不可用, 统一按握手超时处理, 避免把已关闭的 conn 交还给调用方.
		read.err = errors.Join(errors.New("dial handshake deadline exceeded"), read.err)
	}
	if read.err != nil {
		return nil, closeWithContextError(read.err)
	}
	incoming := read.packet

	// 清除写超时，后续由 copyLoop 管理. 读侧从未设 deadline (见 handshakeTimer), 无需清除.
	if err := session.Conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, closeWithContextError(err)
	}

	if incoming.Method != MethodSlaverDialoutSuccess {
		// slaver 在线但拨号失败: 不是僵尸连接, 不计 StaleSessions.
		closeErr := session.Close()
		if incoming.Error != "" {
			return nil, errors.Join(errors.New(incoming.Error), closeErr)
		}
		return nil, errors.Join(errors.New("dial failed"), closeErr)
	}
	// 先停掉 ctx watcher 并 join(stopContextClose 内部 <-stopped 会等 watcher 退出),
	// 再复查 ctx。否则 watcher 可能在本函数把 session 交还给调用方之后才触发 Close,
	// 迟到关闭已返回的连接, 让调用方拿到 (已关闭conn, nil)。与 proxy.go 目的相同但机制不同:
	// proxy.go 用 context.AfterFunc(stop 不 join) 故靠末尾复查 ctx 兜底, 此处 helper 自带 join。
	stopContextClose()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, session.Close())
	}

	return session, nil
}

func (server *Server) onClientDialout(ctx context.Context, conn *websocket.Conn, packet *connPacket) {
	stopContextClose := closeWebSocketOnContextDone(ctx, conn)
	defer stopContextClose()
	writeHandshake := func(packet *connPacket) error {
		deadline := time.Now().Add(dialHandshakeTimeout)
		if d, ok := ctx.Deadline(); ok {
			deadline = d
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		if err := conn.WriteJSON(packet); err != nil {
			return err
		}
		return conn.SetWriteDeadline(time.Time{})
	}
	session, err := server.DialContext(ctx, packet.Network, packet.Address)
	if err != nil {
		if writeErr := writeHandshake(&connPacket{
			Id:     packet.Id,
			Method: MethodClientDialoutError, // 连接错误
			Error:  err.Error(),
		}); writeErr != nil {
			slog.Warn("wsproxy onClientDialout write error response failed",
				slog.Any("dial_err", err), slog.Any("write_err", writeErr))
		}
		if closeErr := conn.Close(); closeErr != nil {
			slog.Warn("wsproxy onClientDialout close client after dial failure failed", slog.Any("err", closeErr))
		}
		return
	}
	// 发送连接成功响应
	if err := writeHandshake(&connPacket{
		Id:     packet.Id,
		Method: MethodClientDialoutSuccess, // 连接成功
	}); err != nil {
		// WebSocket 写入失败，关闭两端连接
		if closeErr := errors.Join(conn.Close(), session.Close()); closeErr != nil {
			slog.Warn("wsproxy onClientDialout close after success response failure failed",
				slog.Any("write_err", err), slog.Any("close_err", closeErr))
		}
		return
	}
	stopContextClose()
	if ctx.Err() != nil {
		if closeErr := errors.Join(conn.Close(), session.Close()); closeErr != nil {
			slog.Warn("wsproxy onClientDialout close after context cancellation failed", slog.Any("err", closeErr))
		}
		return
	}
	server.activeDialouts.Add(1)
	defer server.activeDialouts.Add(-1)
	if err := copyLoop(ctx, conn, session); err != nil {
		slog.Warn("wsproxy onClientDialout copy loop failed", slog.Any("err", err))
	}
}

// ConnectionCount 返回当前连接数
func (server *Server) ConnectionCount() int {
	server.locker.Lock()
	defer server.locker.Unlock()
	return server.sessions.Len()
}

// Stats 返回 wsproxy.Server 运行时计数. sessions 长度短暂持锁读取, 计数器用 atomic.Load;
// 几个字段之间不保证同瞬一致性, 监控用足够.
func (server *Server) Stats() ServerStats {
	server.locker.Lock()
	count := server.sessions.Len()
	server.locker.Unlock()
	return ServerStats{
		Sessions:         count,
		ActiveDialouts:   server.activeDialouts.Load(),
		TokenAuthRejects: server.tokenRejects.Load(),
		BadHandshake:     server.badHandshakes.Load(),
		NoSessionDials:   server.noSessionDials.Load(),
		StaleSessions:    server.staleSessions.Load(),
	}
}

// CloseAll 关闭所有连接并取消在飞 dialout. 返回时所有 copyLoop goroutine 与
// 池内 watcher goroutine 已退出; 被 pop 走的 watcher 属于在飞 dialout 握手,
// 由各自的 handshakeTimer 兜底退出, 不在此处等待 (避免给 CloseAll 引入最长
// 一次握手超时的停机阻塞).
// 多次调用幂等: 首次调用 cancel(), 之后调用是 no-op (cancel 函数本身可重复调用).
func (server *Server) CloseAll() {
	server.locker.Lock()
	server.closed.Store(true)
	var drained []*slaverEntry
	for server.sessions.Len() > 0 {
		front := server.sessions.Front()
		server.sessions.Remove(front)
		entry := front.Value.(*slaverEntry)
		// 标记已出池: watcher 随后因连接被关而读错误时不再自行回收/计 StaleSessions
		// (停机关闭不是僵尸).
		entry.popped = true
		if err := entry.session.Close(); err != nil {
			slog.Warn("wsproxy CloseAll session close failed", slog.Any("err", err))
		}
		drained = append(drained, entry)
	}
	server.locker.Unlock()

	// 取消 ctx 让在飞 onClientDialout 退出 (copyLoop 内的 ctx-watcher 关闭两端 conn).
	server.cancel()
	// 等所有在飞 goroutine 退出后才返回, 给 caller 一个干净的 drain 承诺.
	server.activeWG.Wait()
	// join 池内 watcher: 它们的连接已在上面关闭, ReadJSON 立即出错返回, 等待有界;
	// 必须在释放 locker 之后等 —— watcher 退出路径要取 server.locker.
	for _, entry := range drained {
		<-entry.done
	}
}

// DefaultServer 全局共享代理服务器实例.
// iprogram/cmd/iclouder/web.* 直接引用此变量, 不要随意删除.
// 调用方典型: 起一个 HTTP server, Upgrade 后 go DefaultServer.OnConnection(conn).
var DefaultServer = NewServer()
