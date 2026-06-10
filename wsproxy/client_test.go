package wsproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func checkClose(t *testing.T, name string, closeFn func() error) {
	t.Helper()
	if err := closeFn(); err != nil {
		t.Logf("%s close returned: %v", name, err)
	}
}

func TestClientDialHandshakeHonorsContextDeadline(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer checkClose(t, "server websocket", conn.Close)
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewClient("ws" + server.URL[len("http"):])
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	conn, err := client.Dial(ctx, "tcp", "example.com:443")
	if conn != nil {
		checkClose(t, "client dial conn", conn.Close)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial error = %v, want context deadline", err)
	}
}

func TestCopyLoopReturnsWhenWebSocketSideCloses(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverConn := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		serverConn <- conn
	}))
	defer server.Close()

	wsConn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	var proxyConn *websocket.Conn
	select {
	case proxyConn = <-serverConn:
	case err := <-errCh:
		t.Fatalf("Upgrade failed: %v", err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for websocket")
	}

	pipeReader, pipeWriter := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- copyLoop(context.Background(), proxyConn, pipeReader)
	}()

	if err := wsConn.Close(); err != nil {
		t.Fatalf("ws close failed: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("copyLoop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("copyLoop blocked after websocket close")
	}
	checkClose(t, "pipe writer", pipeWriter.Close)
}

// TestServerCloseAllCancelsInflightDialout 回归 BUG-4:
// 旧实现 onClientDialout 用 context.Background(), CloseAll 不取消在飞 copyLoop.
// 修复后 Server.ctx 被 cancel + activeWG.Wait, CloseAll 返回时 dialout 已退出.
func TestServerCloseAllCancelsInflightDialout(t *testing.T) {
	proxyServer := NewServer()
	upgrader := websocket.Upgrader{}

	// 测试服务器作为 wsproxy.Server 的 OnConnection 入口.
	// 我们要让 OnConnection 进入 client dialout 路径, 即收到 MethodClientDialout.
	// 如果没有可用 slaver, DialContext 会立即 ErrNoConnection, 不会进入 copyLoop,
	// 也不足以触发 activeWG 的真正等待. 因此先在 server 上注册一个 fake slaver,
	// 再让 OnConnection 收到 MethodClientDialout 触发 onClientDialout
	// 走完整流程进入 copyLoop, CloseAll 时 ctx-cancel 让 copyLoop 退出.

	// 准备 fake slaver: 一个会卡住的 websocket 连接, 它的 ReadJSON 会一直等.
	slaverIncoming := make(chan struct{}, 1)
	slaverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		slaverIncoming <- struct{}{}
		// 不主动 read/write, 等连接被对端关闭.
		messageType, message, err := conn.ReadMessage()
		if err == nil {
			t.Logf("unexpected slaver message before close: type=%d bytes=%d", messageType, len(message))
		}
		checkClose(t, "slaver server websocket", conn.Close)
	}))
	defer slaverServer.Close()

	// 注册 slaver 到 proxyServer.
	slaverConn, _, err := websocket.DefaultDialer.Dial("ws"+slaverServer.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("slaver dial failed: %v", err)
	}
	defer checkClose(t, "slaver websocket", slaverConn.Close)
	// 直接入池 (跳过 OnConnection 握手), 走与注册分支相同的 registerSlaverSession.
	if !proxyServer.registerSlaverSession(&Session{Id: "slaver-99", Conn: slaverConn}) {
		t.Fatal("registerSlaverSession rejected fake slaver")
	}

	// 起 OnConnection 入口 server.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer server.Close()

	// Client 发起 dialout 让 OnConnection 启 onClientDialout goroutine.
	clientWS, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer checkClose(t, "client websocket", clientWS.Close)
	if err := clientWS.WriteJSON(&connPacket{
		Id:      "client-1",
		Method:  MethodClientDialout, // A1: client → server 的拨号请求方法
		Network: "tcp",
		Address: "127.0.0.1:1", // 拨号会失败, 但 onClientDialout 在 DialContext 失败前
	}); err != nil {
		t.Fatalf("write dialout req: %v", err)
	}

	// 等 slaver 收到 dialout 请求 - 证明 onClientDialout 已经在跑.
	select {
	case <-slaverIncoming:
	case <-time.After(2 * time.Second):
		t.Fatal("slaver never got dialout request; OnConnection not in flight")
	}

	// 此时 onClientDialout 进了 server.DialContext, 卡在等 slaver.ReadJSON.
	// CloseAll 必须 cancel ctx + wait activeWG, 让 dialout 退出.
	doneCh := make(chan struct{})
	go func() {
		proxyServer.CloseAll()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("CloseAll blocked; in-flight dialout was not cancelled")
	}
}

