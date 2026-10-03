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

// 真实 Upgrade 验证请求头快照及重连，防止认证选项只在首次连接生效。
func TestHandshakeHeadersSnapshotAndReconnect(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer fixture"}}
	option := WithHandshakeHeaders(headers)
	headers.Set("Authorization", "Bearer changed")
	server := NewServer[string]()
	defer server.Close()
	received := make(chan string, 2)
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _ = server.OnConnection(conn, nil)
	}))
	defer handler.Close()
	client := NewClient[string]("ws"+strings.TrimPrefix(handler.URL, "http"), option)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := client.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-received:
			if got != "Bearer fixture" {
				t.Fatal("handshake header snapshot changed")
			}
		case <-ctx.Done():
			t.Fatal("missing Upgrade")
		}
	}
}
