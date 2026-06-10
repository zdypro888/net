package wsc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zdypro888/net"
)

type testPayload struct {
	Kind  string `json:"kind"`
	Value int    `json:"value"`
}

func checkClose(t *testing.T, name string, closeFn func() error) {
	t.Helper()
	if err := closeFn(); err != nil {
		t.Logf("%s close returned: %v", name, err)
	}
}

func TestClientServerWriteAndRequest(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64)
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)
	defer checkClose(t, "client", client.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for session: %v", ctx.Err())
	}

	if err := client.Write(ctx, testPayload{Kind: "notify", Value: 7}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	select {
	case packet := <-session.Handle():
		if packet.ID != "" {
			t.Fatalf("notification should not have request id: %q", packet.ID)
		}
		if packet.Data.Kind != "notify" || packet.Data.Value != 7 {
			t.Fatalf("unexpected notification payload: %#v", packet.Data)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for notification: %v", ctx.Err())
	}

	replyDone := make(chan struct{})
	go func() {
		defer close(replyDone)
		packet := <-session.Handle()
		if packet.ID == "" {
			t.Errorf("request missing id")
			return
		}
		if err := session.Reply(context.Background(), packet.ID, testPayload{Kind: "reply", Value: packet.Data.Value + 1}); err != nil {
			t.Errorf("Reply failed: %v", err)
		}
	}()

	resp, err := client.Request(ctx, testPayload{Kind: "request", Value: 41})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if resp.Kind != "reply" || resp.Value != 42 {
		t.Fatalf("unexpected response: %#v", resp)
	}
	select {
	case <-replyDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for reply goroutine: %v", ctx.Err())
	}
}

func TestRequestTimeoutAndCloseDoNotBlock(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64)
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for session: %v", ctx.Err())
	}

	received := make(chan struct{})
	go func() {
		packet := <-session.Handle()
		if packet.ID == "" {
			t.Errorf("request missing id")
		}
		close(received)
		// Intentionally do not reply. The client-side request context must own
		// the wait, and Close must still be able to tear down the blocked read.
	}()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := client.Request(reqCtx, testPayload{Kind: "request", Value: 99})
	reqCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request error = %v, want context deadline", err)
	}

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatalf("server did not receive timed-out request")
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Close blocked after request timeout")
	}
}

func TestClientCloseDoesNotWaitForPendingRequestContext(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64)
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for session: %v", ctx.Err())
	}

	received := make(chan struct{})
	go func() {
		packet := <-session.Handle()
		if packet.ID == "" {
			t.Errorf("request missing id")
		}
		close(received)
		// Keep the request pending until client.Close tears the session down.
	}()

	requestDone := make(chan error, 1)
	go func() {
		reqCtx, reqCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer reqCancel()
		_, err := client.Request(reqCtx, testPayload{Kind: "request", Value: 100})
		requestDone <- err
	}()

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatalf("server did not receive pending request")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- client.Close()
	}()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Close blocked behind pending Request")
	}

	select {
	case err := <-requestDone:
		// Close 通过 rawconn.asyncGo 退出尾段, 给所有 asyncNotifys 填 net.ErrConnectionClosed;
		// 也可能从 wsc.Session.waitRequest 的 stopChan 分支 (ErrSessionClosed) 或 reqCtx.Done
		// (context.Canceled) 走出.
		if !errors.Is(err, ErrSessionClosed) &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, net.ErrConnectionClosed) {
			t.Fatalf("Request error = %v, want session closed / context canceled / connection closed", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("pending Request did not unblock after Close")
	}
}

func TestClientConnectAfterCloseCreatesFreshSession(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64)
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 2)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("first Connect failed: %v", err)
	}
	select {
	case <-sessionCh:
	case err := <-errCh:
		t.Fatalf("first server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for first session: %v", ctx.Err())
	}
	oldHandle := client.Handle()
	checkClose(t, "client first session", client.Close)
	for {
		select {
		case packet, ok := <-oldHandle:
			if !ok {
				goto oldHandleClosed
			}
			if !packet.Closed {
				t.Fatalf("old Handle channel produced non-close data: %#v", packet)
			}
		case <-ctx.Done():
			t.Fatalf("old Handle channel did not close: %v", ctx.Err())
		}
	}