func TestServerDialContextCancelWhileWaitingForSlaverReply(t *testing.T) {
	proxyServer := NewServer()
	upgrader := websocket.Upgrader{}
	slaverRequest := make(chan struct{})

	slaverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer checkClose(t, "slaver server websocket", conn.Close)
		var req connPacket
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		close(slaverRequest)
		messageType, message, err := conn.ReadMessage()
		if err == nil {
			t.Logf("slaver server read unexpected message before close: type=%d bytes=%d", messageType, len(message))
		}
	}))
	defer slaverServer.Close()

	slaverConn, _, err := websocket.DefaultDialer.Dial("ws"+slaverServer.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("slaver dial failed: %v", err)
	}
	if !proxyServer.registerSlaverSession(&Session{Id: "slaver-cancel", Conn: slaverConn}) {
		t.Fatal("registerSlaverSession rejected fake slaver")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		conn, err := proxyServer.DialContext(ctx, "tcp", "example.com:443")
		if conn != nil {
			checkClose(t, "unexpected dial conn", conn.Close)
		}
		errCh <- err
	}()

	select {
	case <-slaverRequest:
	case <-time.After(time.Second):
		t.Fatal("server never sent slaver dial request")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DialContext did not return after context cancellation")
	}
}

func TestSlaverRunReturnsOnContextCancelWhileWaitingForDialRequest(t *testing.T) {
	upgrader := websocket.Upgrader{}
	registered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer checkClose(t, "server websocket", conn.Close)
		var req connPacket
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		close(registered)
		messageType, message, err := conn.ReadMessage()
		if err == nil {
			t.Logf("server read unexpected message before close: type=%d bytes=%d", messageType, len(message))
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewSlaver().Run(ctx, "ws"+server.URL[len("http"):])
	}()

	select {
	case <-registered:
	case <-time.After(time.Second):
		t.Fatal("slaver did not register")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Slaver.Run did not return after context cancellation")
	}
}

func TestServerTokenRequiredForRegistration(t *testing.T) {
	proxyServer := NewServer()
	proxyServer.Token = "secret"
	defer proxyServer.CloseAll()

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer server.Close()

	writeRegister := func(token string) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		if err := conn.WriteJSON(&connPacket{Id: "slaver-1", Method: MethodRegisterSlaver, Token: token}); err != nil {
			t.Fatalf("WriteJSON register failed: %v", err)
		}
		return conn
	}
	waitForCount := func(want int) bool {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if proxyServer.ConnectionCount() == want {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return proxyServer.ConnectionCount() == want
	}

	checkClose(t, "bad token websocket", writeRegister("wrong").Close)
	if !waitForCount(0) {
		t.Fatalf("ConnectionCount after bad token = %d, want 0", proxyServer.ConnectionCount())
	}

	conn := writeRegister("secret")
	defer checkClose(t, "good token websocket", conn.Close)
	if !waitForCount(1) {
		t.Fatalf("ConnectionCount after good token = %d, want 1", proxyServer.ConnectionCount())
	}
}

