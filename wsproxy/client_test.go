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

func TestPumpCopyLoopReturnsWhenWebSocketSideCloses(t *testing.T) {
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
		done <- (&pump{}).copyLoop(context.Background(), proxyConn, pipeReader)
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
	// 我们要让 OnConnection 进入 dialout 路径, 即收到 MethodSlaverDialout.
	// 因为没有可注册的 slaver, DialContext 会立即 ErrNoConnection 退出 copyLoop.
	// 这条路径不足以触发 activeWG 的真正等待. 改为在 server 上预先 push 一个 fake
	// slaver, 然后让 OnConnection 收到 MethodSlaverDialout 触发 onClientDialout
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
	// 直接 push 到 sessions 池, 跳过 OnConnection 握手.
	proxyServer.locker.Lock()
	proxyServer.sessions.PushBack(&Session{Id: "slaver-99", Conn: slaverConn})
	proxyServer.locker.Unlock()

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
		Method:  MethodSlaverDialout,
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
	proxyServer.locker.Lock()
	proxyServer.sessions.PushBack(&Session{Id: "slaver-cancel", Conn: slaverConn})
	proxyServer.locker.Unlock()

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
