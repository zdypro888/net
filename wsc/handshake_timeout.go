package wsc

import (
	"context"
	"errors"
	"time"
)

type handshakeTimeoutKey struct{}

// WithHandshakeTimeout 为一次连接设置 DNS/TCP/TLS、Upgrade 和应用握手的总预算。
// 不改变客户端共享状态，父上下文较早的 deadline 仍优先；心跳和报文限制不受影响。
func WithHandshakeTimeout(ctx context.Context, timeout time.Duration) (context.Context, error) {
	if ctx == nil || timeout <= 0 {
		return ctx, errors.New("WebSocket handshake timeout must be positive")
	}
	return context.WithValue(ctx, handshakeTimeoutKey{}, timeout), nil
}

func handshakeTimeout(ctx context.Context) time.Duration {
	if timeout, ok := ctx.Value(handshakeTimeoutKey{}).(time.Duration); ok {
		return timeout
	}
	return HandshakeTimeout
}