// TestClientDialThroughServerEndToEnd 回归 A1:
// 旧实现 Server.OnConnection 不处理 MethodClientDialout (Client.Dial 发送的方法),
// 请求落入 default 被当未知方法关闭 — Client.Dial 对自家 Server 必然失败.
// 修复后 Client → Server → Slaver → 目标 TCP 全链路必须互通.
func TestClientDialThroughServerEndToEnd(t *testing.T) {
	// 目标: 本地 TCP echo.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer checkClose(t, "echo listener", listener.Close)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer checkClose(t, "echo conn", conn.Close)
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			t.Logf("echo write failed: %v", err)
		}
	}()

	proxyServer := NewServer()
	defer proxyServer.CloseAll()
	upgrader := websocket.Upgrader{}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer wsServer.Close()
	wsURL := "ws" + wsServer.URL[len("http"):]

	// 真实 slaver 注册.
	slaverCtx, cancelSlaver := context.WithCancel(context.Background())
	defer cancelSlaver()
	slaver := NewSlaver()
	go func() {
		if err := slaver.Run(slaverCtx, wsURL); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("slaver run returned: %v", err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for proxyServer.ConnectionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if proxyServer.ConnectionCount() == 0 {
		t.Fatal("slaver never registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := NewClient(wsURL)
	conn, err := client.Dial(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Client.Dial through Server failed: %v", err)
	}
	defer checkClose(t, "tunnel conn", conn.Close)

	payload := []byte("hello-wsproxy")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("tunnel write failed: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("tunnel read failed: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", buf[:n], payload)
	}
}

// TestServerRemovesSlaverDisconnectedWhilePooled 回归 D1:
// 旧实现池内连接死了也滞留, 直到被 pop 才白费最长 30s 握手超时. 修复后 watcher
// 在注册侧断开时立即出池, 并通过 StaleSessions 暴露 (C2).
func TestServerRemovesSlaverDisconnectedWhilePooled(t *testing.T) {
	proxyServer := NewServer()
	defer proxyServer.CloseAll()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	if err := conn.WriteJSON(&connPacket{Id: "slaver-dead", Method: MethodRegisterSlaver}); err != nil {
		t.Fatalf("register write failed: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for proxyServer.ConnectionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if proxyServer.ConnectionCount() != 1 {
		t.Fatalf("ConnectionCount after register = %d, want 1", proxyServer.ConnectionCount())
	}

	// slaver 注册侧断开: watcher 必须立即出池.
	checkClose(t, "slaver websocket", conn.Close)
	deadline = time.Now().Add(2 * time.Second)
	for proxyServer.ConnectionCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := proxyServer.ConnectionCount(); got != 0 {
		t.Fatalf("ConnectionCount after slaver disconnect = %d, want 0 (dead entry lingering)", got)
	}
	if got := proxyServer.Stats().StaleSessions; got != 1 {
		t.Fatalf("StaleSessions = %d, want 1", got)
	}

	// 池空时 DialContext 报 ErrNoConnection 并计 NoSessionDials (C2).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := proxyServer.DialContext(ctx, "tcp", "example.com:443"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("DialContext on empty pool = %v, want ErrNoConnection", err)
	}
	if got := proxyServer.Stats().NoSessionDials; got != 1 {
		t.Fatalf("NoSessionDials = %d, want 1", got)
	}
}

// TestServerEvictsSlaverSendingUnsolicitedPacketWhilePooled 回归 D1 补丁:
// 池内 slaver 违反协议主动发包时, watcher 读到包后若不出池, 条目将失去看守
// (watcher 已退出), 之后断开无人检测, 退化回"滞留到 pop 时才暴露". 修复后
// 读到任何结果只要条目还在池内就立即驱逐并计 StaleSessions.
func TestServerEvictsSlaverSendingUnsolicitedPacketWhilePooled(t *testing.T) {
	proxyServer := NewServer()
	defer proxyServer.CloseAll()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer checkClose(t, "misbehaving slaver", conn.Close)
	if err := conn.WriteJSON(&connPacket{Id: "slaver-noisy", Method: MethodRegisterSlaver}); err != nil {
		t.Fatalf("register write failed: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for proxyServer.ConnectionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if proxyServer.ConnectionCount() != 1 {
		t.Fatalf("ConnectionCount after register = %d, want 1", proxyServer.ConnectionCount())
	}

	// 入池后主动发包 (协议违例): watcher 必须立即驱逐, 不能等到 pop 才暴露.
	if err := conn.WriteJSON(&connPacket{Id: "slaver-noisy", Method: MethodRegisterSlaver}); err != nil {
		t.Fatalf("unsolicited write failed: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for proxyServer.ConnectionCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := proxyServer.ConnectionCount(); got != 0 {
		t.Fatalf("ConnectionCount after unsolicited packet = %d, want 0 (unwatched entry lingering)", got)
	}
	if got := proxyServer.Stats().StaleSessions; got != 1 {
		t.Fatalf("StaleSessions = %d, want 1", got)
	}
}

// TestServerMaxSessionsRejectsExcessRegistration 回归 D1: 注册池上限.
func TestServerMaxSessionsRejectsExcessRegistration(t *testing.T) {
	proxyServer := NewServer()
	proxyServer.MaxSessions = 1
	defer proxyServer.CloseAll()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		proxyServer.OnConnection(conn)
	}))
	defer server.Close()

	register := func(id string) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		if err := conn.WriteJSON(&connPacket{Id: id, Method: MethodRegisterSlaver}); err != nil {
			t.Fatalf("register write failed: %v", err)
		}
		return conn
	}
	waitForCount := func(want int) bool {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if proxyServer.ConnectionCount() == want {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return proxyServer.ConnectionCount() == want
	}

	first := register("slaver-1")
	defer checkClose(t, "first slaver", first.Close)
	if !waitForCount(1) {
		t.Fatalf("ConnectionCount after first register = %d, want 1", proxyServer.ConnectionCount())
	}

	second := register("slaver-2")
	defer checkClose(t, "second slaver", second.Close)
	// 超限注册被拒: server 关闭连接, 第二个 conn 的读会很快报错.
	// deadline 只是兜底; 读到的必须是 server 主动关闭, 不能是 deadline 超时 ——
	// 否则"server 漏关被拒连接"也能靠超时蒙混过断言.
	if err := second.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("second registration unexpectedly accepted (no close from server)")
	} else if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		t.Fatalf("server did not close rejected connection within deadline: %v", err)
	}
	if got := proxyServer.ConnectionCount(); got != 1 {
		t.Fatalf("ConnectionCount after over-limit register = %d, want 1", got)
	}
}
