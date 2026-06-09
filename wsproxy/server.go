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

// ServerStats 给 oncall 看 wsproxy 池健康度.
type ServerStats struct {
	Sessions         int    // 当前池中可用 session 数
	ActiveDialouts   int32  // 正在跑 copyLoop 的 dialout 数
	TokenAuthRejects uint64 // 累计 token 校验失败次数 (Slaver 注册 / Dialout 入口)
	BadHandshake     uint64 // 累计握手帧解析失败次数 (ReadJSON 失败 / unknown Method)
}

// Server 表示一个代理服务器
type Server struct {
	locker   sync.Mutex
	sessions *list.List // 使用 list 保持顺序，FIFO 方式使用连接
	Token    string

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
		// 注册连接
		session := &Session{Id: incoming.Id, Conn: conn}
		server.locker.Lock()
		if server.closed.Load() {
			server.locker.Unlock()
			if err := conn.Close(); err != nil {
				slog.Warn("wsproxy OnConnection close late rejected slaver failed", slog.Any("err", err))
			}
			return
		}
		server.sessions.PushBack(session)
		server.locker.Unlock()
	case MethodSlaverDialout:
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

// popSession 从连接池中取出第一个会话（FIFO）
func (server *Server) popSession() *Session {
	server.locker.Lock()
	defer server.locker.Unlock()
	if server.sessions.Len() == 0 {
		return nil
	}
	front := server.sessions.Front()
	server.sessions.Remove(front)
	return front.Value.(*Session)
}

// DialContext 通过代理连接到目标地址
func (server *Server) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// 已过期的 ctx 不应消耗池中会话: popSession 取出的连接只用一次, 直接早返回。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// popSession 已经从池中移除了会话，每个连接只用一次
	session := server.popSession()
	if session == nil {
		return nil, ErrNoConnection
	}

	// 设置超时，防止阻塞。与 client.Dial 一致直接用 deadline，不经 now+Until —
	// 否则 ctx 即将到期时算出的负 timeout 会把可用连接的 deadline 设成过去 → 立即超时。
	deadline := time.Now().Add(dialHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	if err := session.Conn.SetWriteDeadline(deadline); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	if err := session.Conn.SetReadDeadline(deadline); err != nil {
		return nil, errors.Join(err, session.Close())
	}

	outgoing := &connPacket{
		Id:      session.Id,
		Method:  MethodSlaverDialout, // Dial
		Network: network,
		Address: address,
		Token:   server.Token,
	}
	if err := session.Conn.WriteJSON(outgoing); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	var incoming connPacket
	if err := session.Conn.ReadJSON(&incoming); err != nil {
		return nil, errors.Join(err, session.Close())
	}

	// 清除超时，后续由 copyLoop 管理
	if err := session.Conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	if err := session.Conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, session.Close())
	}

	if incoming.Method != MethodSlaverDialoutSuccess {
		closeErr := session.Close()
		if incoming.Error != "" {
			return nil, errors.Join(errors.New(incoming.Error), closeErr)
		}
		return nil, errors.Join(errors.New("dial failed"), closeErr)
	}

	return session, nil
}

func (server *Server) onClientDialout(ctx context.Context, conn *websocket.Conn, packet *connPacket) {
	session, err := server.DialContext(ctx, packet.Network, packet.Address)
	if err != nil {
		if writeErr := conn.WriteJSON(&connPacket{
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
	if err := conn.WriteJSON(&connPacket{
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
	server.activeDialouts.Add(1)
	defer server.activeDialouts.Add(-1)
	p := &pump{}
	if err := p.copyLoop(ctx, conn, session); err != nil {
		slog.Warn("wsproxy onClientDialout copy loop failed", slog.Any("err", err))
	}
}

// ConnectionCount 返回当前连接数
func (server *Server) ConnectionCount() int {
	server.locker.Lock()
	defer server.locker.Unlock()
	return server.sessions.Len()
}

// Stats 返回 wsproxy.Server 运行时计数. 不持锁 — atomic.Load / chan-Len 各自安全;
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
	}
}

// CloseAll 关闭所有连接并取消在飞 dialout. 返回时所有 copyLoop goroutine 已退出.
// 多次调用幂等: 首次调用 cancel(), 之后调用是 no-op (cancel 函数本身可重复调用).
func (server *Server) CloseAll() {
	server.locker.Lock()
	server.closed.Store(true)
	for server.sessions.Len() > 0 {
		front := server.sessions.Front()
		server.sessions.Remove(front)
		if err := front.Value.(*Session).Close(); err != nil {
			slog.Warn("wsproxy CloseAll session close failed", slog.Any("err", err))
		}
	}
	server.locker.Unlock()

	// 取消 ctx 让在飞 onClientDialout 退出 (copyLoop 内的 ctx-watcher 关闭两端 conn).
	server.cancel()
	// 等所有在飞 goroutine 退出后才返回, 给 caller 一个干净的 drain 承诺.
	server.activeWG.Wait()
}

// DefaultServer 全局共享代理服务器实例.
// iprogram/cmd/iclouder/web.* 直接引用此变量, 不要随意删除.
// 调用方典型: 起一个 HTTP server, Upgrade 后 go DefaultServer.OnConnection(conn).
var DefaultServer = NewServer()
