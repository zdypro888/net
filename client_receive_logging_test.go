package net

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// 正常结束的读失败和未请求结束的异常关闭必须按本代生命周期区分，而非按错误字符串区分。
func TestReceiveLoggingUsesGenerationStopEvidence(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	for _, mode := range []string{"teardown", "parent-cancel", "unexpected-close"} {
		t.Run(mode, func(t *testing.T) {
			var output bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stop := make(chan struct{})
			conn := newFakeConn()
			conn.Close(context.Background())
			if mode == "teardown" {
				close(stop)
			}
			if mode == "parent-cancel" {
				cancel()
			}
			client := NewClient[testMessage, *fakeConn]()
			received := make(chan testMessage)
			client.receiveGo(ctx, conn, received, stop)
			warned := strings.Contains(output.String(), "receive loop exited on read error")
			if warned != (mode == "unexpected-close") {
				t.Fatalf("wrong close classification mode=%s logs=%s", mode, output.String())
			}
		})
	}
}
