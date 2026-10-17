package wsproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 测试只连接回环地址；模拟待命期限即将到期后，目标拨号需要更长时间。
func TestDialDoesNotInheritPooledHeartbeatDeadline(t *testing.T) {
	server := NewServer()
	defer server.CloseAll()
	peer, entry := recoveryPeer(t, server, "")
	pong := make(chan struct{}, 1)
	peer.SetPongHandler(func(string) error { pong <- struct{}{}; return nil })
	result := make(chan error, 1)
	go func() {
		var request connPacket
		err := peer.ReadJSON(&request)
		if err == nil {
			time.Sleep(150 * time.Millisecond)
			err = peer.WriteJSON(&connPacket{Method: MethodSlaverDialoutSuccess})
		}
		result <- err
	}()
	if err := peer.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pong:
	case <-time.After(time.Second):
		t.Fatal("watcher did not read ping")
	}
	if err := entry.session.Conn.UnderlyingConn().SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	conn, err := server.DialContext(ctx, "tcp", "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("pooled heartbeat interrupted a valid dial budget: %v", err)
	}
	defer conn.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func recoveryPeer(t *testing.T, server *Server, scope string) (*websocket.Conn, *slaverEntry) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	upgraded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	t.Cleanup(upgraded.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upgraded.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	conn := <-accepted
	if !server.registerSlaverSession(scope, &Session{Id: "recovery-test", Conn: conn}) {
		t.Fatal("registration rejected")
	}
	server.locker.Lock()
	entry := server.sessions.Back().Value.(*slaverEntry)
	server.locker.Unlock()
	return peer, entry
}

// 池空时订阅补充事件；别的 token 注册不能被等待者取走。
func TestDialWaitsForSameTokenRefill(t *testing.T) {
	server := NewServer()
	server.ScopeByToken = true
	server.SessionWaitTimeout = time.Second
	defer server.CloseAll()
	result := make(chan error, 1)
	go func() {
		conn, err := server.DialContextToken(t.Context(), "tenant-a", "tcp", "127.0.0.1:1234")
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	recoveryPeer(t, server, "tenant-b")
	select {
	case err := <-result:
		t.Fatalf("dial did not wait for its tenant: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	peer, _ := recoveryPeer(t, server, "tenant-a")
	go func() {
		var packet connPacket
		if peer.ReadJSON(&packet) == nil {
			peer.WriteJSON(&connPacket{Method: MethodSlaverDialoutSuccess})
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refill did not wake waiting dial")
	}
	if server.ConnectionCountToken("tenant-b") != 1 {
		t.Fatal("cross-tenant session consumed")
	}
}

func TestPoolWaitRemainsBoundedAndCancelable(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			server := NewServer()
			server.SessionWaitTimeout = 60 * time.Millisecond
			defer server.CloseAll()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := server.DialContext(ctx, "tcp", "127.0.0.1:1234"); result <- err }()
			switch mode {
			case "cancel":
				cancel()
			case "close":
				server.CloseAll()
			}
			select {
			case err := <-result:
				want := ErrNoConnection
				if mode == "cancel" {
					want = context.Canceled
				}
				if mode == "close" {
					want = ErrServerClosed
				}
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want=%v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("pool wait leaked")
			}
		})
	}
}

func TestPoolWaitHonorsDialHandshakeBudget(t *testing.T) {
	server := NewServer()
	server.SessionWaitTimeout = time.Minute
	defer server.CloseAll()
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx, err := WithHandshakeTimeout(parent, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.DialContext(ctx, "tcp", "127.0.0.1:1234")
	if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("pool wait ignored dial budget: %v", err)
	}
}
