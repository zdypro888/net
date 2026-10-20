package wsproxy

import (
	"context"
	"errors"
	"github.com/zdypro888/net/waitstate"
	"testing"
	"time"
)

func TestPoolWaitReportsOnceAndClearsOnCancellation(t *testing.T) {
	server := NewServer()
	defer server.CloseAll()
	server.SessionWaitTimeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events []string
	ctx = waitstate.Observe(ctx, func(text string) {
		events = append(events, text)
		if text != "" {
			cancel()
		}
	})
	_, err := server.waitSession(ctx, "private-tenant-token")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "等待代理客户端提供可用连接" || events[1] != "" {
		t.Fatalf("events=%v", events)
	}
}