oldHandleClosed:

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("second Connect failed: %v", err)
	}
	defer checkClose(t, "client second session", client.Close)
	newHandle := client.Handle()
	if newHandle == oldHandle {
		t.Fatalf("Handle channel was reused after Close")
	}
	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("second server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for second session: %v", ctx.Err())
	}
	if err := client.Write(ctx, testPayload{Kind: "second", Value: 2}); err != nil {
		t.Fatalf("Write after reconnect failed: %v", err)
	}
	for {
		select {
		case packet := <-session.Handle():
			if packet.Closed {
				continue
			}
			if packet.Data.Kind != "second" || packet.Data.Value != 2 {
				t.Fatalf("unexpected payload after reconnect: %#v", packet.Data)
			}
			return
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reconnect payload: %v", ctx.Err())
		}
	}
}

func TestServerRemovesDisconnectedSessionAfterIdleTimeout(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64, WithSessionIdleTimeout(20*time.Millisecond))
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for session: %v", ctx.Err())
	}
	guid := session.GUID()
	if server.GetSession(guid) == nil {
		t.Fatalf("server did not retain connected session %q", guid)
	}

	checkClose(t, "client", client.Close)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if server.GetSession(guid) == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("session %q was not removed after idle timeout: %v", guid, ctx.Err())
		}
	}
}

func TestServerIdleCleanupDoesNotRemoveAdvancedGeneration(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64, WithSessionIdleTimeout(20*time.Millisecond))
	defer checkClose(t, "server", server.Close)

	session := createSessionWithBuffer[testPayload]("reconnecting-guid", 64)
	session.setOnDisconnect(server.scheduleSessionCleanup)
	server.locker.Lock()
	server.sessions[session.guid] = session
	server.locker.Unlock()

	oldGeneration := session.generation()
	server.scheduleSessionCleanup(session, oldGeneration)
	session.advanceGeneration()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if server.GetSession(session.guid) == nil {
			t.Fatalf("stale idle cleanup removed advanced-generation session")
		}
		server.locker.Lock()
		_, pendingCleanup := server.cleanupTimers[session.guid]
		server.locker.Unlock()
		if !pendingCleanup {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("stale idle cleanup timer was not cleared: %v", ctx.Err())
		}
	}
}

// 取消 idle cleanup 即权威: 一个"已触发但回调尚未执行"的旧 cleanup 在取消之后才跑到
// expireSession, 必须 no-op, 不能误删 session (此前由预推进 connGeneration 保证, 现由
// expireSession 的 timer 存在性门控保证)。
func TestServerCanceledCleanupDoesNotExpireSession(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64, WithSessionIdleTimeout(time.Hour))
	defer checkClose(t, "server", server.Close)

	session := createSessionWithBuffer[testPayload]("canceled-cleanup-guid", 64)
	session.setOnDisconnect(server.scheduleSessionCleanup)
	server.locker.Lock()
	server.sessions[session.guid] = session
	server.locker.Unlock()

	generation := session.generation()
	server.scheduleSessionCleanup(session, generation)
	if !server.cancelSessionCleanup(session.guid) {
		t.Fatalf("expected pending cleanup to be canceled")
	}
	// 模拟已触发的旧 timer 在取消之后才执行回调:
	server.expireSession(session, generation)
	if server.GetSession(session.guid) == nil {
		t.Fatalf("canceled cleanup wrongly expired session")
	}
}

