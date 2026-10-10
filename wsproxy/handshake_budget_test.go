package wsproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExplicitHandshakeBudgetIsNotCappedByDefault(t *testing.T) {
	ctx, err := WithHandshakeTimeout(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(handshakeDeadline(ctx)); remaining < 50*time.Second {
		t.Fatal("explicit budget cut by old default", remaining)
	}
	parent, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	deadline, _ := parent.Deadline()
	if handshakeDeadline(parent) != deadline {
		t.Fatal("budget extended parent deadline")
	}
}

// 反向代理注册的 Upgrade 也必须使用 context 中的显式预算。
func TestSlaverUpgradeHonorsHandshakeBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx, err := WithHandshakeTimeout(parent, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := NewSlaver().waitDialRequest(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if conn != nil {
		conn.Close()
	}
	if err == nil || parent.Err() != nil {
		t.Fatal("slaver ignored handshake budget", err)
	}
	client := NewClient("ws" + strings.TrimPrefix(server.URL, "http"))
	client.HandshakeTimeout = time.Second
	tunnel, err := client.Dial(ctx, "tcp", "example.com:443")
	if tunnel != nil {
		tunnel.Close()
	}
	if err == nil || parent.Err() != nil {
		t.Fatal("client ignored explicit context budget", err)
	}
	if err := (&Slaver{ReconnectDelay: -1}).Run(t.Context(), server.URL); err == nil {
		t.Fatal("negative retry delay accepted")
	}
}
