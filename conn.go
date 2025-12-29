// Package net 提供通用的网络连接抽象和多路复用客户端实现。
// 支持请求-响应模式的异步通信，适用于 RPC、WebSocket 等场景。
package net

import (
	"context"
	"time"
)

// Conn 定义了通用的网络连接接口。
// 实现者需要提供线程安全的读写操作。
type Conn interface {
	// Close 关闭连接并释放资源。
	// 调用后，Read 应当立即返回错误。
	Close(ctx context.Context) error

	// Read 从连接中读取一条消息。
	// 阻塞直到有数据可读或连接关闭。
	// 返回的 data 如果实现了 Notify 接口，将用于请求-响应匹配。
	Read(ctx context.Context) (data any, err error)

	// Write 向连接写入一条消息。
	// 应当是线程安全的，支持并发写入。
	Write(ctx context.Context, data any) error

	// Handle 处理无法匹配到请求的消息（如服务端主动推送）。
	// 在唯一的处理协程中调用，保证线程安全。
	Handle(ctx context.Context, data any) any
}

type ConnHeart interface {
	// Heart 返回心跳消息的数据和下次心跳的时间点。
	Heart(connect bool, count uint64) (any, time.Time)
}

// Notify 定义了可用于请求-响应匹配的消息接口。
// 当发送请求时，如果消息实现了此接口，Client 会等待具有相同 Id 的响应。
// 当接收响应时，如果消息实现了此接口，Client 会尝试匹配等待中的请求。
type Notify interface {
	// Id 返回消息的唯一标识符。
	// 请求和响应必须返回相同的 Id 才能正确匹配。
	// 返回值必须是可比较的类型（用于 map key）。
	Id() (any, bool)
}
