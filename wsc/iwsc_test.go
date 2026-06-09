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
