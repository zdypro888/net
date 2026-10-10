package wsc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 构造预算确实限制 HTTP Upgrade，并拒绝非法选项。
func TestClientBudgetLimitsUpgrade(t *testing.T) {
	for _, p := range []Budgets{{HandshakeTimeout: -1}, {WriteTimeout: -1}, {ReconnectDelay: -1}} {
		if _, err := WithBudgets(p); err == nil {
			t.Fatal("negative budget accepted")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	option, err := WithBudgets(Budgets{HandshakeTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient[string]("ws"+strings.TrimPrefix(server.URL, "http"), option)
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("constructor handshake budget did not precede caller deadline", err)
	}
}

// 服务端等待应用握手采用自己的可配置预算，不依赖客户端取消。
func TestServerBudgetLimitsApplicationHandshake(t *testing.T) {
	option, err := WithBudgets(Budgets{HandshakeTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	wsServer := NewServer[string](option)
	defer wsServer.Close()
	done := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_, err = wsServer.OnConnection(conn, nil)
		}
		done <- err
	}))
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing application handshake accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("server ignored handshake budget")
	}
}

// 对端停止读取时，每帧写预算必须贯穿 Client→Session→wsconnection。
func TestClientBudgetLimitsBlockedFrameWrite(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var request HandshakeRequest
		if conn.ReadJSON(&request) != nil {
			return
		}
		if conn.WriteJSON(HandshakeResponse{Status: http.StatusOK, Codec: CodecJSON}) != nil {
			return
		}
		<-release
	}))
	option, err := WithBudgets(Budgets{WriteTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient[string]("ws"+strings.TrimPrefix(server.URL, "http"), option)
	defer func() { client.Close(); close(release); server.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	// Client.Write 只确认入队；这里验证从构造选项传到底层连接的实际 I/O 预算。
	connection := client.session.wsconn.Load()
	if connection == nil {
		t.Fatal("missing connected websocket")
	}
	if err := connection.Write(ctx, &Message[string]{Data: strings.Repeat("x", 16<<20)}); err == nil || ctx.Err() != nil {
		t.Fatal("frame write ignored configured budget", err)
	}
}