// 复用 session 的重连若在空闲待清理状态下失败, 取消掉的 idle cleanup 必须被补回, 否则该
// session 永不回收。覆盖 OnConnection 失败路径里 hadIdleCleanup 分支的不泄漏保证。
func TestServerReconnectFailureReschedulesIdleCleanup(t *testing.T) {
	// idleTimeout 取 200ms 而非 20ms: 初始 idle timer 若在客户端握手往返(loopback +
	// JSON 上下行)期间就到期, 会抢在 OnConnection 取消它之前删掉 stale session, 使
	// OnConnection 走 created 分支并返回 nil, 令断言 ErrSessionClosed 误判失败。200ms
	// 远大于 loopback 握手耗时, 又仍落在结尾 1s 轮询窗口内, 保留 hadIdleCleanup 补排路径。
	server := NewServerWithBuffer[testPayload](64, WithSessionIdleTimeout(200*time.Millisecond))
	defer checkClose(t, "server", server.Close)

	upgrader := websocket.Upgrader{}
	session := createSessionWithBuffer[testPayload]("failed-reconnect-guid", 64)
	session.setOnDisconnect(server.scheduleSessionCleanup)
	server.locker.Lock()
	server.sessions[session.guid] = session
	server.locker.Unlock()

	// session 已空闲挂着 idle cleanup; 重连到来会先取消它。
	server.scheduleSessionCleanup(session, session.generation())
	checkClose(t, "stale session", session.Close)

	onConnectionErr := make(chan error, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			onConnectionErr <- err
			return
		}
		_, err = server.OnConnection(conn, nil)
		onConnectionErr <- err
	}))
	defer httpServer.Close()

	wsConn, _, err := websocket.DefaultDialer.Dial("ws"+httpServer.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer checkClose(t, "client websocket", wsConn.Close)
	if err := wsConn.WriteJSON(HandshakeRequest{
		GUID:    session.guid,
		Version: ProtocolVersion,
		Codecs:  []string{CodecJSON},
	}); err != nil {
		t.Fatalf("WriteJSON handshake failed: %v", err)
	}
	var resp HandshakeResponse
	if err := wsConn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON handshake response failed: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("handshake status = %d, want 200", resp.Status)
	}
	select {
	case err := <-onConnectionErr:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("OnConnection error = %v, want ErrSessionClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("OnConnection did not return after reset failure")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if server.GetSession(session.guid) == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("reused session was not cleaned after failed reconnect: %v", ctx.Err())
		}
	}
}

func TestServerDisconnectCoalescesBlockedClosedPacketDelivery(t *testing.T) {
	server := NewServerWithBuffer[testPayload](1, WithSessionIdleTimeout(time.Hour))
	defer checkClose(t, "server", server.Close)

	session := createSessionWithBuffer[testPayload]("blocked-closed-consumer-guid", 1)
	session.setOnDisconnect(server.scheduleSessionCleanup)
	server.locker.Lock()
	server.sessions[session.guid] = session
	server.locker.Unlock()

	session.handchan <- &Packet[testPayload]{Data: testPayload{Kind: "queued"}}
	done := make(chan struct{}, 2)
	for range 2 {
		msgchan := make(chan *messagechannel[testPayload])
		close(msgchan)
		go func() {
			session.handleMessageGo(session.handchan, msgchan, session.stopChan, session.generation())
			done <- struct{}{}
		}()
	}

	hasCleanupTimer := func() bool {
		server.locker.Lock()
		defer server.locker.Unlock()
		_, ok := server.cleanupTimers[session.guid]
		return ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !hasCleanupTimer() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("disconnect did not schedule cleanup while Closed packet consumer was blocked: %v", ctx.Err())
		}
	}
	for !session.closedSignal.Load() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("disconnect did not leave a pending Closed signal while consumer was blocked: %v", ctx.Err())
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("second concurrent disconnect did not coalesce while first Closed send was blocked: %v", ctx.Err())
	}

	packet := <-session.handchan
	if packet.Closed {
		t.Fatalf("first queued packet should not be the synthetic Closed packet")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("blocked handleMessageGo did not finish after consumer resumed: %v", ctx.Err())
	}
}

// TestServerDisconnectClosedSignalResetsAfterDelivery 锁定: server 端补发 Closed 投递成功
// 后必须无条件复位 closedSignal, 否则一旦投递时 handchan 恰满(小 buffer 下首次断线即满),
// 信号会永久卡 true, 该 session 后续所有断线的 Closed 都被合并丢弃。bufferSize=1: 投递一个
// Closed 即把 handchan(cap 1)填满, len<cap 永远为假, 是该卡死的确定性复现。
func TestServerDisconnectClosedSignalResetsAfterDelivery(t *testing.T) {
	server := NewServerWithBuffer[testPayload](1, WithSessionIdleTimeout(time.Hour))
	defer checkClose(t, "server", server.Close)

	session := createSessionWithBuffer[testPayload]("closed-signal-reset-guid", 1)
	session.setOnDisconnect(server.scheduleSessionCleanup)
	server.locker.Lock()
	server.sessions[session.guid] = session
	server.locker.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	runDisconnect := func() {
		msgchan := make(chan *messagechannel[testPayload])
		close(msgchan)
		done := make(chan struct{})
		go func() {
			session.handleMessageGo(session.handchan, msgchan, session.stopChan, session.generation())
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("handleMessageGo did not finish: %v", ctx.Err())
		}
	}

	runDisconnect()
	first := <-session.handchan
	if !first.Closed {
		t.Fatalf("first disconnect should deliver a Closed packet, got %#v", first)
	}
	// 消费掉首个 Closed 后(server 端无 client.go 那样的消费即复位钩子), 第二次断线必须仍能投递
	// 一个新的 Closed —— 若 closedSignal 卡在 true, forwardClosed 的 CAS 会失败而静默丢弃。
	runDisconnect()
	select {
	case second := <-session.handchan:
		if !second.Closed {
			t.Fatalf("second disconnect should deliver a Closed packet, got %#v", second)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("second disconnect delivered no Closed packet: closedSignal stuck true after first delivery")
	}
}

func TestWSConnectionCloseDoesNotBlockWhenMessageChannelIsFull(t *testing.T) {
	upgrader := websocket.Upgrader{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer checkClose(t, "websocket conn", conn.Close)
		<-r.Context().Done()
	}))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+httpServer.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	wsConn := createWSConnection[testPayload](conn, 1, defaultCodec)
	wsConn.msgchan <- &messagechannel[testPayload]{Message: &Message[testPayload]{Data: testPayload{Kind: "queued"}}}

	done := make(chan error, 1)
	go func() {
		done <- wsConn.Close(context.Background())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Close blocked on full message channel")
	}
}

// TestServerSessionDirectCloseRemovesTableEntry 回归 D4:
// 调用方拿到 OnConnection 返回的 Session 后直接 session.Close() (不经
// server.RemoveSession) 时, 旧实现 server.sessions[guid] 滞留到 idle-timeout;
// WithSessionIdleTimeout(0) (禁用空闲清理) 下则永不回收. 修复后 Session.Close
// 通过 onClose 回调摘除表项, 与 idle-timeout 配置无关.
func TestServerSessionDirectCloseRemovesTableEntry(t *testing.T) {
	server := NewServerWithBuffer[testPayload](8, WithSessionIdleTimeout(0))
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			return
		}
		select {
		case sessionCh <- session:
		default:
		}
	}))
	defer httpServer.Close()
	defer checkClose(t, "server", server.Close)

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 8)
	defer checkClose(t, "client", client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server session not established")
	}
	guid := session.GUID()
	if server.GetSession(guid) != session {
		t.Fatal("session not registered in server table")
	}

	checkClose(t, "session", session.Close)
	if got := server.GetSession(guid); got != nil {
		t.Fatalf("server table still holds session %q after direct Close; zombie entry", guid)
	}
}
